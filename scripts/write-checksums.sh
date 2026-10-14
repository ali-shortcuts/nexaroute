#!/usr/bin/env bash
# Writes dist/SHA256SUMS covering every distributable release asset
# (plus the SBOM when present) in a deterministic order, then verifies
# the manifest. The manifest never lists itself or the signature
# bundle. Used by build-release.sh and again by the release workflow
# after SBOM generation so the published manifest is complete.
set -euo pipefail
cd "$(dirname "$0")/.."
dist="${1:-dist}"
[[ -d "$dist" ]] || { echo "write-checksums: no such directory: $dist" >&2; exit 1; }
assets=(
  nexaroute-linux-amd64
  nexaroute-linux-arm64
  nexaroute-videogen-linux-amd64
  nexaroute-videogen-linux-arm64
  install.sh
  video.example.json
  VIDEO_GATEWAY.md
  RELEASE_NOTES.md
)
for f in "${assets[@]}"; do
  [[ -s "$dist/$f" ]] || { echo "write-checksums: missing release asset: $dist/$f" >&2; exit 1; }
done
# At most one SBOM may be present; when present it joins the manifest.
sboms=( "$dist"/*-sbom.spdx.json )
if [[ -e "${sboms[0]}" ]]; then
  [[ ${#sboms[@]} -eq 1 ]] || { echo "write-checksums: multiple SBOM files in $dist" >&2; exit 1; }
  assets+=( "$(basename "${sboms[0]}")" )
fi
(
  cd "$dist"
  rm -f SHA256SUMS
  sha256sum "${assets[@]}" > SHA256SUMS
  sha256sum -c SHA256SUMS >/dev/null
)
echo "write-checksums: SHA256SUMS covers ${#assets[@]} assets in $dist"
