#!/usr/bin/env bash
# Self-test for scripts/oidc-live-preflight.sh. Uses only loopback
# python3 fixtures (no real tenant, no credentials, no tokens) and
# asserts PASS/FAIL behavior plus the no-secret guarantee: a config
# that illegally carries a value-bearing client_secret must FAIL, and
# the secret value must never appear in the script's output.
#
# Exit 0 only if every case behaves as asserted.
set -euo pipefail
cd "$(dirname "$0")/.."

TMP=$(mktemp -d)
# Dynamic marker: no secret-like literal is committed in this script.
# The marker is written only to a temp config and must never appear
# in the preflight's output.
CANARY="preflight-test-marker-$$-$RANDOM"
FIXTURE_PID=""
trap 'rm -rf "$TMP"; [[ -n "$FIXTURE_PID" ]] && kill "$FIXTURE_PID" 2>/dev/null || true' EXIT

cat > "$TMP/fixture.py" <<'PYEOF'
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

mode = os.environ.get("FIXTURE_MODE", "ok")
port = int(os.environ.get("FIXTURE_PORT", "0"))

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass
    def do_GET(self):
        base = "http://127.0.0.1:%d" % self.server.server_port
        if self.path == "/.well-known/openid-configuration":
            issuer = base if mode != "issuer-mismatch" else "https://other-issuer.example"
            doc = {
                "issuer": issuer,
                "authorization_endpoint": base + "/auth",
                "token_endpoint": base + "/token",
                "jwks_uri": base + "/jwks",
                "code_challenge_methods_supported": ["S256"],
            }
            if mode == "no-s256":
                doc["code_challenge_methods_supported"] = ["plain"]
            body = json.dumps(doc).encode()
        elif self.path == "/jwks":
            keys = [] if mode == "empty-jwks" else [
                {"kty": "RSA", "kid": "fixture-key-1", "use": "sig", "alg": "RS256",
                 "n": "fixture-modulus-not-a-key", "e": "AQAB"},
            ]
            body = json.dumps({"keys": keys}).encode()
        else:
            self.send_response(404); self.end_headers(); return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

srv = HTTPServer(("127.0.0.1", port), Handler)
print(srv.server_port, flush=True)
srv.serve_forever()
PYEOF

start_fixture() {
	FIXTURE_MODE="$1" FIXTURE_PORT=0 python3 "$TMP/fixture.py" > "$TMP/port" 2> "$TMP/fixture.err" &
	FIXTURE_PID=$!
	for _ in $(seq 1 50); do
		[[ -s "$TMP/port" ]] && break
		sleep 0.1
	done
	PORT=$(cat "$TMP/port")
	ISSUER="http://127.0.0.1:$PORT"
}
stop_fixture() { kill "$FIXTURE_PID" 2>/dev/null || true; wait "$FIXTURE_PID" 2>/dev/null || true; }

run_preflight() {
	# Returns the script's exit code in $1's caller via || true handling.
	set +e
	OUT=$(./scripts/oidc-live-preflight.sh "$@" 2>&1)
	CODE=$?
	set -e
}

fail() { echo "test-oidc-live-preflight: FAIL: $*" >&2; exit 1; }

# Case 1: conformant fixture + matching config shape -> PASS.
start_fixture ok
cat > "$TMP/config-ok.json" <<EOF
{"admin": {"oidc": {
  "enabled": true, "issuer_url": "$ISSUER", "client_id": "staging-client",
  "client_secret_env": "NEXAROUTE_OIDC_CLIENT_SECRET",
  "redirect_url": "http://127.0.0.1:$PORT/admin/auth/oidc/callback",
  "audience": "nexaroute-control-plane", "role_claim": "groups",
  "role_mappings": {"nr-viewers": "viewer", "nr-admins": "admin"}
}}}
EOF
# The loopback callback carries the gateway's exact callback path
# (/admin/auth/oidc/callback); the preflight checks the path, not the
# host, so this fixture callback is accepted with --allow-loopback.
run_preflight --issuer "$ISSUER" --callback "http://127.0.0.1:$PORT/admin/auth/oidc/callback" \
	--audience nexaroute-control-plane --config "$TMP/config-ok.json" --allow-loopback
[[ "$CODE" -eq 0 ]] || fail "conformant fixture expected PASS, got $CODE: $OUT"
echo "$OUT" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["overall"] == "PASS", d' || fail "PASS report missing overall PASS"
stop_fixture

# Case 2: discovery issuer mismatch -> FAIL, and no session/token output.
start_fixture issuer-mismatch
run_preflight --issuer "$ISSUER" --callback "http://127.0.0.1:$PORT/admin/auth/oidc/callback" --allow-loopback
[[ "$CODE" -eq 1 ]] || fail "issuer mismatch expected FAIL(1), got $CODE: $OUT"
echo "$OUT" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert [c for c in d["checks"] if c["id"] == "discovery-issuer-pinning"][0]["status"] == "FAIL", d' || fail "issuer pinning check not FAIL: $OUT"
stop_fixture

# Case 3: empty JWKS -> FAIL.
start_fixture empty-jwks
run_preflight --issuer "$ISSUER" --callback "http://127.0.0.1:$PORT/admin/auth/oidc/callback" --allow-loopback
[[ "$CODE" -eq 1 ]] || fail "empty JWKS expected FAIL(1), got $CODE: $OUT"
echo "$OUT" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert [c for c in d["checks"] if c["id"] == "jwks-fetch"][0]["status"] == "FAIL", d' || fail "JWKS check not FAIL: $OUT"
stop_fixture

# Case 4: provider without S256 -> FAIL.
start_fixture no-s256
run_preflight --issuer "$ISSUER" --callback "http://127.0.0.1:$PORT/admin/auth/oidc/callback" --allow-loopback
[[ "$CODE" -eq 1 ]] || fail "no-S256 expected FAIL(1), got $CODE: $OUT"
stop_fixture

# Case 5: wrong callback path, and config carrying a literal secret -> FAIL,
# with the secret value never echoed.
start_fixture ok
cat > "$TMP/config-secret.json" <<EOF
{"admin": {"oidc": {
  "enabled": true, "issuer_url": "$ISSUER", "client_id": "staging-client",
  "client_secret_env": "NEXAROUTE_OIDC_CLIENT_SECRET",
  "client_secret": "$CANARY",
  "redirect_url": "http://127.0.0.1:$PORT/admin/auth/oidc/callback",
  "audience": "nexaroute-control-plane", "role_claim": "groups",
  "role_mappings": {"nr-viewers": "viewer"}
}}}
EOF
run_preflight --issuer "$ISSUER" --callback "http://127.0.0.1:$PORT/wrong/callback" \
	--config "$TMP/config-secret.json" --allow-loopback
[[ "$CODE" -eq 1 ]] || fail "bad callback + secret config expected FAIL(1), got $CODE: $OUT"
echo "$OUT" | grep -q "$CANARY" && fail "secret value leaked into preflight output"
echo "$OUT" | grep -q 'client_secret must not appear' || fail "secret-in-config check not reported: $OUT"
stop_fixture

# Case 6: --out writes a 0600 report (operator reviews it before
# attaching it anywhere; tenant ids in hostnames are metadata).
start_fixture ok
run_preflight --issuer "$ISSUER" --callback "http://127.0.0.1:$PORT/admin/auth/oidc/callback" \
	--allow-loopback --out "$TMP/report.json"
[[ "$CODE" -eq 0 ]] || fail "--out run expected PASS, got $CODE: $OUT"
[[ "$(stat -c %a "$TMP/report.json")" == "600" ]] || fail "--out report is not mode 0600"
stop_fixture

# Case 7: usage error makes no network request and exits 2.
run_preflight --issuer "$ISSUER"
[[ "$CODE" -eq 2 ]] || fail "missing --callback expected exit 2, got $CODE: $OUT"

echo "test-oidc-live-preflight: PASS (loopback fixtures only; not live acceptance evidence)"
