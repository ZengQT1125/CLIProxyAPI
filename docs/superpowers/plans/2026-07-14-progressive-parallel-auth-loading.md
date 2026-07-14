# Progressive Parallel Auth Loading Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bind the HTTP listener before file credentials are loaded, then load file credentials once with 16 bounded readers and make each completed batch selectable immediately.

**Architecture:** The default `FileTokenStore` path is marked explicitly by `Builder`; only that path delegates initial file discovery to `internal/watcher`. `Watcher.Start` installs fsnotify watches synchronously, while `StartInitialAuthLoad` performs one cancellable directory scan, parallel reads, serialized batch commits, and immutable progress publication. Existing non-file stores keep `Manager.Load`, and the existing Service auth-update/model-registration pipeline remains the only runtime registration path.

**Tech Stack:** Go 1.26+, fsnotify, Gin, `sync/atomic`, existing watcher synthesizers, existing core auth manager and scheduler.

## Global Constraints

- The root cause is duplicated serial pre-listen credential discovery; no task may reintroduce a synchronous file-auth scan before listener bind.
- Default `auth-load-workers` is 16; valid values are clamped to 1 through 64.
- Initial batches flush at 32 completed files or 100 ms after the first pending result.
- Each credential JSON is read at most once during initial loading.
- At most `auth-load-workers` credential payload reads may run concurrently.
- A credential must have pending cooldown restored and models registered before its scheduler entry becomes selectable.
- A live create, write, rename, remove, upload, or delete must win over a generation-zero initial-scan result for the same normalized path.
- Raw credential bytes, file names, emails, tokens, and auth IDs must not appear in progress or aggregate error logs.
- Plugin parser calls are serialized because the parser contract does not promise concurrency.
- Equal-priority routing remains deterministic under existing scheduler ordering; worker completion order is not routing priority.
- PostgreSQL, git, object, custom Manager, and Home startup paths retain their current store-loading behavior.
- Tests assert exported behavior, state, persistence, and HTTP contracts; they do not assert helper calls, source text, goroutine names, or markup.
- Run `gofmt -w` on every changed Go file and run `go build -o test-output ./cmd/server && rm test-output` before completion.

---

## File Map

- Configuration: `internal/config/config.go`, `internal/config/parse.go`, `internal/config/auth_load_workers_test.go`, `config.example.yaml`.
- Cooldown lifecycle: `sdk/cliproxy/auth/conductor.go`, `sdk/cliproxy/auth/cooldown_state_test.go`.
- File synthesis: `internal/watcher/synthesizer/file.go`, `internal/watcher/synthesizer/file_test.go`.
- Watcher loader: `internal/watcher/auth_load.go`, `internal/watcher/auth_load_status.go`, `internal/watcher/watcher.go`, `internal/watcher/events.go`, `internal/watcher/clients.go`, `internal/watcher/dispatcher.go`, `internal/watcher/watcher_test.go`.
- SDK bridge and startup: `sdk/cliproxy/types.go`, `sdk/cliproxy/watcher.go`, `sdk/cliproxy/builder.go`, `sdk/cliproxy/service.go`, `sdk/cliproxy/service_progressive_auth_loading_test.go`.
- Management contract: `internal/api/server.go`, `internal/api/server_test.go`, `internal/api/handlers/management/handler.go`, `internal/api/handlers/management/auth_files.go`, `internal/api/handlers/management/auth_files_list_test.go`, `internal/api/handlers/management/auth_files_upload_test.go`, `internal/api/handlers/management/auth_files_delete_test.go`.
- Evidence and docs: `internal/watcher/auth_load_benchmark_test.go`, `README.md`, `README_CN.md`, `docs/performance/2026-07-14-progressive-auth-loading.md`.

## Watcher Loader Implementation Detail

**Files:**
- Create: `internal/watcher/auth_load_status.go`
- Create: `internal/watcher/auth_load.go`
- Modify: `internal/watcher/watcher.go`
- Modify: `internal/watcher/events.go`
- Modify: `internal/watcher/clients.go`
- Modify: `internal/watcher/dispatcher.go`
- Modify: `internal/watcher/watcher_test.go`
- Modify: `sdk/cliproxy/types.go`
- Modify: `sdk/cliproxy/watcher.go`

**Interfaces:**
- Produces: `watcher.AuthLoadStatus`, `watcher.AuthLoadState`, and JSON field names from the design
- Produces: `watcher.AuthUpdateBatch{Updates []AuthUpdate, Result chan<- []AuthUpdateResult, Initial bool}`
- Produces: `func (w *Watcher) StartInitialAuthLoad(context.Context, int) <-chan struct{}`
- Produces: `func (w *Watcher) AuthLoadStatus() AuthLoadStatus`
- Produces: `func (w *Watcher) MarkAuthPathChanged(string)` for synchronous management mutations
- Produces: `watcher.AuthLoadHooks{Before func(context.Context) error, After func(context.Context) error}`
- Produces: `func (w *Watcher) SetFileAuthLoadingEnabled(bool)`
- Produces: matching `WatcherWrapper.StartInitialAuthLoad` and `WatcherWrapper.AuthLoadStatus`
- Consumes: split synthesizer APIs from Task 3
- Consumed by: Service startup and management status in Tasks 5 and 6

#### Detail 1: Failing watcher behavior tests

Extend `internal/watcher/watcher_test.go` with public-behavior tests covering worker bounds, progressive batches, errors, plugin expansion, live-event precedence, and cancellation. Use a test replacement for the package-level reader only to observe the public concurrency limit, not to assert helper delegation:

```go
func TestInitialAuthLoadHonorsWorkerLimitAndPublishesReady(t *testing.T) {
	authDir := t.TempDir()
	for i := 0; i < 40; i++ {
		path := filepath.Join(authDir, fmt.Sprintf("auth-%03d.json", i))
		if errWrite := os.WriteFile(path, []byte(`{"type":"xai"}`), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
	}

	originalRead := readInitialAuthFile
	defer func() { readInitialAuthFile = originalRead }()
	var active atomic.Int32
	var peak atomic.Int32
	readInitialAuthFile = func(path string) ([]byte, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for current > peak.Load() && !peak.CompareAndSwap(peak.Load(), current) {
		}
		time.Sleep(10 * time.Millisecond)
		return os.ReadFile(path)
	}

	queue := make(chan AuthUpdateBatch, 8)
	w := newInitialLoadTestWatcher(t, authDir, queue)
	done := w.StartInitialAuthLoad(context.Background(), 4)
	loaded := 0
	for {
		select {
		case batch := <-queue:
			loaded += len(batch.Updates)
			batch.Result <- loadedResults(batch.Updates)
		case <-done:
			goto loadComplete
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for initial auth load")
		}
	}
	loadComplete:
	if peak.Load() > 4 {
		t.Fatalf("peak concurrent reads = %d, want <= 4", peak.Load())
	}
	status := w.AuthLoadStatus()
	if status.State != AuthLoadStateReady || status.FilesProcessed != 40 || status.AuthsLoaded != loaded {
		t.Fatalf("status = %+v, loaded = %d", status, loaded)
	}
}

func TestInitialAuthLoadMakesFirstBatchAvailableBeforeCompletion(t *testing.T) {
	authDir := writeInitialLoadAuthFiles(t, 40)
	releaseTail := make(chan struct{})
	originalRead := readInitialAuthFile
	defer func() { readInitialAuthFile = originalRead }()
	readInitialAuthFile = func(path string) ([]byte, error) {
		if filepath.Base(path) >= "auth-032.json" {
			<-releaseTail
		}
		return os.ReadFile(path)
	}
	queue := make(chan AuthUpdateBatch, 8)
	w := newInitialLoadTestWatcher(t, authDir, queue)
	done := w.StartInitialAuthLoad(context.Background(), 4)
	first := <-queue
	first.Result <- loadedResults(first.Updates)
	eventually(t, time.Second, func() bool { return w.AuthLoadStatus().AuthsLoaded > 0 })
	select {
	case <-done:
		t.Fatal("load completed before delayed tail was released")
	default:
	}
	close(releaseTail)
	acknowledgeInitialLoadUntilDone(t, queue, done)
}

func TestInitialAuthLoadMalformedFileDoesNotBlockValidFile(t *testing.T) {
	authDir := t.TempDir()
	mustWriteFile(t, filepath.Join(authDir, "broken.json"), []byte(`{"type":`))
	mustWriteFile(t, filepath.Join(authDir, "valid.json"), []byte(`{"type":"xai"}`))
	queue := make(chan AuthUpdateBatch, 4)
	w := newInitialLoadTestWatcher(t, authDir, queue)
	done := w.StartInitialAuthLoad(context.Background(), 2)
	acknowledgeInitialLoadUntilDone(t, queue, done)
	status := w.AuthLoadStatus()
	if status.State != AuthLoadStateDegraded || status.FilesProcessed != 2 || status.FilesFailed != 1 || status.AuthsLoaded != 1 {
		t.Fatalf("status = %+v", status)
	}
}

func TestInitialAuthLoadPluginExpansionCountsAuths(t *testing.T) {
	authDir := t.TempDir()
	mustWriteFile(t, filepath.Join(authDir, "plugin.json"), []byte(`{"type":"plugin"}`))
	queue := make(chan AuthUpdateBatch, 4)
	w := newInitialLoadTestWatcher(t, authDir, queue)
	w.SetPluginAuthParser(watcherMultiAuthParserFunc(func(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
		return []*coreauth.Auth{{ID: "virtual-a", Provider: "plugin"}, {ID: "virtual-b", Provider: "plugin"}}, true, nil
	}))
	done := w.StartInitialAuthLoad(context.Background(), 2)
	acknowledgeInitialLoadUntilDone(t, queue, done)
	status := w.AuthLoadStatus()
	if status.FilesProcessed != 1 || status.AuthsLoaded != 2 {
		t.Fatalf("status = %+v", status)
	}
}

func TestInitialAuthLoadDropsResultAfterLiveDelete(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "stale.json")
	mustWriteFile(t, path, []byte(`{"type":"xai"}`))
	readCaptured := make(chan struct{})
	releaseRead := make(chan struct{})
	originalRead := readInitialAuthFile
	defer func() { readInitialAuthFile = originalRead }()
	readInitialAuthFile = func(path string) ([]byte, error) {
		data, errRead := os.ReadFile(path)
		close(readCaptured)
		<-releaseRead
		return data, errRead
	}
	queue := make(chan AuthUpdateBatch, 4)
	w := newInitialLoadTestWatcher(t, authDir, queue)
	done := w.StartInitialAuthLoad(context.Background(), 1)
	<-readCaptured
	if errRemove := os.Remove(path); errRemove != nil {
		t.Fatal(errRemove)
	}
	w.handleEvent(fsnotify.Event{Name: path, Op: fsnotify.Remove})
	close(releaseRead)
	<-done
	select {
	case batch := <-queue:
		t.Fatalf("stale scan emitted batch: %+v", batch.Updates)
	default:
	}
}

func TestInitialAuthLoadStopsOnCancellation(t *testing.T) {
	authDir := writeInitialLoadAuthFiles(t, 8)
	releaseRead := make(chan struct{})
	originalRead := readInitialAuthFile
	defer func() { readInitialAuthFile = originalRead }()
	readInitialAuthFile = func(path string) ([]byte, error) {
		<-releaseRead
		return os.ReadFile(path)
	}
	queue := make(chan AuthUpdateBatch, 4)
	w := newInitialLoadTestWatcher(t, authDir, queue)
	ctx, cancel := context.WithCancel(context.Background())
	done := w.StartInitialAuthLoad(ctx, 2)
	cancel()
	close(releaseRead)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loader did not stop after cancellation")
	}
	terminal := w.AuthLoadStatus()
	time.Sleep(20 * time.Millisecond)
	if got := w.AuthLoadStatus(); !reflect.DeepEqual(got, terminal) {
		t.Fatalf("status changed after completion: before=%+v after=%+v", terminal, got)
	}
}

func TestInitialAuthLoadDirectoryFailureIsDegraded(t *testing.T) {
	authDir := t.TempDir()
	queue := make(chan AuthUpdateBatch, 1)
	w := newInitialLoadTestWatcher(t, authDir, queue)
	if errRemove := os.RemoveAll(authDir); errRemove != nil {
		t.Fatal(errRemove)
	}
	done := w.StartInitialAuthLoad(context.Background(), 2)
	<-done
	status := w.AuthLoadStatus()
	if status.State != AuthLoadStateDegraded || !status.ScanComplete || status.FilesDiscovered != 0 {
		t.Fatalf("status = %+v", status)
	}
}

func TestFullAuthRescanAcceptsStablePreviouslyChangedPath(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "changed.json")
	mustWriteFile(t, path, []byte(`{"type":"xai"}`))
	queue := make(chan AuthUpdateBatch, 4)
	w := newInitialLoadTestWatcher(t, authDir, queue)
	w.MarkAuthPathChanged(path)
	firstDone := w.StartInitialAuthLoad(context.Background(), 1)
	<-firstDone
	if status := w.AuthLoadStatus(); status.FilesSkipped != 1 {
		t.Fatalf("first scan status = %+v, want live generation skip", status)
	}
	secondDone := w.StartInitialAuthLoad(context.Background(), 1)
	acknowledgeInitialLoadUntilDone(t, queue, secondDone)
	if status := w.AuthLoadStatus(); status.AuthsLoaded != 1 {
		t.Fatalf("second scan status = %+v, want stable changed path loaded", status)
	}
}

func TestAuthLoadHooksWrapEveryFullScan(t *testing.T) {
	authDir := writeInitialLoadAuthFiles(t, 1)
	queue := make(chan AuthUpdateBatch, 4)
	w := newInitialLoadTestWatcher(t, authDir, queue)
	var before atomic.Int32
	var after atomic.Int32
	w.SetAuthLoadHooks(AuthLoadHooks{
		Before: func(context.Context) error { before.Add(1); return nil },
		After: func(context.Context) error { after.Add(1); return nil },
	})
	for scan := int32(1); scan <= 2; scan++ {
		if scan == 2 {
			mustWriteFile(t, filepath.Join(authDir, "auth-000.json"), []byte(`{"type":"xai","note":"changed"}`))
		}
		done := w.StartInitialAuthLoad(context.Background(), 1)
		batch := <-queue
		if got := before.Load(); got != scan {
			t.Fatalf("before count at batch = %d, want %d", got, scan)
		}
		batch.Result <- loadedResults(batch.Updates)
		<-done
		if got := after.Load(); got != scan {
			t.Fatalf("after count at completion = %d, want %d", got, scan)
		}
	}
}

func TestAuthLoadBeforeHookFailurePreventsCredentialRegistration(t *testing.T) {
	authDir := writeInitialLoadAuthFiles(t, 1)
	queue := make(chan AuthUpdateBatch, 1)
	w := newInitialLoadTestWatcher(t, authDir, queue)
	w.SetAuthLoadHooks(AuthLoadHooks{
		Before: func(context.Context) error { return errors.New("cooldown unavailable") },
	})
	done := w.StartInitialAuthLoad(context.Background(), 1)
	<-done
	status := w.AuthLoadStatus()
	if status.State != AuthLoadStateDegraded || !status.ScanComplete || status.FilesProcessed != 0 {
		t.Fatalf("status = %+v", status)
	}
	select {
	case batch := <-queue:
		t.Fatalf("hook failure emitted auth updates: %+v", batch.Updates)
	default:
	}
}

func TestInitialAuthLoadDisabledForNonFileStore(t *testing.T) {
	authDir := writeInitialLoadAuthFiles(t, 1)
	queue := make(chan AuthUpdateBatch, 1)
	w := newInitialLoadTestWatcher(t, authDir, queue)
	w.SetFileAuthLoadingEnabled(false)
	done := w.StartInitialAuthLoad(context.Background(), 16)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled loader did not return immediately")
	}
	if status := w.AuthLoadStatus(); status.State != AuthLoadStateIdle {
		t.Fatalf("status = %+v, want idle", status)
	}
}
```

Add these shared test helpers in the same file:

```go
func newInitialLoadTestWatcher(t testing.TB, authDir string, queue chan AuthUpdateBatch) *Watcher {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	mustWriteFile(t, configPath, []byte("port: 8317\n"))
	w, errNew := NewWatcher(configPath, authDir, nil)
	if errNew != nil {
		t.Fatal(errNew)
	}
	w.SetConfig(&config.Config{AuthDir: authDir, AuthLoadWorkers: 16})
	w.SetAuthUpdateQueue(queue)
	t.Cleanup(func() { _ = w.Stop() })
	return w
}

type watcherMultiAuthParserFunc func(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error)

func (f watcherMultiAuthParserFunc) ParseAuth(context.Context, pluginapi.AuthParseRequest) (*coreauth.Auth, bool, error) {
	return nil, false, nil
}

func (f watcherMultiAuthParserFunc) ParseAuths(ctx context.Context, req pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
	return f(ctx, req)
}

func mustWriteFile(t testing.TB, path string, data []byte) {
	t.Helper()
	if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
}

func writeInitialLoadAuthFiles(t testing.TB, count int) string {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i < count; i++ {
		mustWriteFile(t, filepath.Join(dir, fmt.Sprintf("auth-%03d.json", i)), []byte(`{"type":"xai"}`))
	}
	return dir
}

func eventually(t testing.TB, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}

func loadedResults(updates []AuthUpdate) []AuthUpdateResult {
	results := make([]AuthUpdateResult, 0, len(updates))
	for _, update := range updates {
		results = append(results, AuthUpdateResult{ID: update.ID, Loaded: true})
	}
	return results
}

func acknowledgeInitialLoadUntilDone(t testing.TB, queue <-chan AuthUpdateBatch, done <-chan struct{}) {
	t.Helper()
	for {
		select {
		case batch := <-queue:
			batch.Result <- loadedResults(batch.Updates)
		case <-done:
			return
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for initial auth load")
		}
	}
}
```

#### Detail 2: Focused failing-test command

Run: `go test ./internal/watcher -run 'Test(InitialAuthLoad|FullAuthRescan|AuthLoad)' -count=1`

Expected: FAIL because the load/status and batch types do not exist.

#### Detail 3: Immutable status types and atomic publication

Create `internal/watcher/auth_load_status.go`:

```go
package watcher

import "time"

type AuthLoadState string

const (
	AuthLoadStateIdle     AuthLoadState = "idle"
	AuthLoadStateLoading  AuthLoadState = "loading"
	AuthLoadStateReady    AuthLoadState = "ready"
	AuthLoadStateDegraded AuthLoadState = "degraded"
)

type AuthLoadStatus struct {
	State           AuthLoadState `json:"state"`
	FilesDiscovered int64         `json:"files_discovered"`
	FilesProcessed  int64         `json:"files_processed"`
	AuthsLoaded     int64         `json:"auths_loaded"`
	FilesFailed     int64         `json:"files_failed"`
	FilesSkipped    int64         `json:"files_skipped"`
	ScanComplete    bool          `json:"scan_complete"`
	StartedAt       time.Time     `json:"started_at"`
	CompletedAt     *time.Time    `json:"completed_at"`
}

func idleAuthLoadStatus() AuthLoadStatus {
	return AuthLoadStatus{State: AuthLoadStateIdle}
}

func (w *Watcher) publishAuthLoadStatus(status AuthLoadStatus) {
	if w == nil {
		return
	}
	if status.CompletedAt != nil {
		completed := *status.CompletedAt
		status.CompletedAt = &completed
	}
	w.authLoadStatus.Store(status)
}

func (w *Watcher) AuthLoadStatus() AuthLoadStatus {
	if w == nil {
		return idleAuthLoadStatus()
	}
	value := w.authLoadStatus.Load()
	if value == nil {
		return idleAuthLoadStatus()
	}
	return value.(AuthLoadStatus)
}
```

#### Detail 4: Acknowledged batch queue contract

Add beside `AuthUpdate` in `internal/watcher/watcher.go`:

```go
type AuthUpdateResult struct {
	ID     string
	Loaded bool
}

type AuthUpdateBatch struct {
	Updates []AuthUpdate
	Result  chan<- []AuthUpdateResult
	Initial bool
}

type AuthLoadHooks struct {
	Before func(context.Context) error
	After  func(context.Context) error
}
```

Change `authQueue`, `SetAuthUpdateQueue`, wrapper fields, and Service-facing channel types from `chan<- AuthUpdate` to `chan<- AuthUpdateBatch`. Change `dispatchLoop` to preserve each pending slice as one channel item:

```go
batch, ok := w.nextPendingBatch(ctx)
if !ok {
	return
}
select {
case queue <- AuthUpdateBatch{Updates: batch}:
case <-ctx.Done():
	return
}
```

Add an acknowledged path used only by initial loading:

```go
func (w *Watcher) dispatchInitialAuthBatch(ctx context.Context, updates []AuthUpdate) []AuthUpdateResult {
	if len(updates) == 0 {
		return nil
	}
	queue := w.getAuthQueue()
	if queue == nil {
		return nil
	}
	resultCh := make(chan []AuthUpdateResult, 1)
	batch := AuthUpdateBatch{Updates: updates, Result: resultCh, Initial: true}
	select {
	case queue <- batch:
	case <-ctx.Done():
		return nil
	}
	select {
	case results := <-resultCh:
		return results
	case <-ctx.Done():
		return nil
	}
}
```

Update existing watcher queue tests to read `batch := <-queue` and assert `batch.Updates`; do not weaken ordering assertions.

#### Detail 5: Loader lifecycle and per-path generation state

Add to `Watcher`:

```go
authLoadMu         sync.Mutex
authLoadCancel     context.CancelFunc
authLoadDone       chan struct{}
authLoadStatus     atomic.Value
pathGenerations    map[string]uint64
nextPathGeneration uint64
authLoadSequence   uint64
authLoadHooks      AuthLoadHooks
fileAuthLoadingEnabled bool
```

Initialize status, the generation map, and `fileAuthLoadingEnabled=true` in `NewWatcher`. Add a lock-protected setter. `StartInitialAuthLoad` returns an already-closed channel without changing status when loading is disabled; `reloadClients` never starts a full file scan in that mode. Add generation methods which always run under `clientsMutex`:

```go
func (w *Watcher) advancePathGenerationLocked(path string) uint64 {
	w.nextPathGeneration++
	w.pathGenerations[path] = w.nextPathGeneration
	return w.nextPathGeneration
}

func (w *Watcher) pathGenerationCurrentLocked(path string, generation uint64) bool {
	return w.pathGenerations[path] == generation
}
```

At the beginning of every relevant auth event in `handleEvent`, advance the normalized path before reading, deleting, or checking hashes. Also advance it in `dispatchPersistedAuthUpdate`, so management uploads/deletes that are already reflected in runtime state supersede an older scan result even before fsnotify delivery.

Expose the same operation for management paths that delete the runtime auth directly:

```go
func (w *Watcher) MarkAuthPathChanged(path string) {
	if w == nil {
		return
	}
	normalized := w.normalizeAuthPath(path)
	if normalized == "" {
		return
	}
	w.clientsMutex.Lock()
	w.advancePathGenerationLocked(normalized)
	w.clientsMutex.Unlock()
}
```

Remove `w.reloadClients(true, nil, false)` from `events.go:start`; `Start` now returns after both fsnotify watches and `processEvents` are active.

Add a lock-protected `SetAuthLoadHooks(AuthLoadHooks)` method. `runInitialAuthLoad` calls a copied `Before` hook before snapshotting inputs or enumerating files. A hook error ends the scan as `degraded` with `scan_complete=true` and `completed_at` set, without reading JSON. After workers, acknowledged commits, and missing-path reconciliation finish, it calls `After` before publishing terminal `ready/degraded`; an `After` error makes the terminal state degraded. Skip `After` when the load context is cancelled.

#### Detail 6: Bounded one-read worker pipeline

Create `internal/watcher/auth_load.go` with these constants and result ownership:

```go
package watcher

const (
	authLoadBatchSize     = 32
	authLoadFlushInterval = 100 * time.Millisecond
	authLoadLogInterval   = time.Second
)

var readInitialAuthFile = os.ReadFile

type initialAuthJob struct {
	path       string
	normalized string
	generation uint64
}

type authLoadSnapshot struct {
	config  *config.Config
	parser  synthesizer.PluginAuthParser
	authDir string
	now     time.Time
}

type initialAuthReadResult struct {
	path       string
	normalized string
	generation uint64
	raw        []byte
	hash       string
	native     synthesizer.NativeAuthFileResult
	readTime   time.Duration
	synthTime  time.Duration
	err        error
}
```

`StartInitialAuthLoad` cancels an older full scan, creates a child context, publishes `loading`, stores a done channel, and launches `runInitialAuthLoad`. Clamp the worker count again at this boundary so direct SDK callers cannot exceed 64:

```go
func (w *Watcher) StartInitialAuthLoad(ctx context.Context, workers int) <-chan struct{} {
	if w == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if workers == 0 {
		workers = config.DefaultAuthLoadWorkers
	}
	workers = max(config.MinAuthLoadWorkers, min(workers, config.MaxAuthLoadWorkers))
	loadCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	w.authLoadMu.Lock()
	if w.authLoadCancel != nil {
		w.authLoadCancel()
	}
	w.authLoadCancel = cancel
	w.authLoadDone = done
	isInitialScan := w.authLoadSequence == 0
	w.authLoadSequence++
	w.authLoadMu.Unlock()
	w.publishAuthLoadStatus(AuthLoadStatus{State: AuthLoadStateLoading, StartedAt: time.Now().UTC()})
	go w.runInitialAuthLoad(loadCtx, workers, done, isInitialScan)
	return done
}
```

`runInitialAuthLoad` must implement this concrete ownership sequence:

```go
func (w *Watcher) runInitialAuthLoad(ctx context.Context, workers int, done chan struct{}, isInitialScan bool) {
	defer close(done)
	snapshot := w.snapshotAuthLoadInputs()
	baseline := w.snapshotFileGenerationBaseline(isInitialScan)
	jobs := make(chan initialAuthJob, workers*2)
	results := make(chan initialAuthReadResult, workers*2)
	var workersWG sync.WaitGroup
	for i := 0; i < workers; i++ {
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			w.initialAuthReadWorker(ctx, snapshot, jobs, results)
		}()
	}
	go func() {
		w.enumerateInitialAuthFiles(ctx, jobs, isInitialScan)
		close(jobs)
		workersWG.Wait()
		close(results)
	}()
	w.aggregateInitialAuthResults(ctx, snapshot, results, baseline)
}
```

`snapshotAuthLoadInputs` must take `clientsMutex`, copy `authDir` and parser, call `Config.CloneForRuntime()`, and capture one UTC `now` before releasing the lock. Workers and the aggregator use only that immutable snapshot, preventing management/config hot reload from racing parallel synthesis and preventing completion order from changing auth timestamps.

`snapshotFileGenerationBaseline` copies every current `fileAuthsByPath` key and its generation under `clientsMutex`; it uses zero values on the first scan and current values on later scans. The final missing-path reconciliation compares against this immutable baseline.

The enumerator calls `os.ReadDir` exactly once, filters direct child `.json` files, increments `files_discovered`, then sets `scan_complete=true`. For the first scan it assigns generation zero to every job; for later full rescans it snapshots `pathGenerations[normalized]` under `clientsMutex` into each job. Each worker copies that generation into its result, checks cancellation, reads one payload once, hashes the same byte slice, and calls `SynthesizeNativeAuthFile`; it never calls the plugin parser and never stats the file.

#### Detail 7: Serialized commit, acknowledgement, and final reconciliation

The aggregator owns one timer, a maximum 32-result pending slice, and a set of paths seen in this scan. On flush it sorts results by normalized path, then for each result:

1. Increment `files_processed` once.
2. Count read/empty/JSON failures without logging the path.
3. Under `clientsMutex`, discard a result and increment `files_skipped` when `pathGenerations[path] != result.generation`.
4. Invoke `SynthesizePluginAuthFile` serially. If handled, use plugin auths; otherwise use worker-produced native auths.
5. Under `clientsMutex`, update `lastAuthHashes`, optional debug content, `fileAuthsByPath`, and `currentAuths`, then compute per-path updates.
6. Treat synthesized auth IDs that already match `currentAuths` as loaded without emitting redundant updates; this keeps later full-rescan counts meaningful.
7. Send all changed auths from the flush in one acknowledged `AuthUpdateBatch`.
8. Map `AuthUpdateResult` IDs back to their source path. Increment `auths_loaded` for unchanged live auths and for changed auths only when `Loaded=true`; count each affected source file once in `files_failed` when registration failed.
9. Nil every `raw` field before the next receive so secret payloads become unreachable.

Use this terminal state rule:

```go
completed := time.Now().UTC()
status.CompletedAt = &completed
if status.FilesFailed > 0 || enumerationFailed {
	status.State = AuthLoadStateDegraded
} else {
	status.State = AuthLoadStateReady
}
w.publishAuthLoadStatus(status)
```

The first scan's fixed generation zero ensures any live event before commit wins. A later full rescan's captured generation allows a previously changed but now stable path to converge, while any event newer than the capture still wins.

After all scan results commit, compare `fileAuthsByPath` with the scan's seen paths. For an absent path whose generation still equals the value captured when that scan started, emit delete updates and remove its hash/path identity state. Never delete config-defined or runtime auths.

Accumulate `directory_enumeration`, `file_read`, `native_synthesis`, `plugin_synthesis`, `batch_registration` (queue send through acknowledgement), and `total_load` durations. Log counts and stage durations no more than once per second and once at completion. Use fields named `files_discovered`, `files_processed`, `auths_loaded`, `files_failed`, `files_skipped`, and the six duration names; omit path and identity fields.

#### Detail 8: Later full rescans and duplicate-read removal

Delete `loadFileClients` and the serial hash/synthesis rescan block from `clients.go`. Split config/runtime refresh from file refresh so `reloadClients` does not call `snapshotCoreAuths`, which would read all JSON files again.

Compute:

```go
needsFullScan := w.fileAuthLoadingIsEnabled() &&
	(rescanAuth || len(affectedOAuthProviders) > 0 || forceAuthRefresh || w.AuthLoadStatus().State == AuthLoadStateLoading)
```

`fileAuthLoadingIsEnabled` reads the field under `clientsMutex`. The last clause ensures a config reload during the initial scan cancels and replaces that scan rather than letting Service finalize cooldown state against a partial auth set. When `needsFullScan` is true, cancel the active loader, wait for its `done` channel so its cancelled `After` hook cannot race the new config, publish a fresh `loading` snapshot before invoking `reloadCallback(cfg)`, then trigger:

```go
w.StartInitialAuthLoad(context.Background(), cfg.AuthLoadWorkers)
```

The pre-callback loading snapshot tells Service that the scan hooks, not the immediate config-update path, own config-auth registration and cooldown preparation/completion. Invoke `reloadCallback(cfg)` before starting the rescan so Service has applied plugin/config changes and `SetPluginAuthParser` exposes the current parser to `snapshotAuthLoadInputs`. When `needsFullScan` is false, synthesize config API-key auths and merge existing `fileAuthsByPath` plus `runtimeAuths` without opening credential JSON files.

#### Detail 9: Clean loader and dispatcher shutdown

In `Watcher.Stop`, cancel `authLoadCancel` under `authLoadMu`, then stop dispatch/timers and close fsnotify. The loader must select on context during enumeration, job send, result send, batch queue send, batch acknowledgement, and timer wait.

#### Detail 10: Wrapper methods

Add to `WatcherWrapper`:

```go
startInitialAuthLoad func(context.Context, int) <-chan struct{}
authLoadStatus       func() watcher.AuthLoadStatus
markAuthPathChanged  func(string)
setAuthLoadHooks     func(watcher.AuthLoadHooks)
setFileAuthLoadingEnabled func(bool)
```

and public nil-safe methods:

```go
func (w *WatcherWrapper) StartInitialAuthLoad(ctx context.Context, workers int) <-chan struct{} {
	if w == nil || w.startInitialAuthLoad == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return w.startInitialAuthLoad(ctx, workers)
}

func (w *WatcherWrapper) AuthLoadStatus() watcher.AuthLoadStatus {
	if w == nil || w.authLoadStatus == nil {
		return watcher.AuthLoadStatus{State: watcher.AuthLoadStateIdle}
	}
	return w.authLoadStatus()
}

func (w *WatcherWrapper) MarkAuthPathChanged(path string) {
	if w != nil && w.markAuthPathChanged != nil {
		w.markAuthPathChanged(path)
	}
}

func (w *WatcherWrapper) SetAuthLoadHooks(hooks watcher.AuthLoadHooks) {
	if w != nil && w.setAuthLoadHooks != nil {
		w.setAuthLoadHooks(hooks)
	}
}

func (w *WatcherWrapper) SetFileAuthLoadingEnabled(enabled bool) {
	if w != nil && w.setFileAuthLoadingEnabled != nil {
		w.setFileAuthLoadingEnabled(enabled)
	}
}
```

Wire `startInitialAuthLoad`, `authLoadStatus`, `markAuthPathChanged`, `setAuthLoadHooks`, and `setFileAuthLoadingEnabled` directly to the concrete watcher in `defaultWatcherFactory`.

#### Detail 11: Race-focused verification commands

Run: `gofmt -w internal/watcher/auth_load.go internal/watcher/auth_load_status.go internal/watcher/watcher.go internal/watcher/events.go internal/watcher/clients.go internal/watcher/dispatcher.go internal/watcher/watcher_test.go sdk/cliproxy/types.go sdk/cliproxy/watcher.go`

Run: `go test -race ./internal/watcher/... -count=1`

Expected: PASS with no race report. Existing start tests now assert watch registration without an implicit reload callback; full-load behavior is asserted through `StartInitialAuthLoad`.

#### Detail 12: Watcher implementation commit

```bash
git add internal/watcher/auth_load.go internal/watcher/auth_load_status.go internal/watcher/watcher.go internal/watcher/events.go internal/watcher/clients.go internal/watcher/dispatcher.go internal/watcher/watcher_test.go sdk/cliproxy/types.go sdk/cliproxy/watcher.go
git commit -m "feat(watcher): load file auths progressively in parallel"
```

---

## Detailed File Inventory

- `internal/config/config.go`: declares and normalizes `AuthLoadWorkers` for file-loaded YAML.
- `internal/config/parse.go`: applies the same default and normalization to management-supplied YAML.
- `internal/config/auth_load_workers_test.go`: covers the new configuration contract; no existing config test owns startup worker limits.
- `config.example.yaml`: documents the one public tuning knob.
- `sdk/cliproxy/auth/conductor.go`: owns pending cooldown snapshots and consumes them as auths register.
- `sdk/cliproxy/auth/cooldown_state_test.go`: extends existing cooldown behavior coverage.
- `internal/watcher/synthesizer/file.go`: separates native synthesis from serialized plugin expansion without changing `SynthesizeAuthFile` callers.
- `internal/watcher/synthesizer/file_test.go`: extends existing file-synthesis contract coverage.
- `internal/watcher/auth_load.go`: owns bounded initial discovery, reads, aggregation, per-path generations, timings, and final reconciliation.
- `internal/watcher/auth_load_status.go`: owns immutable public load-state types and atomic publication.
- `internal/watcher/watcher.go`: adds loader lifecycle fields and exported entry points.
- `internal/watcher/events.go`: installs watches without rescanning and advances live path generations.
- `internal/watcher/clients.go`: removes duplicate initial scans and reuses the bounded loader for later full rescans.
- `internal/watcher/dispatcher.go`: delivers one `AuthUpdateBatch` per commit instead of splitting it into individual channel sends.
- `internal/watcher/watcher_test.go`: extends existing watcher lifecycle, ordering, cancellation, and batch behavior coverage.
- `sdk/cliproxy/types.go`: exposes batch queue, initial-load start, completion, and status through `WatcherWrapper`.
- `sdk/cliproxy/watcher.go`: wires the concrete watcher into `WatcherWrapper`.
- `sdk/cliproxy/builder.go`: marks only a Builder-created manager backed by `*sdkAuth.FileTokenStore` as progressive.
- `sdk/cliproxy/service.go`: reorders startup, consumes batches, binds full-scan cooldown hooks, and starts auto-refresh after initial completion.
- `sdk/cliproxy/service_progressive_auth_loading_test.go`: covers the Service lifecycle contract; no existing Service test runs the normal listener/watcher lifecycle.
- `internal/api/server.go`: injects the status provider and registers the management route.
- `internal/api/handlers/management/handler.go`: stores the status provider.
- `internal/api/handlers/management/auth_files.go`: serves the structured load-status response.
- `internal/api/handlers/management/auth_files_list_test.go`: extends the existing auth-file management contract.
- `internal/api/handlers/management/auth_files_upload_test.go`: verifies successful saves synchronously invalidate an older scan generation.
- `internal/api/handlers/management/auth_files_delete_test.go`: verifies successful deletes synchronously invalidate an older scan generation.
- `internal/watcher/auth_load_benchmark_test.go`: provides the 1,144-file local control; no watcher benchmark file exists.
- `README.md`, `README_CN.md`: document progressive startup and `auth-load-workers`.
- `docs/performance/2026-07-14-progressive-auth-loading.md`: records the required same-HF measurements and acceptance result.

---

### Task 1: Configuration Contract

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/parse.go`
- Create: `internal/config/auth_load_workers_test.go`
- Modify: `config.example.yaml`

**Interfaces:**
- Produces: `config.Config.AuthLoadWorkers int`
- Produces: `config.DefaultAuthLoadWorkers`, `config.MinAuthLoadWorkers`, and `config.MaxAuthLoadWorkers`
- Consumed by: watcher initial and later full scans in Task 4

- [ ] **Step 1: Add failing config contract tests**

Create `internal/config/auth_load_workers_test.go` with table-driven assertions against the public parser:

```go
package config

import "testing"

func TestParseConfigBytesAuthLoadWorkers(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want int
	}{
		{name: "default", yaml: "port: 8317\n", want: 16},
		{name: "minimum", yaml: "auth-load-workers: 1\n", want: 1},
		{name: "maximum", yaml: "auth-load-workers: 64\n", want: 64},
		{name: "below minimum", yaml: "auth-load-workers: -8\n", want: 1},
		{name: "above maximum", yaml: "auth-load-workers: 128\n", want: 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			if cfg.AuthLoadWorkers != tt.want {
				t.Fatalf("AuthLoadWorkers = %d, want %d", cfg.AuthLoadWorkers, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test and verify the missing field failure**

Run: `go test ./internal/config -run TestParseConfigBytesAuthLoadWorkers -count=1`

Expected: FAIL because `Config.AuthLoadWorkers` does not exist.

- [ ] **Step 3: Add one normalized configuration field**

Add beside `AuthDir` in `internal/config/config.go`:

```go
const (
	DefaultPanelGitHubRepository = "https://github.com/caidaoli/Cli-Proxy-API-Management-Center"
	DefaultPprofAddr             = "127.0.0.1:8316"
	DefaultAuthDir               = "~/.cli-proxy-api"
	DefaultAuthLoadWorkers       = 16
	MinAuthLoadWorkers           = 1
	MaxAuthLoadWorkers           = 64
)

// AuthLoadWorkers bounds concurrent credential file reads during a full auth scan.
AuthLoadWorkers int `yaml:"auth-load-workers" json:"auth-load-workers"`
```

Add this method in `internal/config/config.go` and call it after YAML unmarshal in both `LoadConfigOptional` and `ParseConfigBytes`:

```go
func (cfg *Config) normalizeAuthLoadWorkers() {
	if cfg.AuthLoadWorkers == 0 {
		cfg.AuthLoadWorkers = DefaultAuthLoadWorkers
		return
	}
	if cfg.AuthLoadWorkers < MinAuthLoadWorkers {
		log.WithField("value", cfg.AuthLoadWorkers).Warn("auth-load-workers too small; clamping to 1")
		cfg.AuthLoadWorkers = MinAuthLoadWorkers
		return
	}
	if cfg.AuthLoadWorkers > MaxAuthLoadWorkers {
		log.WithField("value", cfg.AuthLoadWorkers).Warn("auth-load-workers too large; clamping to 64")
		cfg.AuthLoadWorkers = MaxAuthLoadWorkers
	}
}
```

Set `cfg.AuthLoadWorkers = DefaultAuthLoadWorkers` alongside the existing pre-unmarshal defaults in both parsing functions, then call `cfg.normalizeAuthLoadWorkers()` before plugin/config sanitization.

- [ ] **Step 4: Document the setting in the example config**

Insert directly after `auth-dir` in `config.example.yaml`:

```yaml
# Maximum concurrent credential file reads during initial and full auth scans.
# Valid range: 1-64. Default: 16.
auth-load-workers: 16
```

- [ ] **Step 5: Format and verify the configuration package**

Run: `gofmt -w internal/config/config.go internal/config/parse.go internal/config/auth_load_workers_test.go`

Run: `go test ./internal/config -run TestParseConfigBytesAuthLoadWorkers -count=1`

Expected: PASS.

- [ ] **Step 6: Commit the configuration contract**

```bash
git add internal/config/config.go internal/config/parse.go internal/config/auth_load_workers_test.go config.example.yaml
git commit -m "feat(config): add bounded auth load workers"
```

---

### Task 2: Progressive Cooldown Restoration

**Files:**
- Modify: `sdk/cliproxy/auth/conductor.go`
- Modify: `sdk/cliproxy/auth/cooldown_state_test.go`

**Interfaces:**
- Produces: `func (m *Manager) PrepareCooldownRestore(context.Context) error`
- Produces: `func (m *Manager) CompleteCooldownRestore(context.Context) error`
- Preserves: `func (m *Manager) RestoreCooldownStates(context.Context) error`
- Consumed by: progressive Service startup in Task 5

- [ ] **Step 1: Add failing tests for restore-before-selectability and one final cleanup**

Extend `sdk/cliproxy/auth/cooldown_state_test.go` with tests that use the existing `recordingCooldownStateStore`:

```go
func TestManager_PreparedCooldownAppliedDuringRegister(t *testing.T) {
	nextRetry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	store := &recordingCooldownStateStore{load: []CooldownStateSnapshot{{
		AuthID: "auth-1",
		Records: []CooldownStateRecord{{
			AuthID: "auth-1", Provider: "xai", Model: "grok-4",
			Status: "cooling", NextRetryAfter: nextRetry,
			Quota: QuotaState{Exceeded: true, NextRecoverAt: nextRetry},
		}},
	}}}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	if errPrepare := manager.PrepareCooldownRestore(context.Background()); errPrepare != nil {
		t.Fatalf("PrepareCooldownRestore() error = %v", errPrepare)
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "auth-1", Provider: "xai"}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	auth, ok := manager.GetByID("auth-1")
	if !ok || auth.ModelStates["grok-4"] == nil || !auth.ModelStates["grok-4"].Unavailable {
		t.Fatalf("registered auth = %+v, want persisted cooldown before scheduler registration", auth)
	}
	if got := store.applyCount.Load(); got != 0 {
		t.Fatalf("Apply count before completion = %d, want 0", got)
	}
	if errComplete := manager.CompleteCooldownRestore(context.Background()); errComplete != nil {
		t.Fatalf("CompleteCooldownRestore() error = %v", errComplete)
	}
	if got := store.applyCount.Load(); got != 1 {
		t.Fatalf("Apply count after completion = %d, want 1", got)
	}
}

func TestManager_CompleteCooldownRestoreClearsStaleSnapshot(t *testing.T) {
	store := &recordingCooldownStateStore{load: []CooldownStateSnapshot{{
		AuthID: "missing-auth",
		Records: []CooldownStateRecord{{
			AuthID: "missing-auth", NextRetryAfter: time.Now().Add(time.Hour),
		}},
	}}}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	if errPrepare := manager.PrepareCooldownRestore(context.Background()); errPrepare != nil {
		t.Fatal(errPrepare)
	}
	if errComplete := manager.CompleteCooldownRestore(context.Background()); errComplete != nil {
		t.Fatal(errComplete)
	}
	batches := store.recordedBatches()
	last := batches[len(batches)-1]
	if len(last) != 1 || last[0].AuthID != "missing-auth" || len(last[0].Records) != 0 {
		t.Fatalf("cleanup batch = %+v, want empty missing-auth snapshot", last)
	}
}

func TestManager_PrepareCooldownRestoreAppliesAlreadyRegisteredAuth(t *testing.T) {
	nextRetry := time.Now().Add(time.Hour)
	store := &recordingCooldownStateStore{load: []CooldownStateSnapshot{{
		AuthID: "auth-live",
		Records: []CooldownStateRecord{{AuthID: "auth-live", NextRetryAfter: nextRetry}},
	}}}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "auth-live", Provider: "xai"}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errPrepare := manager.PrepareCooldownRestore(context.Background()); errPrepare != nil {
		t.Fatal(errPrepare)
	}
	auth, ok := manager.GetByID("auth-live")
	if !ok || !auth.Unavailable || !auth.NextRetryAfter.Equal(nextRetry) {
		t.Fatalf("auth after prepare = %+v", auth)
	}
}
```

- [ ] **Step 2: Run the focused tests and verify the missing methods**

Run: `go test ./sdk/cliproxy/auth -run 'TestManager_(PreparedCooldown|CompleteCooldown)' -count=1`

Expected: FAIL because the two phase methods do not exist.

- [ ] **Step 3: Add pending restore state to Manager**

Add these fields under `cooldownStore` in `Manager`:

```go
pendingCooldownRestore map[string][]CooldownStateRecord
cooldownRestoreIDs     map[string]struct{}
```

Update `SetCooldownStateStore` so assigning `nil` also clears both pending maps under `m.mu`; disabling cooldown persistence during a hot reload must not leave old snapshots available for later registrations.

Implement preparation so the store is read exactly once:

```go
func (m *Manager) PrepareCooldownRestore(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	store := m.cooldownStore
	m.mu.RUnlock()
	if store == nil {
		return nil
	}
	snapshots, errLoad := store.Load(ctx)
	if errLoad != nil {
		return fmt.Errorf("load cooldown snapshots: %w", errLoad)
	}
	pending := make(map[string][]CooldownStateRecord, len(snapshots))
	ids := make(map[string]struct{}, len(snapshots))
	for _, snapshot := range snapshots {
		authID := strings.TrimSpace(snapshot.AuthID)
		if authID == "" {
			continue
		}
		ids[authID] = struct{}{}
		records := append([]CooldownStateRecord(nil), snapshot.Records...)
		for i := range records {
			if strings.TrimSpace(records[i].AuthID) == "" {
				records[i].AuthID = authID
			}
		}
		pending[authID] = records
	}
	m.mu.Lock()
	m.pendingCooldownRestore = pending
	m.cooldownRestoreIDs = ids
	now := time.Now()
	schedulerSnapshots := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		m.applyPendingCooldownRestoreLocked(auth, now)
		if len(m.cooldownStateRecordsForAuthLocked(auth, now)) > 0 {
			m.cooldownRestoreIDs[auth.ID] = struct{}{}
		}
		schedulerSnapshots = append(schedulerSnapshots, auth.Clone())
	}
	m.mu.Unlock()
	if m.scheduler != nil {
		for _, auth := range schedulerSnapshots {
			m.scheduler.upsertAuth(auth)
		}
	}
	return nil
}
```

- [ ] **Step 4: Consume a matching snapshot before scheduler upsert**

Refactor the body of `restoreCooldownRecordLocked` into a helper that accepts an explicit auth pointer, then keep the old method as a lookup wrapper:

```go
func restoreCooldownRecord(auth *Auth, record CooldownStateRecord, now time.Time, cooldownDisabled bool) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || cooldownDisabled {
		return false
	}
	if record.NextRetryAfter.IsZero() || !record.NextRetryAfter.After(now) {
		return false
	}
	updatedAt := record.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	reason := strings.TrimSpace(record.Reason)
	model := strings.TrimSpace(record.Model)
	quota := record.Quota
	if quota.Exceeded && quota.NextRecoverAt.IsZero() {
		quota.NextRecoverAt = record.NextRetryAfter
	}
	if model == "" {
		auth.Unavailable = true
		auth.Status = StatusError
		auth.NextRetryAfter = record.NextRetryAfter
		auth.Quota = quota
		auth.UpdatedAt = updatedAt
		if reason != "" {
			auth.StatusMessage = reason
		}
		auth.LastError = cloneError(record.LastError)
		return true
	}
	state := ensureModelState(auth, model)
	state.Unavailable = true
	state.Status = StatusError
	state.NextRetryAfter = record.NextRetryAfter
	state.Quota = quota
	state.UpdatedAt = updatedAt
	if reason != "" {
		state.StatusMessage = reason
	}
	state.LastError = cloneError(record.LastError)
	updateAggregatedAvailability(auth, now)
	return true
}

func (m *Manager) restoreCooldownRecordLocked(record CooldownStateRecord, now time.Time) bool {
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		return false
	}
	auth := m.auths[authID]
	return restoreCooldownRecord(auth, record, now, m.cooldownDisabledForAuth(auth))
}

func (m *Manager) applyPendingCooldownRestoreLocked(auth *Auth, now time.Time) {
	if auth == nil || auth.ID == "" || m.pendingCooldownRestore == nil {
		return
	}
	records, ok := m.pendingCooldownRestore[auth.ID]
	if !ok {
		return
	}
	delete(m.pendingCooldownRestore, auth.ID)
	disabled := m.cooldownDisabledForAuth(auth)
	for _, record := range records {
		restoreCooldownRecord(auth, record, now, disabled)
	}
}
```

In `Register`, call `applyPendingCooldownRestoreLocked(auth, now)` while holding `m.mu`, before cloning and assigning `m.auths[auth.ID]`. In `Update`, call it after runtime-state carryover and before `authClone := auth.Clone()`. Disabled credentials still pass through the existing `clearCooldownStateForAuth` path after restore, so disabled state wins.

- [ ] **Step 5: Reconcile stale snapshots once at completion**

Implement completion and rewrite `RestoreCooldownStates` as the backward-compatible composition of prepare, apply-to-current-auths, and complete:

```go
func (m *Manager) CompleteCooldownRestore(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	idSet := make(map[string]struct{}, len(m.cooldownRestoreIDs)+len(m.auths))
	for authID := range m.cooldownRestoreIDs {
		idSet[authID] = struct{}{}
	}
	now := time.Now()
	for authID, auth := range m.auths {
		if len(m.cooldownStateRecordsForAuthLocked(auth, now)) > 0 {
			idSet[authID] = struct{}{}
		}
	}
	m.pendingCooldownRestore = nil
	m.cooldownRestoreIDs = nil
	m.mu.Unlock()
	ids := make([]string, 0, len(idSet))
	for authID := range idSet {
		ids = append(ids, authID)
	}
	sort.Strings(ids)
	m.queueCooldownStatePersist(ids...)
	return m.FlushCooldownStates(ctx)
}

func (m *Manager) RestoreCooldownStates(ctx context.Context) error {
	if errPrepare := m.PrepareCooldownRestore(ctx); errPrepare != nil {
		return errPrepare
	}
	return m.CompleteCooldownRestore(ctx)
}
```

Do not queue per-auth cooldown persistence while consuming pending snapshots; the final method owns the one cleanup flush.

- [ ] **Step 6: Format and verify all cooldown tests**

Run: `gofmt -w sdk/cliproxy/auth/conductor.go sdk/cliproxy/auth/cooldown_state_test.go`

Run: `go test ./sdk/cliproxy/auth -run 'TestManager_.*Cooldown|TestFileCooldownStateStore' -count=1`

Expected: PASS, including the existing `TestManager_RestoreCooldownStates` contract.

- [ ] **Step 7: Commit phased cooldown restoration**

```bash
git add sdk/cliproxy/auth/conductor.go sdk/cliproxy/auth/cooldown_state_test.go
git commit -m "feat(auth): restore cooldowns during progressive registration"
```

---

### Task 3: Split Native and Plugin File Synthesis

**Files:**
- Modify: `internal/watcher/synthesizer/file.go`
- Modify: `internal/watcher/synthesizer/file_test.go`

**Interfaces:**
- Produces: `type NativeAuthFileResult struct { Provider string; Auths []*coreauth.Auth }`
- Produces: `SynthesizeNativeAuthFile(*SynthesisContext, string, []byte) (NativeAuthFileResult, error)`
- Produces: `SynthesizePluginAuthFile(*SynthesisContext, string, []byte) ([]*coreauth.Auth, bool, error)`
- Preserves: `SynthesizeAuthFile(*SynthesisContext, string, []byte) []*coreauth.Auth`
- Consumed by: parallel workers and serialized aggregator in Task 4

- [ ] **Step 1: Add failing synthesis boundary tests**

Extend `internal/watcher/synthesizer/file_test.go`:

```go
func TestSynthesizeNativeAuthFileDoesNotCallPluginParser(t *testing.T) {
	called := false
	ctx := &SynthesisContext{
		AuthDir: t.TempDir(),
		Now: time.Now(),
		PluginAuthParser: multiAuthParserFunc(func(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
			called = true
			return nil, false, nil
		}),
	}
	path := filepath.Join(ctx.AuthDir, "xai.json")
	result, errSynthesize := SynthesizeNativeAuthFile(ctx, path, []byte(`{"type":"xai"}`))
	if errSynthesize != nil {
		t.Fatalf("SynthesizeNativeAuthFile() error = %v", errSynthesize)
	}
	if called {
		t.Fatal("native synthesis called plugin parser")
	}
	if result.Provider != "xai" || len(result.Auths) != 1 {
		t.Fatalf("native result = %+v, want one xai auth", result)
	}
}

func TestSynthesizePluginAuthFileReportsHandledExpansion(t *testing.T) {
	ctx := &SynthesisContext{
		AuthDir: t.TempDir(), Now: time.Now(),
		PluginAuthParser: multiAuthParserFunc(func(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
			return []*coreauth.Auth{{ID: "virtual-a"}, {ID: "virtual-b"}}, true, nil
		}),
	}
	auths, handled, errSynthesize := SynthesizePluginAuthFile(ctx, filepath.Join(ctx.AuthDir, "plugin.json"), []byte(`{"type":"plugin"}`))
	if errSynthesize != nil || !handled || len(auths) != 2 {
		t.Fatalf("plugin result = (%+v, %t, %v), want two handled auths", auths, handled, errSynthesize)
	}
}

func TestSynthesizeNativeAuthFileRejectsMalformedJSON(t *testing.T) {
	_, errSynthesize := SynthesizeNativeAuthFile(&SynthesisContext{}, "broken.json", []byte(`{"type":`))
	if errSynthesize == nil {
		t.Fatal("malformed JSON returned nil error")
	}
}
```

- [ ] **Step 2: Run focused tests and verify missing symbols**

Run: `go test ./internal/watcher/synthesizer -run 'TestSynthesize(Native|Plugin)' -count=1`

Expected: FAIL because the split APIs do not exist.

- [ ] **Step 3: Extract decoded native synthesis without changing legacy behavior**

Add the public result and functions, keeping plugin decoration identical to current behavior:

```go
type NativeAuthFileResult struct {
	Provider string
	Auths    []*coreauth.Auth
}

func SynthesizeNativeAuthFile(ctx *SynthesisContext, fullPath string, data []byte) (NativeAuthFileResult, error) {
	if ctx == nil || len(data) == 0 {
		return NativeAuthFileResult{}, fmt.Errorf("auth file payload is empty")
	}
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(data, &metadata); errUnmarshal != nil {
		return NativeAuthFileResult{}, fmt.Errorf("parse auth file: %w", errUnmarshal)
	}
	provider, _ := metadata["type"].(string)
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "gemini" {
		provider = "gemini-cli"
	}
	auths := synthesizeNativeFileAuths(ctx, fullPath, metadata, provider)
	return NativeAuthFileResult{Provider: provider, Auths: auths}, nil
}

func SynthesizePluginAuthFile(ctx *SynthesisContext, fullPath string, data []byte) ([]*coreauth.Auth, bool, error) {
	if ctx == nil || ctx.PluginAuthParser == nil {
		return nil, false, nil
	}
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(data, &metadata); errUnmarshal != nil {
		return nil, false, fmt.Errorf("parse plugin auth file: %w", errUnmarshal)
	}
	provider, _ := metadata["type"].(string)
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "gemini" {
		provider = "gemini-cli"
	}
	auths, handled, errParse := parsePluginFileAuths(ctx.PluginAuthParser, pluginapi.AuthParseRequest{
		Provider: provider, Path: fullPath, FileName: filepath.Base(fullPath), RawJSON: data,
	})
	if errParse != nil || !handled {
		return nil, handled, errParse
	}
	return decoratePluginFileAuths(ctx, fullPath, metadata, compactPluginAuths(auths)), true, nil
}
```

Extract the current native branch (`internal/watcher/synthesizer/file.go:128-220`) behind this exact signature:

```go
func synthesizeNativeFileAuths(ctx *SynthesisContext, fullPath string, metadata map[string]any, provider string) []*coreauth.Auth
```

It must retain the current provider rejection, relative-path ID, Windows normalization, proxy URL, prefix, disabled status, excluded models, model aliases, priority, note, custom headers, and Codex `plan_type` mapping. Extract the current plugin decoration branch (`file.go:91-125`) behind:

```go
func decoratePluginFileAuths(ctx *SynthesisContext, fullPath string, metadata map[string]any, auths []*coreauth.Auth) []*coreauth.Auth
```

It must retain virtual-auth indexing, timestamps, path/source/source-backend attributes, source disabled state, aliases, exclusions, and custom headers. These are behavior-preserving extractions; the new tests only distinguish plugin invocation and parse errors.

Rewrite the compatibility wrapper as:

```go
func SynthesizeAuthFile(ctx *SynthesisContext, fullPath string, data []byte) []*coreauth.Auth {
	if auths, handled, errPlugin := SynthesizePluginAuthFile(ctx, fullPath, data); errPlugin == nil && handled {
		return auths
	}
	result, errNative := SynthesizeNativeAuthFile(ctx, fullPath, data)
	if errNative != nil {
		return nil
	}
	return result.Auths
}
```

- [ ] **Step 4: Verify old and new synthesis contracts**

Run: `gofmt -w internal/watcher/synthesizer/file.go internal/watcher/synthesizer/file_test.go`

Run: `go test ./internal/watcher/synthesizer -count=1`

Expected: PASS, including all existing plugin multi-auth, disabled, aliases, priority, note, and Codex plan tests.

- [ ] **Step 5: Commit synthesis separation**

```bash
git add internal/watcher/synthesizer/file.go internal/watcher/synthesizer/file_test.go
git commit -m "refactor(watcher): split native and plugin auth synthesis"
```

---

### Task 4: Watcher-Owned Bounded Loader and Batch Transport

**Files:**
- Create: `internal/watcher/auth_load_status.go`
- Create: `internal/watcher/auth_load.go`
- Modify: `internal/watcher/watcher.go`
- Modify: `internal/watcher/events.go`
- Modify: `internal/watcher/clients.go`
- Modify: `internal/watcher/dispatcher.go`
- Modify: `internal/watcher/watcher_test.go`
- Modify: `sdk/cliproxy/types.go`
- Modify: `sdk/cliproxy/watcher.go`

**Interfaces:**
- Consumes: `Config.AuthLoadWorkers`, `SynthesizeNativeAuthFile`, and `SynthesizePluginAuthFile`
- Produces: `AuthLoadStatus`, `AuthUpdateBatch`, `StartInitialAuthLoad`, and `AuthLoadStatus` exactly as specified in **Watcher Loader Implementation Detail**
- Produces: one acknowledged batch only after Service makes its auth updates selectable
- Consumed by: Tasks 5 and 6

- [ ] **Step 1: Add the eleven concrete watcher tests from Watcher Loader Implementation Detail**

Implement the full bodies for worker bound/ready status, pre-completion first batch, malformed-file isolation, plugin expansion, live-delete precedence, cancellation, directory-enumeration degradation, a later rescan of a stable path that already has a non-zero generation, before/after hook ordering across two scans, a failing `Before` hook that emits no auth updates, and disabled file loading for a non-file Store. Reuse `internal/watcher/watcher_test.go`; it already owns watcher lifecycle and event-ordering behavior.

- [ ] **Step 2: Run tests to verify the new public contract is absent**

Run: `go test ./internal/watcher -run 'Test(InitialAuthLoad|FullAuthRescan|AuthLoad)' -count=1`

Expected: FAIL on missing `AuthLoadStatus`, `AuthUpdateBatch`, and `StartInitialAuthLoad`.

- [ ] **Step 3: Implement status, batch transport, generations, workers, aggregation, and wrapper wiring**

Use the complete types, method signatures, lifecycle code, commit sequence, terminal-state rule, and wrapper bodies in **Watcher Loader Implementation Detail**. The implementation is incomplete unless all of these observable invariants hold in one run:

```text
watch registration -> listener-safe Start return
one os.ReadDir -> bounded jobs -> one os.ReadFile per JSON
native worker synthesis -> serialized plugin synthesis
generation check -> watcher cache commit -> acknowledged Service batch
model/scheduler completion acknowledgement -> immutable progress update
final missing-path reconciliation -> ready/degraded terminal snapshot
```

- [ ] **Step 4: Delete both duplicate serial startup scans**

Remove `loadFileClients` and the hash/synthesis loop in `reloadClients`. Remove the `reloadClients(true, nil, false)` call from `events.go:start`. A repository search after this step must show no initial-start call to `snapshotCoreAuths` and no auth-directory loop that calls `os.ReadFile` outside `auth_load.go` or the live single-file update path.

- [ ] **Step 5: Update existing watcher queue/start tests for the intentional contract change**

Change queue fixtures from `chan AuthUpdate` to `chan AuthUpdateBatch`, assert `batch.Updates`, and change `TestStartAndStopSuccess` to expect zero reload callbacks from `Start`. Do not remove event, queue saturation, ordering, or cancellation coverage.

- [ ] **Step 6: Format and verify under race detection**

Run: `gofmt -w internal/watcher/auth_load.go internal/watcher/auth_load_status.go internal/watcher/watcher.go internal/watcher/events.go internal/watcher/clients.go internal/watcher/dispatcher.go internal/watcher/watcher_test.go sdk/cliproxy/types.go sdk/cliproxy/watcher.go`

Run: `go test -race ./internal/watcher/... -count=1`

Expected: PASS with no race report.

- [ ] **Step 7: Commit the bounded loader**

```bash
git add internal/watcher/auth_load.go internal/watcher/auth_load_status.go internal/watcher/watcher.go internal/watcher/events.go internal/watcher/clients.go internal/watcher/dispatcher.go internal/watcher/watcher_test.go sdk/cliproxy/types.go sdk/cliproxy/watcher.go
git commit -m "feat(watcher): load file auths progressively in parallel"
```

---

### Task 5: Management Load-Status Contract

**Files:**
- Modify: `internal/api/server.go`
- Modify: `internal/api/handlers/management/handler.go`
- Modify: `internal/api/handlers/management/auth_files.go`
- Modify: `internal/api/handlers/management/auth_files_list_test.go`
- Modify: `internal/api/handlers/management/auth_files_upload_test.go`
- Modify: `internal/api/handlers/management/auth_files_delete_test.go`
- Modify: `internal/api/server_test.go`

**Interfaces:**
- Consumes: `func() watcher.AuthLoadStatus`
- Produces: `api.WithAuthLoadStatusProvider(func() watcher.AuthLoadStatus) ServerOption`
- Produces: `api.WithAuthFileMutationHook(func(string)) ServerOption`
- Produces: `func (s *api.Server) Listening() <-chan net.Addr` as an exact listener-bind signal
- Produces: `GET /v0/management/auth-files/load-status`
- Consumed by: Service construction in Task 6

- [ ] **Step 1: Add failing structured-response tests**

Extend `internal/api/handlers/management/auth_files_list_test.go`:

```go
func TestGetAuthFileLoadStatus(t *testing.T) {
	completed := time.Date(2026, 7, 14, 3, 9, 1, 0, time.UTC)
	tests := []watcher.AuthLoadStatus{
		{State: watcher.AuthLoadStateLoading, FilesDiscovered: 3, FilesProcessed: 1, AuthsLoaded: 1},
		{State: watcher.AuthLoadStateReady, FilesDiscovered: 3, FilesProcessed: 3, AuthsLoaded: 3, ScanComplete: true, CompletedAt: &completed},
		{State: watcher.AuthLoadStateDegraded, FilesDiscovered: 3, FilesProcessed: 3, AuthsLoaded: 2, FilesFailed: 1, ScanComplete: true, CompletedAt: &completed},
	}
	for _, want := range tests {
		t.Run(string(want.State), func(t *testing.T) {
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
			h.SetAuthLoadStatusProvider(func() watcher.AuthLoadStatus { return want })
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/load-status", nil)
			h.GetAuthFileLoadStatus(ctx)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			var got watcher.AuthLoadStatus
			if errDecode := json.Unmarshal(recorder.Body.Bytes(), &got); errDecode != nil {
				t.Fatalf("decode response: %v", errDecode)
			}
			if got.State != want.State || got.FilesProcessed != want.FilesProcessed || got.FilesFailed != want.FilesFailed || got.ScanComplete != want.ScanComplete {
				t.Fatalf("response = %+v, want %+v", got, want)
			}
		})
	}
}
```

Add a route-level assertion to `internal/api/server_test.go` that an authenticated GET to `/v0/management/auth-files/load-status` returns 200 and parsed state rather than checking a raw JSON fragment.

Also add a server lifecycle test that starts `Server.Start` on `127.0.0.1:0`, receives one non-nil address from `Server.Listening`, connects to it, stops the server, and asserts the readiness channel closes without a second value.

- [ ] **Step 2: Run focused tests and verify missing provider/route failures**

Run: `go test ./internal/api/... -run 'Test(GetAuthFileLoadStatus|ManagementAuthFileLoadStatusRoute|ServerListening)' -count=1`

Expected: FAIL because the setter, handler, option, and route do not exist.

- [ ] **Step 3: Store a nil-safe provider in the management handler**

Add to `Handler`:

```go
authLoadStatusProvider func() watcher.AuthLoadStatus
authFileMutationHook   func(string)
```

Add:

```go
func (h *Handler) SetAuthLoadStatusProvider(provider func() watcher.AuthLoadStatus) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.authLoadStatusProvider = provider
	h.mu.Unlock()
}

func (h *Handler) SetAuthFileMutationHook(hook func(string)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.authFileMutationHook = hook
	h.mu.Unlock()
}
```

Implement the endpoint in `auth_files.go`:

```go
func (h *Handler) GetAuthFileLoadStatus(c *gin.Context) {
	h.mu.Lock()
	provider := h.authLoadStatusProvider
	h.mu.Unlock()
	if provider == nil {
		c.JSON(http.StatusOK, watcher.AuthLoadStatus{State: watcher.AuthLoadStateIdle})
		return
	}
	c.JSON(http.StatusOK, provider())
}
```

- [ ] **Step 4: Inject the provider through ServerOption and register the route**

Add both fields to `serverOptionConfig`:

```go
authLoadStatusProvider func() watcher.AuthLoadStatus
authFileMutationHook   func(string)
```

Then add:

```go
func WithAuthLoadStatusProvider(provider func() watcher.AuthLoadStatus) ServerOption {
	return func(options *serverOptionConfig) {
		options.authLoadStatusProvider = provider
	}
}

func WithAuthFileMutationHook(hook func(string)) ServerOption {
	return func(options *serverOptionConfig) {
		options.authFileMutationHook = hook
	}
}
```

After `managementHandlers.NewHandler`, call both setters. Register this route before the parameterized auth-file routes:

```go
mgmt.GET("/auth-files/load-status", s.mgmt.GetAuthFileLoadStatus)
```

After a successful `store.Save` in `saveTokenRecord`, and immediately after every successful file deletion in the single/batch delete helpers, copy the hook under `h.mu` and call it with the normalized path before applying runtime auth changes. Extend the existing upload and delete tests with a hook that records the path; assert that the successfully saved/deleted normalized path is observed before the test's runtime-manager assertion.

Add an exact bind signal to `Server`:

```go
// In Server.
listening     chan net.Addr
listeningOnce sync.Once

// In NewServer's Server literal.
listening: make(chan net.Addr, 1),

func (s *Server) Listening() <-chan net.Addr {
	if s == nil {
		closed := make(chan net.Addr)
		close(closed)
		return closed
	}
	return s.listening
}

func (s *Server) publishListening(addr net.Addr) {
	s.listeningOnce.Do(func() {
		if addr != nil {
			s.listening <- addr
		}
		close(s.listening)
	})
}
```

In `Server.Start`, call `publishListening(listener.Addr())` after TLS/mux listener setup and immediately before the Serve/accept goroutines. On every startup error before that point, call `publishListening(nil)` before returning so Service never waits forever.

- [ ] **Step 5: Format and verify management contracts**

Run: `gofmt -w internal/api/server.go internal/api/handlers/management/handler.go internal/api/handlers/management/auth_files.go internal/api/handlers/management/auth_files_list_test.go internal/api/handlers/management/auth_files_upload_test.go internal/api/handlers/management/auth_files_delete_test.go internal/api/server_test.go`

Run: `go test ./internal/api/... -run 'Test(GetAuthFileLoadStatus|ManagementAuthFileLoadStatusRoute|ServerListening)' -count=1`

Expected: PASS for idle, loading/ready fixture, and degraded fixture response parsing.

- [ ] **Step 6: Commit the management status endpoint**

```bash
git add internal/api/server.go internal/api/handlers/management/handler.go internal/api/handlers/management/auth_files.go internal/api/handlers/management/auth_files_list_test.go internal/api/handlers/management/auth_files_upload_test.go internal/api/handlers/management/auth_files_delete_test.go internal/api/server_test.go
git commit -m "feat(management): expose auth load status"
```

---

### Task 6: Listener-First Service Startup and Progressive Registration

**Files:**
- Modify: `sdk/cliproxy/builder.go`
- Modify: `sdk/cliproxy/service.go`
- Create: `sdk/cliproxy/service_progressive_auth_loading_test.go`

**Interfaces:**
- Consumes: all Task 1 through Task 5 interfaces
- Produces: explicit `Service.progressiveFileAuth bool` ownership marker
- Produces: acknowledged Service batch results only after model and scheduler registration
- Preserves: synchronous `Manager.Load` for every non-default-file/custom Manager path

- [ ] **Step 1: Add a failing listener-first lifecycle test**

Create `sdk/cliproxy/service_progressive_auth_loading_test.go`; a new file is required because no existing Service test owns the normal HTTP/watcher lifecycle:

```go
package cliproxy

func TestServiceRunListensBeforeInitialAuthLoadCompletes(t *testing.T) {
	oldStore := sdkAuth.GetTokenStore()
	sdkAuth.RegisterTokenStore(sdkAuth.NewFileTokenStore())
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(oldStore) })
	authDir := t.TempDir()
	port := reserveTCPPort(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte(fmt.Sprintf("host: 127.0.0.1\nport: %d\nauth-dir: %s\n", port, authDir)), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	loadStarted := make(chan struct{})
	loadRelease := make(chan struct{})
	wrapper := &WatcherWrapper{
		start: func(context.Context) error { return nil },
		stop: func() error { return nil },
		setConfig: func(*config.Config) {},
		setUpdateQueue: func(chan<- watcher.AuthUpdateBatch) {},
		startInitialAuthLoad: func(context.Context, int) <-chan struct{} {
			done := make(chan struct{})
			close(loadStarted)
			go func() { <-loadRelease; close(done) }()
			return done
		},
		authLoadStatus: func() watcher.AuthLoadStatus {
			return watcher.AuthLoadStatus{State: watcher.AuthLoadStateLoading}
		},
	}
	service, errBuild := NewBuilder().
		WithConfig(&config.Config{Host: "127.0.0.1", Port: port, AuthDir: authDir, AuthLoadWorkers: 4}).
		WithConfigPath(configPath).
		WithWatcherFactory(func(string, string, func(*config.Config)) (*WatcherWrapper, error) { return wrapper, nil }).
		Build()
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	defer func() {
		close(loadRelease)
		cancel()
		<-done
	}()
	select {
	case <-loadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("initial auth load did not start")
	}
	response, errGet := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
	if errGet != nil {
		t.Fatalf("listener unavailable while auth load blocked: %v", errGet)
	}
	_ = response.Body.Close()
}

func reserveTCPPort(t testing.TB) int {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if errClose := listener.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	return port
}
```

- [ ] **Step 2: Add failing tests for store ownership and acknowledged availability**

Add these concrete cases in the same test file:

```go
func TestBuilderMarksOnlyDefaultFileManagerProgressive(t *testing.T) {
	oldStore := sdkAuth.GetTokenStore()
	fileStore := sdkAuth.NewFileTokenStore()
	sdkAuth.RegisterTokenStore(fileStore)
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(oldStore) })
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg := &config.Config{AuthDir: t.TempDir(), Port: 8317, AuthLoadWorkers: 16}
	service, errBuild := NewBuilder().WithConfig(cfg).WithConfigPath(configPath).Build()
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	if !service.progressiveFileAuth {
		t.Fatal("default FileTokenStore manager was not marked progressive")
	}
	customManager := coreauth.NewManager(fileStore, nil, nil)
	customService, errCustom := NewBuilder().WithConfig(cfg).WithConfigPath(configPath).WithCoreAuthManager(customManager).Build()
	if errCustom != nil {
		t.Fatal(errCustom)
	}
	if customService.progressiveFileAuth {
		t.Fatal("injected Manager was incorrectly marked progressive")
	}
}

func TestServiceAuthBatchAcknowledgedAfterModelsReachScheduler(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{
		cfg: &config.Config{}, coreManager: manager,
		authUpdates: make(chan watcher.AuthUpdateBatch, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go service.consumeAuthUpdates(ctx)
	resultCh := make(chan []watcher.AuthUpdateResult, 1)
	service.authUpdates <- watcher.AuthUpdateBatch{
		Updates: []watcher.AuthUpdate{{
			Action: watcher.AuthUpdateActionAdd,
			ID: "xai-progressive",
			Auth: &coreauth.Auth{ID: "xai-progressive", Provider: "xai", Status: coreauth.StatusActive},
		}},
		Result: resultCh,
	}
	results := <-resultCh
	if len(results) != 1 || !results[0].Loaded {
		t.Fatalf("batch results = %+v", results)
	}
	models := registry.GetGlobalRegistry().GetModelsForClient("xai-progressive")
	if len(models) == 0 {
		t.Fatal("acknowledgement arrived before model registration")
	}
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("xai-progressive") })
	manager.RegisterExecutor(progressiveBatchTestExecutor{})
	response, errExecute := manager.Execute(ctx, []string{"xai"}, cliproxyexecutor.Request{Model: models[0].ID}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() after acknowledgement error = %v", errExecute)
	}
	if string(response.Payload) != "xai-progressive" {
		t.Fatalf("selected payload = %q, want auth id", response.Payload)
	}
}

type progressiveBatchTestExecutor struct{}

func (progressiveBatchTestExecutor) Identifier() string { return "xai" }
func (progressiveBatchTestExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}
func (progressiveBatchTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (progressiveBatchTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}
func (progressiveBatchTestExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (progressiveBatchTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}
```

- [ ] **Step 3: Run focused Service tests and verify listener blocking/contract failures**

Run: `go test ./sdk/cliproxy -run 'Test(ServiceRunListens|BuilderMarksOnlyDefaultFile|ServiceAuthBatchAcknowledged)' -count=1`

Expected: FAIL on missing fields, batch channel types, or old startup ordering.

- [ ] **Step 4: Mark progressive ownership in Builder without guessing from global state later**

Add `progressiveFileAuth bool` to `Service`. In `Builder.Build`, initialize it only inside the branch that creates the core Manager:

```go
coreManager := b.coreManager
progressiveFileAuth := false
if coreManager == nil {
	tokenStore := sdkAuth.GetTokenStore()
	if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok && b.cfg != nil {
		dirSetter.SetBaseDir(b.cfg.AuthDir)
	}
	_, progressiveFileAuth = tokenStore.(*sdkAuth.FileTokenStore)
	strategy := ""
	sessionAffinity := false
	sessionAffinityTTL := time.Hour
	if b.cfg != nil {
		strategy = strings.ToLower(strings.TrimSpace(b.cfg.Routing.Strategy))
		sessionAffinity = b.cfg.Routing.SessionAffinity
		if ttl := strings.TrimSpace(b.cfg.Routing.SessionAffinityTTL); ttl != "" {
			if parsed, errParse := time.ParseDuration(ttl); errParse == nil && parsed > 0 {
				sessionAffinityTTL = parsed
			}
		}
	}
	var selector coreauth.Selector
	switch strategy {
	case "fill-first", "fillfirst", "ff":
		selector = &coreauth.FillFirstSelector{}
	case "sequential-fill", "sf":
		selector = &coreauth.SequentialFillSelector{}
	default:
		selector = &coreauth.RoundRobinSelector{}
	}
	if sessionAffinity {
		selector = coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
			Fallback: selector,
			TTL: sessionAffinityTTL,
		})
	}
	coreManager = coreauth.NewManager(tokenStore, selector, nil)
}
```

Assign the bool into `Service`. Never recompute it in `Run`; an injected Manager may use a different Store than the process-global token store.

- [ ] **Step 5: Consume acknowledged batches atomically**

Change `Service.authUpdates` to `chan watcher.AuthUpdateBatch`, allocate `make(chan watcher.AuthUpdateBatch, 256)`, and replace the drain loop with one batch call:

```go
case batch, ok := <-s.authUpdates:
	if !ok {
		return
	}
	results := s.handleAuthUpdates(ctx, batch.Updates)
	if batch.Result != nil {
		select {
		case batch.Result <- results:
		case <-ctx.Done():
			return
		}
	}
```

Change `handleAuthUpdates` to return one `AuthUpdateResult` per coalesced update. For add/modify, return `Loaded=true` only after `prepareCoreAuthForModelRegistration`, `runModelRegistrationTasks`, scheduler refresh inside each task, and the one per-batch plugin sync finish. Invalid/nil updates return `Loaded=false`. Deletes return true after removal. Keep deferred alias rebuild once per batch.

Keep the single-update wrapper source-compatible inside the package by discarding the internal result explicitly:

```go
func (s *Service) handleAuthUpdate(ctx context.Context, update watcher.AuthUpdate) {
	_ = s.handleAuthUpdates(ctx, []watcher.AuthUpdate{update})
}
```

Add `serviceStartedAt time.Time`, `listenerReadyAt time.Time`, and `firstFileAuthOnce sync.Once` to `Service`. Set `serviceStartedAt` at the first line of `Run`, set `listenerReadyAt` when `Server.Listening` yields, and only when `batch.Initial` is true and its acknowledgement contains at least one `Loaded=true` result log durations:

```go
s.firstFileAuthOnce.Do(func() {
	log.WithFields(log.Fields{
		"process_start_to_listener": s.listenerReadyAt.Sub(s.serviceStartedAt),
		"listener_to_first_auth":   time.Since(s.listenerReadyAt),
	}).Info("first file auth batch available")
})
```

Do not include update IDs, providers, paths, labels, or file names in this timing log.

- [ ] **Step 6: Set up watcher and queue before constructing/listening, without starting the scan**

After registering the existing deferred `Shutdown`, derive a Service-owned run context and register its cancel afterward so LIFO defer order cancels background work before shutdown waits:

```go
runCtx, runCancel := context.WithCancel(ctx)
defer runCancel()
```

Use `runCtx` for auth queue consumption, watcher context, load hooks, initial loading, and the final select. This guarantees a server-error return also cancels background startup even when the caller's parent context remains live.

Extract a nil-safe helper that performs the existing watcher factory/setup sequence:

```go
func (s *Service) setupFileWatcher(ctx context.Context) (*WatcherWrapper, error) {
	watcherWrapper, errCreate := s.watcherFactory(s.configPath, s.cfg.AuthDir, func(newCfg *config.Config) {
		s.applyWatcherConfigUpdate(newCfg)
	})
	if errCreate != nil {
		return nil, fmt.Errorf("create watcher: %w", errCreate)
	}
	s.watcher = watcherWrapper
	s.ensureAuthUpdateQueue(ctx)
	watcherWrapper.SetAuthUpdateQueue(s.authUpdates)
	watcherWrapper.SetConfig(s.cfg)
	s.registerPluginAuthParser()
	watcherWrapper.SetFileAuthLoadingEnabled(s.progressiveFileAuth)
	if s.progressiveFileAuth {
		watcherWrapper.SetAuthLoadHooks(watcher.AuthLoadHooks{
			Before: s.beforeProgressiveAuthLoad,
			After:  s.afterProgressiveAuthLoad,
		})
	}
	watcherCtx, watcherCancel := context.WithCancel(ctx)
	s.watcherCancel = watcherCancel
	if errStart := watcherWrapper.Start(watcherCtx); errStart != nil {
		watcherCancel()
		return nil, fmt.Errorf("start watcher: %w", errStart)
	}
	return watcherWrapper, nil
}
```

Call this before `api.NewServer`, then append both options to a copy of `s.serverOptions`:

```go
serverOptions := append([]api.ServerOption(nil), s.serverOptions...)
serverOptions = append(serverOptions,
	api.WithAuthLoadStatusProvider(watcherWrapper.AuthLoadStatus),
	api.WithAuthFileMutationHook(watcherWrapper.MarkAuthPathChanged),
)
s.server = api.NewServer(s.cfg, s.coreManager, s.accessManager, s.configPath, serverOptions...)
```

- [ ] **Step 7: Split file and non-file initial store loading**

The progressive file path performs neither `Manager.Load` nor config-auth registration before listener bind; its watcher `Before` hook owns both cooldown preparation and config-auth registration after bind. Keep non-file loading synchronous:

```go
if s.coreManager != nil && !homeEnabled {
	if !s.progressiveFileAuth {
		startupCtx := coreauth.WithSkipPersist(runCtx)
		if errLoad := s.coreManager.Load(runCtx); errLoad != nil {
			log.Warnf("failed to load auth store: %v", errLoad)
		}
		s.registerConfigAPIKeyAuths(startupCtx, s.cfg)
		if s.cfg.SaveCooldownStatus {
			if errRestore := s.coreManager.RestoreCooldownStates(runCtx); errRestore != nil {
				log.Warnf("failed to restore cooldown state: %v", errRestore)
			}
		}
	}
}
```

Implement the two hooks used by every initial or later full file scan:

```go
func (s *Service) beforeProgressiveAuthLoad(ctx context.Context) error {
	if s == nil || s.coreManager == nil || s.cfg == nil {
		return nil
	}
	if s.cfg.SaveCooldownStatus {
		if errPrepare := s.coreManager.PrepareCooldownRestore(ctx); errPrepare != nil {
			return fmt.Errorf("prepare cooldown restore: %w", errPrepare)
		}
	}
	s.registerConfigAPIKeyAuths(coreauth.WithSkipPersist(ctx), s.cfg)
	return nil
}

func (s *Service) afterProgressiveAuthLoad(ctx context.Context) error {
	if s == nil || s.coreManager == nil {
		return nil
	}
	var errComplete error
	if s.cfg != nil && s.cfg.SaveCooldownStatus {
		if errCooldown := s.coreManager.CompleteCooldownRestore(ctx); errCooldown != nil {
			errComplete = fmt.Errorf("complete cooldown restore: %w", errCooldown)
		}
	}
	s.syncPluginModelRuntime(ctx)
	s.startCoreAuthAutoRefresh(ctx)
	return errComplete
}
```

Because `Before` runs inside the background loader only after listener readiness, `.cds` I/O cannot delay listener bind. Because it finishes before enumeration and batch dispatch, no config or file auth becomes selectable without restored cooldown state. The same pair wraps later full rescans, so stale snapshot cleanup is never run before the rescan's auth set has converged.

In `applyWatcherConfigUpdate`, after applying the new config and plugin parser, detect the pre-callback marker published by `reloadClients`:

```go
progressiveFullScanPending := s.progressiveFileAuth && s.watcher != nil &&
	s.watcher.AuthLoadStatus().State == watcher.AuthLoadStateLoading
```

When that value is true, skip the immediate `registerConfigAPIKeyAuths`, `RestoreCooldownStates`, and final `syncPluginModelRuntime`; the upcoming `Before/After` hooks own those operations. Keep `syncPluginRuntimeConfig` before the scan so the plugin parser is current. When false, retain the existing config-only update behavior.

- [ ] **Step 8: Bind first, then start only the file-backed initial loader**

Replace the fixed `time.Sleep(100 * time.Millisecond)` with the exact readiness signal from Task 5:

```go
select {
case addr, ok := <-s.server.Listening():
	if !ok || addr == nil {
		return fmt.Errorf("cliproxy: server stopped before listener became ready")
	}
	s.listenerReadyAt = time.Now()
	log.WithField("address", addr.String()).Info("API server listener ready")
case errServer := <-s.serverErr:
	return errServer
case <-runCtx.Done():
	return runCtx.Err()
}
```

Immediately after listener readiness, start the progressive loader or finish the existing non-file registration:

```go
if s.progressiveFileAuth && watcherWrapper != nil {
	watcherWrapper.StartInitialAuthLoad(runCtx, s.cfg.AuthLoadWorkers)
} else if s.coreManager != nil && !homeEnabled {
	s.registerModelsForAuthBatch(runCtx, s.coreManager.List())
	s.startCoreAuthAutoRefresh(runCtx)
}
```

Add `autoRefreshOnce sync.Once` to `Service` and extract the existing block into:

```go
func (s *Service) startCoreAuthAutoRefresh(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.autoRefreshOnce.Do(func() {
		interval := 15 * time.Minute
		s.coreManager.StartAutoRefresh(ctx, interval)
		log.Infof("core auth auto-refresh started (interval=%s)", interval)
	})
}
```

Do not start it before the initial scan completes, which would create a refresh storm across a partially loaded set.

- [ ] **Step 9: Remove the old post-listen synchronous watcher rescan**

Delete the old watcher construction block at the end of `Run`; all watch setup now happens before `api.NewServer`, and only `StartInitialAuthLoad` starts file discovery after listener bind. Preserve `OnBeforeStart`, `OnAfterStart`, pprof, WebSocket route setup, Home subscriber behavior, and shutdown cancellation.

- [ ] **Step 10: Format and verify Service behavior**

Run: `gofmt -w sdk/cliproxy/builder.go sdk/cliproxy/service.go sdk/cliproxy/service_progressive_auth_loading_test.go`

Run: `go test -race ./sdk/cliproxy -run 'Test(ServiceRunListens|BuilderMarksOnlyDefaultFile|ServiceAuthBatchAcknowledged)' -count=1`

Run: `go test ./sdk/cliproxy/... -count=1`

Expected: PASS; the listener test completes its HTTP request while the loader remains blocked.

- [ ] **Step 11: Commit listener-first startup**

```bash
git add sdk/cliproxy/builder.go sdk/cliproxy/service.go sdk/cliproxy/service_progressive_auth_loading_test.go
git commit -m "feat(cliproxy): start serving during auth loading"
```

---

### Task 7: Performance Evidence, Documentation, and Full Verification

**Files:**
- Create: `internal/watcher/auth_load_benchmark_test.go`
- Modify: `README.md`
- Modify: `README_CN.md`
- Create: `docs/performance/2026-07-14-progressive-auth-loading.md`

**Interfaces:**
- Verifies: listener <= 2 seconds, first selectable file auth <= 3 seconds, full 1,144-file load <= 30 seconds on the same HF volume
- Verifies: one initial payload read per JSON and peak concurrent reads <= configured workers
- Documents: `auth-load-workers`, progressive availability, management status endpoint, and operational fallback `auth-load-workers: 1`

- [ ] **Step 1: Add a local 1,144-file benchmark with read-count validation**

Create `internal/watcher/auth_load_benchmark_test.go`:

```go
package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func BenchmarkInitialAuthLoad1144(b *testing.B) {
	authDir := b.TempDir()
	for i := 0; i < 1144; i++ {
		payload := []byte(fmt.Sprintf(`{"type":"xai","access_token":"token-%d"}`, i))
		if errWrite := os.WriteFile(filepath.Join(authDir, fmt.Sprintf("xai-%04d.json", i)), payload, 0o600); errWrite != nil {
			b.Fatal(errWrite)
		}
	}
	for _, workers := range []int{1, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("workers-%d", workers), func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				var reads atomic.Int64
				originalRead := readInitialAuthFile
				readInitialAuthFile = func(path string) ([]byte, error) {
					reads.Add(1)
					return os.ReadFile(path)
				}
				queue := make(chan AuthUpdateBatch, 64)
				w := newInitialLoadTestWatcher(b, authDir, queue)
				done := w.StartInitialAuthLoad(context.Background(), workers)
				for {
					select {
					case batch := <-queue:
						results := make([]AuthUpdateResult, 0, len(batch.Updates))
						for _, update := range batch.Updates {
							results = append(results, AuthUpdateResult{ID: update.ID, Loaded: true})
						}
						batch.Result <- results
					case <-done:
						_ = w.Stop()
						readInitialAuthFile = originalRead
						if got := reads.Load(); got != 1144 {
							b.Fatalf("payload reads = %d, want 1144", got)
						}
						goto nextIteration
					}
				}
			nextIteration:
			}
		})
	}
}
```

The benchmark reuses the `testing.TB` fixture from Task 4. It does not call the Service or network; its purpose is worker/read structure, while HF validation owns end-to-end latency.

- [ ] **Step 2: Run the local control benchmark and retain output in the performance record**

Run:

```bash
go test ./internal/watcher -run '^$' -bench BenchmarkInitialAuthLoad1144 -benchmem -count=3
```

Expected: all worker variants complete; each iteration internally verifies exactly 1,144 payload reads. Record `ns/op`, `B/op`, and `allocs/op` for worker counts 1, 4, 8, 16, and 32.

- [ ] **Step 3: Document configuration and behavior**

Add to the relevant configuration/startup sections in `README.md` and `README_CN.md`:

```yaml
# Initial/full auth directory scan concurrency. Range: 1-64; default: 16.
auth-load-workers: 16
```

Document these exact semantics in each file's existing language:

- The default file store installs filesystem watches and binds HTTP before scanning credentials.
- Valid credentials become routable batch by batch; requests for models without a loaded credential retain the existing unavailable response.
- `GET /v0/management/auth-files/load-status` reports `idle`, `loading`, `ready`, or `degraded` with counts.
- `auth-load-workers: 1` is the low-resource fallback; it remains progressive and does not restore duplicate scans.
- Non-file and custom Manager stores keep synchronous `Store.List` loading.

- [ ] **Step 4: Create the HF measurement template before deployment**

Create `docs/performance/2026-07-14-progressive-auth-loading.md` with this table:

```markdown
# Progressive Auth Loading - Hugging Face Validation

Environment: same Space, persistent volume, config, and 1,144 JSON credentials used for the 188-second baseline.

| workers | listener ms | first auth ms | full load ms | read failures | peak FD | peak RSS MiB | peak CPU % | request during load | final providers/auths |
|---:|---:|---:|---:|---:|---:|---:|---:|:---:|:---|
| 1 | | | | | | | | | |
| 4 | | | | | | | | | |
| 8 | | | | | | | | | |
| 16 | | | | | | | | | |
| 32 | | | | | | | | | |

Acceptance:

- listener <= 2,000 ms
- first selectable file auth <= 3,000 ms
- full load at selected default <= 30,000 ms
- initial payload reads = discovered JSON files
- peak concurrent reads <= configured workers
- final expected counts: xAI 1,120; Antigravity 19; Codex 5; total files 1,144
```

- [ ] **Step 5: Run the same HF workload at all five worker counts**

For each value `1 4 8 16 32`, change only `auth-load-workers`, restart the same Space without changing the volume, and capture:

```bash
curl -fsS -H "Authorization: Bearer ${MANAGEMENT_KEY}" \
  "http://127.0.0.1:8317/v0/management/auth-files/load-status"
```

Poll at 100 ms until `state` is `ready` or `degraded`. Record process start, the existing `API server started successfully` timestamp, the first status with `auths_loaded > 0`, and terminal `completed_at`. During `loading`, send one request for a model already present in the first acknowledged batch and record its success.

Collect process resource peaks using the HF container's available `/proc/1/fd`, `/proc/1/status`, and process CPU telemetry. Do not add a permanent profiler or debug endpoint for this one validation.

- [ ] **Step 6: Select the default from evidence**

Keep 16 unless the same-HF table shows either:

- peak FD/RSS/CPU exceeds the Space resource limit; or
- 16 is slower than 8 across repeated runs.

If either condition is true, change `DefaultAuthLoadWorkers`, both README examples, `config.example.yaml`, and config test expectation to 8, then rerun the focused config and watcher suites.

- [ ] **Step 7: Run the complete verification matrix**

Run:

```bash
gofmt -w .
go test -race ./internal/watcher/... ./sdk/cliproxy/auth ./sdk/cliproxy ./internal/api/... -count=1
go test ./sdk/auth -run TestFileTokenStoreList -count=1
go test ./... -count=1
go build -o test-output ./cmd/server && rm test-output
```

Expected: every test and the required server build pass. Inspect `git status --short` and confirm no `test-output`, generated credential, usage database, or benchmark artifact is tracked.

- [ ] **Step 8: Verify the root-cause invariants from the final source**

Run:

```bash
rg -n "coreManager\.Load|StartInitialAuthLoad|reloadClients\(true|os\.ReadFile" sdk/cliproxy internal/watcher sdk/auth
```

Expected structural conclusions, verified against call paths rather than raw match counts:

- default progressive file startup does not call `coreManager.Load`;
- `Watcher.Start` does not call `reloadClients(true, ...)`;
- initial JSON payload reads occur only in the bounded loader;
- live single-file updates may still read their one changed file;
- `FileTokenStore.List` remains available for SDK and non-progressive callers.

- [ ] **Step 9: Commit benchmark, documentation, and measured evidence**

```bash
git add internal/watcher/auth_load_benchmark_test.go README.md README_CN.md
git add -f docs/performance/2026-07-14-progressive-auth-loading.md
git commit -m "docs(auth): record progressive loading performance"
```

---

## Final Acceptance Checklist

- [ ] HTTP binds before any default-file credential scan.
- [ ] First acknowledged credential batch is selectable while later files remain unread.
- [ ] Exactly one initial payload read occurs for each discovered JSON file.
- [ ] Peak payload reads never exceed `auth-load-workers`.
- [ ] Malformed/unreadable files produce `degraded` without blocking valid auths.
- [ ] Plugin multi-auth files increase `auths_loaded` by virtual auth count.
- [ ] Live event generation wins over an older initial result.
- [ ] Cooldown state is visible on the auth before its first scheduler upsert with registered models.
- [ ] Shutdown closes loader, timer, acknowledgement, dispatcher, and fsnotify goroutines.
- [ ] Non-file/custom Manager paths still call their Store exactly once.
- [ ] Management response parses as the documented JSON schema.
- [ ] Same-HF 1,144-file timings satisfy the selected worker default.
- [ ] Full tests, race-focused tests, and required build pass.
