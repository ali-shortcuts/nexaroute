#!/usr/bin/env bash
#
# NexaRoute one-command installer (Linux).
#
#   curl -fsSL https://raw.githubusercontent.com/ali-shortcuts/nexaroute/main/scripts/install.sh | bash
#
# Or from a checkout: ./scripts/install.sh [--release v0.3] [--from-source] [--service]
#
# Prefers the GitHub release tarball; when no release exists yet (or with
# --from-source) it clones the repository and builds with the local Go
# toolchain instead. Never overwrites an existing config file.
set -euo pipefail

REPO="${NEXAROUTE_REPO:-ali-shortcuts/nexaroute}"
GIT_URL="${NEXAROUTE_GIT_URL:-https://github.com/${REPO}.git}"
RAW_BASE="https://raw.githubusercontent.com/${REPO}"
VERSION="${NEXAROUTE_VERSION:-latest}"
BIN_DIR="${NEXAROUTE_BIN_DIR:-$HOME/.local/bin}"
CONFIG_DIR="${NEXAROUTE_CONFIG_DIR:-$HOME/.config/nexaroute}"
TARBALL_OVERRIDE="${NEXAROUTE_TARBALL:-}"
FROM_SOURCE=0
WITH_SERVICE=0

usage() {
  cat <<'EOF'
NexaRoute one-command installer (Linux).

Usage:
  install.sh [--release TAG|latest] [--from-source] [--service]
             [--bin-dir DIR] [--config-dir DIR] [--help]

  --release TAG   install a specific GitHub release (default: latest)
  --from-source   clone and build with the local Go toolchain instead of
                  downloading a release tarball (needs git + Go 1.23+)
  --service       also install and start the user-level systemd service
  --bin-dir DIR   binary destination (default: ~/.local/bin)
  --config-dir DIR  config destination (default: ~/.config/nexaroute)
  --help          show this help

Environment overrides (mainly for tests):
  NEXAROUTE_REPO, NEXAROUTE_GIT_URL, NEXAROUTE_VERSION,
  NEXAROUTE_BIN_DIR, NEXAROUTE_CONFIG_DIR, NEXAROUTE_TARBALL
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --release) VERSION="${2:-}"; shift 2 ;;
    --from-source) FROM_SOURCE=1; shift ;;
    --service) WITH_SERVICE=1; shift ;;
    --bin-dir) BIN_DIR="${2:-}"; shift 2 ;;
    --config-dir) CONFIG_DIR="${2:-}"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) echo "Unknown option: $1 (try --help)" >&2; exit 1 ;;
  esac
done

[[ -n "$VERSION" && -n "$BIN_DIR" && -n "$CONFIG_DIR" ]] || { echo "Empty install option." >&2; exit 1; }

case "$(uname -s)" in
  Linux) ;;
  *) echo "NexaRoute install.sh supports Linux only (got $(uname -s))." >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $(uname -m) (need x86_64 or aarch64)." >&2; exit 1 ;;
esac

TMP="$(mktemp -d)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "Missing required command: $1" >&2; exit 1; }
}

SRC_BIN=""
SRC_CONFIG_EXAMPLE=""
SRC_SERVICE=""

download_release() {
  need curl
  local api_url asset_url
  if [[ "$VERSION" == "latest" ]]; then
    api_url="https://api.github.com/repos/${REPO}/releases/latest"
  else
    api_url="https://api.github.com/repos/${REPO}/releases/tags/${VERSION}"
  fi
  echo "Looking up NexaRoute release '${VERSION}'..." >&2
  local code
  code=$(curl -sS -o "$TMP/release.json" -w '%{http_code}' -H 'Accept: application/vnd.github+json' "$api_url" || echo "000")
  if [[ "$code" == "404" ]]; then
    return 2 # no such release: caller may fall back to source
  fi
  if [[ "$code" != "200" ]]; then
    echo "Could not reach the GitHub releases API (HTTP $code)." >&2
    echo "Check your network, or retry with --from-source." >&2
    exit 1
  fi
  asset_url=$(grep -o '"browser_download_url": *"[^"]*linux\.tar\.gz"' "$TMP/release.json" | head -n 1 | sed 's/.*": *"//; s/"$//' || true)
  if [[ -z "$asset_url" ]]; then
    echo "Release '${VERSION}' has no Linux tarball asset." >&2
    return 2
  fi
  echo "Downloading $(basename "$asset_url")..." >&2
  curl -fL -o "$TMP/nexaroute.tar.gz" "$asset_url" >&2
  echo "$TMP/nexaroute.tar.gz"
}

use_tarball() {
  local tarball="$1"
  tar -tzf "$tarball" >/dev/null
  local top
  top=$(tar -tzf "$tarball" 2>/dev/null | head -n 1 | cut -d/ -f1 || true)
  [[ -n "$top" ]] || { echo "Cannot read tarball layout." >&2; exit 1; }
  tar -xzf "$tarball" -C "$TMP"
  SRC_BIN="$TMP/$top/bin/nexaroute-linux-$ARCH"
  SRC_CONFIG_EXAMPLE="$TMP/$top/configs/config.example.json"
  SRC_SERVICE="$TMP/$top/deploy/systemd/nexaroute.service"
  [[ -x "$SRC_BIN" ]] || { echo "Tarball has no executable for $ARCH ($SRC_BIN)." >&2; exit 1; }
  [[ -f "$SRC_CONFIG_EXAMPLE" ]] || { echo "Tarball is missing configs/config.example.json." >&2; exit 1; }
}

go_minor() {
  go version 2>/dev/null | grep -oE 'go[0-9]+\.[0-9]+' | head -n 1 | cut -d. -f2
}

build_from_source() {
  need git
  command -v go >/dev/null 2>&1 || {
    echo "Building from source needs Go 1.23+." >&2
    echo "Install it from https://go.dev/dl/ (or: sudo apt install golang-go, then check 'go version'), and retry." >&2
    exit 1
  }
  local minor
  minor=$(go_minor)
  if [[ ! "$minor" =~ ^[0-9]+$ ]] || [[ "$minor" -lt 23 ]]; then
    echo "Go 1.23+ is required (found: $(go version 2>/dev/null || echo none))." >&2
    exit 1
  fi
  echo "No usable release tarball; building NexaRoute from source..."
  local -a clone_args=(--depth 1)
  if [[ "$VERSION" != "latest" ]]; then
    clone_args+=(--branch "$VERSION")
  fi
  git clone "${clone_args[@]}" "$GIT_URL" "$TMP/src"
  (cd "$TMP/src" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$TMP/nexaroute-src" ./cmd/gateway)
  SRC_BIN="$TMP/nexaroute-src"
  SRC_CONFIG_EXAMPLE="$TMP/src/configs/config.example.json"
  SRC_SERVICE="$TMP/src/deploy/systemd/nexaroute.service"
}

if [[ -n "$TARBALL_OVERRIDE" ]]; then
  echo "Using local tarball: $TARBALL_OVERRIDE"
  use_tarball "$TARBALL_OVERRIDE"
elif [[ "$FROM_SOURCE" == "1" ]]; then
  build_from_source
else
  tarball=""
  if tarball=$(download_release); then
    use_tarball "$tarball"
  else
    rc=$?
    if [[ "$rc" == "2" ]]; then
      build_from_source
    else
      exit "$rc"
    fi
  fi
fi

mkdir -p "$BIN_DIR" "$CONFIG_DIR"
chmod 700 "$CONFIG_DIR"
install -m 0755 "$SRC_BIN" "$BIN_DIR/nexaroute"

CFG="$CONFIG_DIR/config.json"
if [[ ! -f "$CFG" ]]; then
  install -m 0600 "$SRC_CONFIG_EXAMPLE" "$CFG"
  echo "Created config: $CFG"
else
  chmod 600 "$CFG"
  echo "Kept existing config: $CFG"
fi
rm -f "$CFG.bak"

"$BIN_DIR/nexaroute" -version

if [[ "$WITH_SERVICE" == "1" ]]; then
  if [[ -f "$SRC_SERVICE" ]]; then
    mkdir -p "$HOME/.config/systemd/user"
    install -m 0644 "$SRC_SERVICE" "$HOME/.config/systemd/user/nexaroute.service"
    if command -v systemctl >/dev/null 2>&1 && systemctl --user daemon-reload 2>/dev/null; then
      systemctl --user enable --now nexaroute
      echo "Enabled and started the user service: nexaroute"
    else
      echo "WARN: systemctl --user is unavailable; service file installed but not started." >&2
      echo "Start manually: systemctl --user enable --now nexaroute" >&2
    fi
  else
    echo "WARN: no systemd unit found in this package; skipping --service." >&2
  fi
fi

cat <<MSG

Installed: $BIN_DIR/nexaroute
Config:    $CFG

Run:
  $BIN_DIR/nexaroute -config "$CFG"

Dashboard:
  http://127.0.0.1:8080/

The example providers are disabled until you configure and enable them in the Web UI.
MSG

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "Hint: $BIN_DIR is not on your PATH. Add: export PATH=\"\$PATH:$BIN_DIR\"" ;;
esac
