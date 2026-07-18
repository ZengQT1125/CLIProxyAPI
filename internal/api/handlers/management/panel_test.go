package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestUpdateManagementPanelWritesLatestAsset(t *testing.T) {
	const assetBody = "<!doctype html><title>updated panel</title>"

	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	previous := updateLatestManagementPanelHTML
	updateLatestManagementPanelHTML = func(_ context.Context, gotStaticDir string, _ string) (string, error) {
		if gotStaticDir != staticDir {
			t.Fatalf("static directory = %q, want %q", gotStaticDir, staticDir)
		}
		if err := os.WriteFile(filepath.Join(gotStaticDir, managementasset.ManagementFileName), []byte(assetBody), 0o644); err != nil {
			return "", err
		}
		return "test-hash", nil
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

	data, err := os.ReadFile(filepath.Join(staticDir, managementasset.ManagementFileName))
	if err != nil {
		t.Fatalf("failed to read management asset: %v", err)
	}
	if string(data) != assetBody {
		t.Fatalf("management asset body = %q, want %q", string(data), assetBody)
	}
}
