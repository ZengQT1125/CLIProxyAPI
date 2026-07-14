package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

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

func TestPostgresCooldownStateStore_ApplyOnlyChangesNamedAuth(t *testing.T) {
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

	cooldownStore := NewPostgresCooldownStateStore(pg)
	store, ok := cooldownStore.(*PostgresCooldownStateStore)
	if !ok {
		t.Fatalf("NewPostgresCooldownStateStore returned unexpected type %T", cooldownStore)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	authA := "pg-cooldown-a-" + suffix
	authB := "pg-cooldown-b-" + suffix
	t.Cleanup(func() {
		_, _ = pg.db.ExecContext(context.Background(), fmt.Sprintf(`DELETE FROM %s WHERE id IN ($1, $2)`, store.tableName()), authA, authB)
	})

	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	recordA := cliproxyauth.CooldownStateRecord{
		Provider: "xai", AuthID: authA, Model: "grok-4",
		Status: "cooling", NextRetryAfter: next, Reason: "quota", UpdatedAt: next,
	}
	recordB := cliproxyauth.CooldownStateRecord{
		Provider: "xai", AuthID: authB, Model: "grok-3",
		Status: "cooling", NextRetryAfter: next, Reason: "quota", UpdatedAt: next,
	}

	if errApply := store.Apply(ctx, []cliproxyauth.CooldownStateSnapshot{
		{AuthID: authA, Records: []cliproxyauth.CooldownStateRecord{recordA}},
		{AuthID: authB, Records: []cliproxyauth.CooldownStateRecord{recordB}},
	}); errApply != nil {
		t.Fatalf("Apply(seed): %v", errApply)
	}

	recordA.NextRetryAfter = next.Add(time.Minute)
	if errApply := store.Apply(ctx, []cliproxyauth.CooldownStateSnapshot{
		{AuthID: authA, Records: []cliproxyauth.CooldownStateRecord{recordA}},
	}); errApply != nil {
		t.Fatalf("Apply(update auth A): %v", errApply)
	}
	loaded, errLoad := store.Load(ctx)
	if errLoad != nil {
		t.Fatalf("Load after update: %v", errLoad)
	}
	if !cooldownSnapshotsContainAuth(loaded, authA) || !cooldownSnapshotsContainAuth(loaded, authB) {
		t.Fatalf("Apply(auth A) changed unrelated auth B: %+v", loaded)
	}

	if errApply := store.Apply(ctx, []cliproxyauth.CooldownStateSnapshot{{AuthID: authA}}); errApply != nil {
		t.Fatalf("Apply(clear auth A): %v", errApply)
	}
	loaded, errLoad = store.Load(ctx)
	if errLoad != nil {
		t.Fatalf("Load after clear: %v", errLoad)
	}
	if cooldownSnapshotsContainAuth(loaded, authA) || !cooldownSnapshotsContainAuth(loaded, authB) {
		t.Fatalf("clear(auth A) changed unexpected rows: %+v", loaded)
	}
}

func cooldownSnapshotsContainAuth(snapshots []cliproxyauth.CooldownStateSnapshot, authID string) bool {
	for _, snapshot := range snapshots {
		if snapshot.AuthID == authID {
			return true
		}
	}
	return false
}
