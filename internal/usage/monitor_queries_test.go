package usage

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestSQLiteUsageStoreQueryMonitorRequestLogs(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteUsageStore(t)
	defer store.Close()

	base := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
	insertUsageRecords(t, store,
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: base.Add(-3 * time.Hour), Stream: boolPtr(true), Fast: boolPtr(true), CacheWriteTokens: 9, TotalTokens: 10},
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: base.Add(-2 * time.Hour), Failed: true, TotalTokens: 20},
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: base.Add(-1 * time.Hour), TotalTokens: 30},
		UsageRecord{APIKey: "api-2", Model: "model-b", Source: "source-b", RequestedAt: base.Add(-30 * time.Minute), TotalTokens: 40},
	)

	start := base.Add(-4 * time.Hour)
	end := base
	result, err := store.QueryMonitorRequestLogs(ctx, MonitorQueryFilter{
		APIContains: "api-1",
		Start:       &start,
		End:         &end,
	}, 2, 2, 3)
	if err != nil {
		t.Fatalf("QueryMonitorRequestLogs failed: %v", err)
	}

	if result.Total != 3 {
		t.Fatalf("unexpected total: got %d want 3", result.Total)
	}
	if result.Page != 2 || result.PageSize != 2 {
		t.Fatalf("unexpected page: page=%d pageSize=%d", result.Page, result.PageSize)
	}
	if len(result.Items) != 1 {
		t.Fatalf("unexpected item count: got %d want 1", len(result.Items))
	}
	if !result.Items[0].Timestamp.Equal(base.Add(-3 * time.Hour)) {
		t.Fatalf("unexpected item timestamp: got %s", result.Items[0].Timestamp)
	}
	if result.Items[0].CacheWriteTokens != 9 {
		t.Fatalf("cache write tokens = %d, want 9", result.Items[0].CacheWriteTokens)
	}
	if result.Items[0].Stream == nil || !*result.Items[0].Stream {
		t.Fatalf("stream = %v, want true", result.Items[0].Stream)
	}
	if result.Items[0].Fast == nil || !*result.Items[0].Fast {
		t.Fatalf("fast = %v, want true", result.Items[0].Fast)
	}

	stats, ok := result.GroupStats[MonitorGroupKey("source-a", "model-a")]
	if !ok {
		t.Fatalf("expected group stats for source-a/model-a")
	}
	if stats.Total != 3 || stats.Success != 2 {
		t.Fatalf("unexpected group stats: total=%d success=%d", stats.Total, stats.Success)
	}
	if len(stats.Recent) != 3 {
		t.Fatalf("unexpected recent count: %d", len(stats.Recent))
	}
	if !stats.Recent[0].Timestamp.Equal(base.Add(-3*time.Hour)) || !stats.Recent[2].Timestamp.Equal(base.Add(-1*time.Hour)) {
		t.Fatalf("recent order mismatch: %+v", stats.Recent)
	}

	assertStringSliceEqual(t, result.Filters.APIs, []string{"api-1"})
	assertStringSliceEqual(t, result.Filters.Models, []string{"model-a"})
	assertStringSliceEqual(t, result.Filters.Sources, []string{"source-a"})
}

func TestSQLiteUsageStoreQueryMonitorChannelStats(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteUsageStore(t)
	defer store.Close()

	base := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
	insertUsageRecords(t, store,
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: base.Add(-4 * time.Hour), Fast: boolPtr(true), InputTokens: 100, OutputTokens: 20, CachedTokens: 30, CacheWriteTokens: 40},
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: base.Add(-3 * time.Hour), Failed: true, InputTokens: 50, OutputTokens: 10, CachedTokens: 5, CacheWriteTokens: 5},
		UsageRecord{APIKey: "api-1", Model: "model-b", Source: "source-a", RequestedAt: base.Add(-2 * time.Hour), InputTokens: 25, OutputTokens: 7, CacheWriteTokens: 7},
		UsageRecord{APIKey: "api-2", Model: "model-c", Source: "source-b", RequestedAt: base.Add(-1 * time.Hour), InputTokens: 11, OutputTokens: 13, CachedTokens: 17, CacheWriteTokens: 17},
	)

	result, err := store.QueryMonitorChannelStats(ctx, MonitorQueryFilter{Status: "failed"}, 1, 10, 12)
	if err != nil {
		t.Fatalf("QueryMonitorChannelStats failed: %v", err)
	}

	if len(result.Items) != 1 {
		t.Fatalf("unexpected item count: got %d want 1", len(result.Items))
	}
	item := result.Items[0]
	if item.Source != "source-a" {
		t.Fatalf("unexpected source: %s", item.Source)
	}
	if item.TotalRequests != 3 || item.SuccessRequests != 2 || item.FailedRequests != 1 {
		t.Fatalf("unexpected aggregate: %+v", item)
	}
	if item.InputTokens != 125 || item.OutputTokens != 27 || item.CachedTokens != 30 {
		t.Fatalf("unexpected token aggregate: input=%d output=%d cached=%d", item.InputTokens, item.OutputTokens, item.CachedTokens)
	}
	if item.CacheWriteTokens != 47 {
		t.Fatalf("cache write aggregate = %d, want 47", item.CacheWriteTokens)
	}
	if len(item.Models) != 2 {
		t.Fatalf("unexpected model count: %d", len(item.Models))
	}
	if item.Models[0].Model != "model-a" || item.Models[0].Requests != 2 {
		t.Fatalf("unexpected first model: %+v", item.Models[0])
	}
	if item.Models[0].InputTokens != 100 || item.Models[0].OutputTokens != 20 || item.Models[0].CachedTokens != 30 {
		t.Fatalf("unexpected first model token aggregate: %+v", item.Models[0])
	}
	if item.Models[0].CacheWriteTokens != 40 {
		t.Fatalf("first model cache write aggregate = %d, want 40", item.Models[0].CacheWriteTokens)
	}
	if item.Models[0].FastInputTokens != 100 || item.Models[0].FastOutputTokens != 20 || item.Models[0].FastCachedTokens != 30 || item.Models[0].FastCacheWriteTokens != 40 {
		t.Fatalf("first model fast token aggregate = %+v", item.Models[0])
	}

	assertStringSliceEqual(t, result.Filters.APIs, []string{"api-1", "api-2"})
	assertStringSliceEqual(t, result.Filters.Models, []string{"model-a", "model-b", "model-c"})
	assertStringSliceEqual(t, result.Filters.Sources, []string{"source-a", "source-b"})
}

func boolPtr(value bool) *bool { return &value }

func TestSQLiteUsageStoreQueryMonitorChannelStatsPagination(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteUsageStore(t)
	defer store.Close()

	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	insertUsageRecords(t, store,
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: base.Add(-4 * time.Minute)},
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: base.Add(-3 * time.Minute)},
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-b", RequestedAt: base.Add(-2 * time.Minute)},
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-c", RequestedAt: base.Add(-time.Minute)},
	)

	result, err := store.QueryMonitorChannelStats(ctx, MonitorQueryFilter{}, 2, 2, 12)
	if err != nil {
		t.Fatalf("QueryMonitorChannelStats failed: %v", err)
	}
	if result.Total != 3 || result.Page != 2 || result.PageSize != 2 {
		t.Fatalf("unexpected pagination: total=%d page=%d page_size=%d", result.Total, result.Page, result.PageSize)
	}
	if len(result.Items) != 1 || result.Items[0].Source != "source-c" {
		t.Fatalf("unexpected page items: %+v", result.Items)
	}
}

func TestSQLiteUsageStoreQueryMonitorChannelStatsSummary(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteUsageStore(t)
	defer store.Close()

	base := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
	insertUsageRecords(t, store,
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: base.Add(-3 * time.Hour), InputTokens: 100, OutputTokens: 20},
		UsageRecord{APIKey: "api-1", Model: "model-b", Source: "source-a", RequestedAt: base.Add(-2 * time.Hour), InputTokens: 25, OutputTokens: 7},
		UsageRecord{APIKey: "api-2", Model: "model-c", Source: "source-b", RequestedAt: base.Add(-1 * time.Hour), InputTokens: 11, OutputTokens: 13},
	)

	result, err := store.QueryMonitorChannelStats(ctx, MonitorQueryFilter{SummaryOnly: true}, 1, 1, 12)
	if err != nil {
		t.Fatalf("QueryMonitorChannelStats summary failed: %v", err)
	}

	if len(result.Items) != 1 || result.Items[0].Source != "source-a" {
		t.Fatalf("unexpected summary items: %+v", result.Items)
	}
	if len(result.Items[0].Models) != 2 {
		t.Fatalf("summary model count = %d, want 2", len(result.Items[0].Models))
	}
	if len(result.Items[0].Recent) != 0 || len(result.Items[0].Models[0].Recent) != 0 {
		t.Fatalf("summary unexpectedly included recent requests: %+v", result.Items[0])
	}
	if len(result.Filters.APIs) != 0 || len(result.Filters.Models) != 0 || len(result.Filters.Sources) != 0 {
		t.Fatalf("summary unexpectedly included filters: %+v", result.Filters)
	}
}

func TestSQLiteUsageStoreQueryMonitorChannelStatsSummaryCombinesUnknownSources(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteUsageStore(t)
	defer store.Close()

	base := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
	insertUsageRecords(t, store,
		UsageRecord{APIKey: "api-1", Model: "model-empty", Source: "", RequestedAt: base.Add(-2 * time.Hour)},
		UsageRecord{APIKey: "api-1", Model: "model-literal", Source: "unknown", RequestedAt: base.Add(-time.Hour)},
	)

	result, err := store.QueryMonitorChannelStats(ctx, MonitorQueryFilter{SummaryOnly: true}, 1, 1, 12)
	if err != nil {
		t.Fatalf("QueryMonitorChannelStats summary failed: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Source != "unknown" {
		t.Fatalf("unexpected summary items: %+v", result.Items)
	}
	if len(result.Items[0].Models) != 2 {
		t.Fatalf("unknown source model count = %d, want 2", len(result.Items[0].Models))
	}
}

func TestSQLiteUsageStoreQueryMonitorDailyTrendUsesLocalDayRanges(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteUsageStore(t)
	defer store.Close()

	day1 := time.Date(2026, 2, 6, 0, 0, 0, 0, time.Local)
	day2 := day1.AddDate(0, 0, 1)
	day3 := day2.AddDate(0, 0, 1)
	insertUsageRecords(t, store,
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: day1.Add(5 * time.Hour), InputTokens: 99},
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: day1.Add(7 * time.Hour), InputTokens: 10, OutputTokens: 2, ReasoningTokens: 1, CachedTokens: 3, CacheWriteTokens: 4},
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: day1.Add(8 * time.Hour), Failed: true, InputTokens: 999},
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: day2.Add(9 * time.Hour), InputTokens: 20, OutputTokens: 4, ReasoningTokens: 2, CachedTokens: 6, CacheWriteTokens: 8},
		UsageRecord{APIKey: "api-2", Model: "model-b", Source: "source-b", RequestedAt: day2.Add(10 * time.Hour), InputTokens: 500},
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: day3.Add(5 * time.Hour), InputTokens: 30, OutputTokens: 6, ReasoningTokens: 3, CachedTokens: 9, CacheWriteTokens: 12},
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: day3.Add(7 * time.Hour), InputTokens: 77},
	)

	start := day1.Add(6 * time.Hour)
	end := day3.Add(6 * time.Hour)
	result, err := store.QueryMonitorDailyTrend(ctx, MonitorQueryFilter{APIKey: "api-1", Start: &start, End: &end})
	if err != nil {
		t.Fatalf("QueryMonitorDailyTrend failed: %v", err)
	}

	want := []MonitorDailyTrendItem{
		{Date: day1.Format("2006-01-02"), Requests: 2, SuccessRequests: 1, FailedRequests: 1, InputTokens: 10, OutputTokens: 2, ReasoningTokens: 1, CachedTokens: 3, CacheWriteTokens: 4},
		{Date: day2.Format("2006-01-02"), Requests: 1, SuccessRequests: 1, InputTokens: 20, OutputTokens: 4, ReasoningTokens: 2, CachedTokens: 6, CacheWriteTokens: 8},
		{Date: day3.Format("2006-01-02"), Requests: 1, SuccessRequests: 1, InputTokens: 30, OutputTokens: 6, ReasoningTokens: 3, CachedTokens: 9, CacheWriteTokens: 12},
	}
	if len(result) != len(want) {
		t.Fatalf("daily trend length = %d, want %d: %+v", len(result), len(want), result)
	}
	for i := range want {
		if result[i] != want[i] {
			t.Fatalf("daily trend[%d] = %+v, want %+v", i, result[i], want[i])
		}
	}
}

func TestSQLiteUsageStoreQueryMonitorFailureStats(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteUsageStore(t)
	defer store.Close()

	base := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
	insertUsageRecords(t, store,
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", RequestedAt: base.Add(-5 * time.Hour), Failed: true},
		UsageRecord{APIKey: "api-1", Model: "model-b", Source: "source-a", RequestedAt: base.Add(-4 * time.Hour), Failed: true},
		UsageRecord{APIKey: "api-1", Model: "model-b", Source: "source-a", RequestedAt: base.Add(-3 * time.Hour)},
		UsageRecord{APIKey: "api-2", Model: "model-c", Source: "source-b", RequestedAt: base.Add(-2 * time.Hour), Failed: true},
		UsageRecord{APIKey: "api-3", Model: "model-d", Source: "source-c", RequestedAt: base.Add(-1 * time.Hour)},
	)

	result, err := store.QueryMonitorFailureStats(ctx, MonitorQueryFilter{}, 2, 12)
	if err != nil {
		t.Fatalf("QueryMonitorFailureStats failed: %v", err)
	}

	if len(result.Items) != 2 {
		t.Fatalf("unexpected item count: got %d want 2", len(result.Items))
	}
	if result.Items[0].Source != "source-a" || result.Items[0].FailedCount != 2 {
		t.Fatalf("unexpected first item: %+v", result.Items[0])
	}
	if result.Items[1].Source != "source-b" || result.Items[1].FailedCount != 1 {
		t.Fatalf("unexpected second item: %+v", result.Items[1])
	}
	if len(result.Items[0].Models) == 0 || len(result.Items[1].Models) == 0 {
		t.Fatalf("expected models in failure items")
	}

	assertStringSliceEqual(t, result.Filters.Sources, []string{"source-a", "source-b"})
	assertStringSliceEqual(t, result.Filters.Models, []string{"model-a", "model-b", "model-c"})
}

func TestSQLiteUsageStoreQueryMonitorKeyStatsBlocksAuthIndexFilter(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteUsageStore(t)
	defer store.Close()

	base := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
	insertUsageRecords(t, store,
		UsageRecord{APIKey: "api-a", Model: "model-a", Source: "source-a", AuthIndex: "auth-a", RequestedAt: base.Add(-19 * time.Minute)},
		UsageRecord{APIKey: "api-a", Model: "model-a", Source: "source-a", AuthIndex: "auth-a", RequestedAt: base.Add(-9 * time.Minute), Failed: true},
		UsageRecord{APIKey: "api-b", Model: "model-b", Source: "source-b", AuthIndex: "auth-b", RequestedAt: base.Add(-8 * time.Minute)},
		UsageRecord{APIKey: "api-c", Model: "model-c", Source: "source-c", AuthIndex: "auth-c", RequestedAt: base.Add(-7 * time.Minute)},
	)

	rows, err := store.QueryMonitorKeyStatsBlocks(ctx, base.Add(-20*time.Minute).Unix(), base.Unix(), int((10 * time.Minute).Seconds()), []string{"auth-a", "auth-b"}, nil)
	if err != nil {
		t.Fatalf("QueryMonitorKeyStatsBlocks failed: %v", err)
	}

	if len(rows) != 3 {
		t.Fatalf("unexpected row count: got %d want 3 rows=%#v", len(rows), rows)
	}
	var successCount, failureCount int64
	for _, row := range rows {
		if row.AuthIndex == "auth-c" {
			t.Fatalf("unexpected auth index in filtered rows: %+v", row)
		}
		if row.AuthIndex != "auth-a" && row.AuthIndex != "auth-b" {
			t.Fatalf("unexpected auth index in filtered rows: %+v", row)
		}
		if row.Source != "source-a" && row.Source != "source-b" {
			t.Fatalf("unexpected source in filtered rows: %+v", row)
		}
		successCount += row.Success
		failureCount += row.Failure
	}
	if successCount != 2 || failureCount != 1 {
		t.Fatalf("unexpected filtered totals: success=%d failure=%d", successCount, failureCount)
	}
}

func TestSQLiteUsageStoreQueryMonitorKeyStatsBlocksSourceFilter(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteUsageStore(t)
	defer store.Close()

	base := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
	insertUsageRecords(t, store,
		UsageRecord{APIKey: "api-a", Model: "model-a", Source: "email@example.com", AuthIndex: "old-path-index", RequestedAt: base.Add(-19 * time.Minute)},
		UsageRecord{APIKey: "api-a", Model: "model-a", Source: "email@example.com", AuthIndex: "old-path-index", RequestedAt: base.Add(-9 * time.Minute), Failed: true},
		UsageRecord{APIKey: "api-b", Model: "model-b", Source: "other@example.com", AuthIndex: "other-index", RequestedAt: base.Add(-8 * time.Minute)},
	)

	// Query by current auth_index (no rows) plus source alias — historical rows must match.
	rows, err := store.QueryMonitorKeyStatsBlocks(ctx, base.Add(-20*time.Minute).Unix(), base.Unix(), int((10 * time.Minute).Seconds()), []string{"current-index"}, []string{"email@example.com"})
	if err != nil {
		t.Fatalf("QueryMonitorKeyStatsBlocks failed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("unexpected row count: got %d want 2 rows=%#v", len(rows), rows)
	}
	var successCount, failureCount int64
	for _, row := range rows {
		if row.Source != "email@example.com" {
			t.Fatalf("unexpected source: %+v", row)
		}
		if row.AuthIndex != "old-path-index" {
			t.Fatalf("unexpected auth index: %+v", row)
		}
		successCount += row.Success
		failureCount += row.Failure
	}
	if successCount != 1 || failureCount != 1 {
		t.Fatalf("unexpected filtered totals: success=%d failure=%d", successCount, failureCount)
	}
}

func newTestSQLiteUsageStore(t *testing.T) *sqliteUsageStore {
	t.Helper()
	store, err := newSQLiteUsageStoreAtPath(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("newSQLiteUsageStoreAtPath failed: %v", err)
	}
	return store
}

func insertUsageRecords(t *testing.T, store *sqliteUsageStore, records ...UsageRecord) {
	t.Helper()
	added, skipped, err := store.InsertBatch(context.Background(), records)
	if err != nil {
		t.Fatalf("InsertBatch failed: %v", err)
	}
	if added != int64(len(records)) || skipped != 0 {
		t.Fatalf("unexpected insert result: added=%d skipped=%d want_added=%d", added, skipped, len(records))
	}
}

func assertStringSliceEqual(t *testing.T, got, want []string) {
	t.Helper()
	gotCopy := append([]string(nil), got...)
	wantCopy := append([]string(nil), want...)
	sort.Strings(gotCopy)
	sort.Strings(wantCopy)
	if len(gotCopy) != len(wantCopy) {
		t.Fatalf("slice length mismatch: got=%v want=%v", gotCopy, wantCopy)
	}
	for i := range gotCopy {
		if gotCopy[i] != wantCopy[i] {
			t.Fatalf("slice mismatch: got=%v want=%v", gotCopy, wantCopy)
		}
	}
}

func TestSQLiteUsageStoreQueryMonitorRequestDetails(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteUsageStore(t)
	defer store.Close()

	base := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
	insertUsageRecords(t, store,
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", AuthIndex: "0", Method: "POST", Path: "/v1/chat/completions", RequestedAt: base.Add(-3 * time.Hour)},
		UsageRecord{APIKey: "api-1", Model: "model-a", Source: "source-a", AuthIndex: "1", Method: "POST", Path: "/v1/chat/completions", RequestedAt: base.Add(-2 * time.Hour), Failed: true},
		UsageRecord{APIKey: "api-1", Model: "model-b", Source: "source-b", AuthIndex: "0", Method: "GET", Path: "/v1/models", RequestedAt: base.Add(-1 * time.Hour)},
		UsageRecord{APIKey: "api-2", Model: "model-c", Source: "source-b", AuthIndex: "2", Method: "POST", Path: "/v1/responses", RequestedAt: base.Add(-30 * time.Minute)},
	)

	// Test: no filters, returns all ordered by timestamp DESC
	results, err := store.QueryMonitorRequestDetails(ctx, nil, 0, "", "", 100)
	if err != nil {
		t.Fatalf("QueryMonitorRequestDetails failed: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(results))
	}
	if results[0].Path != "/v1/responses" {
		t.Fatalf("expected first result path /v1/responses, got %s", results[0].Path)
	}

	// Test: filter by method
	results, err = store.QueryMonitorRequestDetails(ctx, nil, 0, "GET", "", 100)
	if err != nil {
		t.Fatalf("QueryMonitorRequestDetails with method filter failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for GET, got %d", len(results))
	}
	if results[0].Model != "model-b" {
		t.Fatalf("expected model-b, got %s", results[0].Model)
	}

	// Test: filter by path prefix
	results, err = store.QueryMonitorRequestDetails(ctx, nil, 0, "", "/v1/chat", 100)
	if err != nil {
		t.Fatalf("QueryMonitorRequestDetails with path filter failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for /v1/chat prefix, got %d", len(results))
	}

	// Test: time window filter (center=base-2h, window=2h → covers base-3h to base-1h)
	center := base.Add(-2 * time.Hour)
	results, err = store.QueryMonitorRequestDetails(ctx, &center, 7200, "", "", 100)
	if err != nil {
		t.Fatalf("QueryMonitorRequestDetails with time window failed: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results in time window, got %d", len(results))
	}

	// Test: limit
	results, err = store.QueryMonitorRequestDetails(ctx, nil, 0, "", "", 2)
	if err != nil {
		t.Fatalf("QueryMonitorRequestDetails with limit failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results with limit, got %d", len(results))
	}

	// Test: failed flag is preserved
	results, err = store.QueryMonitorRequestDetails(ctx, nil, 0, "", "", 100)
	if err != nil {
		t.Fatalf("QueryMonitorRequestDetails failed: %v", err)
	}
	failedCount := 0
	for _, r := range results {
		if r.Failed {
			failedCount++
		}
	}
	if failedCount != 1 {
		t.Fatalf("expected 1 failed record, got %d", failedCount)
	}
}
