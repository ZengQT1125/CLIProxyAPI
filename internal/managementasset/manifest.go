package managementasset

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	managementAssetName   = "management.html"
	panelManifestFileName = "panel-manifest.json"
)

// ManagementFileName exposes the control panel release asset filename.
const ManagementFileName = managementAssetName

// Manifest is the release-published metadata contract for a management panel asset.
type Manifest struct {
	Version       string `json:"version"`
	SHA256        string `json:"sha256"`
	Asset         string `json:"asset"`
	MinCLIVersion string `json:"min_cli_version,omitempty"`
}

type releaseVersion struct {
	major uint64
	minor uint64
	patch uint64
}

func parseManifest(data []byte) (Manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode panel manifest: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Manifest{}, err
	}

	manifest.Version = strings.TrimSpace(manifest.Version)
	manifest.SHA256 = strings.ToLower(strings.TrimSpace(manifest.SHA256))
	manifest.Asset = strings.TrimSpace(manifest.Asset)
	manifest.MinCLIVersion = strings.TrimSpace(manifest.MinCLIVersion)
	if _, err := parseReleaseVersion(manifest.Version); err != nil {
		return Manifest{}, fmt.Errorf("invalid panel version %q: %w", manifest.Version, err)
	}
	if manifest.Asset != managementAssetName {
		return Manifest{}, fmt.Errorf("unsupported panel asset %q", manifest.Asset)
	}
	if len(manifest.SHA256) != sha256HexLength {
		return Manifest{}, fmt.Errorf("invalid panel sha256 length")
	}
	if _, err := hex.DecodeString(manifest.SHA256); err != nil {
		return Manifest{}, fmt.Errorf("invalid panel sha256: %w", err)
	}
	if manifest.MinCLIVersion != "" {
		if _, err := parseReleaseVersion(manifest.MinCLIVersion); err != nil {
			return Manifest{}, fmt.Errorf("invalid minimum CLI version %q: %w", manifest.MinCLIVersion, err)
		}
	}
	return manifest, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("panel manifest contains trailing JSON")
		}
		return fmt.Errorf("decode trailing panel manifest data: %w", err)
	}
	return nil
}

func marshalManifest(manifest Manifest) ([]byte, error) {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode panel manifest: %w", err)
	}
	return append(data, '\n'), nil
}

func parseReleaseVersion(value string) (releaseVersion, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "fork/")
	if len(value) > 0 && (value[0] == 'v' || value[0] == 'V') {
		value = value[1:]
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return releaseVersion{}, fmt.Errorf("expected major.minor.patch")
	}

	values := [3]uint64{}
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return releaseVersion{}, fmt.Errorf("invalid numeric segment %q", part)
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return releaseVersion{}, fmt.Errorf("invalid numeric segment %q", part)
		}
		values[index] = number
	}
	return releaseVersion{major: values[0], minor: values[1], patch: values[2]}, nil
}

func compareReleaseVersions(left, right string) (int, error) {
	leftVersion, err := parseReleaseVersion(left)
	if err != nil {
		return 0, err
	}
	rightVersion, err := parseReleaseVersion(right)
	if err != nil {
		return 0, err
	}

	switch {
	case leftVersion.major != rightVersion.major:
		if leftVersion.major < rightVersion.major {
			return -1, nil
		}
		return 1, nil
	case leftVersion.minor != rightVersion.minor:
		if leftVersion.minor < rightVersion.minor {
			return -1, nil
		}
		return 1, nil
	case leftVersion.patch != rightVersion.patch:
		if leftVersion.patch < rightVersion.patch {
			return -1, nil
		}
		return 1, nil
	default:
		return 0, nil
	}
}

func manifestCompatible(manifest Manifest, cliVersion string) bool {
	if manifest.MinCLIVersion == "" || strings.EqualFold(strings.TrimSpace(cliVersion), "dev") {
		return true
	}
	comparison, err := compareReleaseVersions(cliVersion, manifest.MinCLIVersion)
	return err == nil && comparison >= 0
}
