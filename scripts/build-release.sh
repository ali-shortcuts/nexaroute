#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
version="${1:-v0.6.0}"
[[ "$version" =~ ^v[0-9][a-zA-Z0-9._-]*$ ]] || { echo 'Usage: scripts/build-release.sh vVERSION' >&2; exit 1; }
mkdir -p dist
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.version=${version#v} -X github.com/ali-shortcuts/nexaroute/internal/httpapi.gatewayVersion=${version#v}" \
    -o "dist/nexaroute-linux-$arch" ./cmd/gateway
done
install -m 0755 install-user.sh dist/install.sh
# A single manifest, with the exact names used by the installer.
(cd dist && sha256sum nexaroute-linux-amd64 nexaroute-linux-arm64 install.sh > SHA256SUMS && sha256sum -c SHA256SUMS)
printf 'Prepared %s in dist/. Nothing published.\n' "$version"
