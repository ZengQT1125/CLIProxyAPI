package managementasset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/httpfetch"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	defaultManagementRepositoryURL = "https://github.com/caidaoli/Cli-Proxy-API-Management-Center"
	httpUserAgent                  = "CLIProxyAPI-management-updater"
	managementSyncMinInterval      = 30 * time.Second
	updateCheckInterval            = 3 * time.Hour
	maxManifestDownloadSize        = 64 << 10
	maxAssetDownloadSize           = 50 << 20
	sha256HexLength                = sha256.Size * 2
)

var (
	lastUpdateCheckMu   sync.Mutex
	lastUpdateCheckTime time.Time
	currentConfigPtr    atomic.Pointer[config.Config]
	sfGroup             singleflight.Group
)

// LatestRelease describes the latest management panel release.
type LatestRelease struct {
	Version string
}

// UpdateResult describes the result of a manifest-driven panel update check.
type UpdateResult struct {
	Updated bool   `json:"updated"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// SetCurrentConfig stores the latest configuration snapshot for management asset decisions.
func SetCurrentConfig(cfg *config.Config) {
	if cfg == nil {
		currentConfigPtr.Store(nil)
		return
	}
	currentConfigPtr.Store(cfg)
}

// StartAutoUpdater launches the manifest check immediately and then every three hours.
func StartAutoUpdater(ctx context.Context, configFilePath string) {
	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		log.Debug("management asset auto-updater skipped: empty config path")
		return
	}
	go runAutoUpdater(ctx, configFilePath)
}

func runAutoUpdater(ctx context.Context, configFilePath string) {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(updateCheckInterval)
	defer ticker.Stop()

	runOnce := func() {
		cfg := currentConfigPtr.Load()
		if reason, skip := autoUpdateSkipReason(cfg); skip {
			log.Debugf("management asset auto-updater skipped: %s", reason)
			return
		}
		EnsureLatestManagementHTML(ctx, StaticDir(configFilePath), cfg.ProxyURL, cfg.RemoteManagement.PanelGitHubRepository)
	}

	runOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

func autoUpdateSkipReason(cfg *config.Config) (string, bool) {
	if developmentPanelPath() != "" {
		return "development panel override is enabled", true
	}
	if cfg == nil {
		return "config not yet available", true
	}
	if cfg.Home.Enabled {
		return "cluster mode enabled", true
	}
	if cfg.RemoteManagement.DisableControlPanel {
		return "control panel disabled", true
	}
	if cfg.RemoteManagement.DisableAutoUpdatePanel {
		return "disable-auto-update-panel is enabled", true
	}
	return "", false
}

func newHTTPClient(proxyURL string) *http.Client {
	client := &http.Client{}
	sdkCfg := &sdkconfig.SDKConfig{ProxyURL: strings.TrimSpace(proxyURL)}
	util.SetProxy(sdkCfg, client)
	return client
}

func normalizeManagementRepositoryURL(repositoryURL string) string {
	repositoryURL = strings.TrimRight(strings.TrimSpace(repositoryURL), "/")
	if repositoryURL == "" {
		return defaultManagementRepositoryURL
	}
	if len(repositoryURL) >= len(".git") && strings.EqualFold(repositoryURL[len(repositoryURL)-len(".git"):], ".git") {
		repositoryURL = repositoryURL[:len(repositoryURL)-len(".git")]
	}
	return repositoryURL
}

func isDefaultManagementRepository(repositoryURL string) bool {
	return strings.EqualFold(normalizeManagementRepositoryURL(repositoryURL), defaultManagementRepositoryURL)
}

func managementLatestManifestURL(repositoryURL string) string {
	return normalizeManagementRepositoryURL(repositoryURL) + "/releases/latest/download/" + panelManifestFileName
}

func managementLatestAssetURL(repositoryURL string) string {
	return normalizeManagementRepositoryURL(repositoryURL) + "/releases/latest/download/" + managementAssetName
}

func managementReleaseAssetURL(repositoryURL, version, asset string) string {
	return normalizeManagementRepositoryURL(repositoryURL) + "/releases/download/" + url.PathEscape(strings.TrimSpace(version)) + "/" + url.PathEscape(strings.TrimSpace(asset))
}

// GetLatestRelease returns the configured panel release metadata.
func GetLatestRelease(ctx context.Context, proxyURL, repositoryURL string) (LatestRelease, error) {
	if !isDefaultManagementRepository(repositoryURL) {
		return LatestRelease{Version: "custom"}, nil
	}
	return getLatestRelease(ctx, newHTTPClient(proxyURL), repositoryURL)
}

func getLatestRelease(ctx context.Context, client *http.Client, repositoryURL string) (LatestRelease, error) {
	manifest, err := fetchLatestManifest(ctx, client, repositoryURL)
	if err != nil {
		return LatestRelease{}, err
	}
	return LatestRelease{Version: manifest.Version}, nil
}

// UpdateLatestManagementHTML updates the configured management panel release.
func UpdateLatestManagementHTML(ctx context.Context, staticDir, proxyURL, repositoryURL string) (UpdateResult, error) {
	client := newHTTPClient(proxyURL)
	if !isDefaultManagementRepository(repositoryURL) {
		return updateCustomManagementHTML(ctx, staticDir, client, repositoryURL)
	}
	return updateLatestManagementHTML(ctx, staticDir, client, repositoryURL, buildinfo.Version)
}

func updateLatestManagementHTML(ctx context.Context, staticDir string, client *http.Client, repositoryURL, cliVersion string) (UpdateResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	staticDir = strings.TrimSpace(staticDir)
	if staticDir == "" {
		return UpdateResult{}, fmt.Errorf("empty static directory")
	}

	manifest, err := fetchLatestManifest(ctx, client, repositoryURL)
	if err != nil {
		return UpdateResult{}, err
	}
	result := UpdateResult{Version: manifest.Version, SHA256: manifest.SHA256}
	if !manifestCompatible(manifest, cliVersion) {
		log.Infof("management panel %s requires CLI %s; keeping current panel", manifest.Version, manifest.MinCLIVersion)
		return result, nil
	}

	currentPanel, err := loadManagementPanel(staticDir, cliVersion)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("load current management panel: %w", err)
	}
	comparison, err := compareReleaseVersions(manifest.Version, currentPanel.Manifest.Version)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("compare management panel versions: %w", err)
	}
	if comparison <= 0 {
		log.Debugf("management panel %s is not newer than active version %s", manifest.Version, currentPanel.Manifest.Version)
		return result, nil
	}

	assetURL := managementReleaseAssetURL(repositoryURL, manifest.Version, manifest.Asset)
	data, downloadedHash, err := downloadAsset(ctx, client, assetURL)
	if err != nil {
		return UpdateResult{}, err
	}
	if downloadedHash != manifest.SHA256 {
		return UpdateResult{}, fmt.Errorf("management panel sha256 mismatch: got %s want %s", downloadedHash, manifest.SHA256)
	}
	if err = installDiskManagementPanel(staticDir, manifest, data); err != nil {
		return UpdateResult{}, err
	}

	result.Updated = true
	log.Infof("management panel updated to %s (sha256=%s)", manifest.Version, manifest.SHA256)
	return result, nil
}

func updateCustomManagementHTML(ctx context.Context, staticDir string, client *http.Client, repositoryURL string) (UpdateResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	staticDir = strings.TrimSpace(staticDir)
	if staticDir == "" {
		return UpdateResult{}, fmt.Errorf("empty static directory")
	}

	data, downloadedHash, err := downloadAsset(ctx, client, managementLatestAssetURL(repositoryURL))
	if err != nil {
		return UpdateResult{}, err
	}
	if err = installCustomManagementPanel(staticDir, data); err != nil {
		return UpdateResult{}, err
	}

	result := UpdateResult{Updated: true, Version: "custom", SHA256: downloadedHash}
	log.Infof("custom management panel updated (sha256=%s)", downloadedHash)
	return result, nil
}

// EnsureLatestManagementHTML coalesces and throttles automatic update checks.
func EnsureLatestManagementHTML(ctx context.Context, staticDir, proxyURL, repositoryURL string) {
	staticDir = strings.TrimSpace(staticDir)
	if staticDir == "" {
		log.Debug("management asset sync skipped: empty static directory")
		return
	}
	_, err, _ := sfGroup.Do(staticDir, func() (any, error) {
		lastUpdateCheckMu.Lock()
		now := time.Now()
		elapsed := now.Sub(lastUpdateCheckTime)
		if !lastUpdateCheckTime.IsZero() && elapsed < managementSyncMinInterval {
			lastUpdateCheckMu.Unlock()
			log.Debugf("management asset sync skipped by throttle: last attempt %v ago", elapsed.Round(time.Second))
			return UpdateResult{}, nil
		}
		lastUpdateCheckTime = now
		lastUpdateCheckMu.Unlock()

		return UpdateLatestManagementHTML(ctx, staticDir, proxyURL, repositoryURL)
	})
	if err != nil {
		log.WithError(err).Warn("management panel update check failed; keeping verified active panel")
	}
}

func fetchLatestManifest(ctx context.Context, client *http.Client, repositoryURL string) (Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	headers := map[string]string{"Accept": "application/json", "User-Agent": httpUserAgent}
	if token := util.ResolveGitHubToken(); token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	data, err := httpfetch.GetBytes(
		ctx,
		client,
		managementLatestManifestURL(repositoryURL),
		headers,
		maxManifestDownloadSize,
	)
	if err != nil {
		return Manifest{}, fmt.Errorf("fetch panel manifest: %w", err)
	}
	manifest, err := parseManifest(data)
	if err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func downloadAsset(ctx context.Context, client *http.Client, downloadURL string) ([]byte, string, error) {
	if strings.TrimSpace(downloadURL) == "" {
		return nil, "", fmt.Errorf("empty download URL")
	}
	data, err := httpfetch.GetBytes(ctx, client, downloadURL, map[string]string{"User-Agent": httpUserAgent}, maxAssetDownloadSize)
	if err != nil {
		return nil, "", fmt.Errorf("download management panel: %w", err)
	}
	return data, sha256Hex(data), nil
}

func installDiskManagementPanel(staticDir string, manifest Manifest, data []byte) error {
	if got := sha256Hex(data); got != manifest.SHA256 {
		return fmt.Errorf("refusing to install management panel with mismatched sha256")
	}
	if err := os.MkdirAll(staticDir, 0o755); err != nil {
		return fmt.Errorf("prepare management panel directory: %w", err)
	}

	assetPath := filepath.Join(staticDir, panelAssetFileName(manifest.SHA256))
	assetValid := false
	if existing, err := readFileLimited(assetPath, maxAssetDownloadSize); err == nil {
		assetValid = sha256Hex(existing) == manifest.SHA256
	} else if !errors.Is(err, os.ErrNotExist) {
		log.WithError(err).Debug("failed to validate existing management panel asset")
	}
	if !assetValid {
		if err := atomicWriteFile(assetPath, data); err != nil {
			return fmt.Errorf("write management panel asset: %w", err)
		}
	}

	manifestData, err := marshalManifest(manifest)
	if err != nil {
		return err
	}
	if err = atomicWriteFile(filepath.Join(staticDir, panelManifestFileName), manifestData); err != nil {
		return fmt.Errorf("activate management panel manifest: %w", err)
	}
	cleanupOldPanelAssets(staticDir, filepath.Base(assetPath))
	return nil
}

func installCustomManagementPanel(staticDir string, data []byte) error {
	if err := os.MkdirAll(staticDir, 0o755); err != nil {
		return fmt.Errorf("prepare custom management panel directory: %w", err)
	}
	if err := atomicWriteFile(filepath.Join(staticDir, customManagementAssetFileName), data); err != nil {
		return fmt.Errorf("write custom management panel asset: %w", err)
	}
	return nil
}

func cleanupOldPanelAssets(staticDir, activeFileName string) {
	matches, err := filepath.Glob(filepath.Join(staticDir, "management.*.html"))
	if err != nil {
		log.WithError(err).Debug("failed to enumerate old management panel assets")
		return
	}
	for _, match := range matches {
		if filepath.Base(match) == activeFileName {
			continue
		}
		if errRemove := os.Remove(match); errRemove != nil && !os.IsNotExist(errRemove) {
			log.WithError(errRemove).Debug("failed to remove old management panel asset")
		}
	}
}

// StaticDir resolves the directory that stores management panel updates.
func StaticDir(configFilePath string) string {
	if override := strings.TrimSpace(os.Getenv("MANAGEMENT_STATIC_PATH")); override != "" {
		cleaned := filepath.Clean(override)
		if strings.EqualFold(filepath.Base(cleaned), managementAssetName) {
			return filepath.Dir(cleaned)
		}
		return cleaned
	}
	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, "static")
	}
	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		return ""
	}
	base := filepath.Dir(configFilePath)
	if fileInfo, err := os.Stat(configFilePath); err == nil && fileInfo.IsDir() {
		base = configFilePath
	}
	return filepath.Join(base, "static")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func atomicWriteFile(path string, data []byte) (err error) {
	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".management-*")
	if err != nil {
		return err
	}
	tmpName := tmpFile.Name()
	closed := false
	defer func() {
		if !closed {
			if errClose := tmpFile.Close(); err == nil && errClose != nil {
				err = errClose
			}
		}
		if errRemove := os.Remove(tmpName); errRemove != nil && !os.IsNotExist(errRemove) {
			log.WithError(errRemove).Debug("failed to remove temporary management panel file")
		}
	}()

	if _, err = tmpFile.Write(data); err != nil {
		return err
	}
	if err = tmpFile.Chmod(0o644); err != nil {
		return err
	}
	if err = tmpFile.Sync(); err != nil {
		return err
	}
	if err = tmpFile.Close(); err != nil {
		return err
	}
	closed = true
	if err = replaceFile(tmpName, path); err != nil {
		return err
	}
	return nil
}
