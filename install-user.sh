#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) BUNDLED="$ROOT/bin/nexaroute-linux-amd64" ;;
  aarch64|arm64) BUNDLED="$ROOT/bin/nexaroute-linux-arm64" ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

mkdir -p "$HOME/.local/bin" "$HOME/.config/nexaroute"
chmod 700 "$HOME/.config/nexaroute"

TMP_BIN=""
if [[ -x "$BUNDLED" ]]; then
  SRC_BIN="$BUNDLED"
else
  command -v go >/dev/null 2>&1 || {
    echo "No bundled NexaRoute binary was found and Go is not installed." >&2
    echo "Install Go 1.23+ or use a release package containing a Linux binary." >&2
    exit 1
  }
  TMP_BIN="$(mktemp)"
  trap 'rm -f "$TMP_BIN"' EXIT
  echo "Building NexaRoute v0.3 from source..."
  (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -o "$TMP_BIN" ./cmd/gateway)
  SRC_BIN="$TMP_BIN"
fi

install -m 0755 "$SRC_BIN" "$HOME/.local/bin/nexaroute"

CFG="$HOME/.config/nexaroute/config.json"
if [[ ! -f "$CFG" ]]; then
  install -m 0600 "$ROOT/configs/config.example.json" "$CFG"
  echo "Created config: $CFG"
else
  chmod 600 "$CFG"
  echo "Kept existing config: $CFG"
fi
rm -f "$CFG.bak"

cat <<MSG
Installed: $HOME/.local/bin/nexaroute
Config:    $CFG

Run:
  $HOME/.local/bin/nexaroute -config "$CFG"

Dashboard:
  http://127.0.0.1:8080/

The example providers are disabled until you configure and enable them in the Web UI.
MSG
