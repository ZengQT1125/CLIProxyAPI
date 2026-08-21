package auth

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// SetRetryConfig updates additional credential retry rounds, the per-round credential limit, and the cooldown wait interval.
func (m *Manager) SetRetryConfig(retry int, maxRetryInterval time.Duration, maxRetryCredentials int) {
	if m == nil {
		return
	}
	if retry < 0 {
		retry = 0
	}
	if maxRetryCredentials < 0 {
		maxRetryCredentials = 0
	}
	if maxRetryInterval < 0 {
		maxRetryInterval = 0
	}
	m.requestRetry.Store(int32(retry))
	m.maxRetryCredentials.Store(int32(maxRetryCredentials))
	m.maxRetryInterval.Store(maxRetryInterval.Nanoseconds())
}

// RegisterExecutor registers a provider executor with the manager.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	if executor == nil {
		return
	}
	provider := strings.TrimSpace(executor.Identifier())
	if provider == "" {
		return
	}

	var replaced ProviderExecutor
	m.mu.Lock()
	replaced = m.executors[provider]
	m.executors[provider] = executor
	m.mu.Unlock()

	if replaced == nil || replaced == executor {
		return
	}
	if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
		closer.CloseExecutionSession(CloseAllExecutionSessionsID)
	}
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.mu.Lock()
	delete(m.executors, provider)
	m.mu.Unlock()
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return nil, fmt.Errorf("register auth: %w", errWeight)
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	unlockLifecycle := m.lockAuthLifecycle(auth.ID)
	now := time.Now()
	m.mu.Lock()
	_, deferCooldownCleanup := m.cooldownRestoreIDs[auth.ID]
	m.applyPendingCooldownRestoreLocked(auth, now)
	cooldownStateChanged := normalizeModelStates(auth)
	if m.cooldownDisabledForAuth(auth) || auth.Disabled || auth.Status == StatusDisabled {
		cooldownStateChanged = clearCooldownStateForAuth(auth, now) || cooldownStateChanged
	}
	auth.EnsureIndex()
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	m.queueRefreshReschedule(auth.ID)
	_ = m.persist(ctx, auth)
	registered := auth.Clone()
	if cooldownStateChanged && !deferCooldownCleanup {
		m.queueCooldownStatePersist(auth.ID)
	}
	unlockLifecycle()
	m.hook.OnAuthRegistered(ctx, registered.Clone())
	return registered, nil
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return nil, fmt.Errorf("update auth: %w", errWeight)
	}
	unlockLifecycle := m.lockAuthLifecycle(auth.ID)
	var resumeModels []string
	cooldownStateChanged := false
	m.mu.Lock()
	existing, ok := m.auths[auth.ID]
	if !ok || existing == nil {
		m.mu.Unlock()
		unlockLifecycle()
		return nil, nil
	}
	now := time.Now()
	var cooldownRecordsBefore []CooldownStateRecord
	trackCooldownState := m.cooldownStore != nil
	if trackCooldownState {
		cooldownRecordsBefore = m.cooldownStateRecordsForAuthLocked(existing, now)
	}
	resetRuntimeState := shouldResetRuntimeStateOnUpdate(ctx) &&
		!samePersistedAuthMaterial(existing, auth) &&
		!auth.Disabled && auth.Status != StatusDisabled
	if !auth.indexAssigned && auth.Index == "" {
		auth.Index = existing.Index
		auth.indexAssigned = existing.indexAssigned
	}
	auth.Success = existing.Success
	auth.Failed = existing.Failed
	auth.recentRequests = existing.recentRequests
	if resetRuntimeState {
		resumeModels = runtimeStateModelKeys(existing)
		resetRuntimeAvailabilityState(auth, now)
	} else if !existing.Disabled && existing.Status != StatusDisabled && !auth.Disabled && auth.Status != StatusDisabled {
		if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
			auth.ModelStates = existing.ModelStates
		}
		if existing.Quota.Exceeded && existing.Quota.Reason == "credential_quota" && existing.Quota.NextRecoverAt.After(time.Now()) {
			auth.Unavailable = existing.Unavailable
			auth.NextRetryAfter = existing.NextRetryAfter
			auth.Quota = existing.Quota
			if auth.Status == StatusActive {
				auth.Status = existing.Status
			}
		}
	}
	_, deferCooldownCleanup := m.cooldownRestoreIDs[auth.ID]
	m.applyPendingCooldownRestoreLocked(auth, now)
	cooldownStateChanged = normalizeModelStates(auth) || cooldownStateChanged
	if m.cooldownDisabledForAuth(auth) || auth.Disabled || auth.Status == StatusDisabled {
		cooldownStateChanged = clearCooldownStateForAuth(auth, now) || cooldownStateChanged
	}
	auth.EnsureIndex()
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	if trackCooldownState {
		cooldownRecordsAfter := m.cooldownStateRecordsForAuthLocked(authClone, now)
		cooldownStateChanged = !cooldownStateRecordsEqual(cooldownRecordsBefore, cooldownRecordsAfter)
	}
	m.mu.Unlock()
	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	m.queueRefreshReschedule(auth.ID)
	_ = m.persist(ctx, auth)
	updated := auth.Clone()
	if !deferCooldownCleanup && cooldownStateChanged {
		m.queueCooldownStatePersist(auth.ID)
	}
	for _, model := range resumeModels {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(auth.ID, model)
		registry.GetGlobalRegistry().ResumeClientModel(auth.ID, model)
	}
	unlockLifecycle()
	m.hook.OnAuthUpdated(ctx, updated.Clone())
	return updated, nil
}

func resetRuntimeAvailabilityState(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	if auth.Status == "" || auth.Status == StatusUnknown || auth.Status == StatusError {
		auth.Status = StatusActive
	}
	auth.StatusMessage = ""
	auth.Quota = QuotaState{}
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.ModelStates = nil
	auth.UpdatedAt = now
}

func runtimeStateModelKeys(auth *Auth) []string {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(auth.ModelStates))
	models := make([]string, 0, len(auth.ModelStates))
	for model := range auth.ModelStates {
		modelKey := canonicalModelKey(model)
		if modelKey == "" {
			modelKey = strings.TrimSpace(model)
		}
		if modelKey == "" {
			continue
		}
		if _, ok := seen[modelKey]; ok {
			continue
		}
		seen[modelKey] = struct{}{}
		models = append(models, modelKey)
	}
	sort.Strings(models)
	return models
}

// Remove deletes an auth from runtime state without persisting.
// Disk and token-store deletion must be handled by the caller.
func (m *Manager) Remove(ctx context.Context, id string) {
	if m == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	unlockLifecycle := m.lockAuthLifecycle(id)
	defer unlockLifecycle()
	m.removeRuntime(ctx, id)
}

// RemoveIf deletes the current auth only when matches accepts the lifecycle-locked snapshot.
// beforeRemove runs under the same lifecycle lock and must complete any external deletion first.
func (m *Manager) RemoveIf(ctx context.Context, id string, matches func(*Auth) bool, beforeRemove func(*Auth) error) (bool, error) {
	if m == nil {
		return false, nil
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false, nil
	}

	unlockLifecycle := m.lockAuthLifecycle(id)
	defer unlockLifecycle()

	m.mu.RLock()
	current := m.auths[id]
	if current != nil {
		current = current.Clone()
	}
	m.mu.RUnlock()
	if current == nil || (matches != nil && !matches(current)) {
		return false, nil
	}
	if beforeRemove != nil {
		if errRemove := beforeRemove(current.Clone()); errRemove != nil {
			return false, errRemove
		}
	}
	m.removeRuntime(ctx, id)
	return true, nil
}

func (m *Manager) removeRuntime(ctx context.Context, id string) {
	_ = ctx

	m.mu.Lock()
	existing := m.auths[id]
	if existing == nil {
		m.mu.Unlock()
		return
	}
	provider := strings.TrimSpace(existing.Provider)
	delete(m.auths, id)
	if m.modelPoolOffsets != nil {
		delete(m.modelPoolOffsets, id)
	}
	for sessionID, sessionAuths := range m.homeRuntimeAuths {
		if sessionAuths == nil {
			continue
		}
		delete(sessionAuths, id)
		if len(sessionAuths) == 0 {
			delete(m.homeRuntimeAuths, sessionID)
		}
	}
	m.mu.Unlock()

	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.removeAuth(id)
	}
	m.queueRefreshUnschedule(id)
	m.invalidateSessionAffinity(id)

	if provider != "" {
		if exec, ok := m.Executor(provider); ok && exec != nil {
			if closer, okCloser := exec.(ExecutionSessionCloser); okCloser {
				closer.CloseExecutionSession(CloseAllExecutionSessionsID)
			}
		}
	}
	m.queueCooldownStatePersist(id)
}

func (m *Manager) invalidateSessionAffinity(authID string) {
	if m == nil || authID == "" {
		return
	}
	if invalidator, ok := m.selector.(interface{ InvalidateAuth(string) }); ok && invalidator != nil {
		invalidator.InvalidateAuth(authID)
	}
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	if m.store == nil {
		m.mu.Unlock()
		return nil
	}
	items, err := m.store.List(ctx)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.auths = make(map[string]*Auth, len(items))
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		if errWeight := ValidateAuthWeight(auth); errWeight != nil {
			continue
		}
		auth.EnsureIndex()
		m.auths[auth.ID] = auth.Clone()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	m.mu.Unlock()
	m.syncScheduler()
	return nil
}

func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	if m.store == nil || auth == nil {
		return nil
	}
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return fmt.Errorf("persist auth: %w", errWeight)
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if IsConfigAPIKeyAuth(auth) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	if IsPluginVirtualAuth(auth) {
		return nil
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
	if auth.Metadata == nil {
		return nil
	}
	_, err := m.store.Save(ctx, auth)
	return err
}
