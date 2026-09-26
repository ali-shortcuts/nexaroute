#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Uses local fixtures at the download boundary: no GitHub Release is published
# or required, and neither /usr/local/bin nor the caller's config is modified.
# The Python suite installs real release binaries and exercises the full desktop
# lifecycle; the shell suite independently checks clean install/upgrade failure
# atomicity with minimal executable fixtures.
python3 scripts/test-install.py
./scripts/test-installer-e2e.sh
