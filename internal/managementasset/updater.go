package managementasset

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/httpfetch"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/html"
	"golang.org/x/sync/singleflight"
)

const (
	defaultManagementRepositoryURL = "https://github.com/caidaoli/Cli-Proxy-API-Management-Center"
	defaultManagementFallbackURL   = "https://cpamc.router-for.me/"
	managementAssetName            = "management.html"
	managementVersionFileName      = "management.version"
	httpUserAgent                  = "CLIProxyAPI-management-updater"
	managementSyncMinInterval      = 30 * time.Second
	updateCheckInterval            = 3 * time.Hour
	maxAssetDownloadSize           = 50 << 20 // 10 MB safety limit for management asset downloads
)

// ManagementFileName exposes the control panel asset filename.
const ManagementFileName = managementAssetName

var (
	lastUpdateCheckMu   sync.Mutex
	lastUpdateCheckTime time.Time
	currentConfigPtr    atomic.Pointer[config.Config]
	sfGroup             singleflight.Group
)

// SetCurrentConfig stores the latest configuration snapshot for management asset decisions.
func SetCurrentConfig(cfg *config.Config) {
	if cfg == nil {
		currentConfigPtr.Store(nil)
		return
	}
	currentConfigPtr.Store(cfg)
}

// StartAutoUpdater launches a background goroutine that periodically ensures the management asset is up to date.
// It respects the disable-control-panel flag on every iteration and supports hot-reloaded configurations.
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

		staticDir := StaticDir(configFilePath)
		EnsureLatestManagementHTML(ctx, staticDir, cfg.ProxyURL)
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
	client := &http.Client{Timeout: 15 * time.Second}

	sdkCfg := &sdkconfig.SDKConfig{ProxyURL: strings.TrimSpace(proxyURL)}
	util.SetProxy(sdkCfg, client)

	return client
}

func normalizeManagementRepositoryURL(repositoryURL string) string {
	repositoryURL = strings.TrimRight(strings.TrimSpace(repositoryURL), "/")
	if repositoryURL == "" {
		return defaultManagementRepositoryURL
	}
	return repositoryURL
}

func managementReleasePageURL(repositoryURL string) string {
	return normalizeManagementRepositoryURL(repositoryURL) + "/releases"
}

func managementAssetDownloadURL(repositoryURL string) string {
	return normalizeManagementRepositoryURL(repositoryURL) + "/releases/latest/download/" + managementAssetName
}

// LatestRelease describes the latest management panel release.
type LatestRelease struct {
	Version string
}

// GetLatestRelease returns the latest management panel release metadata.
func GetLatestRelease(ctx context.Context, proxyURL string) (LatestRelease, error) {
	return getLatestRelease(ctx, newHTTPClient(proxyURL), defaultManagementRepositoryURL)
}

func getLatestRelease(ctx context.Context, client *http.Client, repositoryURL string) (LatestRelease, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	version, err := fetchLatestReleaseVersion(ctx, client, managementReleasePageURL(repositoryURL))
	if err != nil {
		return LatestRelease{}, err
	}

	return LatestRelease{Version: version}, nil
}

// UpdateLatestManagementHTML downloads the latest management panel asset and writes it atomically.
func UpdateLatestManagementHTML(ctx context.Context, staticDir string, proxyURL string) (string, error) {
	return updateLatestManagementHTML(ctx, staticDir, newHTTPClient(proxyURL), defaultManagementRepositoryURL)
}

func updateLatestManagementHTML(ctx context.Context, staticDir string, client *http.Client, repositoryURL string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	staticDir = strings.TrimSpace(staticDir)
	if staticDir == "" {
		return "", fmt.Errorf("empty static directory")
	}
	if err := os.MkdirAll(staticDir, 0o755); err != nil {
		return "", fmt.Errorf("prepare static directory: %w", err)
	}

	latestVersion, errLatestVersion := fetchLatestReleaseVersion(ctx, client, managementReleasePageURL(repositoryURL))
	if errLatestVersion != nil {
		log.WithError(errLatestVersion).Warn("failed to resolve management asset version before manual update")
	}

	data, downloadedHash, err := downloadAsset(ctx, client, managementAssetDownloadURL(repositoryURL))
	if err != nil {
		return "", err
	}

	localPath := filepath.Join(staticDir, managementAssetName)
	if err = atomicWriteFile(localPath, data); err != nil {
		return "", fmt.Errorf("write management asset: %w", err)
	}
	persistManagementVersion(staticDir, latestVersion)

	log.Infof("management asset updated by manual request (hash=%s)", downloadedHash)
	return downloadedHash, nil
}

// StaticDir resolves the directory that stores the management control panel asset.
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
	fileInfo, err := os.Stat(configFilePath)
	if err == nil {
		if fileInfo.IsDir() {
			base = configFilePath
		}
	}

	return filepath.Join(base, "static")
}

// FilePath resolves the absolute path to the management control panel asset.
func FilePath(configFilePath string) string {
	if override := strings.TrimSpace(os.Getenv("MANAGEMENT_STATIC_PATH")); override != "" {
		cleaned := filepath.Clean(override)
		if strings.EqualFold(filepath.Base(cleaned), managementAssetName) {
			return cleaned
		}
		return filepath.Join(cleaned, ManagementFileName)
	}

	dir := StaticDir(configFilePath)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, ManagementFileName)
}

// EnsureLatestManagementHTML checks the latest management.html asset and updates the local copy when needed.
// It coalesces concurrent sync attempts and returns whether the asset exists after the sync attempt.
func EnsureLatestManagementHTML(ctx context.Context, staticDir string, proxyURL string) bool {
	return ensureLatestManagementHTML(ctx, staticDir, newHTTPClient(proxyURL), defaultManagementRepositoryURL)
}

func ensureLatestManagementHTML(ctx context.Context, staticDir string, client *http.Client, repositoryURL string) bool {
	if ctx == nil {
		ctx = context.Background()
	}

	staticDir = strings.TrimSpace(staticDir)
	if staticDir == "" {
		log.Debug("management asset sync skipped: empty static directory")
		return false
	}
	localPath := filepath.Join(staticDir, managementAssetName)

	_, _, _ = sfGroup.Do(localPath, func() (interface{}, error) {
		lastUpdateCheckMu.Lock()
		now := time.Now()
		timeSinceLastAttempt := now.Sub(lastUpdateCheckTime)
		if !lastUpdateCheckTime.IsZero() && timeSinceLastAttempt < managementSyncMinInterval {
			lastUpdateCheckMu.Unlock()
			log.Debugf(
				"management asset sync skipped by throttle: last attempt %v ago (interval %v)",
				timeSinceLastAttempt.Round(time.Second),
				managementSyncMinInterval,
			)
			return nil, nil
		}
		lastUpdateCheckTime = now
		lastUpdateCheckMu.Unlock()

		localFileMissing := false
		if _, errStat := os.Stat(localPath); errStat != nil {
			if errors.Is(errStat, os.ErrNotExist) {
				localFileMissing = true
			} else {
				log.WithError(errStat).Debug("failed to stat local management asset")
			}
		}

		if errMkdirAll := os.MkdirAll(staticDir, 0o755); errMkdirAll != nil {
			log.WithError(errMkdirAll).Warn("failed to prepare static directory for management asset")
			return nil, nil
		}

		latestVersion, errLatestVersion := fetchLatestReleaseVersion(ctx, client, managementReleasePageURL(repositoryURL))
		if errLatestVersion != nil {
			if !localFileMissing {
				log.WithError(errLatestVersion).Warn("failed to check management asset version; keeping local asset")
				return nil, nil
			}
			log.WithError(errLatestVersion).Warn("failed to check management asset version; downloading missing asset directly")
		} else if !localFileMissing {
			localVersion, errReadVersion := os.ReadFile(filepath.Join(staticDir, managementVersionFileName))
			if errReadVersion == nil && strings.TrimSpace(string(localVersion)) == latestVersion {
				log.Debugf("management asset is already at version %s", latestVersion)
				return nil, nil
			}
		}

		localHash, err := fileSHA256(localPath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				log.WithError(err).Debug("failed to read local management asset hash")
			}
			localHash = ""
		}

		data, downloadedHash, err := downloadAsset(ctx, client, managementAssetDownloadURL(repositoryURL))
		if err != nil {
			if localFileMissing {
				log.WithError(err).Warn("failed to download management asset, trying fallback page")
				if ensureFallbackManagementHTML(ctx, client, localPath) {
					return nil, nil
				}
				return nil, nil
			}
			log.WithError(err).Warn("failed to download management asset")
			return nil, nil
		}

		if localHash != "" && strings.EqualFold(localHash, downloadedHash) {
			persistManagementVersion(staticDir, latestVersion)
			log.Debug("management asset is already up to date")
			return nil, nil
		}

		if err = atomicWriteFile(localPath, data); err != nil {
			log.WithError(err).Warn("failed to update management asset on disk")
			return nil, nil
		}
		persistManagementVersion(staticDir, latestVersion)

		log.Infof("management asset updated successfully (hash=%s)", downloadedHash)
		return nil, nil
	})

	_, err := os.Stat(localPath)
	return err == nil
}

func persistManagementVersion(staticDir, version string) {
	if strings.TrimSpace(version) == "" {
		return
	}
	if errWriteVersion := writeManagementVersion(staticDir, version); errWriteVersion != nil {
		log.WithError(errWriteVersion).Warn("failed to persist management asset version")
	}
}

func writeManagementVersion(staticDir, version string) error {
	version = strings.TrimSpace(version)
	if version == "" {
		return fmt.Errorf("empty management version")
	}
	return atomicWriteFile(filepath.Join(staticDir, managementVersionFileName), []byte(version+"\n"))
}

func ensureFallbackManagementHTML(ctx context.Context, client *http.Client, localPath string) bool {
	data, downloadedHash, err := downloadAsset(ctx, client, defaultManagementFallbackURL)
	if err != nil {
		log.WithError(err).Warn("failed to download fallback management control panel page")
		return false
	}

	log.Warnf("management asset downloaded from fallback URL (hash=%s)", downloadedHash)

	if err = atomicWriteFile(localPath, data); err != nil {
		log.WithError(err).Warn("failed to persist fallback management control panel page")
		return false
	}

	log.Infof("management asset updated from fallback page successfully (hash=%s)", downloadedHash)
	return true
}

func fetchLatestReleaseVersion(ctx context.Context, client *http.Client, releaseURL string) (string, error) {
	if strings.TrimSpace(releaseURL) == "" {
		releaseURL = managementReleasePageURL(defaultManagementRepositoryURL)
	}

	headers := map[string]string{
		"Accept":     "text/html",
		"User-Agent": httpUserAgent,
	}

	data, err := httpfetch.GetBytes(ctx, client, releaseURL, headers, 0)
	if err != nil {
		return "", fmt.Errorf("fetch release page: %w", err)
	}

	version, err := parseLatestReleaseVersion(data, releaseURL)
	if err != nil {
		return "", err
	}
	return version, nil
}

func parseLatestReleaseVersion(data []byte, releaseURL string) (string, error) {
	baseURL, err := url.Parse(strings.TrimSpace(releaseURL))
	if err != nil || baseURL.Host == "" {
		return "", fmt.Errorf("invalid release page url")
	}
	tagPathPrefix := strings.TrimSuffix(baseURL.EscapedPath(), "/") + "/tag/"
	tokenizer := html.NewTokenizer(bytes.NewReader(data))

	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if errToken := tokenizer.Err(); errToken != nil && errToken != io.EOF {
				return "", fmt.Errorf("parse release page: %w", errToken)
			}
			return "", fmt.Errorf("missing release version")
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if token.Data != "a" {
				continue
			}
			for _, attr := range token.Attr {
				if attr.Key != "href" {
					continue
				}
				linkURL, errLink := baseURL.Parse(strings.TrimSpace(attr.Val))
				if errLink != nil || !strings.EqualFold(linkURL.Host, baseURL.Host) {
					continue
				}
				escapedPath := linkURL.EscapedPath()
				if !strings.HasPrefix(escapedPath, tagPathPrefix) {
					continue
				}
				tag, errUnescape := url.PathUnescape(strings.TrimPrefix(escapedPath, tagPathPrefix))
				if errUnescape != nil {
					continue
				}
				tag = strings.TrimSpace(tag)
				if tag != "" {
					return tag, nil
				}
			}
		}
	}
}

func downloadAsset(ctx context.Context, client *http.Client, downloadURL string) ([]byte, string, error) {
	if strings.TrimSpace(downloadURL) == "" {
		return nil, "", fmt.Errorf("empty download url")
	}

	data, err := httpfetch.GetBytes(ctx, client, downloadURL, map[string]string{"User-Agent": httpUserAgent}, maxAssetDownloadSize)
	if err != nil {
		return nil, "", fmt.Errorf("download asset: %w", err)
	}

	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = file.Close()
	}()

	h := sha256.New()
	if _, err = io.Copy(h, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func atomicWriteFile(path string, data []byte) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(path), "management-*.html")
	if err != nil {
		return err
	}

	tmpName := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err = tmpFile.Write(data); err != nil {
		return err
	}

	if err = tmpFile.Chmod(0o644); err != nil {
		return err
	}

	if err = tmpFile.Close(); err != nil {
		return err
	}

	if err = os.Rename(tmpName, path); err != nil {
		return err
	}

	return nil
}
