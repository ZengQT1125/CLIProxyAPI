package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
)

func TestGetManagementPanelLatestVersion(t *testing.T) {
	previous := getLatestManagementPanelRelease
	getLatestManagementPanelRelease = func(context.Context, string) (managementasset.LatestRelease, error) {
		return managementasset.LatestRelease{Version: "v3.2.1"}, nil
	}
	t.Cleanup(func() { getLatestManagementPanelRelease = previous })

	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/panel/latest-version", nil)

	h.GetManagementPanelLatestVersion(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["latest-version"] != "v3.2.1" {
		t.Fatalf("latest-version = %q, want v3.2.1", body["latest-version"])
	}
}

func TestUpdateManagementPanelReportsInstalledRelease(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	previous := updateLatestManagementPanelHTML
	updateLatestManagementPanelHTML = func(_ context.Context, gotStaticDir string, _ string) (managementasset.UpdateResult, error) {
		if gotStaticDir != staticDir {
			t.Fatalf("static directory = %q, want %q", gotStaticDir, staticDir)
		}
		return managementasset.UpdateResult{Updated: true, Version: "v3.2.1", SHA256: "test-hash"}, nil
	}
	t.Cleanup(func() { updateLatestManagementPanelHTML = previous })

	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/panel/update", nil)

	h.UpdateManagementPanel(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body struct {
		Updated bool   `json:"updated"`
		Version string `json:"version"`
		Hash    string `json:"hash"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.Updated || body.Version != "v3.2.1" || body.Hash != "test-hash" {
		t.Fatalf("response = %+v", body)
	}
}
