#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
case "$(uname -s)" in
  Linux) TARGET_OS=linux ;;
  Darwin) TARGET_OS=darwin ;;
  *) echo 'Use Linux, macOS, or WSL for this installer.' >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) TARGET_ARCH=amd64 ;;
  aarch64|arm64) TARGET_ARCH=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
BUNDLED="$ROOT/bin/nexaroute-$TARGET_OS-$TARGET_ARCH"
mkdir -p "$HOME/.local/bin" "$HOME/.config/nexaroute"
chmod 700 "$HOME/.config/nexaroute"
# Stage in the destination directory, then rename: upgrading a running binary
# must not fail with ETXTBSY or leave a partially copied executable.
STAGED="$(mktemp "$HOME/.local/bin/.nexaroute.XXXXXX")"
trap 'rm -f "$STAGED"' EXIT
if [[ -x "$BUNDLED" ]]; then
  install -m 0755 "$BUNDLED" "$STAGED"
else
  command -v go >/dev/null 2>&1 || {
    echo 'No bundled binary found. Install Go 1.23+ or use a release package.' >&2
    exit 1
  }
  echo 'Building NexaRoute from source...'
  (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -o "$STAGED" ./cmd/gateway)
  chmod 755 "$STAGED"
fi
"$STAGED" -version
CFG="$HOME/.config/nexaroute/config.json"
if [[ ! -f "$CFG" ]]; then
  install -m 0600 "$ROOT/configs/config.example.json" "$CFG"
  echo "Created config: $CFG"
else
  chmod 600 "$CFG"
  echo "Kept existing config: $CFG"
fi
mv -f "$STAGED" "$HOME/.local/bin/nexaroute"
cat <<MSG
Installed: $HOME/.local/bin/nexaroute
Config:    $CFG

Run:
  "$HOME/.local/bin/nexaroute" -config "$CFG"

Dashboard: http://127.0.0.1:8080/
Providers start disabled; configure and enable them in the dashboard.
MSG
