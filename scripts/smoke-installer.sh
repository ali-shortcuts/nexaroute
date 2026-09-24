#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

for dependency in curl tar awk; do
  command -v "$dependency" >/dev/null 2>&1 || {
    echo "Missing required command: $dependency" >&2
    exit 1
  }
done

case "$(uname -s)" in
  Linux) OS_ID=linux ;;
  Darwin) OS_ID=darwin ;;
  *) echo "Unsupported smoke-test OS: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH_ID=amd64 ;;
  aarch64|arm64) ARCH_ID=arm64 ;;
  *) echo "Unsupported smoke-test architecture: $(uname -m)" >&2; exit 1 ;;
esac

PAYLOAD="$TMP/package/nexaroute"
DOWNLOAD="$TMP/mock/latest/download"
mkdir -p "$PAYLOAD/bin" "$PAYLOAD/configs" "$DOWNLOAD"
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${target%/*}"
  arch="${target#*/}"
  binary="$ROOT/bin/nexaroute-${os}-${arch}"
  [[ -x "$binary" ]] || { echo "Missing binary required for package test: $binary" >&2; exit 1; }
  install -m 0755 "$binary" "$PAYLOAD/bin/"
done
install -m 0755 "$ROOT/install-user.sh" "$PAYLOAD/"
install -m 0644 "$ROOT/configs/config.example.json" "$PAYLOAD/configs/"

tar -C "$TMP/package" -czf "$DOWNLOAD/nexaroute-universal.tar.gz" nexaroute
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$DOWNLOAD" && sha256sum nexaroute-universal.tar.gz > SHA256SUMS)
else
  (cd "$DOWNLOAD" && shasum -a 256 nexaroute-universal.tar.gz > SHA256SUMS)
fi

HOME_DIR="$TMP/home"
mkdir -p "$HOME_DIR"
NEXAROUTE_RELEASES_URL="file://${TMP}/mock" HOME="$HOME_DIR" "$ROOT/install.sh"
INSTALLED="$HOME_DIR/.local/bin/nexaroute"
[[ -x "$INSTALLED" ]] || { echo 'One-command installer did not install an executable.' >&2; exit 1; }
EXPECTED_VERSION="$("$ROOT/bin/nexaroute-${OS_ID}-${ARCH_ID}" -version)"
ACTUAL_VERSION="$("$INSTALLED" -version)"
[[ "$ACTUAL_VERSION" == "$EXPECTED_VERSION" ]] || {
  echo "Installed version mismatch: expected '$EXPECTED_VERSION', got '$ACTUAL_VERSION'." >&2
  exit 1
}
[[ -f "$HOME_DIR/.config/nexaroute/config.json" ]] || { echo 'Installer did not create the config file.' >&2; exit 1; }
[[ "$(stat -c '%a' "$HOME_DIR/.config/nexaroute/config.json" 2>/dev/null || stat -f '%Lp' "$HOME_DIR/.config/nexaroute/config.json")" == 600 ]] || {
  echo 'Installer created a config file without private permissions.' >&2
  exit 1
}
echo "PASS one-command install ($ACTUAL_VERSION) and private config"

printf 'tampered' > "$DOWNLOAD/nexaroute-universal.tar.gz"
if NEXAROUTE_RELEASES_URL="file://${TMP}/mock" HOME="$TMP/tampered-home" "$ROOT/install.sh" >"$TMP/tamper.log" 2>&1; then
  echo 'Installer accepted a release archive with an invalid checksum.' >&2
  exit 1
fi
if ! grep -q 'Checksum verification failed' "$TMP/tamper.log"; then
  cat "$TMP/tamper.log" >&2
  echo 'Installer failed for a reason other than checksum validation.' >&2
  exit 1
fi
echo 'PASS invalid checksum rejected'
