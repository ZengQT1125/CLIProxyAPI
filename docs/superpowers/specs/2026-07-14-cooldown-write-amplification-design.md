# Cooldown Persistence Write Amplification Design

## Goal

Persist cooldown state without blocking request retries or rewriting cooldown state for unrelated credentials. A burst that changes `K` auth identities must perform `O(K)` row work, not `O(K^2)` repeated full snapshots.

This design supersedes the full-snapshot `Load / Save` semantics in `2026-07-14-pg-cooldown-persistence-design.md`. The configuration gate, persisted JSON envelope, and restart behavior remain unchanged.

## Root Cause

`Manager.MarkResult` detects a change for one auth identity, but `CooldownStateStore.Save` accepts only a process-wide flat snapshot. Every change therefore scans all auths and rewrites every currently cooling auth row. PostgreSQL also enumerates the entire cooldown table to find stale rows.

When one request discovers `K` newly exhausted credentials, the number of upserts is `1 + 2 + ... + K`, or `K(K+1)/2`. Each snapshot also holds the shared `PostgresStore` mutex, opens a transaction, enumerates all stored IDs, and commits before the request can try the next credential.

The structural defect is the persistence contract: a per-auth state transition is represented as a global replacement.

## Success Criteria

- `MarkResult` performs no cooldown storage I/O and does not wait for PostgreSQL or filesystem persistence.
- Persistence work is proportional to the number of changed auth IDs in a batch.
- No request-time operation enumerates all cooldown rows or files.
- Multiple changes for the same auth before a flush persist only the latest in-memory state.
- A failed batch is retained for retry; an explicit flush reports the error.
- Graceful service shutdown flushes pending cooldown changes.
- Updating or clearing auth A never rewrites or deletes auth B.
- Existing `save-cooldown-status`, Home-mode behavior, JSON record shape, and restart restoration behavior remain unchanged.

## Considered Approaches

### 1. Synchronous per-auth writes

Replace the global snapshot with a per-auth update but keep the database call inside `MarkResult`.

This removes quadratic write amplification, but a request that probes many exhausted credentials still waits for one transaction per credential. Remote PostgreSQL latency remains directly additive to request latency.

### 2. Manager-owned incremental batch writer

The Manager records dirty auth IDs and a single writer flushes them after a fixed 100 ms batching window. The writer snapshots only those IDs and applies one batch transaction. This removes storage I/O from the request path, coalesces repeated changes, and keeps storage behavior independent of the concrete backend.

This is the selected approach.

### 3. PostgreSQL-only asynchronous fast path

Add an optional interface or type assertion for PostgreSQL while retaining full-snapshot behavior for file storage.

This leaves two persistence contracts, keeps the incorrect global API, and preserves ordering bugs in the file implementation. It is rejected.

## Persistence Contract

Replace the flat global contract with per-auth snapshots:

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

Semantics:

- Each snapshot replaces persisted cooldown state for exactly one `AuthID`.
- A non-empty `Records` slice upserts that auth's complete cooldown envelope.
- An empty `Records` slice deletes only that auth's persisted state.
- `Apply` must not inspect, rewrite, or delete IDs absent from the input batch.
- A batch may contain multiple auth IDs; implementations must treat duplicate IDs as invalid input or normalize them before writing. The Manager always emits unique IDs.
- `Load` preserves the auth grouping and returns the row/file `AuthID` even when its record list is empty. This allows startup reconciliation to remove malformed or obsolete empty entries without a global table scan.

The persisted record and envelope JSON formats remain version 1.

## Manager Data Flow

`Manager` owns a `cooldownStatePersister` with a dirty-ID set. The persister has no permanent goroutine. The first dirty mark starts one delayed flush; later marks join the same batch. After the batch completes, it exits when no dirty IDs remain.

```text
MarkResult / Update / Reset / Delete
              |
              v
       mark auth_id dirty       request returns
              |
         100 ms batch window
              |
              v
 snapshot current state for dirty IDs only
              |
              v
       store.Apply(batch)
```

The worker serializes flushes. It drains dirty IDs only after acquiring the flush token. If state changes during an in-flight write, that ID is added to the next batch, so an older snapshot cannot be the final persisted state.

Normal background failures requeue the whole batch and retry after one second. Because PostgreSQL applies the batch transactionally and file updates are idempotent per auth, retrying the complete batch is safe. Logging remains warning-level and contains no auth secrets.

`Manager.FlushCooldownStates(ctx)` forces immediate persistence and returns any error. Startup restoration uses it to complete stale cleanup before continuing. Service shutdown calls it after the HTTP server and auth update producers have stopped.

The 100 ms window intentionally allows loss of the last unflushed cooldown changes on process crash. Cooldown state is recoverable runtime health, not transaction-critical user data; eliminating request-path database latency is the higher priority. Graceful shutdown does not lose pending changes.

## Store Implementations

### PostgreSQL

`PostgresCooldownStateStore.Apply` marshals the changed snapshots before opening a transaction. Inside one short transaction it:

1. upserts every non-empty snapshot by `AuthID`;
2. deletes every empty snapshot by `AuthID`;
3. commits.

It does not execute `SELECT id FROM cooldown_store`, does not touch unrelated rows, and does not use the token/config store's process-wide mutex. `database/sql` and PostgreSQL row locking provide the required isolation. Different instances remain last-write-wins only when they update the same auth ID; they no longer delete unrelated state owned by another instance.

### File store

`FileCooldownStateStore` keeps an in-memory `AuthID -> .cds path` index populated by `Load` and successful writes. A non-empty snapshot writes only the corresponding file atomically. An empty snapshot removes only the indexed file. If an auth file path changes, the new file is written before the old indexed path is removed.

No incremental write walks the cooldown directory. `Load` remains the only directory-wide operation.

## Restore and Reconciliation

`RestoreCooldownStates` loads grouped snapshots, restores unexpired records, and builds the union of:

- IDs loaded from the store, including empty or unknown entries;
- IDs currently registered in the Manager.

It marks that union dirty and performs an explicit flush. The result writes current active cooldown state and deletes loaded stale entries per auth. This is the only global reconciliation path and runs during startup or explicit configuration reload, not after each upstream result.

## Shutdown and Reconfiguration

- Disabling `save-cooldown-status` makes future snapshot application a no-op and drops pending persistence once the worker observes the nil store.
- Enabling or replacing the store uses the latest Manager state on the next flush; queued IDs are never coupled to a stale store pointer.
- `Service.Shutdown` stops request/auth producers, stops the HTTP server, then calls `FlushCooldownStates(ctx)` and reports an error if persistence fails.
- No new network timeout is introduced. The caller-provided shutdown context remains authoritative.

## Testing

Tests extend existing files rather than creating parallel suites.

`sdk/cliproxy/auth/cooldown_state_test.go` covers public Manager/store behavior:

- a cooldown change emits one snapshot for only the changed auth;
- clearing a cooldown emits an empty snapshot for only that auth;
- a blocking store cannot block `MarkResult`;
- a failed explicit flush retains the batch for a later successful flush;
- a change made during an in-flight apply is persisted by a later batch;
- restoration deletes stale loaded IDs and keeps active state.

`sdk/cliproxy/auth/cooldown_state_test.go` also verifies file-store isolation: applying auth A must not rewrite or delete auth B.

`internal/store/postgres_cooldown_store_test.go` verifies the same public incremental contract against PostgreSQL when `PGSTORE_DSN` is available. Tests must not assert SQL strings, helper delegation, or statement order.

## Performance Validation

The deterministic regression target is structural:

- request-thread store calls per cooldown change: `0`;
- upsert/delete row work for a batch of `K` changed auth IDs: at most `K`;
- global ID enumerations in `Apply`: `0`;
- transactions for changes inside one batching window: `1`.

With a live PostgreSQL environment, seed 1,000 cooling auths and compare sequential 429 handling before and after using the same DSN, hardware, and concurrency. Record request p50/p95/p99, transaction count, statement calls, WAL bytes, and lock wait. The current environment has no `PGSTORE_DSN`, so live latency numbers are not a completion gate; the public behavior tests and query-shape removal are.

## Non-Goals

- Distributed consensus or cross-instance merge semantics for simultaneous updates to the same auth ID.
- A configurable debounce duration.
- A new management API for cooldown rows.
- Embedding cooldown state into `auth_store`.
- Changing the default value of `save-cooldown-status`.
