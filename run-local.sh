#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
CFG="${NEXAROUTE_CONFIG:-$ROOT/config.local.json}"
if [[ ! -f "$CFG" ]]; then
  cp "$ROOT/configs/config.example.json" "$CFG"
  chmod 600 "$CFG"
  echo "Created $CFG"
fi
case "$(uname -s)" in
  Linux) OS_ID=linux ;;
  Darwin) OS_ID=darwin ;;
  *) OS_ID="" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH_ID=amd64 ;;
  aarch64|arm64) ARCH_ID=arm64 ;;
  *) ARCH_ID="" ;;
esac
BIN=""
if [[ -n "$OS_ID" && -n "$ARCH_ID" ]]; then
  BIN="$ROOT/bin/nexaroute-${OS_ID}-${ARCH_ID}"
fi
if [[ -n "$BIN" && -x "$BIN" ]]; then
  exec "$BIN" -config "$CFG"
fi
cd "$ROOT"
exec go run ./cmd/gateway -config "$CFG"
