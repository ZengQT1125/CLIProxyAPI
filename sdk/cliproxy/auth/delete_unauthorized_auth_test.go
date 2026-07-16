package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type deleteTrackingStore struct {
	mu         sync.Mutex
	deletedIDs []string
}

func (s *deleteTrackingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *deleteTrackingStore) Save(context.Context, *Auth) (string, error) { return "", nil }

func (s *deleteTrackingStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletedIDs = append(s.deletedIDs, id)
	return nil
}

func (s *deleteTrackingStore) deleted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deletedIDs...)
}

type blockingDeleteStore struct {
	mu            sync.Mutex
	auths         map[string]*Auth
	deleteStarted chan struct{}
	allowDelete   chan struct{}
	deleteOnce    sync.Once
}

func newBlockingDeleteStore() *blockingDeleteStore {
	return &blockingDeleteStore{
		auths:         make(map[string]*Auth),
		deleteStarted: make(chan struct{}),
		allowDelete:   make(chan struct{}),
	}
}

func (s *blockingDeleteStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *blockingDeleteStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	s.auths[auth.ID] = auth.Clone()
	s.mu.Unlock()
	return auth.ID, nil
}

func (s *blockingDeleteStore) Delete(_ context.Context, id string) error {
	s.deleteOnce.Do(func() { close(s.deleteStarted) })
	<-s.allowDelete
	s.mu.Lock()
	delete(s.auths, id)
	s.mu.Unlock()
	return nil
}

func (s *blockingDeleteStore) auth(id string) (*Auth, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	auth, ok := s.auths[id]
	return auth.Clone(), ok
}

type refreshOAuthError struct {
	status  int
	code    string
	message string
}

func (e *refreshOAuthError) Error() string {
	return fmt.Sprintf("token refresh failed with status %d: %s", e.status, e.message)
}

func (e *refreshOAuthError) StatusCode() int { return e.status }

func (e *refreshOAuthError) OAuthErrorCode() string { return e.code }

type credentialProbeHTTPError struct {
	status int
	body   string
}

func (e *credentialProbeHTTPError) Error() string { return e.body }

func (e *credentialProbeHTTPError) StatusCode() int { return e.status }

type oauthRefreshFailureExecutor struct {
	schedulerProviderTestExecutor
	err   error
	calls atomic.Int32
}

func (e *oauthRefreshFailureExecutor) Refresh(context.Context, *Auth) (*Auth, error) {
	e.calls.Add(1)
	return nil, e.err
}

type probingOAuthRefreshFailureExecutor struct {
	*oauthRefreshFailureExecutor
	executeErr error
	probeErr   error
	probeCalls atomic.Int32
}

func (e *probingOAuthRefreshFailureExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.executeErr
}

func (e *probingOAuthRefreshFailureExecutor) ProbeCredential(context.Context, *Auth) error {
	e.probeCalls.Add(1)
	return e.probeErr
}

func registerExpiredRefreshAuth(t *testing.T, manager *Manager, id, provider string) {
	t.Helper()
	auth := &Auth{
		ID:       id,
		Provider: provider,
		Metadata: map[string]any{
			"access_token":             "expired-access-token",
			"refresh_token":            "refresh-token",
			"expired":                  time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			"refresh_interval_seconds": 1,
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
}

func waitForAuthCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestManagerMarkResultKeepsUnauthorizedAuthWhenDeleteDisabled(t *testing.T) {
	SetDeleteUnauthorizedAuth(false)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:  "auth-1",
		Model:   "gpt-5",
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
	})

	if _, ok := manager.GetByID("auth-1"); !ok {
		t.Fatal("expected auth to remain registered")
	}
	if got := store.deleted(); len(got) != 0 {
		t.Fatalf("expected no deleted auths, got %v", got)
	}
}

func TestManagerMarkResultDeletesUnauthorizedAuthWhenEnabled(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:  "auth-1",
		Model:   "gpt-5",
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
	})

	if _, ok := manager.GetByID("auth-1"); ok {
		t.Fatal("expected auth to be removed from manager")
	}
	if got := store.deleted(); len(got) != 1 || got[0] != "auth-1" {
		t.Fatalf("expected auth-1 to be deleted, got %v", got)
	}
}

func TestManagerAutoRefreshKeepsInvalidGrantWhenConversationProbeSucceeds(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &probingOAuthRefreshFailureExecutor{
		oauthRefreshFailureExecutor: &oauthRefreshFailureExecutor{
			schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
			err:                           &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_grant", message: "Refresh token has been revoked"},
		},
	}
	manager.RegisterExecutor(executor)
	registerExpiredRefreshAuth(t, manager, "xai-access-token-valid", "xai")

	manager.StartAutoRefresh(context.Background(), time.Millisecond)
	t.Cleanup(manager.StopAutoRefresh)
	waitForAuthCondition(t, func() bool { return executor.probeCalls.Load() == 1 }, "credential conversation probe")

	retained, ok := manager.GetByID("xai-access-token-valid")
	if !ok || retained == nil {
		t.Fatal("expected auth with working access token to remain registered")
	}
	if retained.Unavailable || retained.Status != StatusActive {
		t.Fatalf("retained auth state = unavailable:%v status:%s, want active", retained.Unavailable, retained.Status)
	}
	if retained.LastError == nil || retained.LastError.Code != "invalid_grant" {
		t.Fatalf("retained LastError = %#v, want invalid_grant refresh failure", retained.LastError)
	}
	if got := store.deleted(); len(got) != 0 {
		t.Fatalf("deleted auths = %v, want none", got)
	}
}

func TestManagerAutoRefreshDeletesInvalidGrantWhenConversationProbeIsUnauthorized(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &probingOAuthRefreshFailureExecutor{
		oauthRefreshFailureExecutor: &oauthRefreshFailureExecutor{
			schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
			err:                           &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_grant", message: "Refresh token has been revoked"},
		},
		probeErr: &refreshOAuthError{status: http.StatusUnauthorized, message: "access token rejected"},
	}
	manager.RegisterExecutor(executor)
	registerExpiredRefreshAuth(t, manager, "xai-revoked", "xai")

	manager.StartAutoRefresh(context.Background(), time.Millisecond)
	t.Cleanup(manager.StopAutoRefresh)
	waitForAuthCondition(t, func() bool { return len(store.deleted()) == 1 }, "revoked auth deletion")

	if _, ok := manager.GetByID("xai-revoked"); ok {
		t.Fatal("expected revoked auth to be removed from manager")
	}
	if got := store.deleted(); len(got) != 1 || got[0] != "xai-revoked" {
		t.Fatalf("deleted auths = %v, want [xai-revoked]", got)
	}
	if got := executor.probeCalls.Load(); got != 1 {
		t.Fatalf("conversation probe calls = %d, want 1", got)
	}
}

func TestManagerAutoRefreshDeletesXAIWhenConversationProbeReturnsPermissionDenied(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &probingOAuthRefreshFailureExecutor{
		oauthRefreshFailureExecutor: &oauthRefreshFailureExecutor{
			schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
			err:                           &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_grant", message: "Refresh token has been revoked"},
		},
		probeErr: &credentialProbeHTTPError{
			status: http.StatusForbidden,
			body:   `{"code":"permission-denied","error":"Access to the chat endpoint is denied."}`,
		},
	}
	manager.RegisterExecutor(executor)
	registerExpiredRefreshAuth(t, manager, "xai-permission-denied", "xai")

	manager.StartAutoRefresh(context.Background(), time.Millisecond)
	t.Cleanup(manager.StopAutoRefresh)
	waitForAuthCondition(t, func() bool { return len(store.deleted()) == 1 }, "permission-denied auth deletion")

	if _, ok := manager.GetByID("xai-permission-denied"); ok {
		t.Fatal("expected permission-denied xai auth to be removed from manager")
	}
	if got := store.deleted(); len(got) != 1 || got[0] != "xai-permission-denied" {
		t.Fatalf("deleted auths = %v, want [xai-permission-denied]", got)
	}
	if got := executor.probeCalls.Load(); got != 1 {
		t.Fatalf("conversation probe calls = %d, want 1", got)
	}
}

func TestManagerAutoRefreshKeepsNonXAIWhenConversationProbeReturnsPermissionDenied(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &probingOAuthRefreshFailureExecutor{
		oauthRefreshFailureExecutor: &oauthRefreshFailureExecutor{
			schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
			err:                           &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_grant", message: "Refresh token has been revoked"},
		},
		probeErr: &credentialProbeHTTPError{
			status: http.StatusForbidden,
			body:   `{"code":"permission-denied","error":"Access to the chat endpoint is denied."}`,
		},
	}
	manager.RegisterExecutor(executor)
	registerExpiredRefreshAuth(t, manager, "codex-permission-denied", "codex")

	manager.StartAutoRefresh(context.Background(), time.Millisecond)
	t.Cleanup(manager.StopAutoRefresh)
	waitForAuthCondition(t, func() bool { return executor.probeCalls.Load() == 1 }, "non-xai permission-denied credential probe")

	retained, ok := manager.GetByID("codex-permission-denied")
	if !ok || retained == nil {
		t.Fatal("expected non-xai auth to remain after permission-denied probe")
	}
	if !retained.Unavailable || retained.Status != StatusError {
		t.Fatalf("retained auth state = unavailable:%v status:%s, want terminal error", retained.Unavailable, retained.Status)
	}
	if got := store.deleted(); len(got) != 0 {
		t.Fatalf("deleted auths = %v, want none", got)
	}
}

func TestManagerAutoRefreshKeepsInvalidGrantWhenConversationProbeIsInconclusive(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &probingOAuthRefreshFailureExecutor{
		oauthRefreshFailureExecutor: &oauthRefreshFailureExecutor{
			schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
			err:                           &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_grant", message: "Refresh token has been revoked"},
		},
		probeErr: &refreshOAuthError{status: http.StatusServiceUnavailable, message: "temporary upstream failure"},
	}
	manager.RegisterExecutor(executor)
	registerExpiredRefreshAuth(t, manager, "xai-probe-inconclusive", "xai")

	manager.StartAutoRefresh(context.Background(), time.Millisecond)
	t.Cleanup(manager.StopAutoRefresh)
	waitForAuthCondition(t, func() bool { return executor.probeCalls.Load() == 1 }, "inconclusive credential conversation probe")

	retained, ok := manager.GetByID("xai-probe-inconclusive")
	if !ok || retained == nil {
		t.Fatal("expected auth to remain after inconclusive probe")
	}
	if !retained.Unavailable || retained.Status != StatusError {
		t.Fatalf("retained auth state = unavailable:%v status:%s, want terminal error", retained.Unavailable, retained.Status)
	}
	if got := store.deleted(); len(got) != 0 {
		t.Fatalf("deleted auths = %v, want none", got)
	}
}

func TestManagerRequestUnauthorizedDoesNotRunDuplicateConversationProbe(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &probingOAuthRefreshFailureExecutor{
		oauthRefreshFailureExecutor: &oauthRefreshFailureExecutor{
			schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
			err:                           &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_grant", message: "Refresh token has been revoked"},
		},
		executeErr: &refreshOAuthError{status: http.StatusUnauthorized, message: "access token rejected"},
	}
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:       "xai-request-unauthorized",
		Provider: "xai",
		Metadata: map[string]any{
			"access_token":  "rejected-access-token",
			"refresh_token": "revoked-refresh-token",
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "xai", []*registry.ModelInfo{{ID: "grok-4.5"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	manager.RefreshSchedulerEntry(auth.ID)

	_, errExecute := manager.Execute(context.Background(), []string{"xai"}, cliproxyexecutor.Request{
		Model:   "grok-4.5",
		Payload: []byte(`{"model":"grok-4.5","input":"hello"}`),
	}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("Execute() error = nil, want unauthorized")
	}
	if got := executor.probeCalls.Load(); got != 0 {
		t.Fatalf("conversation probe calls = %d, want 0 after a real conversation already returned 401", got)
	}
	if _, ok := manager.GetByID(auth.ID); ok {
		t.Fatal("expected unauthorized auth to be removed")
	}
	if got := store.deleted(); len(got) != 1 || got[0] != auth.ID {
		t.Fatalf("deleted auths = %v, want [%s]", got, auth.ID)
	}
}

func TestManagerAutoRefreshRetainsInvalidGrantWhenDisabled(t *testing.T) {
	SetDeleteUnauthorizedAuth(false)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &oauthRefreshFailureExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
		err:                           &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_grant", message: "Refresh token has been revoked"},
	}
	manager.RegisterExecutor(executor)
	registerExpiredRefreshAuth(t, manager, "xai-retained", "xai")

	manager.StartAutoRefresh(context.Background(), time.Millisecond)
	t.Cleanup(manager.StopAutoRefresh)
	waitForAuthCondition(t, func() bool {
		auth, ok := manager.GetByID("xai-retained")
		return ok && auth.LastError != nil && auth.LastError.Code == "invalid_grant"
	}, "retained invalid_grant state")

	retained, ok := manager.GetByID("xai-retained")
	if !ok || retained == nil {
		t.Fatal("expected invalid auth to remain registered")
	}
	if !retained.Unavailable || retained.Status != StatusError {
		t.Fatalf("retained auth state = unavailable:%v status:%s", retained.Unavailable, retained.Status)
	}
	if !retained.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %s, want zero terminal state", retained.NextRefreshAfter)
	}
	if got := store.deleted(); len(got) != 0 {
		t.Fatalf("deleted auths = %v, want none", got)
	}

	calls := executor.calls.Load()
	time.Sleep(20 * time.Millisecond)
	if got := executor.calls.Load(); got != calls {
		t.Fatalf("refresh calls = %d after terminal failure, want %d", got, calls)
	}
}

func TestManagerAutoRefreshKeepsNonInvalidGrantBadRequest(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &oauthRefreshFailureExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
		err:                           &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_request", message: "malformed request"},
	}
	manager.RegisterExecutor(executor)
	registerExpiredRefreshAuth(t, manager, "xai-bad-request", "xai")

	refreshStartedAt := time.Now()
	manager.StartAutoRefresh(context.Background(), time.Millisecond)
	t.Cleanup(manager.StopAutoRefresh)
	waitForAuthCondition(t, func() bool {
		auth, ok := manager.GetByID("xai-bad-request")
		return ok && auth.LastError != nil && !auth.NextRefreshAfter.IsZero()
	}, "transient bad-request backoff")

	retained, ok := manager.GetByID("xai-bad-request")
	if !ok || retained.LastError.StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected retained HTTP 400 auth, got %#v", retained)
	}
	if got := store.deleted(); len(got) != 0 {
		t.Fatalf("deleted auths = %v, want none", got)
	}
	if got := retained.NextRefreshAfter; got.Before(refreshStartedAt.Add(4*time.Minute+59*time.Second)) || got.After(time.Now().Add(5*time.Minute+time.Second)) {
		t.Fatalf("NextRefreshAfter = %s, want approximately five-minute backoff", got)
	}
}

func TestManagerAutoRefreshKeepsStructuredBadRequestContainingUnauthorizedText(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	for _, message := range []string{"upstream status 401", "upstream 401 unauthorized"} {
		t.Run(message, func(t *testing.T) {
			store := &deleteTrackingStore{}
			manager := NewManager(store, nil, nil)
			executor := &oauthRefreshFailureExecutor{
				schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
				err:                           &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_request", message: message},
			}
			manager.RegisterExecutor(executor)
			registerExpiredRefreshAuth(t, manager, "xai-structured-bad-request", "xai")

			refreshStartedAt := time.Now()
			manager.StartAutoRefresh(context.Background(), time.Millisecond)
			t.Cleanup(manager.StopAutoRefresh)
			waitForAuthCondition(t, func() bool {
				auth, ok := manager.GetByID("xai-structured-bad-request")
				return ok && auth.LastError != nil && !auth.NextRefreshAfter.IsZero()
			}, "structured bad-request backoff")

			retained, ok := manager.GetByID("xai-structured-bad-request")
			if !ok || retained.LastError.StatusCode() != http.StatusBadRequest {
				t.Fatalf("expected retained HTTP 400 auth, got %#v", retained)
			}
			if got := store.deleted(); len(got) != 0 {
				t.Fatalf("deleted auths = %v, want none", got)
			}
			if got := retained.NextRefreshAfter; got.Before(refreshStartedAt.Add(4*time.Minute+59*time.Second)) || got.After(time.Now().Add(5*time.Minute+time.Second)) {
				t.Fatalf("NextRefreshAfter = %s, want approximately five-minute backoff", got)
			}
		})
	}
}

func TestManagerAutoRefreshDeletesUnauthorizedWhenEnabled(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &oauthRefreshFailureExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
		err:                           &refreshOAuthError{status: http.StatusUnauthorized, message: "expired credentials"},
	}
	manager.RegisterExecutor(executor)
	registerExpiredRefreshAuth(t, manager, "xai-unauthorized-delete", "xai")

	manager.StartAutoRefresh(context.Background(), time.Millisecond)
	t.Cleanup(manager.StopAutoRefresh)
	waitForAuthCondition(t, func() bool { return len(store.deleted()) == 1 }, "unauthorized auth deletion")

	if _, ok := manager.GetByID("xai-unauthorized-delete"); ok {
		t.Fatal("expected unauthorized auth to be removed from manager")
	}
}

func TestManagerAutoRefreshRetainsUnauthorizedWhenDisabled(t *testing.T) {
	SetDeleteUnauthorizedAuth(false)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &oauthRefreshFailureExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
		err:                           &refreshOAuthError{status: http.StatusUnauthorized, message: "expired credentials"},
	}
	manager.RegisterExecutor(executor)
	registerExpiredRefreshAuth(t, manager, "xai-unauthorized-retain", "xai")

	manager.StartAutoRefresh(context.Background(), time.Millisecond)
	t.Cleanup(manager.StopAutoRefresh)
	waitForAuthCondition(t, func() bool {
		auth, ok := manager.GetByID("xai-unauthorized-retain")
		return ok && auth.LastError != nil && auth.LastError.Code == "unauthorized"
	}, "retained unauthorized state")

	retained, ok := manager.GetByID("xai-unauthorized-retain")
	if !ok || !retained.Unavailable || retained.Status != StatusError || !retained.NextRefreshAfter.IsZero() {
		t.Fatalf("retained unauthorized auth = %#v", retained)
	}
	if got := store.deleted(); len(got) != 0 {
		t.Fatalf("deleted auths = %v, want none", got)
	}
}

func TestManagerAutoRefreshDeleteDoesNotRemoveReplacementAuth(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := newBlockingDeleteStore()
	manager := NewManager(store, nil, nil)
	executor := &oauthRefreshFailureExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
		err:                           &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_grant", message: "revoked"},
	}
	manager.RegisterExecutor(executor)
	registerExpiredRefreshAuth(t, manager, "xai-replaced", "xai")

	manager.StartAutoRefresh(context.Background(), time.Millisecond)
	t.Cleanup(manager.StopAutoRefresh)
	select {
	case <-store.deleteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for terminal deletion")
	}

	replacement := &Auth{
		ID:       "xai-replaced",
		Provider: "xai",
		Metadata: map[string]any{
			"access_token":  "replacement-access-token",
			"refresh_token": "replacement-refresh-token",
		},
	}
	registered := make(chan error, 1)
	go func() {
		_, errRegister := manager.Register(context.Background(), replacement)
		registered <- errRegister
	}()

	select {
	case errRegister := <-registered:
		t.Fatalf("replacement registration completed before terminal deletion: %v", errRegister)
	case <-time.After(20 * time.Millisecond):
	}
	close(store.allowDelete)
	if errRegister := <-registered; errRegister != nil {
		t.Fatalf("register replacement auth: %v", errRegister)
	}

	registeredAuth, ok := manager.GetByID("xai-replaced")
	if !ok || authAccessToken(registeredAuth) != "replacement-access-token" {
		t.Fatalf("manager auth = %#v, want replacement", registeredAuth)
	}
	persisted, ok := store.auth("xai-replaced")
	if !ok || authAccessToken(persisted) != "replacement-access-token" {
		t.Fatalf("stored auth = %#v, want replacement", persisted)
	}
	selected, errSelect := manager.SelectAuth(context.Background(), "xai", "", cliproxyexecutor.Options{})
	if errSelect != nil || selected == nil || selected.ID != "xai-replaced" {
		t.Fatalf("selected auth = %#v, err = %v", selected, errSelect)
	}
}

func TestManagerMarkResultDeleteDoesNotRemoveReplacementAuth(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := newBlockingDeleteStore()
	manager := NewManager(store, nil, nil)
	manager.RegisterExecutor(schedulerProviderTestExecutor{provider: "xai"})
	initial := &Auth{
		ID:       "xai-result-replaced",
		Provider: "xai",
		Metadata: map[string]any{"access_token": "initial-access-token", "refresh_token": "initial-refresh-token"},
	}
	if _, errRegister := manager.Register(context.Background(), initial); errRegister != nil {
		t.Fatalf("register initial auth: %v", errRegister)
	}

	resultDone := make(chan struct{})
	go func() {
		manager.MarkResult(context.Background(), Result{
			AuthID:  initial.ID,
			Success: false,
			Error:   &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
		})
		close(resultDone)
	}()
	select {
	case <-store.deleteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result deletion")
	}

	replacement := &Auth{
		ID:       initial.ID,
		Provider: "xai",
		Metadata: map[string]any{"access_token": "replacement-access-token", "refresh_token": "replacement-refresh-token"},
	}
	registered := make(chan error, 1)
	go func() {
		_, errRegister := manager.Register(context.Background(), replacement)
		registered <- errRegister
	}()

	select {
	case errRegister := <-registered:
		t.Fatalf("replacement registration completed before result deletion: %v", errRegister)
	case <-time.After(20 * time.Millisecond):
	}
	close(store.allowDelete)
	<-resultDone
	if errRegister := <-registered; errRegister != nil {
		t.Fatalf("register replacement auth: %v", errRegister)
	}

	registeredAuth, ok := manager.GetByID(initial.ID)
	if !ok || authAccessToken(registeredAuth) != "replacement-access-token" {
		t.Fatalf("manager auth = %#v, want replacement", registeredAuth)
	}
	persisted, ok := store.auth(initial.ID)
	if !ok || authAccessToken(persisted) != "replacement-access-token" {
		t.Fatalf("stored auth = %#v, want replacement", persisted)
	}
	selected, errSelect := manager.SelectAuth(context.Background(), "xai", "", cliproxyexecutor.Options{})
	if errSelect != nil || selected == nil || selected.ID != initial.ID {
		t.Fatalf("selected auth = %#v, err = %v", selected, errSelect)
	}
}
