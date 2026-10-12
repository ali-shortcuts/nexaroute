#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Uses local fixtures at the download boundary: no GitHub Release is published
# or required, and neither /usr/local/bin nor the caller's config is modified.
# The Python suite installs real release binaries and exercises the full desktop
# lifecycle; the shell suite independently checks clean install/upgrade failure
# atomicity with minimal executable fixtures.
if [[ ! -x dist/nexaroute-linux-amd64 || ! -x dist/nexaroute-videogen-linux-amd64 ]]; then
  ./scripts/build-release.sh v0.20.1
fi
python3 scripts/test-install.py
./scripts/test-installer-e2e.sh
