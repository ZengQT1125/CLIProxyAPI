package management

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestListAuthFiles_OmitsRecentRequestsAndKeepsTotals(t *testing.T) {
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
}
