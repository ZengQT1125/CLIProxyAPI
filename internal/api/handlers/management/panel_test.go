package management

import (
	"crypto/sha256"
	"encoding/hex"
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
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v3.2.1","assets":[]}`))
	}))
	defer releaseServer.Close()

	h := &Handler{
		cfg: &config.Config{
			RemoteManagement: config.RemoteManagement{
				PanelGitHubRepository: releaseServer.URL + "/repos/acme/panel/releases/latest",
			},
		},
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
	sum := sha256.Sum256([]byte(assetBody))
	digest := hex.EncodeToString(sum[:])

	assetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(assetBody))
	}))
	defer assetServer.Close()

	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"tag_name":"v3.2.1",
			"assets":[{
				"name":"management.html",
				"browser_download_url":"` + assetServer.URL + `/management.html",
				"digest":"sha256:` + digest + `"
			}]
		}`))
	}))
	defer releaseServer.Close()

	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)

	h := &Handler{
		cfg: &config.Config{
			RemoteManagement: config.RemoteManagement{
				PanelGitHubRepository: releaseServer.URL + "/repos/acme/panel/releases/latest",
			},
		},
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
