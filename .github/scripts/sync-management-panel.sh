#!/usr/bin/env bash
set -euo pipefail

panel_repository="caidaoli/Cli-Proxy-API-Management-Center"
panel_tag="${1:-${PANEL_TAG:-}}"
expected_sha256="${2:-${PANEL_SHA256:-}}"
source_dir="${3:-${PANEL_RELEASE_DIR:-}}"
resolve_latest=false

if [[ -z "$panel_tag" && -z "$expected_sha256" && -z "$source_dir" ]]; then
	resolve_latest=true
elif [[ -z "$panel_tag" || -z "$expected_sha256" ]]; then
	printf 'panel tag and expected SHA-256 must be provided together\n' >&2
	exit 1
fi

if [[ "$resolve_latest" != true ]]; then
	if [[ ! "$panel_tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
		printf 'panel tag must be an exact stable semantic version such as v1.58.0\n' >&2
		exit 1
	fi
	expected_sha256="$(printf '%s' "$expected_sha256" | tr '[:upper:]' '[:lower:]')"
	if [[ ! "$expected_sha256" =~ ^[0-9a-f]{64}$ ]]; then
		printf 'expected SHA-256 must contain exactly 64 hexadecimal characters\n' >&2
		exit 1
	fi
fi

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
assets_dir="$repository_root/internal/managementasset/assets"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

download_github_asset() {
	local source_url="$1"
	local output_path="$2"
	local effective_url
	effective_url="$(curl --fail --location --silent --show-error --proto '=https' --proto-redir '=https' \
		--write-out '%{url_effective}' --output "$output_path" "$source_url")"
	case "$effective_url" in
		https://github.com/* | https://*.githubusercontent.com/*) ;;
		*)
			printf 'refusing release redirect to untrusted URL %s\n' "$effective_url" >&2
			exit 1
			;;
	esac
}

if [[ -n "$source_dir" ]]; then
	cp "$source_dir/management.html" "$work_dir/management.html"
	cp "$source_dir/panel-manifest.json" "$work_dir/panel-manifest.json"
elif [[ "$resolve_latest" == true ]]; then
	download_github_asset \
		"https://github.com/${panel_repository}/releases/latest/download/panel-manifest.json" \
		"$work_dir/panel-manifest.json"
else
	release_url="https://github.com/${panel_repository}/releases/download/${panel_tag}"
	download_github_asset "$release_url/panel-manifest.json" "$work_dir/panel-manifest.json"
fi

manifest_values="$(python3 - "$work_dir/panel-manifest.json" "$work_dir/manifest.canonical.json" <<'PY'
import json
import pathlib
import re
import sys

source = pathlib.Path(sys.argv[1])
target = pathlib.Path(sys.argv[2])
data = json.loads(source.read_text(encoding="utf-8"))
allowed = {"version", "sha256", "asset", "min_cli_version"}
required = {"version", "sha256", "asset"}
unknown = set(data) - allowed
missing = required - set(data)
if unknown:
    raise SystemExit(f"unsupported manifest fields: {sorted(unknown)}")
if missing:
    raise SystemExit(f"missing manifest fields: {sorted(missing)}")
canonical = {
    "version": str(data["version"]).strip(),
    "sha256": str(data["sha256"]).strip().lower(),
    "asset": str(data["asset"]).strip(),
}
if re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", canonical["version"]) is None:
    raise SystemExit(f"invalid panel version: {canonical['version']}")
if re.fullmatch(r"[0-9a-f]{64}", canonical["sha256"]) is None:
    raise SystemExit("invalid panel SHA-256")
minimum = str(data.get("min_cli_version", "")).strip()
if minimum:
    if re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", minimum) is None:
        raise SystemExit(f"invalid min_cli_version: {minimum}")
    canonical["min_cli_version"] = minimum
target.write_text(json.dumps(canonical, indent=2) + "\n", encoding="utf-8")
print("\t".join((canonical["version"], canonical["sha256"], canonical["asset"])))
PY
)"
IFS=$'\t' read -r manifest_version manifest_sha256 manifest_asset <<< "$manifest_values"

if [[ "$resolve_latest" == true ]]; then
	panel_tag="$manifest_version"
	expected_sha256="$manifest_sha256"
else
	if [[ "$manifest_version" != "$panel_tag" ]]; then
		printf 'manifest version %s does not match requested tag %s\n' "$manifest_version" "$panel_tag" >&2
		exit 1
	fi
	if [[ "$manifest_sha256" != "$expected_sha256" ]]; then
		printf 'manifest SHA-256 %s does not match expected SHA-256 %s\n' "$manifest_sha256" "$expected_sha256" >&2
		exit 1
	fi
fi
if [[ "$manifest_asset" != "management.html" ]]; then
	printf 'unsupported panel asset %s\n' "$manifest_asset" >&2
	exit 1
fi

if [[ -z "$source_dir" ]]; then
	release_url="https://github.com/${panel_repository}/releases/download/${panel_tag}"
	download_github_asset "$release_url/management.html" "$work_dir/management.html"
fi

if command -v sha256sum >/dev/null 2>&1; then
	actual_sha256="$(sha256sum "$work_dir/management.html" | awk '{print $1}')"
else
	actual_sha256="$(shasum -a 256 "$work_dir/management.html" | awk '{print $1}')"
fi
if [[ "$actual_sha256" != "$expected_sha256" ]]; then
	printf 'downloaded panel SHA-256 %s does not match expected SHA-256 %s\n' "$actual_sha256" "$expected_sha256" >&2
	exit 1
fi

gzip -9 -n -c "$work_dir/management.html" > "$work_dir/management.html.gz"
mkdir -p "$assets_dir"
mv "$work_dir/management.html.gz" "$assets_dir/management.html.gz"
mv "$work_dir/manifest.canonical.json" "$assets_dir/panel-manifest.json"

printf 'Pinned management panel %s (%s)\n' "$panel_tag" "$expected_sha256"
