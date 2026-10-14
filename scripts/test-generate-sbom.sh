#!/usr/bin/env bash
# Regression tests for the Syft trust chain in generate-sbom.sh:
#   * a corrupt cached TARBALL with no way to re-download must fail
#     closed (no SBOM produced);
#   * a tampered cached EXECUTABLE must never run — the binary is
#     always extracted fresh from the SHA-256-verified tarball.
# The executable case needs the genuine tarball: it is copied from
# the default cache when present, otherwise downloaded once; if
# neither is possible the case is skipped with a note (the corrupt-
# tarball case always runs and needs no network).
set -euo pipefail
cd "$(dirname "$0")/.."

SYFT_VERSION="1.54.1"
TARBALL="syft_${SYFT_VERSION}_linux_amd64.tar.gz"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

dist="$work/dist"
mkdir -p "$dist"
printf 'fixture asset\n' > "$dist/nexaroute-linux-amd64"

# Case 1: corrupt cached tarball + downloads disabled -> fail closed.
cache1="$work/cache1"
mkdir -p "$cache1/$SYFT_VERSION"
printf 'tampered' > "$cache1/$SYFT_VERSION/$TARBALL"
printf '#!/bin/sh\necho should-never-run\n' > "$cache1/$SYFT_VERSION/syft"
chmod +x "$cache1/$SYFT_VERSION/syft"
if SYFT_CACHE_DIR="$cache1" SYFT_DOWNLOAD_BASE="file:///nonexistent-syft-test" \
    ./scripts/generate-sbom.sh v9.9.9 "$dist" >/dev/null 2>&1; then
  echo "test-generate-sbom: FAIL — corrupt tarball with downloads disabled was accepted" >&2
  exit 1
fi
if [[ -e "$dist/nexaroute-v9.9.9-sbom.spdx.json" ]]; then
  echo "test-generate-sbom: FAIL — an SBOM was produced despite the corrupt tarball" >&2
  exit 1
fi
echo "test-generate-sbom: corrupt cached tarball fails closed"

# Case 2: tampered cached executable + genuine tarball -> the genuine
# Syft (extracted fresh from the verified tarball) produces the SBOM
# and the planted executable never runs.
src_tarball="${SYFT_CACHE_DIR:-$HOME/.cache/nexaroute-syft}/$SYFT_VERSION/$TARBALL"
if [[ ! -f "$src_tarball" ]]; then
  mkdir -p "$work/dl"
  if curl -fsSL "https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}/${TARBALL}" -o "$work/dl/$TARBALL" 2>/dev/null; then
    src_tarball="$work/dl/$TARBALL"
  else
    src_tarball=""
  fi
fi
if [[ -z "$src_tarball" || ! -f "$src_tarball" ]]; then
  echo "test-generate-sbom: SKIP — genuine Syft tarball unavailable (no cache, no network)"
else
  cache2="$work/cache2"
  mkdir -p "$cache2/$SYFT_VERSION"
  cp "$src_tarball" "$cache2/$SYFT_VERSION/$TARBALL"
  marker="$work/tampered-executable-ran"
  printf '#!/bin/sh\ntouch "%s"\necho garbage\n' "$marker" > "$cache2/$SYFT_VERSION/syft"
  chmod +x "$cache2/$SYFT_VERSION/syft"
  SYFT_CACHE_DIR="$cache2" ./scripts/generate-sbom.sh v9.9.9 "$dist" >/dev/null
  if [[ -e "$marker" ]]; then
    echo "test-generate-sbom: FAIL — the tampered cached executable was executed" >&2
    exit 1
  fi
  python3 - "$dist/nexaroute-v9.9.9-sbom.spdx.json" <<'PYEOF'
import json, sys
doc = json.load(open(sys.argv[1]))
creators = " ".join(doc.get("creationInfo", {}).get("creators", []))
assert "Tool: syft-" in creators, f"SBOM was not produced by genuine Syft: {creators!r}"
print("test-generate-sbom: tampered cached executable ignored; genuine Syft produced the SBOM")
PYEOF
fi
echo "test-generate-sbom: PASS"
