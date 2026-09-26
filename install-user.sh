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

verify_sha() {
  local bin="$1"
  local sums="$ROOT/SHA256SUMS"
  if [[ ! -f "$sums" ]]; then
    sums="$ROOT/bin/SHA256SUMS"
  fi
  if [[ ! -f "$sums" ]]; then
    return 0
  fi
  command -v sha256sum >/dev/null 2>&1 || return 0
  local base
  base="$(basename "$bin")"
  local expected
  expected="$(awk -v f="$base" '$2==f {print $1}' "$sums" | head -n1)"
  if [[ -z "$expected" ]]; then
    return 0
  fi
  local got
  got="$(sha256sum "$bin" | awk '{print $1}')"
  if [[ "$got" != "$expected" ]]; then
    echo "SHA256 mismatch for $base" >&2
    echo "  expected $expected" >&2
    echo "  got      $got" >&2
    exit 1
  fi
}

TMP_BIN=""
if [[ -x "$BUNDLED" ]]; then
  verify_sha "$BUNDLED"
  SRC_BIN="$BUNDLED"
else
  command -v go >/dev/null 2>&1 || {
    echo "No bundled NexaRoute binary was found and Go is not installed." >&2
    echo "Install a release package containing a Linux binary, or install Go 1.23+ to build from source." >&2
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

cat <<MSG
Installed: $HOME/.local/bin/nexaroute
Config:    $CFG

Ensure ~/.local/bin is on your PATH, then run:

  nexaroute

The gateway listens on the configured address (default http://127.0.0.1:8080/),
opens the Web UI when a browser is available, and prints the URL otherwise.
Existing configuration is preserved on upgrade.
MSG
