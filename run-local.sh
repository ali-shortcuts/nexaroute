#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
CFG="${NEXAROUTE_CONFIG:-$ROOT/config.local.json}"
if [[ ! -f "$CFG" ]]; then
  cp "$ROOT/configs/config.example.json" "$CFG"
  chmod 600 "$CFG"
  echo "Created $CFG"
fi
case "$(uname -m)" in
  x86_64|amd64) BIN="$ROOT/bin/nexaroute-linux-amd64" ;;
  aarch64|arm64) BIN="$ROOT/bin/nexaroute-linux-arm64" ;;
  *) BIN="" ;;
esac
if [[ -n "$BIN" && -x "$BIN" ]]; then
  exec "$BIN" -config "$CFG"
fi
cd "$ROOT"
exec go run ./cmd/gateway -config "$CFG"
