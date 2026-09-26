#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

export PATH="/home/user/.local/go/bin:/usr/local/bin:/usr/bin:/bin"

echo '== build release-style artifacts =='
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o bin/nexaroute-linux-amd64 ./cmd/gateway
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o bin/nexaroute-linux-arm64 ./cmd/gateway
(cd bin && sha256sum nexaroute-linux-amd64 nexaroute-linux-arm64 > SHA256SUMS)
cp bin/SHA256SUMS SHA256SUMS

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
pkg="$tmp/nexaroute"
mkdir -p "$pkg/bin" "$pkg/configs" "$pkg/scripts"
install -m 0755 bin/nexaroute-linux-amd64 "$pkg/bin/nexaroute-linux-amd64"
install -m 0755 bin/nexaroute-linux-arm64 "$pkg/bin/nexaroute-linux-arm64"
install -m 0644 bin/SHA256SUMS "$pkg/SHA256SUMS"
install -m 0755 install-user.sh "$pkg/install-user.sh"
install -m 0644 configs/config.example.json "$pkg/configs/config.example.json"

echo '== install without Go on PATH =='
home="$tmp/home"
mkdir -p "$home"
PATH_NO_GO="/usr/bin:/bin"
HOME="$home" PATH="$PATH_NO_GO" "$pkg/install-user.sh"
test -x "$home/.local/bin/nexaroute"
test -f "$home/.config/nexaroute/config.json"
test "$(stat -c '%a' "$home/.config/nexaroute/config.json")" = "600"
expected_version="$(sed -n 's/^const version = "\([^"]*\)"/NexaRoute v\1/p' cmd/gateway/main.go)"
test "$("$home/.local/bin/nexaroute" -version)" = "$expected_version"

echo '== existing config preserved on upgrade =='
python3 - "$home/.config/nexaroute/config.json" <<'PY'
import json, sys
p = sys.argv[1]
with open(p, encoding='utf-8') as f:
    cfg = json.load(f)
cfg['listen'] = '127.0.0.1:18082'
cfg.setdefault('probe', {})['enabled'] = False
cfg['probe']['on_start'] = False
with open(p, 'w', encoding='utf-8') as f:
    json.dump(cfg, f, indent=2)
PY
chmod 600 "$home/.config/nexaroute/config.json"
HOME="$home" PATH="$PATH_NO_GO" "$pkg/install-user.sh"
python3 - "$home/.config/nexaroute/config.json" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1], encoding='utf-8'))
assert cfg['listen'] == '127.0.0.1:18082', cfg['listen']
PY

echo '== headless start + UI reachable =='
unset DISPLAY WAYLAND_DISPLAY || true
export NEXAROUTE_NO_BROWSER=1
export NEXAROUTE_INSTANCE_LOCK="$home/.config/nexaroute/instance.lock"
export NEXAROUTE_CONFIG="$home/.config/nexaroute/config.json"
"$home/.local/bin/nexaroute" -config "$NEXAROUTE_CONFIG" -no-browser >"$tmp/gw.log" 2>&1 &
pid=$!
cleanup_gw() {
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}
trap 'cleanup_gw; rm -rf "$tmp"' EXIT
ok=0
for _ in $(seq 1 80); do
  if curl -fsS http://127.0.0.1:18082/healthz >/dev/null 2>&1; then
    ok=1
    break
  fi
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "gateway exited:" >&2
    cat "$tmp/gw.log" >&2
    exit 1
  fi
  sleep 0.1
done
test "$ok" = 1
curl -fsS http://127.0.0.1:18082/ >/dev/null

echo '== duplicate process protection =='
set +e
HOME="$home" NEXAROUTE_NO_BROWSER=1 NEXAROUTE_INSTANCE_LOCK="$NEXAROUTE_INSTANCE_LOCK" \
  "$home/.local/bin/nexaroute" -config "$NEXAROUTE_CONFIG" -no-browser >"$tmp/second.log" 2>&1
rc=$?
set -e
test "$rc" = 0
grep -q 'already running' "$tmp/second.log"
grep -q 'http://127.0.0.1:18082/' "$tmp/second.log"
# still only one listener
curl -fsS http://127.0.0.1:18082/healthz >/dev/null

echo 'INSTALLER TEST PASS'
