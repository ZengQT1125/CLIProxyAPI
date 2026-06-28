package managementasset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestGetLatestReleaseUsesConfiguredPanelReleaseURL(t *testing.T) {
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/panel/releases/latest" {
			t.Fatalf("release path = %q, want /repos/acme/panel/releases/latest", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v9.8.7","assets":[]}`))
	}))
	defer releaseServer.Close()

	release, err := GetLatestRelease(context.Background(), "", releaseServer.URL+"/repos/acme/panel/releases/latest")
	if err != nil {
		t.Fatalf("GetLatestRelease() error = %v", err)
	}
	if release.Version != "v9.8.7" {
		t.Fatalf("release version = %q, want v9.8.7", release.Version)
	}
}

func TestUpdateLatestManagementHTMLWritesReleaseAsset(t *testing.T) {
	const assetBody = "<!doctype html><title>new panel</title>"
	sum := sha256.Sum256([]byte(assetBody))
	digest := hex.EncodeToString(sum[:])

	assetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(assetBody))
	}))
	defer assetServer.Close()

	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"tag_name":"v2.0.0",
			"assets":[{
				"name":"management.html",
				"browser_download_url":"` + assetServer.URL + `/management.html",
				"digest":"sha256:` + digest + `"
			}]
		}`))
	}))
	defer releaseServer.Close()

	staticDir := t.TempDir()
	gotHash, err := UpdateLatestManagementHTML(context.Background(), staticDir, "", releaseServer.URL+"/repos/acme/panel/releases/latest")
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
}
