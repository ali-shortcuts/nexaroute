#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Uses local fixtures at the download boundary: no GitHub Release is published
# or required, and neither /usr/local/bin nor the caller's config is modified.
python3 scripts/test-install.py
