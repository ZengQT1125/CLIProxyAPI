package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

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
	path              string
	normalized        string
	generation        uint64
	raw               []byte
	hash              string
	native            synthesizer.NativeAuthFileResult
	readTime          time.Duration
	synthTime         time.Duration
	err               error
	enumerationResult bool
	discovered        int64
}

func (w *Watcher) StartInitialAuthLoad(ctx context.Context, workers int) <-chan struct{} {
	done := make(chan struct{})
	if w == nil || !w.fileAuthLoadingIsEnabled() {
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

func (w *Watcher) runInitialAuthLoad(ctx context.Context, workers int, done chan struct{}, isInitialScan bool) {
	defer close(done)
	w.authLoadMu.Lock()
	hooks := w.authLoadHooks
	w.authLoadMu.Unlock()
	if hooks.Before != nil {
		if errBefore := hooks.Before(ctx); errBefore != nil {
			w.publishTerminalAuthLoadStatus(true, true)
			return
		}
	}
	if ctx.Err() != nil {
		w.publishTerminalAuthLoadStatus(false, false)
		return
	}
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
		w.enumerateInitialAuthFiles(ctx, snapshot, jobs, results, isInitialScan)
		close(jobs)
		workersWG.Wait()
		close(results)
	}()
	w.aggregateInitialAuthResults(ctx, snapshot, results, baseline, hooks.After)
}

func (w *Watcher) snapshotAuthLoadInputs() authLoadSnapshot {
	w.clientsMutex.RLock()
	defer w.clientsMutex.RUnlock()
	var cfg *config.Config
	if w.config != nil {
		cfg = w.config.CloneForRuntime()
	}
	return authLoadSnapshot{config: cfg, parser: w.pluginAuthParser, authDir: w.authDir, now: time.Now().UTC()}
}

func (w *Watcher) snapshotFileGenerationBaseline(isInitialScan bool) map[string]uint64 {
	w.clientsMutex.RLock()
	defer w.clientsMutex.RUnlock()
	baseline := make(map[string]uint64, len(w.fileAuthsByPath))
	for path := range w.fileAuthsByPath {
		if !isInitialScan {
			baseline[path] = w.pathGenerations[path]
			continue
		}
		baseline[path] = 0
	}
	return baseline
}

func (w *Watcher) enumerateInitialAuthFiles(ctx context.Context, snapshot authLoadSnapshot, jobs chan<- initialAuthJob, results chan<- initialAuthReadResult, isInitialScan bool) {
	entries, errReadDir := os.ReadDir(snapshot.authDir)
	if errReadDir != nil {
		w.sendInitialAuthResult(ctx, results, initialAuthReadResult{enumerationResult: true, err: errReadDir})
		return
	}
	var discovered int64
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if entry == nil || entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		path := filepath.Join(snapshot.authDir, entry.Name())
		normalized := w.normalizeAuthPath(path)
		generation := uint64(0)
		if !isInitialScan {
			w.clientsMutex.RLock()
			generation = w.pathGenerations[normalized]
			w.clientsMutex.RUnlock()
		}
		discovered++
		select {
		case jobs <- initialAuthJob{path: path, normalized: normalized, generation: generation}:
		case <-ctx.Done():
			return
		}
	}
	w.sendInitialAuthResult(ctx, results, initialAuthReadResult{enumerationResult: true, discovered: discovered})
}

func (w *Watcher) initialAuthReadWorker(ctx context.Context, snapshot authLoadSnapshot, jobs <-chan initialAuthJob, results chan<- initialAuthReadResult) {
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-jobs:
			if !ok {
				return
			}
			result := initialAuthReadResult{path: job.path, normalized: job.normalized, generation: job.generation}
			readStarted := time.Now()
			result.raw, result.err = readInitialAuthFile(job.path)
			result.readTime = time.Since(readStarted)
			if result.err == nil && len(result.raw) == 0 {
				result.err = os.ErrInvalid
			}
			if result.err == nil {
				sum := sha256.Sum256(result.raw)
				result.hash = hex.EncodeToString(sum[:])
				synthStarted := time.Now()
				result.native, result.err = synthesizer.SynthesizeNativeAuthFile(&synthesizer.SynthesisContext{
					Config: snapshot.config, AuthDir: snapshot.authDir, Now: snapshot.now, IDGenerator: synthesizer.NewStableIDGenerator(),
				}, job.path, result.raw)
				result.synthTime = time.Since(synthStarted)
			}
			if !w.sendInitialAuthResult(ctx, results, result) {
				return
			}
		}
	}
}

func (w *Watcher) sendInitialAuthResult(ctx context.Context, results chan<- initialAuthReadResult, result initialAuthReadResult) bool {
	select {
	case results <- result:
		return true
	case <-ctx.Done():
		return false
	}
}

func (w *Watcher) aggregateInitialAuthResults(ctx context.Context, snapshot authLoadSnapshot, results <-chan initialAuthReadResult, baseline map[string]uint64, after func(context.Context) error) {
	status := w.AuthLoadStatus()
	pending := make([]initialAuthReadResult, 0, authLoadBatchSize)
	seen := make(map[string]struct{})
	enumerationFailed := false
	timer := time.NewTimer(authLoadFlushInterval)
	defer timer.Stop()
	flush := func() {
		if len(pending) == 0 {
			return
		}
		sort.Slice(pending, func(i, j int) bool { return pending[i].normalized < pending[j].normalized })
		w.commitInitialAuthResults(ctx, snapshot, pending, &status, seen)
		for i := range pending {
			pending[i].raw = nil
		}
		pending = pending[:0]
		w.publishAuthLoadStatus(status)
	}
	for {
		select {
		case result, ok := <-results:
			if !ok {
				flush()
				if ctx.Err() == nil {
					w.reconcileMissingInitialAuthPaths(ctx, baseline, seen, &status)
					if after != nil && after(ctx) != nil {
						enumerationFailed = true
					}
				}
				completed := time.Now().UTC()
				status.CompletedAt = &completed
				if status.FilesFailed > 0 || enumerationFailed {
					status.State = AuthLoadStateDegraded
				} else {
					status.State = AuthLoadStateReady
				}
				w.publishAuthLoadStatus(status)
				return
			}
			if result.enumerationResult {
				status.FilesDiscovered = result.discovered
				status.ScanComplete = true
				if result.err != nil {
					enumerationFailed = true
				}
				w.publishAuthLoadStatus(status)
				continue
			}
			seen[result.normalized] = struct{}{}
			pending = append(pending, result)
			if len(pending) >= authLoadBatchSize {
				flush()
			}
		case <-timer.C:
			flush()
			timer.Reset(authLoadFlushInterval)
		case <-ctx.Done():
			pending = nil
		}
	}
}

func (w *Watcher) commitInitialAuthResults(ctx context.Context, snapshot authLoadSnapshot, results []initialAuthReadResult, status *AuthLoadStatus, seen map[string]struct{}) {
	updates := make([]AuthUpdate, 0, len(results))
	pathByID := make(map[string]string)
	unchanged := make(map[string]int64)
	failedPaths := make(map[string]struct{})
	for i := range results {
		result := &results[i]
		status.FilesProcessed++
		if result.err != nil {
			status.FilesFailed++
			continue
		}
		w.clientsMutex.Lock()
		if !w.pathGenerationCurrentLocked(result.normalized, result.generation) {
			w.clientsMutex.Unlock()
			status.FilesSkipped++
			continue
		}
		w.clientsMutex.Unlock()
		pluginStarted := time.Now()
		pluginAuths, handled, errPlugin := synthesizer.SynthesizePluginAuthFile(&synthesizer.SynthesisContext{
			Config: snapshot.config, AuthDir: snapshot.authDir, Now: snapshot.now, IDGenerator: synthesizer.NewStableIDGenerator(), PluginAuthParser: snapshot.parser,
		}, result.path, result.raw)
		result.synthTime += time.Since(pluginStarted)
		if errPlugin != nil {
			status.FilesFailed++
			continue
		}
		auths := result.native.Auths
		if handled {
			auths = pluginAuths
		}
		newByID := authSliceToMap(auths)
		w.clientsMutex.Lock()
		if !w.pathGenerationCurrentLocked(result.normalized, result.generation) {
			w.clientsMutex.Unlock()
			status.FilesSkipped++
			continue
		}
		if w.lastAuthHashes == nil {
			w.lastAuthHashes = make(map[string]string)
		}
		if w.fileAuthsByPath == nil {
			w.fileAuthsByPath = make(map[string]map[string]*coreauth.Auth)
		}
		w.lastAuthHashes[result.normalized] = result.hash
		oldByID := cloneAuthMap(w.fileAuthsByPath[result.normalized])
		if len(newByID) == 0 {
			delete(w.fileAuthsByPath, result.normalized)
		} else {
			w.fileAuthsByPath[result.normalized] = cloneAuthMap(newByID)
		}
		pathUpdates := w.computePerPathUpdatesLocked(oldByID, newByID)
		w.clientsMutex.Unlock()
		for _, auth := range newByID {
			if auth == nil {
				continue
			}
			changed := false
			for _, update := range pathUpdates {
				if update.ID == auth.ID {
					changed = true
					break
				}
			}
			if !changed {
				unchanged[result.normalized]++
			}
		}
		for _, update := range pathUpdates {
			updates = append(updates, update)
			pathByID[update.ID] = result.normalized
		}
	}
	for _, count := range unchanged {
		status.AuthsLoaded += count
	}
	acknowledged := w.dispatchInitialAuthBatch(ctx, updates)
	loadedByID := make(map[string]bool, len(acknowledged))
	for _, result := range acknowledged {
		loadedByID[result.ID] = result.Loaded
	}
	for _, update := range updates {
		if loadedByID[update.ID] {
			status.AuthsLoaded++
			continue
		}
		failedPaths[pathByID[update.ID]] = struct{}{}
	}
	status.FilesFailed += int64(len(failedPaths))
}

func (w *Watcher) reconcileMissingInitialAuthPaths(ctx context.Context, baseline map[string]uint64, seen map[string]struct{}, status *AuthLoadStatus) {
	updates := make([]AuthUpdate, 0)
	w.clientsMutex.Lock()
	for path, generation := range baseline {
		if _, present := seen[path]; present || !w.pathGenerationCurrentLocked(path, generation) {
			continue
		}
		oldByID := cloneAuthMap(w.fileAuthsByPath[path])
		delete(w.fileAuthsByPath, path)
		delete(w.lastAuthHashes, path)
		delete(w.lastAuthContents, path)
		updates = append(updates, w.computePerPathUpdatesLocked(oldByID, map[string]*coreauth.Auth{})...)
	}
	w.clientsMutex.Unlock()
	if len(updates) > 0 {
		w.dispatchInitialAuthBatch(ctx, updates)
	}
}

func cloneAuthMap(auths map[string]*coreauth.Auth) map[string]*coreauth.Auth {
	cloned := make(map[string]*coreauth.Auth, len(auths))
	for id, auth := range auths {
		if auth != nil {
			cloned[id] = auth.Clone()
		}
	}
	return cloned
}

func (w *Watcher) publishTerminalAuthLoadStatus(degraded, scanComplete bool) {
	status := w.AuthLoadStatus()
	status.ScanComplete = scanComplete
	completed := time.Now().UTC()
	status.CompletedAt = &completed
	if degraded {
		status.State = AuthLoadStateDegraded
	} else {
		status.State = AuthLoadStateReady
	}
	w.publishAuthLoadStatus(status)
}

func (w *Watcher) logAuthLoadSummary(status AuthLoadStatus) {
	log.WithFields(log.Fields{
		"files_discovered": status.FilesDiscovered,
		"files_processed":  status.FilesProcessed,
		"auths_loaded":     status.AuthsLoaded,
		"files_failed":     status.FilesFailed,
		"files_skipped":    status.FilesSkipped,
	}).Debug("auth load progress")
}
