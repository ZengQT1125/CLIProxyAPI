package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/store"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// stubTokenStore is a non-Postgres coreauth.Store for branch tests.
type stubTokenStore struct{}

func (stubTokenStore) List(_ context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (stubTokenStore) Save(_ context.Context, _ *coreauth.Auth) (string, error) {
	return "", nil
}
func (stubTokenStore) Delete(_ context.Context, _ string) error { return nil }

// TestCooldownStateStoreForTokenStore_FileStore verifies that a non-Postgres
// token store with a valid authDir returns a *FileCooldownStateStore.
func TestCooldownStateStoreForTokenStore_FileStore(t *testing.T) {
	dir := t.TempDir()
	got := cooldownStateStoreForTokenStore(stubTokenStore{}, dir)
	if _, ok := got.(*coreauth.FileCooldownStateStore); !ok {
		t.Fatalf("got %T, want *coreauth.FileCooldownStateStore", got)
	}
}

// TestCooldownStateStoreForTokenStore_EmptyAuthDir verifies that an empty
// authDir returns nil regardless of store type.
func TestCooldownStateStoreForTokenStore_EmptyAuthDir(t *testing.T) {
	got := cooldownStateStoreForTokenStore(stubTokenStore{}, "")
	if got != nil {
		t.Fatalf("got %T, want nil", got)
	}
}

// TestCooldownStateStoreForTokenStore_NilPostgres verifies that a nil
// PostgresStore pointer (wrapped as interface) falls through to nil because
// NewPostgresCooldownStateStore(nil) returns a true nil, and authDir is "".
func TestCooldownStateStoreForTokenStore_NilPostgres(t *testing.T) {
	var pg *store.PostgresStore = nil
	got := cooldownStateStoreForTokenStore(pg, "")
	if got != nil {
		t.Fatalf("nil PostgresStore with empty authDir: got %T, want nil", got)
	}
}

// TestCooldownStateStoreForTokenStore_NilPostgresFallsBackToFile verifies
// that a nil *PostgresStore with a valid authDir still returns a file store.
func TestCooldownStateStoreForTokenStore_NilPostgresFallsBackToFile(t *testing.T) {
	var pg *store.PostgresStore = nil
	dir := t.TempDir()
	got := cooldownStateStoreForTokenStore(pg, dir)
	// nil *PostgresStore triggers the file-store path because
	// NewPostgresCooldownStateStore(nil) returns a true nil.
	if _, ok := got.(*coreauth.FileCooldownStateStore); !ok {
		t.Fatalf("got %T, want *coreauth.FileCooldownStateStore", got)
	}
}

// TestConfigureCooldownStateStore_FileWhenNonPostgres exercises the full
// configureCooldownStateStore path with a non-PG token store.
func TestConfigureCooldownStateStore_FileWhenNonPostgres(t *testing.T) {
	prev := sdkAuth.GetTokenStore()
	sdkAuth.RegisterTokenStore(stubTokenStore{})
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(prev) })

	dir := t.TempDir()
	svc := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	svc.configureCooldownStateStore(&config.Config{
		SaveCooldownStatus: true,
		AuthDir:            dir,
	})
	// Manager should have a live file store — RestoreCooldownStates must not error.
	if err := svc.coreManager.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatalf("RestoreCooldownStates: %v", err)
	}
}

// TestConfigureCooldownStateStore_NilWhenDisabled verifies SaveCooldownStatus=false
// sets a nil store (RestoreCooldownStates is a no-op).
func TestConfigureCooldownStateStore_NilWhenDisabled(t *testing.T) {
	svc := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	svc.configureCooldownStateStore(&config.Config{SaveCooldownStatus: false})
	if err := svc.coreManager.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatalf("disabled: %v", err)
	}
}

// TestConfigureCooldownStateStore_NilWhenHome verifies that Home.Enabled=true
// always forces a nil store regardless of SaveCooldownStatus.
func TestConfigureCooldownStateStore_NilWhenHome(t *testing.T) {
	svc := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	cfg := &config.Config{SaveCooldownStatus: true}
	cfg.Home.Enabled = true
	svc.configureCooldownStateStore(cfg)
	if err := svc.coreManager.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatalf("home enabled: %v", err)
	}
}
