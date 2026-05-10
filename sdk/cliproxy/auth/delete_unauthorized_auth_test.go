package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
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
