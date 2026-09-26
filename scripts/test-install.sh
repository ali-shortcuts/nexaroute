#!/usr/bin/env bash
# Offline installer E2E using local release-style assets and command shims.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/fixture" "$tmp/bin" "$tmp/home/.local/bin" "$tmp/home/.local/bin-arm"

for arch in amd64 arm64; do
  cat > "$tmp/fixture/nexaroute-linux-$arch" <<BIN
#!/bin/sh
echo "fake $arch gateway"
BIN
  chmod 0755 "$tmp/fixture/nexaroute-linux-$arch"
done
(cd "$tmp/fixture" && sha256sum nexaroute-linux-amd64 nexaroute-linux-arm64 > SHA256SUMS)
cat > "$tmp/fixture/release.json" <<'JSON'
{"tag_name":"v-test","assets":[{"name":"nexaroute-linux-amd64"},{"name":"nexaroute-linux-arm64"},{"name":"SHA256SUMS"}]}
JSON

cat > "$tmp/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
out=""
url=""
while (($#)); do
  if [[ "$1" == "-o" ]]; then out="$2"; shift 2; else url="$1"; shift; fi
done
case "$url" in
  */releases/latest) cp "$FIXTURE/release.json" "$out" ;;
  */SHA256SUMS) cp "$FIXTURE/SHA256SUMS" "$out" ;;
  */nexaroute-linux-amd64|*/nexaroute-linux-arm64) cp "$FIXTURE/${url##*/}" "$out" ;;
  *) echo "unexpected URL: $url" >&2; exit 1 ;;
esac
CURL
cat > "$tmp/bin/uname" <<'UNAME'
#!/usr/bin/env bash
if [[ "${1:-}" == "-m" && -n "${FAKE_ARCH:-}" ]]; then echo "$FAKE_ARCH"; else exec /usr/bin/uname "$@"; fi
UNAME
chmod +x "$tmp/bin/curl" "$tmp/bin/uname"
export FIXTURE="$tmp/fixture"
export HOME="$tmp/home"
export NEXAROUTE_INSTALL_DIR="$HOME/.local/bin"
export PATH="$tmp/bin:$HOME/.local/bin:$PATH"

bash "$ROOT/scripts/install.sh"
test -x "$HOME/.local/bin/nexaroute"
test "$(command -v nexaroute)" = "$HOME/.local/bin/nexaroute"
test "$(nexaroute)" = "fake amd64 gateway"
config="$HOME/.config/nexaroute/config.json"
test -d "$(dirname "$config")"
printf '{"preserve":"runtime-config-canary"}\n' > "$config"
bash "$ROOT/scripts/install.sh"
grep -Fq runtime-config-canary "$config"

# Exercise architecture selection and asset naming as if on Linux ARM64.
export FAKE_ARCH=aarch64
export NEXAROUTE_INSTALL_DIR="$HOME/.local/bin-arm"
export PATH="$tmp/bin:$HOME/.local/bin-arm:$PATH"
bash "$ROOT/scripts/install.sh"
test -x "$HOME/.local/bin-arm/nexaroute"
test "$(nexaroute)" = "fake arm64 gateway"

# A corrupt checksum must fail before replacing the existing executable.
export FAKE_ARCH=x86_64
export NEXAROUTE_INSTALL_DIR="$HOME/.local/bin"
export PATH="$tmp/bin:$HOME/.local/bin:$PATH"
old_binary="$(sha256sum "$HOME/.local/bin/nexaroute" | cut -d ' ' -f1)"
printf '%064d  %s\n' 0 nexaroute-linux-amd64 > "$tmp/fixture/SHA256SUMS"
if bash "$ROOT/scripts/install.sh" >/dev/null 2>&1; then
  echo "installer accepted a bad checksum" >&2
  exit 1
fi
test "$(sha256sum "$HOME/.local/bin/nexaroute" | cut -d ' ' -f1)" = "$old_binary"
test "$(nexaroute)" = "fake amd64 gateway"
printf 'PASS: amd64/arm64 asset selection, PATH command, config preservation, checksum rejection, and safe failure\n'
