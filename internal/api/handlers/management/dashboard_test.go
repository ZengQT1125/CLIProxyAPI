package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestGetDashboardSummaryReturnsResourceCounts(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "auth-1", Provider: "codex", FileName: "auth-1.json", Attributes: map[string]string{"path": "/tmp/auth-1.json"}},
		{ID: "auth-2", Provider: "claude", FileName: "auth-2.json", Attributes: map[string]string{"path": "/tmp/auth-2.json"}},
		{ID: "hidden", Provider: "codex", FileName: "hidden.json", Disabled: true},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register auth %q: %v", auth.ID, errRegister)
		}
	}

	modelRegistry := registry.GetGlobalRegistry()
	modelClientID := "dashboard-summary-test"
	modelRegistry.RegisterClient(modelClientID, "codex", []*registry.ModelInfo{{ID: "dashboard-model", Name: "dashboard-model"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(modelClientID) })
	wantModels := len(modelRegistry.GetAvailableModels("openai"))

	handler := NewHandlerWithoutConfigFilePath(&config.Config{
		SDKConfig: config.SDKConfig{APIKeys: []string{"key-1", "key-2", "key-3"}},
		GeminiKey: []config.GeminiKey{{APIKey: "gemini-1"}, {APIKey: "gemini-2"}, {}},
		CodexKey:  []config.CodexKey{{APIKey: "codex-1"}, {}},
		ClaudeKey: []config.ClaudeKey{{APIKey: "claude-1"}, {APIKey: "claude-2"}, {APIKey: "claude-3"}, {}},
		OpenAICompatibility: []config.OpenAICompatibility{
			{Name: "provider-1", BaseURL: "https://one.example.com"},
			{Name: "provider-2", BaseURL: "https://two.example.com"},
			{Name: "missing-base-url"},
		},
	}, manager)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/custom/dashboard", nil)
	handler.GetDashboardSummary(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got dashboardSummaryResponse
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &got); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if got.APIKeys != 3 || got.AuthFiles != 2 || got.Models != wantModels {
		t.Fatalf("resource counts = %+v, want api_keys=3 auth_files=2 models=%d", got, wantModels)
	}
	if got.Providers != (dashboardProviderSummary{Gemini: 2, Codex: 1, Claude: 3, OpenAI: 2}) {
		t.Fatalf("provider counts = %+v", got.Providers)
	}
}
