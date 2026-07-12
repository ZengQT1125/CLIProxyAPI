package usage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteUsageStorePersistsCacheWriteTokens(t *testing.T) {
	ctx := context.Background()
	store, err := newSQLiteUsageStoreAtPath(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("newSQLiteUsageStoreAtPath failed: %v", err)
	}
	defer store.Close()

	err = store.Insert(ctx, UsageRecord{
		APIKey:           "api-1",
		Model:            "gpt-5.6",
		RequestedAt:      time.Unix(1_700_000_000, 0),
		InputTokens:      100,
		OutputTokens:     20,
		CachedTokens:     30,
		CacheWriteTokens: 40,
		TotalTokens:      120,
	})
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	details, err := store.GetDetails(ctx, 0, 10)
	if err != nil {
		t.Fatalf("GetDetails failed: %v", err)
	}
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].CacheWriteTokens != 40 {
		t.Fatalf("cache write tokens = %d, want 40", details[0].CacheWriteTokens)
	}
}

func TestSQLiteUsageStoreMigratesCacheWriteTokensColumn(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db failed: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		CREATE TABLE usage_records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			api_key TEXT NOT NULL,
			model TEXT NOT NULL,
			source TEXT,
			auth_index TEXT,
			failed INTEGER NOT NULL DEFAULT 0,
			requested_at INTEGER NOT NULL,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0
		)
	`)
	if err != nil {
		t.Fatalf("create legacy schema failed: %v", err)
	}
	if err = db.Close(); err != nil {
		t.Fatalf("close legacy db failed: %v", err)
	}

	store, err := newSQLiteUsageStoreAtPath(dbPath)
	if err != nil {
		t.Fatalf("open migrated store failed: %v", err)
	}
	defer store.Close()

	columns := make(map[string]struct{})
	rows, err := store.db.QueryContext(ctx, "PRAGMA table_info(usage_records)")
	if err != nil {
		t.Fatalf("query columns failed: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan column failed: %v", err)
		}
		columns[name] = struct{}{}
	}
	if _, ok := columns["cache_write_tokens"]; !ok {
		t.Fatal("cache_write_tokens column missing after migration")
	}
}

func TestResolveLocalUsageDBPath(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "auth")

	t.Setenv("PGSTORE_LOCAL_PATH", filepath.Join(t.TempDir(), "pglocal"))
	got := resolveLocalUsageDBPath(authDir)
	want := filepath.Join(getEnvOrFatal(t, "PGSTORE_LOCAL_PATH"), defaultLocalUsageFileName)
	if got != want {
		t.Fatalf("unexpected local db path: got %q want %q", got, want)
	}

	t.Setenv("PGSTORE_LOCAL_PATH", filepath.Join(t.TempDir(), "custom.db"))
	got = resolveLocalUsageDBPath(authDir)
	want = getEnvOrFatal(t, "PGSTORE_LOCAL_PATH")
	if got != want {
		t.Fatalf("unexpected db file path: got %q want %q", got, want)
	}

	t.Setenv("PGSTORE_LOCAL_PATH", "")
	got = resolveLocalUsageDBPath(authDir)
	want = filepath.Join(authDir, defaultLocalUsageFileName)
	if got != want {
		t.Fatalf("unexpected fallback db path: got %q want %q", got, want)
	}
}

func TestSQLiteUsageStoreReset(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sqlite", "usage.db")

	store, err := newSQLiteUsageStoreAtPath(dbPath)
	if err != nil {
		t.Fatalf("newSQLiteUsageStoreAtPath failed: %v", err)
	}
	defer store.Close()

	err = store.Insert(ctx, UsageRecord{
		APIKey:      "api-1",
		Model:       "model-1",
		Source:      "source-1",
		AuthIndex:   "0",
		Failed:      false,
		RequestedAt: time.Now(),
		TotalTokens: 10,
	})
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	details, err := store.GetDetails(ctx, 0, 10)
	if err != nil {
		t.Fatalf("GetDetails before reset failed: %v", err)
	}
	if len(details) != 1 {
		t.Fatalf("unexpected detail count before reset: got %d want 1", len(details))
	}

	if err = store.Reset(ctx); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}

	details, err = store.GetDetails(ctx, 0, 10)
	if err != nil {
		t.Fatalf("GetDetails after reset failed: %v", err)
	}
	if len(details) != 0 {
		t.Fatalf("unexpected detail count after reset: got %d want 0", len(details))
	}
}

func TestSQLiteUsageStoreDisablesMemoryMappedIO(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sqlite", "usage.db")

	store, err := newSQLiteUsageStoreAtPath(dbPath)
	if err != nil {
		t.Fatalf("newSQLiteUsageStoreAtPath failed: %v", err)
	}
	defer store.Close()

	var mmapSize int64
	if err = store.db.QueryRowContext(ctx, "PRAGMA mmap_size").Scan(&mmapSize); err != nil {
		t.Fatalf("query mmap_size failed: %v", err)
	}
	if mmapSize != 0 {
		t.Fatalf("sqlite mmap_size should be disabled: got %d want 0", mmapSize)
	}
}

func TestSQLiteUsageStoreEnsureSchemaSkipsCoveredSingleIndexes(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sqlite", "usage.db")

	store, err := newSQLiteUsageStoreAtPath(dbPath)
	if err != nil {
		t.Fatalf("newSQLiteUsageStoreAtPath failed: %v", err)
	}
	defer store.Close()

	names, err := sqliteIndexNameSet(ctx, store, "usage_records")
	if err != nil {
		t.Fatalf("sqliteIndexNameSet failed: %v", err)
	}

	if _, ok := names["idx_usage_requested_at"]; ok {
		t.Fatalf("unexpected redundant index created: idx_usage_requested_at")
	}
	if _, ok := names["idx_usage_api_key"]; ok {
		t.Fatalf("unexpected redundant index created: idx_usage_api_key")
	}
	if _, ok := names["idx_usage_requested_at_id"]; !ok {
		t.Fatalf("expected composite index missing: idx_usage_requested_at_id")
	}
	if _, ok := names["idx_usage_api_model"]; !ok {
		t.Fatalf("expected composite index missing: idx_usage_api_model")
	}
}

func TestSQLiteUsageStoreEnsureSchemaDropsLegacyCoveredSingleIndexes(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sqlite", "usage.db")

	store, err := newSQLiteUsageStoreAtPath(dbPath)
	if err != nil {
		t.Fatalf("newSQLiteUsageStoreAtPath failed: %v", err)
	}
	defer store.Close()

	legacyIndexes := []string{
		"CREATE INDEX IF NOT EXISTS idx_usage_requested_at ON usage_records(requested_at DESC)",
		"CREATE INDEX IF NOT EXISTS idx_usage_api_key ON usage_records(api_key)",
	}
	for _, query := range legacyIndexes {
		if _, err = store.db.ExecContext(ctx, query); err != nil {
			t.Fatalf("create legacy index failed: %v", err)
		}
	}

	if err = store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema failed: %v", err)
	}

	names, err := sqliteIndexNameSet(ctx, store, "usage_records")
	if err != nil {
		t.Fatalf("sqliteIndexNameSet failed: %v", err)
	}

	if _, ok := names["idx_usage_requested_at"]; ok {
		t.Fatalf("legacy redundant index should be dropped: idx_usage_requested_at")
	}
	if _, ok := names["idx_usage_api_key"]; ok {
		t.Fatalf("legacy redundant index should be dropped: idx_usage_api_key")
	}
}

func sqliteIndexNameSet(ctx context.Context, store *sqliteUsageStore, tableName string) (map[string]struct{}, error) {
	rows, err := store.db.QueryContext(ctx, `
		SELECT name
		FROM sqlite_master
		WHERE type='index' AND tbl_name = ?
	`, tableName)
	if err != nil {
		return nil, fmt.Errorf("query sqlite indexes: %w", err)
	}
	defer rows.Close()

	names := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan sqlite index name: %w", err)
		}
		names[name] = struct{}{}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite index names: %w", err)
	}
	return names, nil
}

func getEnvOrFatal(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("expected env %q to be set", key)
	}
	return value
}
