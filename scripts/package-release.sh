#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="v$(sed -n 's/^const Version = "\([^"]*\)"/\1/p' internal/buildinfo/version.go)"
[[ -n "$VERSION" && "$VERSION" != v ]] || { echo 'Missing version.' >&2; exit 1; }
if [[ -n "${GITHUB_REF_NAME:-}" && "${GITHUB_REF_TYPE:-}" == tag && "$GITHUB_REF_NAME" != "$VERSION" ]]; then
  echo "Tag $GITHUB_REF_NAME does not match binary $VERSION" >&2; exit 1
fi
mkdir -p dist
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FILES=()
for target in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  target_os="${target%-*}"; target_arch="${target#*-}"
  pkg="$WORK/$target/nexaroute"
  mkdir -p "$pkg/bin" "$pkg/configs" "$pkg/scripts" "$pkg/deploy/systemd"
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -trimpath -ldflags='-s -w' -o "$pkg/bin/nexaroute-$target" ./cmd/gateway
  install -m 0755 install-user.sh "$pkg/"
  install -m 0755 scripts/install-systemd-user.sh "$pkg/scripts/"
  install -m 0644 deploy/systemd/nexaroute.service "$pkg/deploy/systemd/"
  install -m 0644 configs/config.example.json "$pkg/configs/"
  install -m 0644 README.md LICENSE SECURITY.md "$pkg/"
  asset="nexaroute-$VERSION-$target.tar.gz"
  tar -C "$WORK/$target" -czf "dist/$asset" nexaroute
  FILES+=("$asset")
done
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/nexaroute-windows-amd64.exe ./cmd/gateway
FILES+=(nexaroute-windows-amd64.exe)
(cd dist && sha256sum "${FILES[@]}" > SHA256SUMS)
echo "Packaged $VERSION"
