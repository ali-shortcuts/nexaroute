#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
case "$(uname -m)" in
  x86_64|amd64) BIN="$ROOT/bin/ulg-linux-amd64" ;;
  aarch64|arm64) BIN="$ROOT/bin/ulg-linux-arm64" ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
[[ -x "$BIN" ]] || { echo "Missing prebuilt binary: $BIN" >&2; exit 1; }
mkdir -p "$HOME/.local/bin" "$HOME/.config/universal-llm-gateway"
install -m 0755 "$BIN" "$HOME/.local/bin/ulg"
CFG="$HOME/.config/universal-llm-gateway/config.json"
if [[ ! -f "$CFG" ]]; then
  install -m 0600 "$ROOT/configs/config.example.json" "$CFG"
  echo "Created config: $CFG"
else
  echo "Kept existing config: $CFG"
fi
cat <<MSG
Installed: $HOME/.local/bin/ulg

Run:
  $HOME/.local/bin/ulg -config "$CFG"

Dashboard:
  http://127.0.0.1:8080/

The example providers are disabled until you configure and enable them in the Web UI.
MSG
