#!/usr/bin/env bash
# Install the latest published NexaRoute Linux release without a source checkout.
set -Eeuo pipefail

REPO="ali-shortcuts/nexaroute"
API="https://api.github.com/repos/${REPO}/releases/latest"
ASSET_BASE="https://github.com/${REPO}/releases/download"

fail() { printf 'nexaroute installer: %s\n' "$*" >&2; exit 1; }
for tool in curl sha256sum install; do command -v "$tool" >/dev/null 2>&1 || fail "required command not found: $tool"; done

case "$(uname -s)" in Linux) ;; *) fail "only Linux is currently supported" ;; esac
case "$(uname -m)" in
  x86_64|amd64) asset="nexaroute-linux-amd64" ;;
  aarch64|arm64) asset="nexaroute-linux-arm64" ;;
  *) fail "unsupported architecture: $(uname -m) (supported: x86_64, aarch64)" ;;
esac

if [[ -n "${NEXAROUTE_INSTALL_DIR:-}" ]]; then
  bindir="$NEXAROUTE_INSTALL_DIR"
elif [[ "${EUID:-$(id -u)}" -eq 0 ]]; then
  bindir="/usr/local/bin"
else
  bindir="${HOME:?HOME must be set}/.local/bin"
fi

tmp="$(mktemp -d)" || fail "could not create temporary directory"
trap 'rm -rf "$tmp"' EXIT

if [[ -n "${NEXAROUTE_LOCAL_RELEASE_DIR:-}" ]]; then
  tag="${NEXAROUTE_LOCAL_RELEASE_TAG:-v0.6.0}"
  [[ -d "$NEXAROUTE_LOCAL_RELEASE_DIR" ]] || fail "local release directory does not exist: $NEXAROUTE_LOCAL_RELEASE_DIR"
  [[ -f "$NEXAROUTE_LOCAL_RELEASE_DIR/$asset" ]] || fail "local release asset not found: $NEXAROUTE_LOCAL_RELEASE_DIR/$asset"
  [[ -f "$NEXAROUTE_LOCAL_RELEASE_DIR/SHA256SUMS" ]] || fail "local release SHA256SUMS not found: $NEXAROUTE_LOCAL_RELEASE_DIR/SHA256SUMS"
  cp "$NEXAROUTE_LOCAL_RELEASE_DIR/$asset" "$tmp/$asset"
  cp "$NEXAROUTE_LOCAL_RELEASE_DIR/SHA256SUMS" "$tmp/SHA256SUMS"
else
  command -v python3 >/dev/null 2>&1 || fail "python3 is required to read release metadata"
  curl --fail --silent --show-error --location --retry 3 "$API" -o "$tmp/release.json" || fail "could not retrieve latest release metadata from GitHub"
  readarray -t release_info < <(python3 - "$tmp/release.json" <<'PY'
import json, sys
try:
    release = json.load(open(sys.argv[1], encoding='utf-8'))
    tag = release['tag_name']
    assets = {a['name'] for a in release['assets']}
    if not tag or 'SHA256SUMS' not in assets:
        raise ValueError('release must include a tag and SHA256SUMS')
    print(tag)
    print(' '.join(sorted(assets)))
except Exception as e:
    print(f'invalid GitHub release metadata: {e}', file=sys.stderr)
    sys.exit(1)
PY
) || fail "invalid release metadata"
  [[ ${#release_info[@]} -ge 2 ]] || fail "could not parse release metadata"
  tag="${release_info[0]}"
  assets=" ${release_info[1]} "
  [[ "$assets" == *" $asset "* ]] || fail "release $tag has no asset for $(uname -m): $asset"
  [[ "$assets" == *" SHA256SUMS "* ]] || fail "release $tag has no SHA256SUMS"

  curl --fail --silent --show-error --location --retry 3 "$ASSET_BASE/$tag/$asset" -o "$tmp/$asset" || fail "failed to download $asset"
  curl --fail --silent --show-error --location --retry 3 "$ASSET_BASE/$tag/SHA256SUMS" -o "$tmp/SHA256SUMS" || fail "failed to download release checksums"
fi
(cd "$tmp" && grep -E "^[[:xdigit:]]{64}[[:space:]]+\*?${asset}$" SHA256SUMS > checksum.selected && [[ -s checksum.selected ]] && sha256sum --check checksum.selected) || fail "checksum verification failed for $asset"
[[ -s "$tmp/$asset" ]] || fail "downloaded binary is empty"

mkdir -p "$bindir"
install -m 0755 "$tmp/$asset" "$bindir/nexaroute.new"
mv -f "$bindir/nexaroute.new" "$bindir/nexaroute"
config_dir="${XDG_CONFIG_HOME:-${HOME:?HOME must be set}/.config}/nexaroute"
mkdir -p "$config_dir"
chmod 700 "$config_dir"
printf 'Installed NexaRoute %s to %s/nexaroute\n' "$tag" "$bindir"
printf 'Configuration directory: %s (existing configuration was not changed)\n' "$config_dir"
case ":$PATH:" in *":$bindir:"*) ;; *) printf 'Add this to PATH if needed: export PATH="%s:$PATH"\n' "$bindir" ;; esac
printf 'Run: nexaroute\n'
