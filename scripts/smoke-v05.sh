#!/usr/bin/env bash
# Live smoke test for v0.5 features against a running gateway binary.
set -euo pipefail
cd "$(dirname "$0")"

TMP=$(mktemp -d)
trap 'kill $GW_PID 2>/dev/null || true; rm -rf "$TMP"' EXIT

# Fake OpenAI-compatible upstream: counts calls, echoes usage.
CALLS=0
cat > "$TMP/upstream.py" <<'EOF'
import http.server, json, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('content-length', 0))
        body = self.rfile.read(n)
        with open('/tmp/nexa_smoke_calls.txt', 'a') as f:
            f.write('POST\n')
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(b'{"id":"c1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":21,"completion_tokens":5,"total_tokens":26}}')
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', 18099), H).serve_forever()
EOF
python3 "$TMP/upstream.py" &
UP_PID=$!
sleep 0.5

mkdir -p "$TMP/home/.config/nexaroute"
cat > "$TMP/home/.config/nexaroute/config.json" <<EOF
{
  "listen": "127.0.0.1:18098",
  "probe": {"enabled": false, "on_start": false},
  "routing": {"hedging_enabled": false},
  "cache": {"enabled": true, "ttl_seconds": 60, "max_entries": 16, "max_body_bytes": 1048576},
  "client_auth": {"enabled": false, "keys": [], "rpm": 0},
  "providers": [{
    "id": "local", "name": "Local", "type": "openai_compatible",
    "base_url": "http://127.0.0.1:18099", "auth_mode": "none", "enabled": true,
    "models": [{"id": "m", "model": "upstream-model", "enabled": true, "weight": 1,
                "context_window": 200000, "input_cost_per_mtok": 3, "output_cost_per_mtok": 15,
                "capabilities": {"streaming": true, "tools": true, "vision": true, "reasoning": true}}]
  }]
}
EOF

HOME="$TMP/home" ./bin/nexaroute-linux-amd64 -config "$TMP/home/.config/nexaroute/config.json" > "$TMP/gw.log" 2>&1 &
GW_PID=$!
sleep 1.2

B='{"model":"m","messages":[{"role":"user","content":"hello gateway"}]}'

echo "-- request 1 (cache MISS expected)"
curl -s -D - -o /dev/null -X POST -H "Content-Type: application/json" -d "$B" http://127.0.0.1:18098/v1/chat/completions | grep -i "X-NexaRoute-Cache\|HTTP/"
echo "-- request 2 (cache HIT expected)"
curl -s -D - -o /dev/null -X POST -H "Content-Type: application/json" -d "$B" http://127.0.0.1:18098/v1/chat/completions | grep -i "X-NexaRoute-Cache\|X-Gateway-Deployment"
echo "-- models listing"
curl -s http://127.0.0.1:18098/v1/models | head -c 200; echo
echo "-- metrics: usage + cache lines"
curl -s http://127.0.0.1:18098/metrics | grep -E "nexaroute_deployment_tokens_total|nexaroute_estimated_cost|nexaroute_cache_hits"
echo "-- healthz/readyz"
curl -s http://127.0.0.1:18098/healthz; echo
curl -s http://127.0.0.1:18098/readyz; echo
echo "-- UI served"
curl -s http://127.0.0.1:18098/ | grep -c "sCacheRate\|sTokens" || true
echo "SMOKE v0.5 PASS"
