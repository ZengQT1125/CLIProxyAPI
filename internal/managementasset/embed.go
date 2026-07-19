package managementasset

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	log "github.com/sirupsen/logrus"
)

const (
	PanelSourceEmbedded       = "embedded"
	PanelSourceDisk           = "disk"
	PanelSourceDevelopment    = "development"
	managementPanelDevPathEnv = "MANAGEMENT_PANEL_DEV_PATH"
)

//go:embed assets/management.html.gz
var embeddedManagementHTMLGzip []byte

//go:embed assets/panel-manifest.json
var embeddedManagementManifestJSON []byte

var (
	embeddedPanelOnce sync.Once
	embeddedPanelData Panel
	embeddedPanelErr  error
)

// Panel is a management panel selected for serving.
type Panel struct {
	HTML     []byte
	Manifest Manifest
	Source   string
}

// LoadManagementPanel selects an explicit development override, a verified newer disk panel, or the embedded baseline.
func LoadManagementPanel(configFilePath string) (Panel, error) {
	if path := developmentPanelPath(); path != "" {
		return loadDevelopmentManagementPanel(path)
	}
	return loadManagementPanel(StaticDir(configFilePath), buildinfo.Version)
}

func developmentPanelPath() string {
	return strings.TrimSpace(os.Getenv(managementPanelDevPathEnv))
}

func loadDevelopmentManagementPanel(path string) (Panel, error) {
	path = filepath.Clean(path)
	data, err := readFileLimited(path, maxAssetDownloadSize)
	if err != nil {
		return Panel{}, fmt.Errorf("read development management panel %s: %w", path, err)
	}
	return Panel{
		HTML: data,
		Manifest: Manifest{
			Version: "dev",
			SHA256:  sha256Hex(data),
			Asset:   managementAssetName,
		},
		Source: PanelSourceDevelopment,
	}, nil
}

func loadManagementPanel(staticDir, cliVersion string) (Panel, error) {
	baseline, err := loadEmbeddedManagementPanel()
	if err != nil {
		return Panel{}, err
	}
	if staticDir == "" {
		return baseline, nil
	}

	diskPanel, ok, err := loadDiskManagementPanel(staticDir, baseline.Manifest, cliVersion)
	if err != nil {
		log.WithError(err).Warn("ignoring invalid management panel on disk")
		return baseline, nil
	}
	if ok {
		return diskPanel, nil
	}
	return baseline, nil
}

func loadEmbeddedManagementPanel() (Panel, error) {
	embeddedPanelOnce.Do(func() {
		manifest, err := parseManifest(embeddedManagementManifestJSON)
		if err != nil {
			embeddedPanelErr = fmt.Errorf("parse embedded management panel manifest: %w", err)
			return
		}

		reader, err := gzip.NewReader(bytes.NewReader(embeddedManagementHTMLGzip))
		if err != nil {
			embeddedPanelErr = fmt.Errorf("open embedded management panel: %w", err)
			return
		}
		data, err := io.ReadAll(io.LimitReader(reader, maxAssetDownloadSize+1))
		if errClose := reader.Close(); errClose != nil && err == nil {
			err = errClose
		}
		if err != nil {
			embeddedPanelErr = fmt.Errorf("decompress embedded management panel: %w", err)
			return
		}
		if len(data) > maxAssetDownloadSize {
			embeddedPanelErr = fmt.Errorf("embedded management panel exceeds size limit")
			return
		}
		if got := sha256Hex(data); got != manifest.SHA256 {
			embeddedPanelErr = fmt.Errorf("embedded management panel hash mismatch")
			return
		}
		embeddedPanelData = Panel{HTML: data, Manifest: manifest, Source: PanelSourceEmbedded}
	})
	return embeddedPanelData, embeddedPanelErr
}

func loadDiskManagementPanel(staticDir string, baseline Manifest, cliVersion string) (Panel, bool, error) {
	manifestData, err := readFileLimited(filepath.Join(staticDir, panelManifestFileName), maxManifestDownloadSize)
	if err != nil {
		if os.IsNotExist(err) {
			return Panel{}, false, nil
		}
		return Panel{}, false, fmt.Errorf("read disk panel manifest: %w", err)
	}
	manifest, err := parseManifest(manifestData)
	if err != nil {
		return Panel{}, false, err
	}
	comparison, err := compareReleaseVersions(manifest.Version, baseline.Version)
	if err != nil {
		return Panel{}, false, fmt.Errorf("compare disk panel version: %w", err)
	}
	if comparison <= 0 || !manifestCompatible(manifest, cliVersion) {
		return Panel{}, false, nil
	}

	assetPath := filepath.Join(staticDir, panelAssetFileName(manifest.SHA256))
	data, err := readFileLimited(assetPath, maxAssetDownloadSize)
	if err != nil {
		return Panel{}, false, fmt.Errorf("read disk panel asset: %w", err)
	}
	if got := sha256Hex(data); got != manifest.SHA256 {
		return Panel{}, false, fmt.Errorf("disk panel hash mismatch")
	}
	return Panel{HTML: data, Manifest: manifest, Source: PanelSourceDisk}, true, nil
}

func panelAssetFileName(digest string) string {
	return "management." + digest + ".html"
}

func readFileLimited(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close management panel file")
		}
	}()

	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d byte limit", limit)
	}
	return data, nil
}
