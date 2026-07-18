package managementasset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestAutoUpdateSkipReason(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *config.Config
		wantReason string
		wantSkip   bool
	}{
		{
			name:       "nil config",
			cfg:        nil,
			wantReason: "config not yet available",
			wantSkip:   true,
		},
		{
			name: "cluster mode",
			cfg: &config.Config{
				Home: config.HomeConfig{Enabled: true},
			},
			wantReason: "cluster mode enabled",
			wantSkip:   true,
		},
		{
			name: "control panel disabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{DisableControlPanel: true},
			},
			wantReason: "control panel disabled",
			wantSkip:   true,
		},
		{
			name: "auto update disabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{DisableAutoUpdatePanel: true},
			},
			wantReason: "disable-auto-update-panel is enabled",
			wantSkip:   true,
		},
		{
			name:       "enabled",
			cfg:        &config.Config{},
			wantReason: "",
			wantSkip:   false,
		},
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

func TestDefaultManagementRepositoryTargetsForkPanel(t *testing.T) {
	const wantRepositoryURL = "https://github.com/caidaoli/Cli-Proxy-API-Management-Center"
	const wantReleaseURL = "https://github.com/caidaoli/Cli-Proxy-API-Management-Center/releases"
	const wantAssetURL = "https://github.com/caidaoli/Cli-Proxy-API-Management-Center/releases/latest/download/management.html"
	if defaultManagementRepositoryURL != wantRepositoryURL {
		t.Fatalf("default repository URL = %q, want %q", defaultManagementRepositoryURL, wantRepositoryURL)
	}
	if got := managementReleasePageURL(defaultManagementRepositoryURL); got != wantReleaseURL {
		t.Fatalf("release page URL = %q, want %q", got, wantReleaseURL)
	}
	if got := managementAssetDownloadURL(defaultManagementRepositoryURL); got != wantAssetURL {
		t.Fatalf("asset download URL = %q, want %q", got, wantAssetURL)
	}
}

func TestGetLatestRelease(t *testing.T) {
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases" {
			t.Fatalf("release path = %q, want /releases", r.URL.Path)
		}
		if accept := r.Header.Get("Accept"); accept != "text/html" {
			t.Fatalf("Accept header = %q, want text/html", accept)
		}
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			t.Fatalf("Authorization header must not be sent to the public releases page")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html>
<a href="https://example.com/releases/tag/v99.0.0">other repository</a>
<a href="/releases/tag/v9.8.7">latest release</a>
<a href="/releases/tag/v9.8.6">older release</a>`))
	}))
	defer releaseServer.Close()

	t.Setenv("GITSTORE_GIT_URL", "https://github.com/acme/private.git")
	t.Setenv("GITSTORE_GIT_TOKEN", "must-not-be-sent")
	release, err := getLatestRelease(context.Background(), releaseServer.Client(), releaseServer.URL)
	if err != nil {
		t.Fatalf("GetLatestRelease() error = %v", err)
	}
	if release.Version != "v9.8.7" {
		t.Fatalf("release version = %q, want v9.8.7", release.Version)
	}
}

func TestEnsureLatestManagementHTMLSkipsAssetDownloadWhenVersionMatches(t *testing.T) {
	staticDir := t.TempDir()
	localPath := filepath.Join(staticDir, ManagementFileName)
	if errWrite := os.WriteFile(localPath, []byte("current panel"), 0o644); errWrite != nil {
		t.Fatalf("write management asset: %v", errWrite)
	}
	if errWrite := os.WriteFile(filepath.Join(staticDir, managementVersionFileName), []byte("v1.2.3\n"), 0o644); errWrite != nil {
		t.Fatalf("write management version: %v", errWrite)
	}

	var assetRequests atomic.Int32
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases":
			_, _ = w.Write([]byte(`<a href="/releases/tag/v1.2.3">v1.2.3</a>`))
		case "/releases/latest/download/management.html":
			assetRequests.Add(1)
			_, _ = w.Write([]byte("new panel"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer releaseServer.Close()

	resetManagementSyncThrottle(t)
	if ok := ensureLatestManagementHTML(context.Background(), staticDir, releaseServer.Client(), releaseServer.URL); !ok {
		t.Fatal("ensureLatestManagementHTML() = false, want true")
	}
	if got := assetRequests.Load(); got != 0 {
		t.Fatalf("asset requests = %d, want 0 when versions match", got)
	}
	data, errRead := os.ReadFile(localPath)
	if errRead != nil {
		t.Fatalf("read management asset: %v", errRead)
	}
	if string(data) != "current panel" {
		t.Fatalf("management asset = %q, want existing content", data)
	}
}

func TestEnsureLatestManagementHTMLDownloadsWhenVersionChanges(t *testing.T) {
	staticDir := t.TempDir()
	localPath := filepath.Join(staticDir, ManagementFileName)
	if errWrite := os.WriteFile(localPath, []byte("old panel"), 0o644); errWrite != nil {
		t.Fatalf("write management asset: %v", errWrite)
	}
	if errWrite := os.WriteFile(filepath.Join(staticDir, managementVersionFileName), []byte("v1.2.2\n"), 0o644); errWrite != nil {
		t.Fatalf("write management version: %v", errWrite)
	}

	var assetRequests atomic.Int32
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases":
			_, _ = w.Write([]byte(`<a href="/releases/tag/v1.2.3">v1.2.3</a>`))
		case "/releases/latest/download/management.html":
			assetRequests.Add(1)
			_, _ = w.Write([]byte("new panel"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer releaseServer.Close()

	resetManagementSyncThrottle(t)
	if ok := ensureLatestManagementHTML(context.Background(), staticDir, releaseServer.Client(), releaseServer.URL); !ok {
		t.Fatal("ensureLatestManagementHTML() = false, want true")
	}
	if got := assetRequests.Load(); got != 1 {
		t.Fatalf("asset requests = %d, want 1 when version changes", got)
	}
	data, errRead := os.ReadFile(localPath)
	if errRead != nil {
		t.Fatalf("read management asset: %v", errRead)
	}
	if string(data) != "new panel" {
		t.Fatalf("management asset = %q, want new content", data)
	}
	version, errReadVersion := os.ReadFile(filepath.Join(staticDir, managementVersionFileName))
	if errReadVersion != nil {
		t.Fatalf("read management version: %v", errReadVersion)
	}
	if string(version) != "v1.2.3\n" {
		t.Fatalf("management version = %q, want v1.2.3", version)
	}
}

func resetManagementSyncThrottle(t *testing.T) {
	t.Helper()
	lastUpdateCheckMu.Lock()
	previous := lastUpdateCheckTime
	lastUpdateCheckTime = time.Time{}
	lastUpdateCheckMu.Unlock()
	t.Cleanup(func() {
		lastUpdateCheckMu.Lock()
		lastUpdateCheckTime = previous
		lastUpdateCheckMu.Unlock()
	})
}

func TestUpdateLatestManagementHTMLDownloadsDirectReleaseAsset(t *testing.T) {
	const assetBody = "<!doctype html><title>new panel</title>"
	sum := sha256.Sum256([]byte(assetBody))
	digest := hex.EncodeToString(sum[:])

	assetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/acme/panel/releases":
			_, _ = w.Write([]byte(`<a href="/acme/panel/releases/tag/v9.8.7">v9.8.7</a>`))
		case "/acme/panel/releases/latest/download/management.html":
			_, _ = w.Write([]byte(assetBody))
		default:
			http.NotFound(w, r)
		}
	}))
	defer assetServer.Close()

	staticDir := t.TempDir()
	gotHash, err := updateLatestManagementHTML(context.Background(), staticDir, assetServer.Client(), assetServer.URL+"/acme/panel")
	if err != nil {
		t.Fatalf("UpdateLatestManagementHTML() error = %v", err)
	}
	if gotHash != digest {
		t.Fatalf("hash = %q, want %q", gotHash, digest)
	}

	data, err := os.ReadFile(filepath.Join(staticDir, ManagementFileName))
	if err != nil {
		t.Fatalf("failed to read management asset: %v", err)
	}
	if string(data) != assetBody {
		t.Fatalf("management asset body = %q, want %q", string(data), assetBody)
	}
	version, errReadVersion := os.ReadFile(filepath.Join(staticDir, managementVersionFileName))
	if errReadVersion != nil {
		t.Fatalf("failed to read management version: %v", errReadVersion)
	}
	if string(version) != "v9.8.7\n" {
		t.Fatalf("management version = %q, want v9.8.7", version)
	}
}
