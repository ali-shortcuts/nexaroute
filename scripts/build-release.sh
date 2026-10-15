#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
version="${1:-v0.22.0}"
[[ "$version" =~ ^v[0-9][a-zA-Z0-9._-]*$ ]] || { echo 'Usage: scripts/build-release.sh vVERSION' >&2; exit 1; }
rm -rf dist
mkdir -p dist
for arch in amd64 arm64; do
  common_ldflags="-s -w -X main.version=${version#v}"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
    -ldflags="$common_ldflags -X github.com/ali-shortcuts/nexaroute/internal/httpapi.gatewayVersion=${version#v}" \
    -o "dist/nexaroute-linux-$arch" ./cmd/gateway
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
    -ldflags="$common_ldflags" \
    -o "dist/nexaroute-videogen-linux-$arch" ./cmd/videogen
done
install -m 0755 scripts/install.sh dist/install.sh
cp configs/video.example.json dist/video.example.json
cp docs/VIDEO_GATEWAY.md dist/VIDEO_GATEWAY.md
	cp "docs/releases/${version}.md" dist/RELEASE_NOTES.md
# The manifest is written by the shared writer so the release workflow
# regenerates the identical manifest (now including the SBOM) later.
./scripts/write-checksums.sh dist
printf 'Prepared %s in dist/ with gateway, videogen, installer, config and docs. Nothing published.\n' "$version"
