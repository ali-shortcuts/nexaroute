#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CFG="${NEXAROUTE_CONFIG:-$ROOT/config.local.json}"
if [[ ! -f "$CFG" ]]; then
  cp "$ROOT/configs/config.example.json" "$CFG"
  chmod 600 "$CFG"
  echo "Created $CFG"
fi
cd "$ROOT"
exec go run ./cmd/gateway -config "$CFG"
