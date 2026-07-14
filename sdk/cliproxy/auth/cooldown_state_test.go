package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordingCooldownStateStore struct {
	applyCount atomic.Int32
	mu         sync.Mutex
	snapshots  []CooldownStateSnapshot
	load       []CooldownStateSnapshot
}

func (s *recordingCooldownStateStore) Load(context.Context) ([]CooldownStateSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneCooldownStateSnapshots(s.load), nil
}

func (s *recordingCooldownStateStore) Apply(_ context.Context, snapshots []CooldownStateSnapshot) error {
	s.applyCount.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots = cloneCooldownStateSnapshots(snapshots)
	return nil
}

func cloneCooldownStateSnapshots(snapshots []CooldownStateSnapshot) []CooldownStateSnapshot {
	if len(snapshots) == 0 {
		return nil
	}
	cloned := make([]CooldownStateSnapshot, len(snapshots))
	for i := range snapshots {
		cloned[i].AuthID = snapshots[i].AuthID
		cloned[i].Records = make([]CooldownStateRecord, len(snapshots[i].Records))
		for j := range snapshots[i].Records {
			cloned[i].Records[j] = snapshots[i].Records[j]
			cloned[i].Records[j].LastError = cloneError(snapshots[i].Records[j].LastError)
		}
	}
	return cloned
}

func TestFileCooldownStateStore_StateRelativePath(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "auths")
	store := NewFileCooldownStateStoreWithAuthDir(authDir, authDir)

	cases := []struct {
		name   string
		record CooldownStateRecord
		want   string
	}{
		{
			name: "absolute auth file under auth dir",
			record: CooldownStateRecord{
				AuthID:   "auth-1",
				AuthFile: filepath.Join(authDir, "nested", "xai.json"),
			},
			want: filepath.Join("nested", "xai.cds"),
		},
		{
			name: "relative auth file",
			record: CooldownStateRecord{
				AuthID:   "auth-2",
				AuthFile: filepath.Join("team", "xai.json"),
			},
			want: filepath.Join("team", "xai.cds"),
		},
		{
			name: "absolute auth file outside auth dir",
			record: CooldownStateRecord{
				AuthID:   "auth-3",
				AuthFile: filepath.Join(t.TempDir(), "outside.json"),
			},
			want: "outside.cds",
		},
		{
			name: "relative parent escape is rejected",
			record: CooldownStateRecord{
				AuthID:   "auth-4",
				AuthFile: filepath.Join("..", "escape.json"),
			},
			want: "",
		},
		{
			name: "auth id fallback",
			record: CooldownStateRecord{
				AuthID: "auth/id 5",
			},
			want: "auth_id_5.cds",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := store.stateRelativePath(tc.record); got != tc.want {
				t.Fatalf("stateRelativePath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFileCooldownStateStore_ApplyOnlyChangesNamedAuth(t *testing.T) {
	authDir := t.TempDir()
	store := NewFileCooldownStateStoreWithAuthDir(authDir, authDir)
	ctx := context.Background()

	nextRetry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	updatedAt := time.Now().UTC().Truncate(time.Second)
	record1 := CooldownStateRecord{
		Provider:       "xai",
		AuthID:         "auth-1",
		AuthFile:       filepath.Join(authDir, "xai-1.json"),
		Model:          "grok-4",
		Status:         "cooling",
		NextRetryAfter: nextRetry,
		Reason:         "quota",
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: nextRetry,
			BackoffLevel:  1,
		},
		LastError: &Error{Message: "rate limited", HTTPStatus: 429},
		UpdatedAt: updatedAt,
	}
	record2 := record1
	record2.AuthID = "auth-2"
	record2.AuthFile = filepath.Join(authDir, "xai-2.json")
	record2.Model = "grok-3"

	if errApply := store.Apply(ctx, []CooldownStateSnapshot{
		{AuthID: record1.AuthID, Records: []CooldownStateRecord{record1}},
		{AuthID: record2.AuthID, Records: []CooldownStateRecord{record2}},
	}); errApply != nil {
		t.Fatalf("Apply() returned error: %v", errApply)
	}
	if _, errStat := os.Stat(filepath.Join(authDir, "xai-1.cds")); errStat != nil {
		t.Fatalf("expected xai-1.cds to exist: %v", errStat)
	}
	if _, errStat := os.Stat(filepath.Join(authDir, "xai-2.cds")); errStat != nil {
		t.Fatalf("expected xai-2.cds to exist: %v", errStat)
	}

	loaded, errLoad := store.Load(ctx)
	if errLoad != nil {
		t.Fatalf("Load() returned error: %v", errLoad)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded snapshots = %d, want 2", len(loaded))
	}

	if errApply := store.Apply(ctx, []CooldownStateSnapshot{{AuthID: record1.AuthID}}); errApply != nil {
		t.Fatalf("Apply(clear auth-1) returned error: %v", errApply)
	}
	if _, errStat := os.Stat(filepath.Join(authDir, "xai-1.cds")); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("expected xai-1.cds to be removed, stat error = %v", errStat)
	}
	if _, errStat := os.Stat(filepath.Join(authDir, "xai-2.cds")); errStat != nil {
		t.Fatalf("expected xai-2.cds to remain: %v", errStat)
	}

	loaded, errLoad = store.Load(ctx)
	if errLoad != nil {
		t.Fatalf("Load() after clear returned error: %v", errLoad)
	}
	if len(loaded) != 1 || loaded[0].AuthID != record2.AuthID || len(loaded[0].Records) != 1 {
		t.Fatalf("loaded snapshots = %+v, want only auth-2", loaded)
	}
	loadedRecord := loaded[0].Records[0]
	if loadedRecord.Model != record2.Model || !loadedRecord.NextRetryAfter.Equal(nextRetry) {
		t.Fatalf("loaded record = %+v, want %+v", loadedRecord, record2)
	}
	if loadedRecord.LastError == nil || loadedRecord.LastError.HTTPStatus != 429 {
		t.Fatalf("loaded last error = %+v, want HTTP 429", loadedRecord.LastError)
	}
}

func TestFileCooldownStateStore_ConcurrentApply(t *testing.T) {
	authDir := t.TempDir()
	store := NewFileCooldownStateStoreWithAuthDir(authDir, authDir)
	ctx := context.Background()
	nextRetry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.Apply(ctx, []CooldownStateSnapshot{
				{AuthID: "auth-1", Records: []CooldownStateRecord{{
					Provider:       "xai",
					AuthID:         "auth-1",
					AuthFile:       filepath.Join(authDir, "xai.json"),
					Model:          "grok-4",
					Status:         "cooling",
					NextRetryAfter: nextRetry.Add(time.Duration(i) * time.Second),
					UpdatedAt:      nextRetry,
				}}},
			})
		}()
	}
	wg.Wait()
	close(errs)
	for errApply := range errs {
		if errApply != nil {
			t.Fatalf("Apply() returned error: %v", errApply)
		}
	}

	loaded, errLoad := store.Load(ctx)
	if errLoad != nil {
		t.Fatalf("Load() returned error: %v", errLoad)
	}
	if len(loaded) != 1 || len(loaded[0].Records) != 1 {
		t.Fatalf("loaded snapshots = %+v, want one auth with one record", loaded)
	}

	tmpMatches, errGlob := filepath.Glob(filepath.Join(authDir, "*.tmp"))
	if errGlob != nil {
		t.Fatalf("glob temp files: %v", errGlob)
	}
	if len(tmpMatches) != 0 {
		t.Fatalf("leftover temp files = %v, want none", tmpMatches)
	}
}

func TestManager_MarkResult_PersistsCooldownOnlyWhenStateChanges(t *testing.T) {
	store := &recordingCooldownStateStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)

	auth := &Auth{ID: "auth-1", Provider: "xai", Status: StatusActive}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "xai", Model: "grok-4", Success: true})
	if got := store.applyCount.Load(); got != 0 {
		t.Fatalf("healthy success saved cooldown state %d times, want 0", got)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "xai",
		Model:    "grok-4",
		Success:  false,
		Error:    &Error{Message: "upstream unavailable", HTTPStatus: 500},
	})
	if got := store.applyCount.Load(); got != 1 {
		t.Fatalf("cooldown failure saved cooldown state %d times, want 1", got)
	}

	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "xai", Model: "grok-4", Success: true})
	if got := store.applyCount.Load(); got != 2 {
		t.Fatalf("cooldown clear saved cooldown state %d times, want 2", got)
	}

	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "xai", Model: "grok-4", Success: true})
	if got := store.applyCount.Load(); got != 2 {
		t.Fatalf("clean success saved cooldown state %d times, want 2", got)
	}
}

func TestManager_RestoreCooldownStates(t *testing.T) {
	nextRetry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	store := &recordingCooldownStateStore{
		load: []CooldownStateSnapshot{
			{AuthID: "auth-1", Records: []CooldownStateRecord{{
				Provider:       "xai",
				AuthID:         "auth-1",
				Model:          "grok-4",
				Status:         "cooling",
				NextRetryAfter: nextRetry,
				Reason:         "quota",
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: nextRetry,
				},
				LastError: &Error{Message: "rate limited", HTTPStatus: 429},
				UpdatedAt: nextRetry.Add(-time.Minute),
			}}},
		},
	}
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "auth-1", Provider: "xai"}); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}

	if errRestore := manager.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatalf("RestoreCooldownStates() returned error: %v", errRestore)
	}

	auth, ok := manager.GetByID("auth-1")
	if !ok {
		t.Fatal("restored auth was not found")
	}
	state := auth.ModelStates["grok-4"]
	if state == nil {
		t.Fatal("model state was not restored")
	}
	if !state.Unavailable || state.Status != StatusError || !state.NextRetryAfter.Equal(nextRetry) {
		t.Fatalf("restored state = %+v, want unavailable status error until %v", state, nextRetry)
	}
	if state.LastError == nil || state.LastError.HTTPStatus != 429 {
		t.Fatalf("restored last error = %+v, want HTTP 429", state.LastError)
	}
	if got := store.applyCount.Load(); got != 1 {
		t.Fatalf("restore cleanup saved cooldown state %d times, want 1", got)
	}
}
