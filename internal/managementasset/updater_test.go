package managementasset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestAutoUpdateSkipReason(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *config.Config
		wantReason string
		wantSkip   bool
	}{
		{name: "nil config", wantReason: "config not yet available", wantSkip: true},
		{name: "cluster mode", cfg: &config.Config{Home: config.HomeConfig{Enabled: true}}, wantReason: "cluster mode enabled", wantSkip: true},
		{name: "control panel disabled", cfg: &config.Config{RemoteManagement: config.RemoteManagement{DisableControlPanel: true}}, wantReason: "control panel disabled", wantSkip: true},
		{name: "auto update disabled", cfg: &config.Config{RemoteManagement: config.RemoteManagement{DisableAutoUpdatePanel: true}}, wantReason: "disable-auto-update-panel is enabled", wantSkip: true},
		{name: "enabled", cfg: &config.Config{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotReason, gotSkip := autoUpdateSkipReason(tt.cfg)
			if gotReason != tt.wantReason || gotSkip != tt.wantSkip {
				t.Fatalf("autoUpdateSkipReason() = (%q, %t), want (%q, %t)", gotReason, gotSkip, tt.wantReason, tt.wantSkip)
			}
		})
	}
}

func TestDefaultManagementRepositoryTargetsForkManifest(t *testing.T) {
	const wantRepositoryURL = "https://github.com/caidaoli/Cli-Proxy-API-Management-Center"
	const wantManifestURL = "https://github.com/caidaoli/Cli-Proxy-API-Management-Center/releases/latest/download/panel-manifest.json"
	const wantAssetURL = "https://github.com/caidaoli/Cli-Proxy-API-Management-Center/releases/download/v1.58.0/management.html"

	if defaultManagementRepositoryURL != wantRepositoryURL {
		t.Fatalf("default repository URL = %q, want %q", defaultManagementRepositoryURL, wantRepositoryURL)
	}
	if got := managementLatestManifestURL(defaultManagementRepositoryURL); got != wantManifestURL {
		t.Fatalf("manifest URL = %q, want %q", got, wantManifestURL)
	}
	if got := managementReleaseAssetURL(defaultManagementRepositoryURL, "v1.58.0", ManagementFileName); got != wantAssetURL {
		t.Fatalf("asset URL = %q, want %q", got, wantAssetURL)
	}
}

func TestLoadManagementPanelUsesEmbeddedBaselineWithoutDisk(t *testing.T) {
	t.Setenv("MANAGEMENT_STATIC_PATH", t.TempDir())

	panel, err := LoadManagementPanel(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("LoadManagementPanel() error = %v", err)
	}
	if panel.Source != PanelSourceEmbedded {
		t.Fatalf("panel source = %q, want %q", panel.Source, PanelSourceEmbedded)
	}
	if len(panel.HTML) == 0 {
		t.Fatal("embedded panel is empty")
	}
	assertPanelHash(t, panel.HTML, panel.Manifest.SHA256)
}

func TestLoadManagementPanelUsesVerifiedNewerDiskPanel(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	manifest := writeTestDiskPanel(t, staticDir, "v99.0.0", []byte("<!doctype html><title>disk panel</title>"), "")

	panel, err := LoadManagementPanel(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("LoadManagementPanel() error = %v", err)
	}
	if panel.Source != PanelSourceDisk {
		t.Fatalf("panel source = %q, want %q", panel.Source, PanelSourceDisk)
	}
	if panel.Manifest.Version != manifest.Version {
		t.Fatalf("panel version = %q, want %q", panel.Manifest.Version, manifest.Version)
	}
	if string(panel.HTML) != "<!doctype html><title>disk panel</title>" {
		t.Fatalf("panel HTML = %q", panel.HTML)
	}
}

func TestLoadManagementPanelRejectsDiskPanelWithBadHash(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	manifest := writeTestDiskPanel(t, staticDir, "v99.0.0", []byte("valid panel"), "")
	assetPath := filepath.Join(staticDir, "management."+manifest.SHA256+".html")
	if err := os.WriteFile(assetPath, []byte("tampered panel"), 0o644); err != nil {
		t.Fatalf("tamper disk panel: %v", err)
	}

	panel, err := LoadManagementPanel(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("LoadManagementPanel() error = %v", err)
	}
	if panel.Source != PanelSourceEmbedded {
		t.Fatalf("panel source = %q, want %q", panel.Source, PanelSourceEmbedded)
	}
}

func TestLoadManagementPanelKeepsEmbeddedBaselineForOlderDiskPanel(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	writeTestDiskPanel(t, staticDir, "v1.0.0", []byte("older panel"), "")

	panel, err := LoadManagementPanel(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("LoadManagementPanel() error = %v", err)
	}
	if panel.Source != PanelSourceEmbedded {
		t.Fatalf("panel source = %q, want %q", panel.Source, PanelSourceEmbedded)
	}
}

func TestManifestContractRejectsUnsupportedMetadata(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "unknown field", data: `{"version":"v1.2.3","sha256":"` + testSHA256("panel") + `","asset":"management.html","url":"https://example.com"}`},
		{name: "unsupported asset", data: `{"version":"v1.2.3","sha256":"` + testSHA256("panel") + `","asset":"other.html"}`},
		{name: "prerelease version", data: `{"version":"v1.2.3-beta.1","sha256":"` + testSHA256("panel") + `","asset":"management.html"}`},
		{name: "trailing JSON", data: `{"version":"v1.2.3","sha256":"` + testSHA256("panel") + `","asset":"management.html"}{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseManifest([]byte(tt.data)); err == nil {
				t.Fatal("parseManifest() error = nil")
			}
		})
	}
}

func TestGetLatestReleaseReadsManifestOnly(t *testing.T) {
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/latest/download/panel-manifest.json" {
			t.Fatalf("request path = %q", r.URL.Path)
		}
		if accept := r.Header.Get("Accept"); accept != "application/json" {
			t.Fatalf("Accept header = %q, want application/json", accept)
		}
		writeTestManifestResponse(t, w, Manifest{Version: "v9.8.7", SHA256: testSHA256("panel"), Asset: ManagementFileName})
	}))
	defer releaseServer.Close()

	release, err := getLatestRelease(context.Background(), releaseServer.Client(), releaseServer.URL)
	if err != nil {
		t.Fatalf("getLatestRelease() error = %v", err)
	}
	if release.Version != "v9.8.7" {
		t.Fatalf("release version = %q, want v9.8.7", release.Version)
	}
}

func TestUpdateLatestManagementHTMLSkipsAssetDownloadWhenVersionMatches(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	baseline, err := LoadManagementPanel(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("load embedded baseline: %v", err)
	}

	var assetRequests atomic.Int32
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest/download/panel-manifest.json":
			writeTestManifestResponse(t, w, baseline.Manifest)
		case "/releases/download/" + baseline.Manifest.Version + "/management.html":
			assetRequests.Add(1)
			_, _ = w.Write([]byte("unexpected download"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer releaseServer.Close()

	result, err := updateLatestManagementHTML(context.Background(), staticDir, releaseServer.Client(), releaseServer.URL, "dev")
	if err != nil {
		t.Fatalf("updateLatestManagementHTML() error = %v", err)
	}
	if result.Updated {
		t.Fatal("Updated = true, want false for the embedded version")
	}
	if got := assetRequests.Load(); got != 0 {
		t.Fatalf("asset requests = %d, want 0", got)
	}
}

func TestUpdateLatestManagementHTMLDownloadsExactManifestRelease(t *testing.T) {
	const version = "v99.0.0"
	assetBody := []byte("<!doctype html><title>new panel</title>")
	manifest := Manifest{Version: version, SHA256: testSHA256Bytes(assetBody), Asset: ManagementFileName}

	var assetRequests atomic.Int32
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/acme/panel/releases/latest/download/panel-manifest.json":
			writeTestManifestResponse(t, w, manifest)
		case "/acme/panel/releases/download/v99.0.0/management.html":
			assetRequests.Add(1)
			_, _ = w.Write(assetBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer releaseServer.Close()

	staticDir := t.TempDir()
	result, err := updateLatestManagementHTML(context.Background(), staticDir, releaseServer.Client(), releaseServer.URL+"/acme/panel", "dev")
	if err != nil {
		t.Fatalf("updateLatestManagementHTML() error = %v", err)
	}
	if !result.Updated || result.Version != version || result.SHA256 != manifest.SHA256 {
		t.Fatalf("update result = %+v", result)
	}
	if got := assetRequests.Load(); got != 1 {
		t.Fatalf("asset requests = %d, want 1", got)
	}

	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	panel, err := LoadManagementPanel(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("LoadManagementPanel() error = %v", err)
	}
	if panel.Source != PanelSourceDisk || string(panel.HTML) != string(assetBody) {
		t.Fatalf("loaded panel source=%q body=%q", panel.Source, panel.HTML)
	}
}

func TestUpdateLatestManagementHTMLRejectsBadAssetHash(t *testing.T) {
	manifest := Manifest{Version: "v99.0.0", SHA256: testSHA256("expected"), Asset: ManagementFileName}
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest/download/panel-manifest.json":
			writeTestManifestResponse(t, w, manifest)
		case "/releases/download/v99.0.0/management.html":
			_, _ = w.Write([]byte("tampered"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer releaseServer.Close()

	staticDir := t.TempDir()
	if _, err := updateLatestManagementHTML(context.Background(), staticDir, releaseServer.Client(), releaseServer.URL, "dev"); err == nil {
		t.Fatal("updateLatestManagementHTML() error = nil, want hash mismatch")
	}
	if _, err := os.Stat(filepath.Join(staticDir, panelManifestFileName)); !os.IsNotExist(err) {
		t.Fatalf("disk manifest exists after rejected update: %v", err)
	}
}

func TestRejectedUpdatePreservesLastVerifiedDiskPanel(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	previousBody := []byte("verified previous panel")
	previousManifest := writeTestDiskPanel(t, staticDir, "v98.0.0", previousBody, "")
	manifest := Manifest{Version: "v99.0.0", SHA256: testSHA256("expected"), Asset: ManagementFileName}
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest/download/panel-manifest.json":
			writeTestManifestResponse(t, w, manifest)
		case "/releases/download/v99.0.0/management.html":
			_, _ = w.Write([]byte("tampered"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer releaseServer.Close()

	if _, err := updateLatestManagementHTML(context.Background(), staticDir, releaseServer.Client(), releaseServer.URL, "dev"); err == nil {
		t.Fatal("updateLatestManagementHTML() error = nil, want hash mismatch")
	}
	panel, err := LoadManagementPanel(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("LoadManagementPanel() error = %v", err)
	}
	if panel.Source != PanelSourceDisk || panel.Manifest.Version != previousManifest.Version || string(panel.HTML) != string(previousBody) {
		t.Fatalf("active panel after rejected update = source=%q version=%q body=%q", panel.Source, panel.Manifest.Version, panel.HTML)
	}
}

func TestUpdateLatestManagementHTMLSkipsIncompatibleRelease(t *testing.T) {
	manifest := Manifest{
		Version:       "v99.0.0",
		SHA256:        testSHA256("panel"),
		Asset:         ManagementFileName,
		MinCLIVersion: "v99.0.0",
	}
	var assetRequests atomic.Int32
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest/download/panel-manifest.json":
			writeTestManifestResponse(t, w, manifest)
		case "/releases/download/v99.0.0/management.html":
			assetRequests.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer releaseServer.Close()

	result, err := updateLatestManagementHTML(context.Background(), t.TempDir(), releaseServer.Client(), releaseServer.URL, "v8.31.0")
	if err != nil {
		t.Fatalf("updateLatestManagementHTML() error = %v", err)
	}
	if result.Updated {
		t.Fatal("Updated = true, want false for incompatible release")
	}
	if got := assetRequests.Load(); got != 0 {
		t.Fatalf("asset requests = %d, want 0", got)
	}
}

func writeTestDiskPanel(t *testing.T, staticDir, version string, body []byte, minCLIVersion string) Manifest {
	t.Helper()
	manifest := Manifest{
		Version:       version,
		SHA256:        testSHA256Bytes(body),
		Asset:         ManagementFileName,
		MinCLIVersion: minCLIVersion,
	}
	if err := os.WriteFile(filepath.Join(staticDir, "management."+manifest.SHA256+".html"), body, 0o644); err != nil {
		t.Fatalf("write disk panel: %v", err)
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err = os.WriteFile(filepath.Join(staticDir, "panel-manifest.json"), manifestData, 0o644); err != nil {
		t.Fatalf("write disk manifest: %v", err)
	}
	return manifest
}

func writeTestManifestResponse(t *testing.T, w http.ResponseWriter, manifest Manifest) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(manifest); err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
}

func assertPanelHash(t *testing.T, data []byte, want string) {
	t.Helper()
	if got := testSHA256Bytes(data); got != want {
		t.Fatalf("panel hash = %q, want %q", got, want)
	}
}

func testSHA256(value string) string {
	return testSHA256Bytes([]byte(value))
}

func testSHA256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
