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
