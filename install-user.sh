#!/usr/bin/env bash
# Source-checkout convenience entry point. The canonical installer implementation
# lives in scripts/install.sh and is the same file shipped in GitHub Releases.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
exec bash "$ROOT/scripts/install.sh" "$@"
