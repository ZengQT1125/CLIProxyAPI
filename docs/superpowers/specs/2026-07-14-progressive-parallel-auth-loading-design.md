# Progressive Parallel Auth Loading Design

## Goal

Make a CLIProxyAPI instance with more than one thousand file credentials begin serving quickly on a slow persistent filesystem, while loading the complete credential set in the background.

The implementation must combine bounded parallel file reads with progressive availability. It must not hide the same amount of serial work behind a later startup phase or read every credential again when the watcher starts.

## Baseline and Root Cause

The Hugging Face deployment currently takes about 188 seconds between early startup logging and the HTTP listener becoming available. The observed workload contains 1,144 credential files, including 1,120 xAI credentials.

The downloaded SQLite usage database is healthy:

- 42,030 rows and approximately 29 MiB;
- both SQLite integrity checks pass;
- no rows are older than the configured 30-day retention window;
- rebuilding the newly added auth-index expression index takes about 70 ms on the local reference machine.

The database therefore does not explain a recurring 188-second startup delay. It does reveal the credential scale: 892 distinct auth indexes appear in retained usage, including 672 active in the latest seven days.

The dominant startup path is synchronous small-file I/O:

1. `Service.Run` calls `coreManager.Load` before constructing and starting the API server.
2. `Manager.Load` calls `FileTokenStore.List`.
3. `FileTokenStore.List` walks the auth directory and serially reads, parses, and stats each JSON file.
4. After the API server starts, the watcher performs another auth-directory rescan to build hashes and watcher state. Existing watcher code has paths that read the same file more than once during that rescan.

At the observed scale, 188 seconds is about 164 ms per credential file. That is consistent with serialized metadata and small-file operations on a remote persistent filesystem.

The root design flaw is duplicated, serial, pre-listen credential discovery. Parallelizing only `FileTokenStore.List` would reduce one pass but preserve duplicate watcher work and still prevent progressive availability.

## Success Criteria

Using the same Hugging Face instance, persistent volume, configuration, and 1,144-file credential set:

- the HTTP listener starts within two seconds of normal configuration initialization;
- the first valid credential batch is selectable within three seconds;
- the complete credential set loads within 30 seconds with the default worker count;
- each credential JSON is read at most once during the initial load;
- at most `auth-load-workers` credential files are open concurrently;
- loaded credentials become selectable without waiting for the full directory scan;
- a malformed or unreadable file does not block valid credentials;
- watcher state, auth-manager state, model registrations, and cooldown state converge to the same result as a completed synchronous startup;
- changes that occur during initial loading win over stale scan results;
- shutdown cancels the loader and leaves no worker or progress goroutine running.

These are wall-clock and behavioral targets, not assumptions. Completion requires measurements on the same Hugging Face workload.

## Considered Approaches

### 1. Parallelize `FileTokenStore.List` and keep atomic startup

A worker pool could read files concurrently and return one complete slice to `Manager.Load`.

This reduces the first scan but does not provide progressive availability. It also leaves the watcher to rescan and reread the same files after startup. It attacks one symptom rather than the ownership problem and is rejected.

### 2. Start one goroutine per credential file

This is superficially simple, but 1,144 simultaneous opens and parses can exhaust file descriptors, amplify memory use, and cause the Hugging Face filesystem to throttle or collapse. Completion order becomes uncontrolled and cancellation becomes noisy. It is rejected.

### 3. Watcher-owned bounded scan with progressive batch registration

The watcher becomes the single owner of initial file discovery. A bounded worker pool reads each file once and produces all data needed by both watcher bookkeeping and runtime registration. A serialized aggregator commits small batches to watcher caches and the service auth pipeline. The API server starts before this scan.

This removes duplicate work, bounds resource use, preserves one source of truth for file lifecycle, and provides progressive availability. This is the selected approach.

## Startup Sequence

`Service.Run` changes from "load everything, then listen" to the following sequence:

1. Initialize in-memory usage, configuration, retry policy, plugin configuration, and the core auth manager.
2. Synthesize the small set of config-defined API-key auths synchronously.
3. Construct the watcher, attach its update queue and plugin parser, and register filesystem watches.
4. Construct and start the API server.
5. Start the watcher-owned initial auth scan in the background.
6. Start normal auth refresh only after the initial scan completes, preventing a startup refresh storm.
7. Keep processing live watcher events throughout the service lifetime.

The API server and management API are available while file credentials load. Proxy requests use the credentials that have completed registration. If no loaded credential supports a requested provider/model yet, the existing unavailable/auth-not-found response remains authoritative; there is no global `503` gate because progressive availability was explicitly selected.

Config-defined API keys remain available immediately and do not wait for the file scan.

For the default `FileTokenStore`, `Service.Run` does not call `coreManager.Load`; the watcher scan is the only initial auth source. `Watcher.Start` is split so watch registration returns synchronously and initial loading has an explicit asynchronous entry point exposed through `WatcherWrapper`. Non-file stores retain `coreManager.Load` and do not enter this file-scan path.

## Single-Read Initial Scan

The watcher performs one directory enumeration and sends JSON paths into a bounded job channel. The default worker count is 16, configured by a new `auth-load-workers` integer with an accepted range of 1 through 64.

Each worker performs exactly one credential payload read and derives:

- the raw bytes;
- the SHA-256 hash used by watcher change detection;
- the top-level provider classification;
- native synthesized auth records when the provider uses a built-in parser;
- the normalized path and source metadata;
- any parse or read error safe to expose in aggregate logs.

The worker result contains no secret material in printable error or progress fields. Raw bytes remain inside the result only until synthesis and hash state have been committed, then become unreachable.

Plugin parsing must remain concurrency-safe. The current plugin parser contract does not promise concurrent calls, so plugin-owned payloads retain their raw bytes until the aggregator serially invokes plugin parsing and performs multi-auth expansion. Native xAI, Codex, Antigravity, and Vertex parsing stays inside the parallel workers and is not forced through that serialization point.

Directory discovery and workers use bounded channels for backpressure. No slice containing all raw credential payloads is accumulated.

## Batch Commit and Progressive Registration

One aggregator owns commit ordering. It flushes when either condition is met:

- 32 files have completed; or
- 100 ms has elapsed since the first pending result.

A commit performs these steps as one logical batch:

1. update watcher hashes and per-path auth identity sets;
2. translate successful scan results into add/modify auth updates;
3. update the core auth manager without persisting unchanged source files;
4. register executors and models for the new auths;
5. refresh scheduler entries only after each auth's models are registered;
6. publish one immutable progress snapshot.

The manager mutation is serialized, but file reads and JSON decoding remain parallel. This keeps lock hold times short and prevents partially initialized auth records from entering the scheduler.

Batch registration must use the existing deferred API-key alias rebuild mechanism and perform shared plugin/runtime synchronization once per batch, not once per credential. The final batch performs one complete alias and plugin-model reconciliation.

Equal-priority selection must remain deterministic according to existing scheduler semantics; worker completion order must not become an implicit routing priority.

## Initial Scan and Live Event Ordering

The filesystem watch is installed before the initial scan begins. Each path has an in-memory generation marker:

- initial scan results use generation zero;
- any live create, write, rename, or remove event advances that path's generation;
- an initial result is discarded if a newer live generation exists for the same path.

This prevents a slow initial read from resurrecting a credential that was deleted or overwriting a newer credential version. Normal watcher events continue through the existing incremental update path.

Holding one global rescan mutex for the entire 30-second load is not acceptable because it would delay management uploads and deletes. Ordering is resolved per path instead.

## Cooldown Restoration

Progressive availability cannot temporarily ignore persisted cooldowns. Otherwise an exhausted credential could receive traffic before the final reconciliation.

When `save-cooldown-status` is enabled, startup loads cooldown snapshots once into a pending map keyed by auth ID. Before an auth becomes selectable, its matching snapshot is applied to the auth and scheduler state. The pending entry is then consumed.

After the initial scan completes, the manager reconciles unconsumed snapshots as stale entries and performs the existing explicit cooldown flush. It must not reread all `.cds` files for every auth batch.

When cooldown persistence is disabled, this path is a no-op.

## Progress Model

The service publishes an immutable `AuthLoadStatus` snapshot with these fields:

```json
{
  "state": "loading",
  "files_discovered": 1144,
  "files_processed": 320,
  "auths_loaded": 320,
  "files_failed": 0,
  "files_skipped": 0,
  "scan_complete": true,
  "started_at": "2026-07-14T03:08:44Z",
  "completed_at": null
}
```

States are `idle`, `loading`, `ready`, and `degraded`.

- `ready` means directory discovery and all queued files completed.
- `degraded` means the scan completed but one or more files failed, or directory enumeration ended with an error. Successfully loaded auths remain available.
- `files_discovered` can grow while directory enumeration is still active; `scan_complete` distinguishes a provisional total from the final total.
- `auths_loaded` may exceed `files_processed` because a plugin file can expand into multiple virtual auths.
- `files_skipped` counts intentionally ignored JSON payloads, while `files_failed` is reserved for read, parse, or registration errors.

Expose this snapshot through `GET /v0/management/auth-files/load-status`. Existing auth-file list responses keep their current schema. The management panel may poll this endpoint to render `processed / discovered`; the server implementation does not depend on a panel release.

Log progress at most once per second and once at completion. Logs contain counts and elapsed durations only, never file names, emails, tokens, or auth IDs.

## Error Handling

- An unreadable, empty, malformed, or registration-failing file increments `files_failed` and does not stop other workers.
- An intentionally unsupported or suppressed provider file increments `files_skipped` without marking the load degraded.
- A directory enumeration failure transitions the load to `degraded`, closes the job channel, and lets already discovered jobs finish.
- A batch registration error rejects only the affected auth records, increments the failure count, and continues unless the context is cancelled.
- A fatal server, watcher setup, or queue initialization error still fails startup because progressive loading cannot be correct without live event reconciliation.
- Context cancellation stops enumeration, workers, the aggregator timer, and pending batch processing.
- Errors are wrapped with operation context but must not include credential content.

## Configuration

Add one configuration field:

```yaml
# Maximum concurrent credential file reads during initial loading.
# Default: 16. Valid range: 1-64.
auth-load-workers: 16
```

Batch size and the 100 ms flush interval remain internal constants. They exist to bound lock and registration overhead, not as user-facing tuning knobs.

Changing `auth-load-workers` through a hot reload affects the next full initial/rescan operation; it does not resize an active worker pool.

## Testing

Tests cover public behavior and cross-module contracts, not helper delegation or source structure.

Extend existing watcher, service, manager, and management-handler test files:

- a blocked initial loader does not prevent the HTTP server from accepting connections;
- the first completed batch becomes selectable before the loader completes;
- configured worker bounds are honored under a delayed file workload;
- malformed files do not prevent valid files from loading;
- plugin multi-auth files update both progress counts and manager state correctly;
- a live update or delete wins over an older initial scan result for the same path;
- model registration completes before an auth is selectable;
- a persisted cooldown is applied before the matching auth becomes selectable;
- cancellation terminates the initial loader without leaking work;
- the load-status endpoint returns parsed structured fields for loading, ready, and degraded states;
- the existing synchronous `Store.List` behavior remains valid for SDK callers and non-file stores.

Tests must not assert goroutine names, helper calls, channel layout, source text, or log formatting.

## Performance Validation

Add stage timings for:

- process start to listener bind;
- listener bind to first registered file auth;
- directory enumeration;
- file read and synthesis;
- batch registration and model/scheduler update;
- total load completion.

On the same Hugging Face deployment, run the unchanged 1,144-file workload with worker counts 1, 4, 8, 16, and 32. Record:

- listener time;
- first-auth time;
- full-load wall time;
- file read failures;
- peak open file descriptors;
- peak RSS and CPU;
- request success during loading;
- final provider/auth counts.

The selected default remains 16 unless it fails the resource bounds or is slower than 8 on that exact workload. A single-worker run is the comparable control and must reproduce the current order of magnitude.

The same run must confirm one initial read per JSON file. If filesystem tracing is unavailable on Hugging Face, an instrumented read counter is acceptable for this structural metric.

## Rollout and Fallback

The progressive loader is the normal file-backed startup path; it is not hidden behind a permanent compatibility flag. `auth-load-workers: 1` provides an operational fallback without restoring duplicate scans or blocking listener startup.

PostgreSQL and other stores that do not use the watcher-owned file scan continue using their existing store load path. Git and object stores may adopt the same scan after their local mirror bootstrap, but that is not required for the Hugging Face fix.

Completion logging reports the final file/auth counts and elapsed time so regressions are visible without debug logging.

## Non-Goals

- Parallel token refresh or upstream credential validation.
- Loading all raw credential files into a single archive or database.
- Changing credential JSON formats.
- Changing routing priority or round-robin semantics.
- Reworking periodic auth auto-refresh.
- Bundling a management-panel release in this repository.
- Optimizing PostgreSQL auth loading.
