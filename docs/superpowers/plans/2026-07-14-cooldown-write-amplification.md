# Cooldown Persistence Write Amplification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove cooldown persistence I/O from request execution and replace global snapshot writes with incremental per-auth batches.

**Architecture:** `CooldownStateStore` loads and applies grouped per-auth snapshots. `Manager` owns a lazy single-writer persister that coalesces dirty auth IDs for 100 ms, snapshots only those IDs, retries failed background writes, and supports explicit flush for restore and shutdown. PostgreSQL applies only changed IDs in one short transaction; the file backend updates only the corresponding `.cds` files.

**Tech Stack:** Go 1.26, `context`, `database/sql`, PostgreSQL JSONB, existing SDK auth Manager and store implementations.

**Spec:** `docs/superpowers/specs/2026-07-14-cooldown-write-amplification-design.md`

## Global Constraints

- Keep `save-cooldown-status` default `false` and Home-mode persistence disabled.
- Keep cooldown envelope JSON version 1 and existing `CooldownStateRecord` fields.
- No request-time storage I/O, full auth snapshot, cooldown table enumeration, or directory walk.
- No compatibility interface or PostgreSQL-only fast path.
- No new network timeout.
- Extend existing test files; assert public state/store contracts, not SQL strings or private helper delegation.
- Run `gofmt -w` after Go edits and the required server build before completion.

## File Map

| File | Responsibility |
|---|---|
| `sdk/cliproxy/auth/cooldown_state.go` | Grouped store contract and incremental file implementation |
| `sdk/cliproxy/auth/cooldown_state_persister.go` | Lazy dirty-ID batch writer and explicit flush |
| `sdk/cliproxy/auth/conductor.go` | Manager wiring, per-auth snapshot generation, dirty marking, restore reconciliation |
| `sdk/cliproxy/auth/cooldown_state_test.go` | File and Manager behavior regressions |
| `internal/store/postgres_cooldown_store.go` | Incremental PostgreSQL Load/Apply transaction |
| `internal/store/postgres_cooldown_store_test.go` | PostgreSQL public contract integration test |
| `sdk/cliproxy/service.go` | Graceful shutdown flush |

---

### Task 1: Replace the global store contract with per-auth snapshots

**Files:**
- Modify: `sdk/cliproxy/auth/cooldown_state_test.go`
- Modify: `sdk/cliproxy/auth/cooldown_state.go`

**Interfaces:**
- Produces: `CooldownStateSnapshot`, `CooldownStateStore.Load`, `CooldownStateStore.Apply`
- Preserves: record/envelope JSON version 1 and file path derivation

- [ ] **Step 1: Write failing file-store contract tests**

Replace the full-snapshot test with an incremental isolation test:

```go
func TestFileCooldownStateStore_ApplyOnlyChangesNamedAuth(t *testing.T) {
	store := NewFileCooldownStateStoreWithAuthDir(t.TempDir(), authDir)
	if err := store.Apply(ctx, []CooldownStateSnapshot{
		{AuthID: "auth-1", Records: []CooldownStateRecord{record1}},
		{AuthID: "auth-2", Records: []CooldownStateRecord{record2}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(ctx, []CooldownStateSnapshot{{AuthID: "auth-1"}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].AuthID != "auth-2" || len(loaded[0].Records) != 1 || loaded[0].Records[0].Model != record2.Model {
		t.Fatalf("loaded snapshots = %+v, want only auth-2", loaded)
	}
}
```

Adapt concurrent file-store coverage to call `Apply` with one `CooldownStateSnapshot` per call.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
go test ./sdk/cliproxy/auth -run 'TestFileCooldownStateStore_ApplyOnlyChangesNamedAuth|TestFileCooldownStateStore_ConcurrentApply' -count=1
```

Expected: compile failure because `CooldownStateSnapshot` and `Apply` do not exist.

- [ ] **Step 3: Implement the grouped contract and incremental file store**

Add the contract:

```go
type CooldownStateSnapshot struct {
	AuthID  string
	Records []CooldownStateRecord
}

type CooldownStateStore interface {
	Load(context.Context) ([]CooldownStateSnapshot, error)
	Apply(context.Context, []CooldownStateSnapshot) error
}
```

Add `paths map[string]string` to `FileCooldownStateStore`. Populate it during `Load`. Implement `Apply` so a non-empty snapshot writes only that auth's file and an empty snapshot removes only the indexed file. Validate the whole input before file I/O, write a new path before removing an old path, and remove `removeAllStateFiles` / `removeStaleStateFiles` because incremental writes must never walk the directory.

- [ ] **Step 4: Run focused file tests and verify GREEN**

```bash
go test ./sdk/cliproxy/auth -run 'TestFileCooldownStateStore' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add sdk/cliproxy/auth/cooldown_state.go sdk/cliproxy/auth/cooldown_state_test.go
git commit -m "refactor(auth): make cooldown store incremental"
```

---

### Task 2: Move cooldown persistence off the request path

**Files:**
- Create: `sdk/cliproxy/auth/cooldown_state_persister.go`
- Modify: `sdk/cliproxy/auth/conductor.go`
- Modify: `sdk/cliproxy/auth/cooldown_state_test.go`

**Interfaces:**
- Produces: `Manager.FlushCooldownStates(context.Context) error`
- Consumes: `CooldownStateStore.Apply(context.Context, []CooldownStateSnapshot)`

- [ ] **Step 1: Write failing Manager behavior tests**

Change `recordingCooldownStateStore` to record `Apply` batches. Add tests with real Manager state transitions:

```go
func TestManager_MarkResult_DoesNotWaitForCooldownStore(t *testing.T) {
	store := &blockingCooldownStateStore{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "auth-1", Provider: "xai", Status: StatusActive}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		manager.MarkResult(context.Background(), Result{
			AuthID: "auth-1", Provider: "xai", Model: "grok-4", Success: false,
			Error: &Error{Message: "rate limited", HTTPStatus: http.StatusTooManyRequests},
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("MarkResult blocked on cooldown persistence")
	}
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("cooldown Apply did not start")
	}
	close(store.release)
	if err := manager.FlushCooldownStates(context.Background()); err != nil {
		t.Fatal(err)
	}
}
```

`blockingCooldownStateStore` implements the real grouped store interface: `Load` returns nil, and `Apply` closes `started`, waits on `release`, then returns nil. Assertions concern Manager latency and persisted snapshots, not the test double itself.

Also cover:

- one changed auth produces one snapshot even when another auth is cooling;
- clearing an auth produces an empty snapshot for that auth only;
- when `Apply` returns an error, a later successful `FlushCooldownStates` writes the retained batch;
- a state change made while an older batch is blocked becomes a later batch and wins.

- [ ] **Step 2: Run Manager tests and verify RED**

```bash
go test ./sdk/cliproxy/auth -run 'TestManager_(MarkResult|FlushCooldownStates|RestoreCooldownStates)' -count=1
```

Expected: compile failure because Manager still calls `Save` synchronously and has no flush method.

- [ ] **Step 3: Implement the lazy persister**

Create `cooldown_state_persister.go` with:

```go
const (
	cooldownPersistDebounce   = 100 * time.Millisecond
	cooldownPersistRetryDelay = time.Second
)

type cooldownStatePersister struct {
	apply      func(context.Context, []string) error
	mu         sync.Mutex
	dirty      map[string]struct{}
	running    bool
	flushToken chan struct{}
}
```

`mark` deduplicates IDs and starts a worker only when idle. The worker waits one fixed batch window, calls `flush(context.Background())`, retries failures after one second, and exits with no dirty IDs. `flush` acquires `flushToken` before draining IDs; on error it requeues the complete batch. It loops until no dirty IDs remain so graceful shutdown captures changes made during an earlier apply.

- [ ] **Step 4: Wire Manager incremental snapshots and dirty marks**

Initialize the persister in `NewManager`:

```go
manager.cooldownPersister = newCooldownStatePersister(manager.persistCooldownStateIDs)
```

Replace `persistCooldownStates` / `cooldownStateSnapshot` with:

```go
func (m *Manager) persistCooldownStateIDs(ctx context.Context, authIDs []string) error
func (m *Manager) cooldownStateSnapshots(authIDs []string) ([]CooldownStateSnapshot, CooldownStateStore)
func (m *Manager) queueCooldownStatePersist(authIDs ...string)
func (m *Manager) FlushCooldownStates(ctx context.Context) error
```

Every existing state-changing call site queues only its known auth ID. `SetConfig` returns and queues all IDs whose cooldown was cleared. `Remove` queues the removed ID. `MarkResult` queues `result.AuthID` after releasing `m.mu` and never calls storage directly.

Change `RestoreCooldownStates` to load grouped snapshots, restore records, reconcile loaded IDs plus currently active cooling IDs, queue that union, and call `FlushCooldownStates(ctx)`.

- [ ] **Step 5: Run Manager tests and verify GREEN**

```bash
go test ./sdk/cliproxy/auth -run 'TestManager_(MarkResult|FlushCooldownStates|RestoreCooldownStates)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Run the full auth package**

```bash
go test ./sdk/cliproxy/auth -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add sdk/cliproxy/auth/cooldown_state_persister.go sdk/cliproxy/auth/conductor.go sdk/cliproxy/auth/cooldown_state_test.go
git commit -m "fix(auth): batch cooldown persistence off request path"
```

---

### Task 3: Make PostgreSQL apply only changed auth IDs

**Files:**
- Modify: `internal/store/postgres_cooldown_store_test.go`
- Modify: `internal/store/postgres_cooldown_store.go`

**Interfaces:**
- Implements: `CooldownStateStore.Load` grouped result and `Apply` replacement semantics
- Removes: full-table ID enumeration, stale-row scan, and shared `PostgresStore.mu`

- [ ] **Step 1: Write the failing PostgreSQL isolation test**

Update the live-PG test to seed auth A and auth B, then:

```go
if err := store.Apply(ctx, []cliproxyauth.CooldownStateSnapshot{
	{AuthID: authA, Records: []cliproxyauth.CooldownStateRecord{recordA}},
}); err != nil {
	t.Fatal(err)
}
loaded, err := store.Load(ctx)
if err != nil {
	t.Fatal(err)
}
if !snapshotContainsAuth(loaded, authA) || !snapshotContainsAuth(loaded, authB) {
	t.Fatalf("Apply(auth A) changed unrelated auth B: %+v", loaded)
}

if err := store.Apply(ctx, []cliproxyauth.CooldownStateSnapshot{{AuthID: authA}}); err != nil {
	t.Fatal(err)
}
loaded, err = store.Load(ctx)
if err != nil {
	t.Fatal(err)
}
if snapshotContainsAuth(loaded, authA) || !snapshotContainsAuth(loaded, authB) {
	t.Fatalf("clear(auth A) changed unexpected rows: %+v", loaded)
}
```

Define `snapshotContainsAuth` in the test file as a simple loop over loaded public snapshots.

Do not assert generated SQL text or statement order.

- [ ] **Step 2: Run store tests and verify RED**

```bash
go test ./internal/store -run 'TestPostgresCooldownStateStore|TestCooldownStateEnvelope' -count=1
```

Expected without `PGSTORE_DSN`: compile failure until `Apply` is implemented; after compilation, the live test may SKIP.

- [ ] **Step 3: Implement grouped Load and incremental Apply**

`Load` returns one `CooldownStateSnapshot` per row, using the SQL row ID as authoritative `AuthID` even if the envelope is empty.

`Apply` validates and marshals all non-empty snapshots before `BeginTx`. Inside one transaction it executes one upsert per non-empty auth and one keyed delete per empty auth, then commits. Delete all `SELECT id FROM ...` and stale-list logic. Do not acquire `c.s.mu`; cooldown writes no longer share process locking with token/config persistence.

- [ ] **Step 4: Run store tests and verify GREEN/SKIP**

```bash
go test -v ./internal/store -run 'TestPostgresCooldownStateStore|TestCooldownStateEnvelope' -count=1
```

Expected: helper tests PASS; live PG test PASS with a DSN or SKIP with the explicit `PGSTORE_DSN not set` reason.

- [ ] **Step 5: Commit**

```bash
git add internal/store/postgres_cooldown_store.go internal/store/postgres_cooldown_store_test.go
git commit -m "fix(store): apply cooldown updates incrementally"
```

---

### Task 4: Flush on shutdown and verify the full change

**Files:**
- Modify: `sdk/cliproxy/service.go`
- Test: covered at the Manager lifecycle boundary in `sdk/cliproxy/auth/cooldown_state_test.go`; do not add a new service harness solely to assert one delegation call

**Interfaces:**
- Consumes: `Manager.FlushCooldownStates(ctx)`
- Preserves: existing shutdown context and error aggregation

- [ ] **Step 1: Add the shutdown flush**

After `s.server.Stop(shutdownCtx)` has stopped request producers, call:

```go
if s.coreManager != nil {
	if errFlush := s.coreManager.FlushCooldownStates(ctx); errFlush != nil {
		log.Errorf("failed to flush cooldown state: %v", errFlush)
		if shutdownErr == nil {
			shutdownErr = errFlush
		}
	}
}
```

Do not add a new timeout; use the supplied shutdown context.

- [ ] **Step 2: Format all Go changes**

```bash
gofmt -w sdk/cliproxy/auth/cooldown_state.go sdk/cliproxy/auth/cooldown_state_persister.go sdk/cliproxy/auth/conductor.go sdk/cliproxy/auth/cooldown_state_test.go internal/store/postgres_cooldown_store.go internal/store/postgres_cooldown_store_test.go sdk/cliproxy/service.go
```

- [ ] **Step 3: Run focused and package tests**

```bash
go test ./sdk/cliproxy/auth ./internal/store ./sdk/cliproxy -count=1
```

Expected: PASS, with only the live PostgreSQL test skipped when `PGSTORE_DSN` is unset.

- [ ] **Step 4: Run the full repository test suite**

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 5: Run the required server build**

```bash
go build -o test-output ./cmd/server && rm test-output
```

Expected: exit 0 and no remaining `test-output` file.

- [ ] **Step 6: Verify structural performance targets**

```bash
rg -n 'SELECT id FROM|persistCooldownStates|cooldownStateSnapshot|\.Save\(ctx, records\)' sdk/cliproxy/auth internal/store/postgres_cooldown_store.go
```

Expected: no cooldown persistence hot-path or full-table stale-enumeration matches.

- [ ] **Step 7: Commit**

```bash
git add sdk/cliproxy/service.go
git commit -m "fix(cliproxy): flush cooldown state on shutdown"
```
