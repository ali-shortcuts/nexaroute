#!/usr/bin/env bash
# Offline installer E2E using local release-style artifacts and a curl shim.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/fixture" "$tmp/bin" "$tmp/home/.local/bin"
asset="nexaroute-linux-amd64"
cat > "$tmp/fixture/$asset" <<'BIN'
#!/bin/sh
echo "fake release gateway"
BIN
chmod 0755 "$tmp/fixture/$asset"
(cd "$tmp/fixture" && sha256sum "$asset" > SHA256SUMS)
cat > "$tmp/fixture/release.json" <<'JSON'
{"tag_name":"v-test","assets":[{"name":"nexaroute-linux-amd64"},{"name":"SHA256SUMS"}]}
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
  */nexaroute-linux-amd64) cp "$FIXTURE/nexaroute-linux-amd64" "$out" ;;
  *) echo "unexpected URL: $url" >&2; exit 1 ;;
esac
CURL
chmod +x "$tmp/bin/curl"
export FIXTURE="$tmp/fixture"
export HOME="$tmp/home"
export NEXAROUTE_INSTALL_DIR="$HOME/.local/bin"
export PATH="$tmp/bin:$HOME/.local/bin:$PATH"

bash "$ROOT/scripts/install.sh"
test -x "$HOME/.local/bin/nexaroute"
test "$(command -v nexaroute)" = "$HOME/.local/bin/nexaroute"
test "$(nexaroute)" = "fake release gateway"
config="$HOME/.config/nexaroute/config.json"
test -d "$(dirname "$config")"
printf '{"preserve":"runtime-config-canary"}\n' > "$config"
bash "$ROOT/scripts/install.sh"
grep -Fq runtime-config-canary "$config"
old_binary="$(sha256sum "$HOME/.local/bin/nexaroute" | cut -d ' ' -f1)"
printf '%064d  %s\\n' 0 "$asset" > "$tmp/fixture/SHA256SUMS"
if bash "$ROOT/scripts/install.sh" >/dev/null 2>&1; then
  echo "installer accepted a bad checksum" >&2
  exit 1
fi
test "$(sha256sum "$HOME/.local/bin/nexaroute" | cut -d ' ' -f1)" = "$old_binary"
test "$(nexaroute)" = "fake release gateway"
printf 'PASS: offline release install, PATH command, config upgrade preservation, checksum rejection and safe failure\n'
