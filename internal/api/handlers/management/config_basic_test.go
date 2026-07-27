package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestPutRoutingStrategyAcceptsSequentialFillAlias(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("routing:\n  strategy: round-robin\n"), 0o600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg := &config.Config{
		Routing: config.RoutingConfig{Strategy: "round-robin"},
	}
	h := NewHandler(cfg, configPath, coreauth.NewManager(nil, nil, nil))

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/routing/strategy", strings.NewReader(`{"value":"sf"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PutRoutingStrategy(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("PutRoutingStrategy status = %d, want %d with body %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := h.cfg.Routing.Strategy; got != "sequential-fill" {
		t.Fatalf("handler config strategy = %q, want %q", got, "sequential-fill")
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read saved config: %v", err)
	}
	if !strings.Contains(string(data), "sequential-fill") {
		t.Fatalf("saved config = %q, want it to contain %q", string(data), "sequential-fill")
	}
}

func TestConfigYAMLPreservesPanelRepository(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	const source = `remote-management:
  allow-remote: true
  panel-github-repository: https://github.com/acme/incompatible-panel
  panel-repo: https://github.com/acme/legacy-panel
`
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte(source), 0o600); errWrite != nil {
		t.Fatalf("failed to write config file: %v", errWrite)
	}
	h := NewHandler(&config.Config{}, configPath, coreauth.NewManager(nil, nil, nil))

	getRecorder := httptest.NewRecorder()
	getContext, _ := gin.CreateTestContext(getRecorder)
	getContext.Request = httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
	h.GetConfigYAML(getContext)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("GetConfigYAML status = %d, want %d with body %s", getRecorder.Code, http.StatusOK, getRecorder.Body.String())
	}
	if !strings.Contains(getRecorder.Body.String(), "panel-github-repository: https://github.com/acme/incompatible-panel") {
		t.Fatalf("GetConfigYAML lost the supported panel repository:\n%s", getRecorder.Body.String())
	}
	if strings.Contains(getRecorder.Body.String(), "panel-repo:") {
		t.Fatalf("GetConfigYAML retained the legacy panel repository key:\n%s", getRecorder.Body.String())
	}

	putRecorder := httptest.NewRecorder()
	putContext, _ := gin.CreateTestContext(putRecorder)
	putContext.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", strings.NewReader(source))
	h.PutConfigYAML(putContext)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("PutConfigYAML status = %d, want %d with body %s", putRecorder.Code, http.StatusOK, putRecorder.Body.String())
	}
	persisted, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatalf("failed to read persisted config: %v", errRead)
	}
	if !strings.Contains(string(persisted), "panel-github-repository: https://github.com/acme/incompatible-panel") {
		t.Fatalf("PutConfigYAML lost the supported panel repository:\n%s", persisted)
	}
	if strings.Contains(string(persisted), "panel-repo:") {
		t.Fatalf("PutConfigYAML persisted the legacy panel repository key:\n%s", persisted)
	}
	if !strings.Contains(string(persisted), "allow-remote: true") {
		t.Fatalf("PutConfigYAML lost supported remote management settings:\n%s", persisted)
	}
	if got := h.cfg.RemoteManagement.PanelGitHubRepository; got != "https://github.com/acme/incompatible-panel" {
		t.Fatalf("handler panel repository = %q", got)
	}
}
