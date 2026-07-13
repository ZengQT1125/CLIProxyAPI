# PG Cooldown Persistence Design

## Goal

When auth tokens are stored in PostgreSQL (`PGSTORE_DSN`), runtime credential cooldown health must also survive process restart. Selectors must not re-pick credentials that are still inside a cooldown window after restart.

## Problem

Cooldown persistence already exists for file-backed auth:

- Config gate: `save-cooldown-status` (default `false`)
- Store: `FileCooldownStateStore` writes per-auth `.cds` files under `auth-dir`
- Runtime: `Manager.persistCooldownStates` / `RestoreCooldownStates`

Under PostgreSQL this is broken even when the flag is true:

1. `configureCooldownStateStore` always installs `FileCooldownStateStore` on the local spool `authDir`.
2. `PostgresStore.syncAuthFromDatabase` does `os.RemoveAll(s.authDir)` then rewrites auth JSON from DB.
3. `.cds` files live in that same directory and are wiped on every bootstrap.
4. There is no `cooldown_store` (or equivalent) in PostgreSQL.

Root cause: cooldown is tied to a disposable local spool, not to the durable PG backend.

## Scope

In scope:

- Persist/restore cooldown via PostgreSQL when the active token store is `*PostgresStore`.
- Keep the existing `CooldownStateStore` interface and Manager lifecycle.
- Keep `save-cooldown-status` as the feature gate (default remains `false`).
- Keep Home mode forcing cooldown persistence off.

Out of scope:

- Multi-instance strong consistency / distributed locks (explicitly **last-write-wins**, mode A).
- Git / Object store dedicated cooldown backends (file `.cds` remains for non-PG).
- Changing default of `save-cooldown-status` to true.
- Management API CRUD for cooldown records.
- Dual-writing PG + `.cds` under PG mode.

## Approach

**Separate `PostgresCooldownStateStore` type** implements `auth.CooldownStateStore` against a new table `cooldown_store`.

Reason: `PostgresStore` already has `Save(ctx, *Auth) (string, error)` for the token `Store` interface. Go forbids a second `Save` with a different signature on the same type, so `PostgresStore` cannot implement `CooldownStateStore` directly.

`Service.configureCooldownStateStore`:

1. If `!SaveCooldownStatus` or Home enabled → `SetCooldownStateStore(nil)`.
2. Else if active token store is `*store.PostgresStore` → `SetCooldownStateStore(NewPostgresCooldownStateStore(pg))`.
3. Else → existing `NewFileCooldownStateStoreWithAuthDir(authDir, authDir)`.

Manager, MarkResult, Restore, and selector logic stay unchanged.

## Data Model

### Table

```sql
CREATE TABLE IF NOT EXISTS cooldown_store (
  id TEXT PRIMARY KEY,
  content JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

- `id`: same identity space as `auth_store.id` (relative auth id under spool).
- Schema-qualified via existing `fullTableName` / `PGSTORE_SCHEMA`.
- Table name default: `cooldown_store`.
- Add optional `CooldownTable` on `PostgresStoreConfig` (empty → `cooldown_store`), same pattern as `AuthTable` / `ConfigTable`.

### Content JSON (align with `.cds` envelope)

```json
{
  "version": 1,
  "auth_id": "<auth id>",
  "provider": "<provider>",
  "updated_at": "<RFC3339>",
  "records": [
    {
      "auth_id": "<auth id>",
      "model": "<model or empty for auth-level>",
      "status": "cooling",
      "next_retry_after": "<RFC3339>",
      "reason": "...",
      "quota": {},
      "last_error": {},
      "updated_at": "<RFC3339>"
    }
  ]
}
```

Reuse the same Go types already used by file store (`cooldownStateFile` / `CooldownStateRecord`) or share marshaling helpers so file and PG payloads stay compatible in shape.

## Load / Save Semantics

Match `FileCooldownStateStore`:

### Load

- `SELECT id, content FROM cooldown_store`.
- Unmarshal each row's `records`.
- Return flattened `[]CooldownStateRecord`.
- Missing table after EnsureSchema should not happen; empty table → empty slice, not error.

### Save(records)

- Group records by auth id (same grouping key as file path groups by auth).
- For each auth with ≥1 still-cooling record: upsert row (`INSERT ... ON CONFLICT DO UPDATE`).
- Delete rows whose ids are not in the desired set (stale cleanup), including full clear when `records` is empty.
- Do not write expired records; Manager already filters via `authCooldownStateRecord` / `modelCooldownStateRecord` (`NextRetryAfter.After(now)`).

### Restore

Unchanged:

- Skip expired (`!NextRetryAfter.After(now)`).
- Skip missing / disabled / cooling-disabled auths.
- After restore, `persistCooldownStates` rewrites store to drop expired leftovers.

## Wiring

| Location | Change |
|---|---|
| `internal/store/postgresstore.go` | `EnsureSchema` creates `cooldown_store`; add `CooldownTable` default |
| `internal/store/postgres_cooldown_store.go` (new) | `PostgresCooldownStateStore` with `Load`/`Save`; holds `*PostgresStore` (or db + naming helpers) |
| `sdk/cliproxy/service.go` | `configureCooldownStateStore` type-asserts `GetTokenStore()` to `*store.PostgresStore` and installs `NewPostgresCooldownStateStore` |
| `config.example.yaml` | Note: with `PGSTORE_DSN`, cooldown is stored in PG table `cooldown_store`, not `.cds` |

Coupling: `sdk/cliproxy` already depends on internal packages where needed; type-assert to `*store.PostgresStore` is acceptable. Do **not** import `internal/store` into `sdk/cliproxy/auth`.

Share JSON envelope marshaling with the file store shape (`version` / `records`) so payloads stay isomorphic; either duplicate the small envelope struct in `internal/store` or export a minimal DTO from `auth` if one already exists without pulling file I/O.

## Error Handling

- Persist failures: log warn, do not fail the user request (existing `failed to persist cooldown state` path).
- Restore failures at startup/config reload: log warn, continue with empty cooldown state.
- PG down after startup: same as auth path — warn on save; in-memory state remains source until process exit.

## Multi-instance (Mode A)

Documented limitation: `Save` is a full snapshot from one Manager. Concurrent instances last-write-wins and may drop cooldowns only known to another instance. Acceptable for single-instance / non-coordinated multi-replica. No locks in this design.

## Testing

1. **Unit / store tests** for PG cooldown Load/Save/stale delete (sqlmock or testcontainers if the repo already uses one for PG; otherwise pure logic helpers + mock DB if present).
2. **`configureCooldownStateStore`**: PG token store → non-nil store that is not file-only; file token store → `FileCooldownStateStore`; flag false / Home → nil.
3. **Existing** `cooldown_state_test.go` Manager tests remain on mock `CooldownStateStore` — no change required for core restore/persist-on-change behavior.
4. Manual / integration (optional): enable `save-cooldown-status`, force a cooldown, restart with `PGSTORE_DSN`, confirm auth stays unavailable until `next_retry_after`.

## Verification

- Focused tests for new store methods and configure branch.
- `go test` for `internal/store` and `sdk/cliproxy` packages touched.
- `go build -o cli-proxy-api ./cmd/server/`.

## Non-goals Recap

- Do not embed cooldown into `auth_store` JSONB.
- Do not preserve `.cds` across `RemoveAll(authDir)` as the PG solution.
- Do not enable the feature by default.
- Do not implement git/object cooldown tables in this change.
