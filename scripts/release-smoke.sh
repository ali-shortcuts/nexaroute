#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="${1:-$ROOT/dist}"
PORT="${NEXAROUTE_RELEASE_SMOKE_PORT:-18082}"
TMP="$(mktemp -d)"
PID=""
cleanup(){ if [[ -n "$PID" ]]; then kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; fi; rm -rf "$TMP"; }
trap cleanup EXIT
for dep in curl python3; do command -v "$dep" >/dev/null || { echo "missing $dep" >&2; exit 1; }; done
BIN="$DIST/nexaroute-linux-amd64"
[[ -x "$BIN" ]] || { echo "missing release binary: $BIN" >&2; exit 1; }
python3 - "$ROOT/configs/config.example.json" "$TMP/config.json" "$PORT" <<'PY'
import json, sys
src, dst, port = sys.argv[1:]
cfg = json.load(open(src, encoding='utf-8'))
cfg['listen'] = '127.0.0.1:' + port
cfg.setdefault('probe', {})['enabled'] = False
cfg['probe']['on_start'] = False
cfg['providers'] = []
cfg['video'] = {'enabled': True, 'store_path': 'video-jobs.json', 'queue_size': 8, 'workers': 1, 'development_fake_provider': True}
json.dump(cfg, open(dst, 'w', encoding='utf-8'), indent=2)
PY
chmod 600 "$TMP/config.json"
"$BIN" -no-browser -config "$TMP/config.json" >"$TMP/gateway.log" 2>&1 & PID=$!
BASE="http://127.0.0.1:$PORT"
for _ in $(seq 1 100); do curl -fsS "$BASE/healthz" >/dev/null 2>&1 && break; kill -0 "$PID" 2>/dev/null || { cat "$TMP/gateway.log" >&2; exit 1; }; sleep .1; done
curl -fsS "$BASE/healthz" >/dev/null
curl -fsS "$BASE/" | grep -q 'Video Studio'
providers="$(curl -fsS "$BASE/v1/video/providers")"
echo "$providers" | grep -q 'fake'
job='{"project_id":"release-smoke","mode":"text_to_video","prompt":"A clean product reveal in a dark studio","duration_seconds":1,"provider_preference":"fake"}'
code="$(curl -sS -o "$TMP/job.json" -w '%{http_code}' -H 'content-type: application/json' -X POST --data "$job" "$BASE/v1/video/jobs")"
[[ "$code" == 202 ]] || { cat "$TMP/job.json" >&2; exit 1; }
python3 - "$TMP/job.json" <<'PY'
import json, sys
x=json.load(open(sys.argv[1]))
assert x.get('job_id') or x.get('id'), x
PY
printf 'RELEASE SMOKE PASS — gateway, embedded UI, Video Studio route, fake provider and async job admission are operational.\n'
