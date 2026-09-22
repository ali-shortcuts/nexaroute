#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${PORT:-18080}"
ADDR="127.0.0.1:${PORT}"
BASE="http://${ADDR}"
TMP="$(mktemp -d)"
PID=""
cleanup() {
  if [[ -n "${PID}" ]]; then
    kill "${PID}" 2>/dev/null || true
    wait "${PID}" 2>/dev/null || true
  fi
  rm -rf "${TMP}"
}
trap cleanup EXIT

case "$(uname -m)" in
  x86_64|amd64) BIN="$ROOT/bin/ulg-linux-amd64" ;;
  aarch64|arm64) BIN="$ROOT/bin/ulg-linux-arm64" ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

for dep in curl python3; do
  command -v "$dep" >/dev/null 2>&1 || { echo "Missing required command: $dep" >&2; exit 1; }
done
[[ -x "$BIN" ]] || { echo "Missing executable binary: $BIN" >&2; exit 1; }

python3 - "$ROOT/configs/config.example.json" "$TMP/config.json" "$ADDR" <<'PY'
import json, sys
src, dst, addr = sys.argv[1:]
with open(src, encoding='utf-8') as f:
    cfg = json.load(f)
cfg['listen'] = addr
cfg.setdefault('probe', {})['enabled'] = False
cfg['probe']['on_start'] = False
cfg['providers'] = []
with open(dst, 'w', encoding='utf-8') as f:
    json.dump(cfg, f, indent=2)
PY
chmod 600 "$TMP/config.json"

"$BIN" -config "$TMP/config.json" >"$TMP/gateway.log" 2>&1 &
PID=$!

for _ in $(seq 1 80); do
  if curl -fsS "$BASE/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "Gateway exited during startup:" >&2
    cat "$TMP/gateway.log" >&2
    exit 1
  fi
  sleep 0.1
done
curl -fsS "$BASE/healthz" >/dev/null

status() {
  local method="$1" url="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sS -o "$TMP/resp" -w '%{http_code}' -X "$method" -H 'content-type: application/json' --data-binary "$body" "$url"
  else
    curl -sS -o "$TMP/resp" -w '%{http_code}' -X "$method" "$url"
  fi
}
expect_status() {
  local want="$1" got="$2" label="$3"
  if [[ "$got" != "$want" ]]; then
    echo "FAIL $label: expected HTTP $want, got $got" >&2
    cat "$TMP/resp" >&2 || true
    exit 1
  fi
  echo "PASS $label ($got)"
}

expect_status 200 "$(status GET "$BASE/")" "embedded Web UI"
expect_status 200 "$(status GET "$BASE/api/hello")" "runtime hello"
expect_status 200 "$(status GET "$BASE/v1/models")" "model list"
expect_status 200 "$(status GET "$BASE/admin/api/snapshot")" "admin snapshot"
expect_status 200 "$(status POST "$BASE/v1/messages/count_tokens" '{"model":"claude-auto","messages":[{"role":"user","content":"hello world"}]}')" "count_tokens fallback"
python3 - "$TMP/resp" <<'PY'
import json, sys
x = json.load(open(sys.argv[1]))
assert isinstance(x.get('input_tokens'), int) and x['input_tokens'] > 0, x
assert x.get('estimated') is True, x
PY

CREATE='{"provider":{"id":"smoke-openai","name":"Smoke OpenAI","type":"openai_compatible","base_url":"http://127.0.0.1:19999/v1","api_key":"secret-one","auth_mode":"bearer","enabled":false,"models":[{"id":"m1","model":"model-1","enabled":true,"priority":10,"weight":1,"capabilities":{"streaming":true,"tools":true,"vision":false,"reasoning":false}}]},"preserve_secret":false}'
expect_status 201 "$(status POST "$BASE/admin/api/providers" "$CREATE")" "provider create"
expect_status 200 "$(status GET "$BASE/admin/api/providers/smoke-openai?reveal=1")" "provider reveal"
python3 - "$TMP/resp" <<'PY'
import json, sys
x = json.load(open(sys.argv[1]))
assert x['provider']['base_url'] == 'http://127.0.0.1:19999/v1', x
assert x.get('resolved_api_key') == 'secret-one', x
PY

UPDATE='{"provider":{"id":"smoke-openai","name":"Smoke Renamed","type":"openai_compatible","base_url":"http://127.0.0.1:19999/v1","auth_mode":"bearer","enabled":false,"models":[{"id":"m1","model":"model-1","enabled":true,"priority":10,"weight":1,"capabilities":{"streaming":true,"tools":true,"vision":false,"reasoning":false}}]},"preserve_secret":true}'
expect_status 200 "$(status PUT "$BASE/admin/api/providers/smoke-openai" "$UPDATE")" "provider edit"
expect_status 200 "$(status GET "$BASE/admin/api/providers/smoke-openai?reveal=1")" "provider re-open"
python3 - "$TMP/resp" <<'PY'
import json, sys
x = json.load(open(sys.argv[1]))
assert x['provider']['name'] == 'Smoke Renamed', x
assert x['provider']['base_url'] == 'http://127.0.0.1:19999/v1', x
assert x.get('resolved_api_key') == 'secret-one', x
PY

expect_status 200 "$(status GET "$BASE/admin/api/providers/smoke-openai")" "provider redacted read"
python3 - "$TMP/resp" <<'PY'
import json, sys
x = json.load(open(sys.argv[1]))
assert x['provider'].get('api_key', '') == '', x
assert x.get('has_secret') is True, x
PY

expect_status 200 "$(status DELETE "$BASE/admin/api/providers/smoke-openai")" "provider delete"

if ! kill -0 "$PID" 2>/dev/null; then
  echo "Gateway died during smoke test" >&2
  cat "$TMP/gateway.log" >&2
  exit 1
fi

echo "SMOKE PASS — local runtime, UI, admin persistence and token-count fallback are operational."
