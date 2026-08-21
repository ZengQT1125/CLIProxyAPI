package executor

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestPrepareRequestRejectsMissingToken asserts that executors fail loudly when a
// credential carries no usable token. Silently forwarding an unauthenticated
// request makes the upstream answer with a misleading error (Anthropic replies
// "x-api-key header is required"), which hides the real cause from operators.
func TestPrepareRequestRejectsMissingToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		preparer cliproxyauth.RequestPreparer
		auth     *cliproxyauth.Auth
	}{
		{
			name:     "kimi",
			preparer: NewKimiExecutor(&config.Config{}),
			auth: &cliproxyauth.Auth{
				ID:       "kimi-oauth",
				Provider: "kimi",
				Metadata: map[string]any{"type": "kimi"},
			},
		},
		{
			name:     "openai-compat",
			preparer: NewOpenAICompatExecutor("example", &config.Config{}),
			auth: &cliproxyauth.Auth{
				ID:       "compat-key",
				Provider: "openai-compatibility",
				Metadata: map[string]any{"type": "openai-compatibility"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodPost, "https://upstream.example.com/v1/messages", nil)
			err := tc.preparer.PrepareRequest(req, tc.auth)
			if err == nil {
				t.Fatalf("PrepareRequest() error = nil, want missing-token error")
			}

			var status statusErr
			if !errors.As(err, &status) {
				t.Fatalf("PrepareRequest() error type = %T, want statusErr", err)
			}
			if status.StatusCode() != http.StatusUnauthorized {
				t.Errorf("PrepareRequest() status = %d, want %d", status.StatusCode(), http.StatusUnauthorized)
			}
		})
	}
}

// TestPrepareRequestKeepsWorkingCredentials guards against the assertion above
// rejecting credentials that do carry a token.
func TestPrepareRequestKeepsWorkingCredentials(t *testing.T) {
	t.Parallel()

	claude := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "claude-oauth",
		Provider: "claude",
		Metadata: map[string]any{"type": "claude", "access_token": "sk-ant-oat01-token"},
	}

	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err := claude.PrepareRequest(req, auth); err != nil {
		t.Fatalf("PrepareRequest() error = %v, want nil", err)
	}
	if got, want := req.Header.Get("Authorization"), "Bearer sk-ant-oat01-token"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}
