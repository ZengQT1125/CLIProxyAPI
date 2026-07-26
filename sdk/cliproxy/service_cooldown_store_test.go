package cliproxy

import (
	"context"
	"testing"

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

// stubCooldownStateStore is an inert coreauth.CooldownStateStore used to assert
// which store the resolver picks.
type stubCooldownStateStore struct{}

func (*stubCooldownStateStore) Load(_ context.Context) ([]coreauth.CooldownStateSnapshot, error) {
	return nil, nil
}

func (*stubCooldownStateStore) Apply(_ context.Context, _ []coreauth.CooldownStateSnapshot) error {
	return nil
}

// providerTokenStore is a token store that advertises its own cooldown backend,
// mirroring what *store.PostgresStore does.
type providerTokenStore struct {
	stubTokenStore
	backend coreauth.CooldownStateStore
}

func (p providerTokenStore) CooldownStateStore() coreauth.CooldownStateStore {
	return p.backend
}

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

// TestCooldownStateStoreForTokenStore_BackendProvider verifies that a token
// store advertising CooldownStateStoreProvider wins over the file store, even
// when authDir is usable.
func TestCooldownStateStoreForTokenStore_BackendProvider(t *testing.T) {
	backend := &stubCooldownStateStore{}
	got := cooldownStateStoreForTokenStore(providerTokenStore{backend: backend}, t.TempDir())
	if got != coreauth.CooldownStateStore(backend) {
		t.Fatalf("got %T, want the backend-provided store", got)
	}
}

// TestCooldownStateStoreForTokenStore_ProviderReturningNil verifies that a
// provider yielding nil falls back to the file store instead of installing a
// dead store.
func TestCooldownStateStoreForTokenStore_ProviderReturningNil(t *testing.T) {
	got := cooldownStateStoreForTokenStore(providerTokenStore{}, t.TempDir())
	if _, ok := got.(*coreauth.FileCooldownStateStore); !ok {
		t.Fatalf("got %T, want *coreauth.FileCooldownStateStore", got)
	}
}

// TestResolveCooldownStateStore_PrefersCapturedBackend verifies that the store
// captured at Build time (from a CooldownStateStoreProvider token store) takes
// precedence over the authDir-derived file store.
func TestResolveCooldownStateStore_PrefersCapturedBackend(t *testing.T) {
	prev := sdkAuth.GetTokenStore()
	sdkAuth.RegisterTokenStore(stubTokenStore{})
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(prev) })

	backend := &stubCooldownStateStore{}
	svc := &Service{cooldownStateStore: backend}
	got := svc.resolveCooldownStateStore(&config.Config{
		SaveCooldownStatus: true,
		AuthDir:            t.TempDir(),
	})
	if got != coreauth.CooldownStateStore(backend) {
		t.Fatalf("got %T, want the captured backend store", got)
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
