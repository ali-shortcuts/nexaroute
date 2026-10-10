#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
version="${1:-v0.17.0}"
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
cp "docs/releases/${version}.md" dist/RELEASE_NOTES.md 2>/dev/null || cp docs/releases/v0.17.0-rc.1.md dist/RELEASE_NOTES.md
(cd dist && sha256sum nexaroute-linux-amd64 nexaroute-linux-arm64 nexaroute-videogen-linux-amd64 nexaroute-videogen-linux-arm64 install.sh video.example.json VIDEO_GATEWAY.md RELEASE_NOTES.md > SHA256SUMS && sha256sum -c SHA256SUMS)
printf 'Prepared %s in dist/ with gateway, videogen, installer, config and docs. Nothing published.\n' "$version"
