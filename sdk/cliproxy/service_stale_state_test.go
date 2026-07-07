package cliproxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestServiceApplyCoreAuthAddOrUpdate_DeleteReAddDoesNotInheritStaleRuntimeState(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	authID := "service-stale-state-auth"
	modelID := "stale-model"
	lastRefreshedAt := time.Date(2026, time.March, 1, 8, 0, 0, 0, time.UTC)
	nextRefreshAfter := lastRefreshedAt.Add(30 * time.Minute)

	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:               authID,
		Provider:         "claude",
		Status:           coreauth.StatusActive,
		LastRefreshedAt:  lastRefreshedAt,
		NextRefreshAfter: nextRefreshAfter,
		ModelStates: map[string]*coreauth.ModelState{
			modelID: {
				Quota: coreauth.QuotaState{BackoffLevel: 7},
			},
		},
	})

	service.applyCoreAuthRemoval(context.Background(), authID)

	if _, ok := service.coreManager.GetByID(authID); ok {
		t.Fatalf("expected auth %q to be removed from runtime state", authID)
	}

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "claude",
		Status:   coreauth.StatusActive,
	})

	updated, ok := service.coreManager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("expected re-added auth to be present")
	}
	if updated.Disabled {
		t.Fatalf("expected re-added auth to be active")
	}
	if !updated.LastRefreshedAt.IsZero() {
		t.Fatalf("expected LastRefreshedAt to reset on delete -> re-add, got %v", updated.LastRefreshedAt)
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("expected NextRefreshAfter to reset on delete -> re-add, got %v", updated.NextRefreshAfter)
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected ModelStates to reset on delete -> re-add, got %d entries", len(updated.ModelStates))
	}
	if models := registry.GetGlobalRegistry().GetModelsForClient(authID); len(models) == 0 {
		t.Fatalf("expected re-added auth to re-register models in global registry")
	}
}

func TestHandleAuthUpdate_ReplaceMaterialClearsRuntimeErrorState(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	ctx := context.Background()
	authID := "codex-user@example.com.json"
	modelID := "gpt-5"

	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})

	if _, errRegister := service.coreManager.Register(ctx, &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	service.coreManager.MarkResult(ctx, coreauth.Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    modelID,
		Success:  false,
		Error: &coreauth.Error{
			Code:       "unauthorized",
			Message:    "401 Your authentication token has been invalidated",
			HTTPStatus: http.StatusUnauthorized,
		},
	})

	failedAuth, ok := service.coreManager.GetByID(authID)
	if !ok || failedAuth == nil {
		t.Fatalf("expected auth %q to exist", authID)
	}
	if len(failedAuth.ModelStates) == 0 || failedAuth.LastError == nil {
		t.Fatalf("expected runtime error state before replacement, got %+v", failedAuth)
	}

	service.handleAuthUpdate(ctx, watcher.AuthUpdate{
		Action:          watcher.AuthUpdateActionModify,
		ID:              authID,
		Auth:            &coreauth.Auth{ID: authID, Provider: "codex", Status: coreauth.StatusActive},
		ReplaceMaterial: true,
	})

	updated, ok := service.coreManager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("expected updated auth %q to exist", authID)
	}
	if updated.Status != coreauth.StatusActive {
		t.Fatalf("status = %q, want %q", updated.Status, coreauth.StatusActive)
	}
	if updated.LastError != nil || updated.StatusMessage != "" || updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("runtime error state survived replacement: status_message=%q unavailable=%v next=%v last_error=%+v",
			updated.StatusMessage, updated.Unavailable, updated.NextRetryAfter, updated.LastError)
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("model states survived replacement: %+v", updated.ModelStates)
	}
}

func TestForceHomeRuntimeConfigEnablesUsageStatistics(t *testing.T) {
	cfg := &config.Config{
		UsageStatisticsEnabled: false,
		SaveCooldownStatus:     true,
	}

	forceHomeRuntimeConfig(cfg)

	if !cfg.UsageStatisticsEnabled {
		t.Fatal("expected home runtime config to force usage statistics enabled")
	}
	if cfg.SaveCooldownStatus {
		t.Fatal("expected home runtime config to force cooldown status persistence disabled")
	}
}

func TestApplyHomeOverlayForcesUsageStatisticsEnabled(t *testing.T) {
	baseCfg := &config.Config{}
	baseCfg.Home.Enabled = true
	service := &Service{cfg: baseCfg}

	service.applyHomeOverlay(&config.Config{
		UsageStatisticsEnabled: false,
		SaveCooldownStatus:     true,
	})

	if service.cfg == nil || !service.cfg.UsageStatisticsEnabled {
		t.Fatal("expected home overlay to force usage statistics enabled")
	}
	if !service.cfg.Home.Enabled {
		t.Fatal("expected home overlay to preserve local home settings")
	}
	if service.cfg.SaveCooldownStatus {
		t.Fatal("expected home overlay to force cooldown status persistence disabled")
	}
}
