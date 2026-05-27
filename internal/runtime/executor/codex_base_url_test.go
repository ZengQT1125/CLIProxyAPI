package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexapi"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestResolveCodexBaseURLDefaultsToChatGPTCodexBackend(t *testing.T) {
	t.Setenv("CODEX_BASE_URL", "")

	got := resolveCodexBaseURL(nil)

	if got != codexapi.DefaultBaseURL {
		t.Fatalf("base URL = %q, want %q", got, codexapi.DefaultBaseURL)
	}
}

func TestResolveCodexBaseURLUsesEnvironmentOverride(t *testing.T) {
	t.Setenv("CODEX_BASE_URL", " https://proxy.example.com/codex/ ")

	got := resolveCodexBaseURL(nil)

	if got != "https://proxy.example.com/codex/" {
		t.Fatalf("base URL = %q, want %q", got, "https://proxy.example.com/codex/")
	}
}

func TestResolveCodexBaseURLKeepsAuthAttributePriority(t *testing.T) {
	t.Setenv("CODEX_BASE_URL", "https://env.example.com/codex")
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": "https://auth.example.com/codex",
	}}

	got := resolveCodexBaseURL(auth)

	if got != "https://auth.example.com/codex" {
		t.Fatalf("base URL = %q, want %q", got, "https://auth.example.com/codex")
	}
}
