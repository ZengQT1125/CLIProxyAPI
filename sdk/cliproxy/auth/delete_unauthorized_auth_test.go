package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

type oauthRefreshFailureExecutor struct {
	schedulerProviderTestExecutor
	err   error
	calls atomic.Int32
}

func (e *oauthRefreshFailureExecutor) Refresh(context.Context, *Auth) (*Auth, error) {
	e.calls.Add(1)
	return nil, e.err
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

func TestManagerAutoRefreshDeletesInvalidGrantWhenEnabled(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &oauthRefreshFailureExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
		err:                           &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_grant", message: "Refresh token has been revoked"},
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
}
