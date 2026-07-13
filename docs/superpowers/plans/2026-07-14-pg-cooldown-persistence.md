# PG Cooldown Persistence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When the token store is PostgreSQL, persist/restore credential cooldown state in a `cooldown_store` table so restarts do not reselect credentials still inside a cooldown window.

**Architecture:** Keep `auth.CooldownStateStore` + Manager lifecycle unchanged. Add `PostgresCooldownStateStore` (separate type; `PostgresStore.Save(*Auth)` already exists so PG store cannot implement `CooldownStateStore` itself). Wire `configureCooldownStateStore` to install the PG store when `GetTokenStore()` is `*store.PostgresStore` and `save-cooldown-status` is true; otherwise keep file `.cds`. Schema created in `EnsureSchema`.

**Tech Stack:** Go 1.26, `database/sql` + `pgx` stdlib driver, existing `sdk/cliproxy/auth` cooldown types, Gin service wiring.

**Spec:** `docs/superpowers/specs/2026-07-14-pg-cooldown-persistence-design.md`

## Global Constraints

- Feature gate remains `save-cooldown-status` (default `false`); do not flip default.
- Home mode still forces cooldown persistence off.
- Multi-instance is last-write-wins (full snapshot Save); no locks.
- Do not dual-write `.cds` under PG mode.
- Do not embed cooldown into `auth_store` JSONB.
- Do not import `internal/store` into `sdk/cliproxy/auth`.
- Tests: public contract only (Load/Save behavior, configure branch). No private-implementation assertions.

## File Map

| File | Responsibility |
|---|---|
| `internal/store/postgresstore.go` | `CooldownTable` config default; `EnsureSchema` creates `cooldown_store` |
| `internal/store/postgres_cooldown_store.go` (new) | `PostgresCooldownStateStore` Load/Save |
| `internal/store/postgres_cooldown_store_test.go` (new) | Pure helpers + Save/Load grouping tests (no live PG required for helpers; SQL path tested via fake or skip-without-DSN as noted per task) |
| `sdk/cliproxy/service.go` | `configureCooldownStateStore` PG branch |
| `sdk/cliproxy/service_cooldown_store_test.go` (new) | configure branch unit tests |
| `config.example.yaml` | Document PG table vs `.cds` |

---

### Task 1: Schema + config field for `cooldown_store`

**Files:**
- Modify: `internal/store/postgresstore.go`
- Test: `internal/store/postgres_cooldown_store_test.go` (create; schema SQL builder assertion if extracted, otherwise covered by EnsureSchema compile + Task 2)

**Interfaces:**
- Consumes: existing `PostgresStoreConfig`, `fullTableName`, `EnsureSchema`
- Produces: `PostgresStoreConfig.CooldownTable`; default `cooldown_store`; EnsureSchema creates the table

- [ ] **Step 1: Add default and config field**

In `internal/store/postgresstore.go` constants:

```go
const (
	defaultConfigTable   = "config_store"
	defaultAuthTable     = "auth_store"
	defaultCooldownTable = "cooldown_store"
	defaultConfigKey     = "config"
)
```

In `PostgresStoreConfig`:

```go
type PostgresStoreConfig struct {
	DSN          string
	Schema       string
	ConfigTable  string
	AuthTable    string
	CooldownTable string
	SpoolDir     string
}
```

In `NewPostgresStore`, after AuthTable default:

```go
if cfg.CooldownTable == "" {
	cfg.CooldownTable = defaultCooldownTable
}
```

- [ ] **Step 2: Extend EnsureSchema**

After the auth table `CREATE TABLE` block, before `return nil`:

```go
cooldownTable := s.fullTableName(s.cfg.CooldownTable)
if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
	CREATE TABLE IF NOT EXISTS %s (
		id TEXT PRIMARY KEY,
		content JSONB NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)
`, cooldownTable)); err != nil {
	return fmt.Errorf("postgres store: create cooldown table: %w", err)
}
```

- [ ] **Step 3: Compile package**

Run: `go build ./internal/store/`

Expected: success

- [ ] **Step 4: Commit**

```bash
git add internal/store/postgresstore.go
git commit -m "feat(store): add cooldown_store table to Postgres EnsureSchema"
```

---

### Task 2: `PostgresCooldownStateStore` Load/Save

**Files:**
- Create: `internal/store/postgres_cooldown_store.go`
- Create: `internal/store/postgres_cooldown_store_test.go`

**Interfaces:**
- Consumes: `*PostgresStore` (same package; private `db`/`cfg`/`fullTableName`/`mu` access OK)
- Produces:
  - `func NewPostgresCooldownStateStore(s *PostgresStore) *PostgresCooldownStateStore`
  - `func (c *PostgresCooldownStateStore) Load(ctx context.Context) ([]cliproxyauth.CooldownStateRecord, error)`
  - `func (c *PostgresCooldownStateStore) Save(ctx context.Context, records []cliproxyauth.CooldownStateRecord) error`
  - Compile-time check: `var _ cliproxyauth.CooldownStateStore = (*PostgresCooldownStateStore)(nil)`

**Semantics (must match design):**
- Group Save by `strings.TrimSpace(record.AuthID)`; skip empty AuthID
- Row `id` = that AuthID (do not use AuthFile for PG keys)
- Content envelope JSON:

```go
type cooldownStateEnvelope struct {
	Version   int                              `json:"version"`
	AuthID    string                           `json:"auth_id,omitempty"`
	Provider  string                           `json:"provider,omitempty"`
	UpdatedAt time.Time                        `json:"updated_at"`
	Records   []cliproxyauth.CooldownStateRecord `json:"records"`
}
```

- Upsert each group with `version: 1`, AuthID/Provider from first record, Records sorted by Model
- After upserts, delete rows whose `id` is not in the desired set (full wipe if no groups)
- Load: SELECT all rows, unmarshal `records`, flatten; invalid row → return error (same strictness as file parse failure)
- Nil store / nil db → Load returns nil,nil; Save returns nil
- Honor `ctx.Err()`
- Hold `s.mu` for Save (same lock as auth writes) to avoid racing Bootstrap; Load may use `s.mu` or DB-only — prefer `s.mu` for consistency

- [ ] **Step 1: Write failing tests for grouping helper and envelope**

Create `internal/store/postgres_cooldown_store_test.go`:

```go
package store

import (
	"encoding/json"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestGroupCooldownRecordsByAuthID(t *testing.T) {
	next := time.Now().UTC().Truncate(time.Second)
	in := []cliproxyauth.CooldownStateRecord{
		{AuthID: "a1", Model: "m2", Provider: "xai", NextRetryAfter: next},
		{AuthID: "a1", Model: "m1", Provider: "xai", NextRetryAfter: next},
		{AuthID: "", Model: "skip", NextRetryAfter: next},
		{AuthID: "a2", Model: "", Provider: "claude", NextRetryAfter: next},
	}
	groups := groupCooldownRecordsByAuthID(in)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	if len(groups["a1"]) != 2 {
		t.Fatalf("a1 records = %d, want 2", len(groups["a1"]))
	}
	if len(groups["a2"]) != 1 {
		t.Fatalf("a2 records = %d, want 1", len(groups["a2"]))
	}
}

func TestCooldownStateEnvelopeRoundTrip(t *testing.T) {
	next := time.Now().UTC().Truncate(time.Second)
	records := []cliproxyauth.CooldownStateRecord{
		{
			AuthID:         "auth-1",
			Provider:       "xai",
			Model:          "grok-4",
			Status:         "cooling",
			NextRetryAfter: next,
			Reason:         "quota",
			LastError:      &cliproxyauth.Error{Message: "rate limited", HTTPStatus: 429},
			UpdatedAt:      next,
		},
	}
	raw, err := marshalCooldownEnvelope("auth-1", "xai", records)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var env cooldownStateEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Version != 1 || env.AuthID != "auth-1" || len(env.Records) != 1 {
		t.Fatalf("envelope = %+v", env)
	}
	if env.Records[0].LastError == nil || env.Records[0].LastError.HTTPStatus != 429 {
		t.Fatalf("last error = %+v", env.Records[0].LastError)
	}
}
```

Export helpers used by Save (same package, unexported names OK):

- `groupCooldownRecordsByAuthID(records []cliproxyauth.CooldownStateRecord) map[string][]cliproxyauth.CooldownStateRecord`
- `marshalCooldownEnvelope(authID, provider string, records []cliproxyauth.CooldownStateRecord) ([]byte, error)`

- [ ] **Step 2: Run tests — expect fail (helpers missing)**

Run: `go test ./internal/store/ -run 'TestGroupCooldownRecordsByAuthID|TestCooldownStateEnvelopeRoundTrip' -count=1`

Expected: FAIL compile or undefined helpers

- [ ] **Step 3: Implement store + helpers**

Create `internal/store/postgres_cooldown_store.go`:

```go
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

var _ cliproxyauth.CooldownStateStore = (*PostgresCooldownStateStore)(nil)

type PostgresCooldownStateStore struct {
	s *PostgresStore
}

func NewPostgresCooldownStateStore(s *PostgresStore) *PostgresCooldownStateStore {
	if s == nil {
		return nil
	}
	return &PostgresCooldownStateStore{s: s}
}

type cooldownStateEnvelope struct {
	Version   int                              `json:"version"`
	AuthID    string                           `json:"auth_id,omitempty"`
	Provider  string                           `json:"provider,omitempty"`
	UpdatedAt time.Time                        `json:"updated_at"`
	Records   []cliproxyauth.CooldownStateRecord `json:"records"`
}

func groupCooldownRecordsByAuthID(records []cliproxyauth.CooldownStateRecord) map[string][]cliproxyauth.CooldownStateRecord {
	out := make(map[string][]cliproxyauth.CooldownStateRecord)
	for _, rec := range records {
		id := strings.TrimSpace(rec.AuthID)
		if id == "" {
			continue
		}
		out[id] = append(out[id], rec)
	}
	return out
}

func marshalCooldownEnvelope(authID, provider string, records []cliproxyauth.CooldownStateRecord) ([]byte, error) {
	sorted := append([]cliproxyauth.CooldownStateRecord(nil), records...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Model < sorted[j].Model
	})
	env := cooldownStateEnvelope{
		Version:   1,
		AuthID:    authID,
		Provider:  provider,
		UpdatedAt: time.Now().UTC(),
		Records:   sorted,
	}
	return json.Marshal(env)
}

func (c *PostgresCooldownStateStore) tableName() string {
	if c == nil || c.s == nil {
		return ""
	}
	name := c.s.cfg.CooldownTable
	if name == "" {
		name = defaultCooldownTable
	}
	return c.s.fullTableName(name)
}

func (c *PostgresCooldownStateStore) Load(ctx context.Context) ([]cliproxyauth.CooldownStateRecord, error) {
	if c == nil || c.s == nil || c.s.db == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.s.mu.Lock()
	defer c.s.mu.Unlock()

	query := fmt.Sprintf(`SELECT id, content FROM %s`, c.tableName())
	rows, err := c.s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("postgres cooldown: list: %w", err)
	}
	defer rows.Close()

	out := make([]cliproxyauth.CooldownStateRecord, 0)
	for rows.Next() {
		var id string
		var content []byte
		if err = rows.Scan(&id, &content); err != nil {
			return nil, fmt.Errorf("postgres cooldown: scan: %w", err)
		}
		var env cooldownStateEnvelope
		if err = json.Unmarshal(content, &env); err != nil {
			return nil, fmt.Errorf("postgres cooldown: parse %s: %w", id, err)
		}
		out = append(out, env.Records...)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres cooldown: iterate: %w", err)
	}
	return out, nil
}

func (c *PostgresCooldownStateStore) Save(ctx context.Context, records []cliproxyauth.CooldownStateRecord) error {
	if c == nil || c.s == nil || c.s.db == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.s.mu.Lock()
	defer c.s.mu.Unlock()

	groups := groupCooldownRecordsByAuthID(records)
	table := c.tableName()

	desired := make(map[string]struct{}, len(groups))
	for authID, group := range groups {
		provider := ""
		if len(group) > 0 {
			provider = strings.TrimSpace(group[0].Provider)
		}
		payload, errMarshal := marshalCooldownEnvelope(authID, provider, group)
		if errMarshal != nil {
			return fmt.Errorf("postgres cooldown: marshal %s: %w", authID, errMarshal)
		}
		// pgx accepts []byte for JSONB
		query := fmt.Sprintf(`
			INSERT INTO %s (id, content, created_at, updated_at)
			VALUES ($1, $2, NOW(), NOW())
			ON CONFLICT (id)
			DO UPDATE SET content = EXCLUDED.content, updated_at = NOW()
		`, table)
		if _, err := c.s.db.ExecContext(ctx, query, authID, json.RawMessage(payload)); err != nil {
			return fmt.Errorf("postgres cooldown: upsert %s: %w", authID, err)
		}
		desired[authID] = struct{}{}
	}

	// Stale cleanup
	rows, err := c.s.db.QueryContext(ctx, fmt.Sprintf(`SELECT id FROM %s`, table))
	if err != nil {
		return fmt.Errorf("postgres cooldown: list ids: %w", err)
	}
	defer rows.Close()

	stale := make([]string, 0)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return fmt.Errorf("postgres cooldown: scan id: %w", err)
		}
		if _, ok := desired[id]; !ok {
			stale = append(stale, id)
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("postgres cooldown: iterate ids: %w", err)
	}
	for _, id := range stale {
		if _, err = c.s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, table), id); err != nil {
			return fmt.Errorf("postgres cooldown: delete %s: %w", id, err)
		}
	}
	return nil
}
```

Notes for implementer:
- If `json.RawMessage(payload)` fails driver typing, cast `payload` as `[]byte` or use string — match how `persistAuth` passes JSONB (`json.RawMessage(data)` already used in `postgresstore.go`).
- Do not leave unused `database/sql` import if only used via `c.s.db`.

- [ ] **Step 4: Run helper tests**

Run: `go test ./internal/store/ -run 'TestGroupCooldownRecordsByAuthID|TestCooldownStateEnvelopeRoundTrip' -count=1`

Expected: PASS

- [ ] **Step 5: Optional live-PG roundtrip (skip if no DSN)**

Append to test file:

```go
func TestPostgresCooldownStateStore_SaveLoadCleanStale(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PGSTORE_DSN"))
	if dsn == "" {
		t.Skip("PGSTORE_DSN not set")
	}
	ctx := context.Background()
	pg, err := NewPostgresStore(ctx, PostgresStoreConfig{
		DSN:      dsn,
		Schema:   os.Getenv("PGSTORE_SCHEMA"),
		SpoolDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	if err := pg.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	store := NewPostgresCooldownStateStore(pg)
	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rec := cliproxyauth.CooldownStateRecord{
		Provider: "xai", AuthID: "pg-cooldown-test-auth", Model: "grok-4",
		Status: "cooling", NextRetryAfter: next, Reason: "quota", UpdatedAt: next,
	}
	// seed stale row
	_, _ = pg.db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (id, content) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET content = EXCLUDED.content`,
		store.tableName()), "pg-cooldown-stale", []byte(`{"version":1,"records":[]}`))

	if err := store.Save(ctx, []cliproxyauth.CooldownStateRecord{rec}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, r := range loaded {
		if r.AuthID == rec.AuthID && r.Model == rec.Model {
			found = true
		}
		if r.AuthID == "pg-cooldown-stale" {
			t.Fatalf("stale auth still present")
		}
	}
	if !found {
		t.Fatalf("expected record missing: %+v", loaded)
	}
	if err := store.Save(ctx, nil); err != nil {
		t.Fatalf("Save(nil): %v", err)
	}
}
```

(Add needed imports: `context`, `fmt`, `os`, `strings`.)

Run: `go test ./internal/store/ -run TestPostgresCooldownStateStore_SaveLoadCleanStale -count=1`

Expected: SKIP without DSN; PASS with valid PGSTORE_DSN

- [ ] **Step 6: Commit**

```bash
git add internal/store/postgres_cooldown_store.go internal/store/postgres_cooldown_store_test.go
git commit -m "feat(store): PostgresCooldownStateStore for durable cooldown state"
```

---

### Task 3: Wire `configureCooldownStateStore` to PG

**Files:**
- Modify: `sdk/cliproxy/service.go` (`configureCooldownStateStore`, imports)
- Create: `sdk/cliproxy/service_cooldown_store_test.go`

**Interfaces:**
- Consumes: `store.NewPostgresCooldownStateStore`, `sdkAuth.GetTokenStore()`, `*store.PostgresStore`
- Produces: when `SaveCooldownStatus && !Home && token store is *PostgresStore`, Manager uses PG cooldown store

- [ ] **Step 1: Write failing configure tests**

`sdk/cliproxy/service_cooldown_store_test.go`:

```go
package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/store"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// stubTokenStore is a non-Postgres coreauth.Store for branch tests.
type stubTokenStore struct{}

func (stubTokenStore) List(ctx context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (stubTokenStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	return "", nil
}
func (stubTokenStore) Delete(ctx context.Context, id string) error { return nil }

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
	// Force a persist to ensure store is non-nil: mark nothing; use internal by Restore no-op.
	// Probe via RestoreCooldownStates (nil store returns nil without error).
	if err := svc.coreManager.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatalf("RestoreCooldownStates: %v", err)
	}
	// Non-nil file store: Save empty should not error and is reachable via Manager only when store set.
	// Use a second call path: set flag false clears store.
	svc.configureCooldownStateStore(&config.Config{SaveCooldownStatus: false, AuthDir: dir})
	if err := svc.coreManager.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatalf("after disable: %v", err)
	}
}

func TestConfigureCooldownStateStore_NilWhenDisabledOrHome(t *testing.T) {
	svc := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	svc.configureCooldownStateStore(&config.Config{SaveCooldownStatus: false})
	if err := svc.coreManager.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatalf("disabled: %v", err)
	}
	cfg := &config.Config{SaveCooldownStatus: true}
	cfg.Home.Enabled = true
	svc.configureCooldownStateStore(cfg)
	if err := svc.coreManager.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatalf("home: %v", err)
	}
}
```

**Better probe (preferred in implementation):** export nothing; instead add a small test-only approach:

Put type detection in a package-level helper in `service.go`:

```go
func cooldownStateStoreForTokenStore(tokenStore coreauth.Store, authDir string) coreauth.CooldownStateStore {
	if pg, ok := tokenStore.(*store.PostgresStore); ok && pg != nil {
		return store.NewPostgresCooldownStateStore(pg)
	}
	if authDir == "" {
		return nil
	}
	return coreauth.NewFileCooldownStateStoreWithAuthDir(authDir, authDir)
}
```

Then test the helper directly (no need to poke Manager private fields):

```go
func TestCooldownStateStoreForTokenStore_Postgres(t *testing.T) {
	// Cannot construct PostgresStore without DSN easily; type branch for non-PG:
	got := cooldownStateStoreForTokenStore(stubTokenStore{}, t.TempDir())
	if _, ok := got.(*coreauth.FileCooldownStateStore); !ok {
		t.Fatalf("got %T, want *FileCooldownStateStore", got)
	}
}

func TestCooldownStateStoreForTokenStore_EmptyAuthDir(t *testing.T) {
	if got := cooldownStateStoreForTokenStore(stubTokenStore{}, ""); got != nil {
		t.Fatalf("got %T, want nil", got)
	}
}
```

For PG branch without live DSN: use a nil-safe path — `NewPostgresCooldownStateStore(nil)` returns nil; type assert with non-nil only when real store exists. Optional:

```go
func TestCooldownStateStoreForTokenStore_PostgresType(t *testing.T) {
	// Build minimal PostgresStore only if we can; else verify function prefers *PostgresStore via a local fake impossible without same type.
	// Document: live PG covered by internal/store integration test + manual.
}
```

Implementer: **must** extract `cooldownStateStoreForTokenStore` so file-vs-PG branch is unit-testable without private Manager fields. Full PG object construction without DSN is not required for service tests.

- [ ] **Step 2: Run tests — fail until helper exists**

Run: `go test ./sdk/cliproxy/ -run 'TestCooldownStateStoreForTokenStore|TestConfigureCooldownStateStore' -count=1`

Expected: FAIL

- [ ] **Step 3: Implement wiring**

Add import:

```go
"github.com/router-for-me/CLIProxyAPI/v7/internal/store"
```

Replace `configureCooldownStateStore` body:

```go
func (s *Service) configureCooldownStateStore(cfg *config.Config) {
	if s == nil || s.coreManager == nil {
		return
	}
	if cfg == nil || !cfg.SaveCooldownStatus || cfg.Home.Enabled {
		s.coreManager.SetCooldownStateStore(nil)
		return
	}
	authDir, errResolve := resolveCooldownStateAuthDir(cfg)
	if errResolve != nil {
		log.Warnf("failed to resolve cooldown state directory: %v", errResolve)
		s.coreManager.SetCooldownStateStore(nil)
		return
	}
	cds := cooldownStateStoreForTokenStore(sdkAuth.GetTokenStore(), authDir)
	if cds == nil {
		s.coreManager.SetCooldownStateStore(nil)
		return
	}
	s.coreManager.SetCooldownStateStore(cds)
}

func cooldownStateStoreForTokenStore(tokenStore coreauth.Store, authDir string) coreauth.CooldownStateStore {
	if pg, ok := tokenStore.(*store.PostgresStore); ok && pg != nil {
		if cds := store.NewPostgresCooldownStateStore(pg); cds != nil {
			return cds
		}
	}
	if strings.TrimSpace(authDir) == "" {
		return nil
	}
	return coreauth.NewFileCooldownStateStoreWithAuthDir(authDir, authDir)
}
```

Important: for PG path, **do not require authDir** — even if `resolveCooldownStateAuthDir` fails, prefer PG when token store is Postgres:

Refine:

```go
func (s *Service) configureCooldownStateStore(cfg *config.Config) {
	if s == nil || s.coreManager == nil {
		return
	}
	if cfg == nil || !cfg.SaveCooldownStatus || cfg.Home.Enabled {
		s.coreManager.SetCooldownStateStore(nil)
		return
	}
	if pg, ok := sdkAuth.GetTokenStore().(*store.PostgresStore); ok && pg != nil {
		s.coreManager.SetCooldownStateStore(store.NewPostgresCooldownStateStore(pg))
		return
	}
	authDir, errResolve := resolveCooldownStateAuthDir(cfg)
	if errResolve != nil {
		log.Warnf("failed to resolve cooldown state directory: %v", errResolve)
		s.coreManager.SetCooldownStateStore(nil)
		return
	}
	if authDir == "" {
		s.coreManager.SetCooldownStateStore(nil)
		return
	}
	s.coreManager.SetCooldownStateStore(coreauth.NewFileCooldownStateStoreWithAuthDir(authDir, authDir))
}
```

And keep helper for tests:

```go
func cooldownStateStoreForTokenStore(tokenStore coreauth.Store, authDir string) coreauth.CooldownStateStore {
	if pg, ok := tokenStore.(*store.PostgresStore); ok && pg != nil {
		return store.NewPostgresCooldownStateStore(pg)
	}
	if strings.TrimSpace(authDir) == "" {
		return nil
	}
	return coreauth.NewFileCooldownStateStoreWithAuthDir(authDir, authDir)
}
```

Use the helper from `configureCooldownStateStore` to avoid duplicated type assert (call helper after gate checks; for file path pass resolved authDir; for PG authDir ignored).

Final `configureCooldownStateStore`:

```go
func (s *Service) configureCooldownStateStore(cfg *config.Config) {
	if s == nil || s.coreManager == nil {
		return
	}
	if cfg == nil || !cfg.SaveCooldownStatus || cfg.Home.Enabled {
		s.coreManager.SetCooldownStateStore(nil)
		return
	}
	authDir := ""
	if _, isPG := sdkAuth.GetTokenStore().(*store.PostgresStore); !isPG {
		var errResolve error
		authDir, errResolve = resolveCooldownStateAuthDir(cfg)
		if errResolve != nil {
			log.Warnf("failed to resolve cooldown state directory: %v", errResolve)
			s.coreManager.SetCooldownStateStore(nil)
			return
		}
	}
	s.coreManager.SetCooldownStateStore(cooldownStateStoreForTokenStore(sdkAuth.GetTokenStore(), authDir))
}
```

- [ ] **Step 4: Run tests**

Run:

```bash
go test ./sdk/cliproxy/ -run 'TestCooldownStateStoreForTokenStore|TestConfigureCooldownStateStore' -count=1
go test ./sdk/cliproxy/auth/ -run 'Cooldown' -count=1
go build -o /tmp/cli-proxy-api ./cmd/server/
```

Expected: PASS + build OK

- [ ] **Step 5: Commit**

```bash
git add sdk/cliproxy/service.go sdk/cliproxy/service_cooldown_store_test.go
git commit -m "feat(cliproxy): use PostgresCooldownStateStore when token store is PG"
```

---

### Task 4: Document config.example.yaml

**Files:**
- Modify: `config.example.yaml` (around `save-cooldown-status`)

- [ ] **Step 1: Update comments**

Replace the block at lines 148–150 with:

```yaml
# When true, persist per-auth cooldown status across restarts.
# - File / git / object token store: write .cds files next to auth files under auth-dir.
# - PostgreSQL token store (PGSTORE_DSN): write rows to cooldown_store (not .cds).
#   Local spool authDir is wiped on bootstrap, so file-based .cds cannot survive restart under PG.
# Default is false; when false, cooldown status is kept in memory only.
# Disabled automatically when home mode is enabled.
save-cooldown-status: false
```

- [ ] **Step 2: Commit**

```bash
git add config.example.yaml
git commit -m "docs(config): document PG cooldown_store for save-cooldown-status"
```

---

### Task 5: Verification

- [ ] **Step 1: Full focused test + build**

```bash
go test ./internal/store/ -count=1
go test ./sdk/cliproxy/ -run 'Cooldown|Stale' -count=1
go test ./sdk/cliproxy/auth/ -run 'Cooldown' -count=1
go build -o /tmp/cli-proxy-api ./cmd/server/
```

Expected: all PASS (optional live PG test SKIP without DSN), build OK

- [ ] **Step 2: Manual checklist (operator)**

1. Set `PGSTORE_DSN`, `save-cooldown-status: true`
2. Trigger a quota/429 cooldown on one auth
3. Confirm row in `cooldown_store`
4. Restart process
5. Confirm that auth remains unavailable until `next_retry_after`
6. Confirm after expiry the row is cleaned on next persist/restore

---

## Spec Coverage Checklist

| Spec requirement | Task |
|---|---|
| New `cooldown_store` table + EnsureSchema | Task 1 |
| `PostgresCooldownStateStore` Load/Save, separate type | Task 2 |
| Group by auth id, stale delete, envelope JSON | Task 2 |
| Wire configureCooldownStateStore for PG | Task 3 |
| Gate `save-cooldown-status` / Home off | Task 3 (unchanged gates) |
| No dual-write `.cds` under PG | Task 3 (PG returns before file path) |
| config.example.yaml note | Task 4 |
| Multi-instance LWW documented | Spec only; Save full snapshot preserves LWW |
| Tests for store + configure | Tasks 2–3 |
| Do not change default flag | All tasks |

## Placeholder / consistency self-review

- No TBD left; method names fixed: `NewPostgresCooldownStateStore`, `cooldownStateStoreForTokenStore`, `groupCooldownRecordsByAuthID`, `marshalCooldownEnvelope`.
- `Save` name collision resolved via separate type (spec-critical).
- PG path does not depend on spool authDir surviving Bootstrap.
