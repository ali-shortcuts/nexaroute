#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS" in
  Linux) OS_ID=linux ;;
  Darwin) OS_ID=darwin ;;
  *) echo "Unsupported operating system: $OS (supported: Linux, macOS)" >&2; exit 1 ;;
esac
case "$ARCH" in
  x86_64|amd64) ARCH_ID=amd64 ;;
  aarch64|arm64) ARCH_ID=arm64 ;;
  *) echo "Unsupported architecture: $ARCH (supported: amd64, arm64)" >&2; exit 1 ;;
esac
BUNDLED="$ROOT/bin/nexaroute-${OS_ID}-${ARCH_ID}"

mkdir -p "$HOME/.local/bin" "$HOME/.config/nexaroute"
chmod 700 "$HOME/.config/nexaroute"

TMP_BIN=""
if [[ -x "$BUNDLED" ]]; then
  SRC_BIN="$BUNDLED"
else
  command -v go >/dev/null 2>&1 || {
    echo "No bundled NexaRoute binary was found for ${OS_ID}/${ARCH_ID}, and Go is not installed." >&2
    echo "Download the universal release archive or install Go 1.23+ to build from source." >&2
    exit 1
  }
  TMP_BIN="$(mktemp)"
  trap 'rm -f "$TMP_BIN"' EXIT
  echo "Building NexaRoute from source..."
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

VERSION="$("$HOME/.local/bin/nexaroute" -version)"
cat <<MSG
Installed: $HOME/.local/bin/nexaroute ($VERSION)
Config:    $CFG

Run:
  $HOME/.local/bin/nexaroute -config "$CFG"

Dashboard:
  http://127.0.0.1:8080/

The example providers are disabled until you configure and enable them in the Web UI.
MSG

case ":${PATH}:" in
  *":$HOME/.local/bin:"*) ;;
  *) echo "Note: add $HOME/.local/bin to PATH to run 'nexaroute' from any terminal." ;;
esac
