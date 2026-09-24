#!/usr/bin/env bash
# Live smoke test for the v0.5 feature tier against a built gateway binary.
#
# Boots a fake OpenAI-compatible upstream plus the gateway on scratch ports and
# asserts the cache plane, hedging fast path, usage/cost metrics, health
# endpoints and the embedded UI. Robustness rules for this script:
#   * every process it starts is killed on exit (including the fake upstream);
#   * the fake upstream logs to a file, never to this script's stdout, so a
#     leaked child can never hold the caller's pipe open;
#   * every curl has a hard --max-time, and readiness is polled instead of
#     assumed, so the script cannot hang;
#   * assertions fail loudly with a non-zero exit.
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ -n ${NEXAROUTE_BIN:-} ]]; then
  BIN=$NEXAROUTE_BIN
else
  case "$(uname -m)" in
    x86_64|amd64) BIN=./bin/nexaroute-linux-amd64 ;;
    aarch64|arm64) BIN=./bin/nexaroute-linux-arm64 ;;
    *) BIN=./bin/nexaroute-linux-amd64 ;;
  esac
fi
if [[ ! -x $BIN ]]; then
  echo "smoke-v05: missing $BIN (build it with: make build-linux-amd64)" >&2
  exit 2
fi
command -v python3 >/dev/null || { echo "smoke-v05: python3 required" >&2; exit 2; }

UP_PORT=${NEXAROUTE_SMOKE_UP_PORT:-18199}
GW_PORT=${NEXAROUTE_SMOKE_GW_PORT:-18198}
TMP=$(mktemp -d)
GW_PID="" UP_PID=""
cleanup() {
  if [[ -n $GW_PID ]]; then kill "$GW_PID" 2>/dev/null || true; fi
  if [[ -n $UP_PID ]]; then kill "$UP_PID" 2>/dev/null || true; fi
  wait 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

fail() {
  echo "smoke-v05: FAIL: $*" >&2
  if [[ -s ${TMP:-}/gw.log ]]; then
    echo "--- last gateway log lines ---" >&2
    tail -20 "$TMP/gw.log" >&2
  fi
  if [[ -s ${TMP:-}/upstream.log ]]; then
    echo "--- last upstream log lines ---" >&2
    tail -10 "$TMP/upstream.log" >&2
  fi
  exit 1
}
pass() { echo "  ok: $*"; }

# --- fake upstream --------------------------------------------------------
# Responds instantly for fast-model and after a delay for slow-model, so the
# hedging fast path can be observed end to end.
cat > "$TMP/upstream.py" <<'PY'
import http.server, json, time

def body(model):
    return json.dumps({
        "id": "c1", "object": "chat.completion", "created": 1, "model": model,
        "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 21, "completion_tokens": 5, "total_tokens": 26},
    }).encode()

class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        payload = self.rfile.read(n)
        try:
            model = json.loads(payload or b"{}").get("model", "upstream-model")
        except Exception:
            model = "upstream-model"
        if model == "slow-model":
            time.sleep(0.6)
        out = body(model)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def log_message(self, *a):
        pass

http.server.ThreadingHTTPServer(("127.0.0.1", int(__import__("sys").argv[1])), H).serve_forever()
PY
python3 "$TMP/upstream.py" "$UP_PORT" >"$TMP/upstream.log" 2>&1 &
UP_PID=$!

# Wait for the fake upstream to accept connections before the gateway can route
# to it, otherwise the first request races the listener and fails with a 502.
up_ready=""
for _ in $(seq 1 100); do
  if (exec 3<>"/dev/tcp/127.0.0.1/$UP_PORT") 2>/dev/null; then up_ready=1; break; fi
  if ! kill -0 "$UP_PID" 2>/dev/null; then
    echo "--- fake upstream log ---" >&2; cat "$TMP/upstream.log" >&2
    fail "fake upstream exited during startup"
  fi
  sleep 0.1
done
[[ -n $up_ready ]] || fail "fake upstream did not start listening on 127.0.0.1:$UP_PORT"

# --- gateway config -------------------------------------------------------
mkdir -p "$TMP/home/.config/nexaroute"
cat > "$TMP/home/.config/nexaroute/config.json" <<EOF
{
  "listen": "127.0.0.1:$GW_PORT",
  "probe": {"enabled": false, "on_start": false},
  "routing": {"strategy": "priority", "hedging_enabled": true, "hedging_delay_ms": 50, "max_attempts": 4},
  "cache": {"enabled": true, "ttl_seconds": 60, "max_entries": 16, "max_body_bytes": 1048576},
  "client_auth": {"enabled": false, "keys": [], "rpm": 0},
  "providers": [
    {"id": "slow", "name": "Slow", "type": "openai_compatible",
     "base_url": "http://127.0.0.1:$UP_PORT", "auth_mode": "none", "enabled": true,
     "models": [{"id": "m", "model": "slow-model", "enabled": true, "weight": 1, "priority": 0,
                 "context_window": 200000, "input_cost_per_mtok": 3, "output_cost_per_mtok": 15,
                 "capabilities": {"streaming": true, "tools": true, "vision": true, "reasoning": true}}]},
    {"id": "fast", "name": "Fast", "type": "openai_compatible",
     "base_url": "http://127.0.0.1:$UP_PORT", "auth_mode": "none", "enabled": true,
     "models": [{"id": "m", "model": "fast-model", "enabled": true, "weight": 1, "priority": 1,
                 "capabilities": {"streaming": true, "tools": true}}]}
  ]
}
EOF
HOME="$TMP/home" "$BIN" -config "$TMP/home/.config/nexaroute/config.json" >"$TMP/gw.log" 2>&1 &
GW_PID=$!

base="http://127.0.0.1:$GW_PORT"
ready=""
for _ in $(seq 1 100); do
  if curl -fsS --max-time 1 "$base/healthz" >/dev/null 2>&1; then ready=1; break; fi
  if ! kill -0 "$GW_PID" 2>/dev/null; then
    echo "--- gateway log ---"; cat "$TMP/gw.log" >&2
    fail "gateway exited during startup"
  fi
  sleep 0.1
done
[[ -n $ready ]] || { cat "$TMP/gw.log" >&2; fail "gateway did not become ready within 10s"; }
pass "gateway ready on $base"

# --- helpers --------------------------------------------------------------
# curl -f would abort the script under set -e before we can explain what went
# wrong, so statuses are captured and asserted explicitly.
req() { # method url [body] -> prints HTTP status; body in $TMP/resp
  local method=$1 url=$2 body=${3:-}
  if [[ -n $body ]]; then
    curl -sS --max-time 10 -o "$TMP/resp" -w '%{http_code}' -X "$method" \
      -H 'content-type: application/json' --data-binary "$body" "$url"
  else
    curl -sS --max-time 10 -o "$TMP/resp" -w '%{http_code}' -X "$method" "$url"
  fi
}
reqh() { # method url [body] -> prints HTTP status; body in $TMP/resp, headers in $TMP/hdr
  local method=$1 url=$2 body=${3:-}
  if [[ -n $body ]]; then
    curl -sS --max-time 10 -D "$TMP/hdr" -o "$TMP/resp" -w '%{http_code}' -X "$method" \
      -H 'content-type: application/json' --data-binary "$body" "$url"
  else
    curl -sS --max-time 10 -D "$TMP/hdr" -o "$TMP/resp" -w '%{http_code}' -X "$method" "$url"
  fi
}
expect() { # want got label
  [[ $2 == "$1" ]] || fail "$3: expected HTTP $1, got $2 ($(head -c 200 "$TMP/resp" 2>/dev/null))"
}

# --- cache plane ----------------------------------------------------------
B='{"model":"m","messages":[{"role":"user","content":"hello gateway"}]}'
expect 200 "$(reqh POST "$base/v1/chat/completions" "$B")" "cacheable chat completion"
# Only hits carry a response header; misses are observable as a counter.
grep -qi '^x-nexaroute-cache: hit' "$TMP/hdr" && fail "first request must not be served from the cache"
expect 200 "$(req GET "$base/metrics")" "/metrics after the first request"
grep -qE '^nexaroute_cache_misses_total 1$' "$TMP/resp" || fail "expected one cache miss in /metrics: $(grep '^nexaroute_cache_misses_total' "$TMP/resp")"
pass "first request is a cache MISS"
expect 200 "$(reqh POST "$base/v1/chat/completions" "$B")" "cached chat completion"
grep -qi '^x-nexaroute-cache: hit' "$TMP/hdr" || fail "second request should be a cache hit: $(grep -i x-nexaroute-cache "$TMP/hdr" || echo 'no cache header')"
pass "second request is a cache HIT"

# --- hedging fast path ----------------------------------------------------
# The strategy is "priority", so slow-model (priority 0) is always the primary
# and fast-model is the hedge partner. The hedge launches after 50ms and the
# client must be served by it without waiting for the slow upstream's 600ms
# answer: that is the regression guard for a losing leg being cancelled
# instead of awaited.
HB='{"model":"m","messages":[{"role":"user","content":"hedge please"}],"temperature":0.9}'
start=$(date +%s%N)
expect 200 "$(reqh POST "$base/v1/chat/completions" "$HB")" "hedged chat completion"
elapsed_ms=$(( ($(date +%s%N) - start) / 1000000 ))
grep -qi '^x-gateway-deployment: fast/m' "$TMP/hdr" || fail "hedged race should be won by fast/m: $(grep -i '^x-gateway-deployment' "$TMP/hdr" || echo 'no deployment header')"
[[ $elapsed_ms -lt 450 ]] || fail "hedged response took ${elapsed_ms}ms; the losing leg must not delay the winner (the slow upstream alone takes 600ms)"
pass "hedged race won by fast/m in ${elapsed_ms}ms while the slow primary needs 600ms"

# --- models, metrics, health ---------------------------------------------
expect 200 "$(req GET "$base/v1/models")" "/v1/models"
# Deployment IDs are "provider/model-id"; upstream model names are listed too.
for want in '"slow/m"' '"fast/m"' '"slow-model"' '"fast-model"'; do
  grep -q "$want" "$TMP/resp" || fail "/v1/models does not list $want: $(head -c 200 "$TMP/resp")"
done
pass "/v1/models lists both deployments and their upstream models"
expect 200 "$(req GET "$base/metrics")" "/metrics"
for want in nexaroute_deployment_tokens_total nexaroute_estimated_cost nexaroute_cache_hits_total nexaroute_cache_misses_total; do
  grep -q "$want" "$TMP/resp" || fail "/metrics missing $want"
done
pass "/metrics exposes usage, cost and cache series"
expect 200 "$(req GET "$base/healthz")" "/healthz"
grep -q '"ok":true' "$TMP/resp" || fail "/healthz payload is not ok"
expect 200 "$(req GET "$base/readyz")" "/readyz"
pass "/healthz and /readyz respond"

# --- embedded UI ----------------------------------------------------------
expect 200 "$(req GET "$base/")" "embedded Web UI"
grep -q 'sCacheRate' "$TMP/resp" || fail "UI shell is missing dashboard markers"
expect 200 "$(req GET "$base/app.js")" "embedded app.js"
grep -q 'fillPresetSelect' "$TMP/resp" || fail "app.js is not served by the binary"
pass "embedded UI and app.js are served"

echo "SMOKE v0.5 PASS"
