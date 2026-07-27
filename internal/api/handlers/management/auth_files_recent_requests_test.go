package management

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestListAuthFiles_OmitsRecentRequestsAndKeepsTotals(t *testing.T) {
	lastErrorAt := time.Date(2026, time.July, 27, 2, 30, 0, 0, time.UTC)
	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID: "runtime-only-auth-1", Provider: "codex",
		Attributes: map[string]string{
			"runtime_only": "true",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
		Success: 7, Failed: 3,
		LastRequestError: &coreauth.RequestErrorSnapshot{
			Message:    "upstream rejected request",
			Code:       "request_scoped",
			HTTPStatus: 400,
			Timestamp:  lastErrorAt,
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatal(errRegister)
	}

	entry := firstAuthFileEntry(t, NewHandlerWithoutConfigFilePath(
		&config.Config{AuthDir: t.TempDir()}, manager,
	))
	if _, exists := entry["recent_requests"]; exists {
		t.Fatalf("recent_requests must be omitted: %#v", entry)
	}
	if entry["success"] != float64(7) || entry["failed"] != float64(3) {
		t.Fatalf("unexpected totals: %#v", entry)
	}
	lastError, ok := entry["last_request_error"].(map[string]any)
	if !ok {
		t.Fatalf("expected last_request_error object, got %#v", entry["last_request_error"])
	}
	if got := lastError["message"]; got != "upstream rejected request" {
		t.Fatalf("last_request_error.message = %#v", got)
	}
	if got := lastError["code"]; got != "request_scoped" {
		t.Fatalf("last_request_error.code = %#v", got)
	}
	if got := lastError["status_code"]; got != float64(400) {
		t.Fatalf("last_request_error.status_code = %#v", got)
	}
	if got := lastError["timestamp"]; got != lastErrorAt.Format(time.RFC3339) {
		t.Fatalf("last_request_error.timestamp = %#v", got)
	}
}
