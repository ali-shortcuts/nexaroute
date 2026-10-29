#!/usr/bin/env bash
# Secret-free OIDC live-tenant preflight for NexaRoute (issue #647).
#
# What this is: a read-only check an operator runs BEFORE any live login,
# against an operator-owned staging tenant (Keycloak, Entra ID, Okta,
# Auth0, Google). It fetches only the public discovery document and the
# public JWKS, and optionally validates the sanitized shape of a gateway
# config file. It performs no login, requests no token, and accepts no
# secret, token, code, or credential of any kind as input. It never reads
# the environment variable named by admin.oidc.client_secret_env.
#
# What this is NOT: live acceptance evidence. A PASS here proves only
# that the tenant publishes a conformant discovery document and JWKS
# and that the supplied config shape matches the tenant. The interactive
# Authorization Code + PKCE browser login, role-mapping checks,
# logout/revocation, and audit inspection still require the operator
# procedure in docs/reports/OIDC_OPERATOR_RUNBOOK_647.md, and the live
# tenant remains BLOCKED until that procedure is executed on the real
# tenant. A local loopback fixture PASS is local evidence only.
#
# Exit codes: 0 = every check PASS; 1 = at least one check FAIL;
# 2 = usage error (no network request is made, no report is written).
# URLs echoed in the report are reduced to scheme, host, and path:
# user info, query strings, and fragments are never printed. Config
# values that could identify tenant groups are reported as counts only.
set -euo pipefail

ISSUER=""
CALLBACK=""
AUDIENCE=""
CONFIG=""
OUT=""
TIMEOUT=15
ALLOW_LOOPBACK=0

usage() {
	echo "usage: $0 --issuer <issuer-url> --callback <callback-url> [--audience <aud>] [--config <config.json>] [--timeout <seconds>] [--allow-loopback] [--out <report.json>]" >&2
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--issuer) ISSUER="${2:-}"; shift 2 ;;
	--callback) CALLBACK="${2:-}"; shift 2 ;;
	--audience) AUDIENCE="${2:-}"; shift 2 ;;
	--config) CONFIG="${2:-}"; shift 2 ;;
	--timeout) TIMEOUT="${2:-}"; shift 2 ;;
	--allow-loopback) ALLOW_LOOPBACK=1; shift ;;
	--out) OUT="${2:-}"; shift 2 ;;
	-h|--help) usage; exit 0 ;;
	*) echo "oidc-live-preflight: unknown argument: $1" >&2; usage; exit 2 ;;
	esac
done

if [[ -z "$ISSUER" || -z "$CALLBACK" ]]; then
	echo "oidc-live-preflight: --issuer and --callback are required" >&2
	usage
	exit 2
fi
if ! [[ "$TIMEOUT" =~ ^[0-9]+$ ]] || [[ "$TIMEOUT" -lt 1 || "$TIMEOUT" -gt 60 ]]; then
	echo "oidc-live-preflight: --timeout must be between 1 and 60" >&2
	exit 2
fi
if ! command -v curl >/dev/null 2>&1 || ! command -v python3 >/dev/null 2>&1; then
	echo "oidc-live-preflight: curl and python3 are required" >&2
	exit 2
fi
if [[ -n "$CONFIG" && ! -f "$CONFIG" ]]; then
	echo "oidc-live-preflight: config file not found: $CONFIG" >&2
	exit 2
fi

# All JSON parsing and URL sanitizing happens in python3 so no value
# ever passes through shell expansion into the report. The python exit
# code is captured explicitly because a FAIL report must still reach
# the operator (set -e must not discard it).
REPORT=$(ISSUER="$ISSUER" CALLBACK="$CALLBACK" AUDIENCE="$AUDIENCE" CONFIG="$CONFIG" \
	TIMEOUT="$TIMEOUT" ALLOW_LOOPBACK="$ALLOW_LOOPBACK" python3 - <<'PYEOF' || true
import json, os, re, subprocess, sys, urllib.parse

issuer = os.environ["ISSUER"]
callback = os.environ["CALLBACK"]
audience = os.environ["AUDIENCE"]
config_path = os.environ["CONFIG"]
timeout = os.environ["TIMEOUT"]
allow_loopback = os.environ["ALLOW_LOOPBACK"] == "1"

checks = []
def check(cid, status, detail):
    checks.append({"id": cid, "status": status, "detail": detail})

def sanitize(raw):
    try:
        u = urllib.parse.urlparse(raw)
        return urllib.parse.urlunparse((u.scheme, u.netloc, u.path, "", "", ""))
    except Exception:
        return "(unparsable url)"

def endpoint_ok(raw, issuer_raw):
    """Mirror of the gateway's validateOIDCDiscovery endpoint rule:
    absolute HTTPS, or loopback HTTP only when the issuer itself is a
    loopback HTTP URL and --allow-loopback was given."""
    try:
        u = urllib.parse.urlparse(raw)
    except Exception:
        return False
    if not u.scheme or not u.hostname or u.username or u.fragment:
        return False
    if u.scheme == "https":
        return True
    if u.scheme != "http" or not allow_loopback:
        return False
    try:
        iu = urllib.parse.urlparse(issuer_raw)
    except Exception:
        return False
    def loopback(host):
        if not host:
            return False
        host = host.rstrip(".").lower()
        if host == "localhost":
            return True
        import ipaddress
        try:
            return ipaddress.ip_address(host).is_loopback
        except ValueError:
            return False
    return iu.scheme == "http" and loopback(iu.hostname) and loopback(u.hostname)

def fetch(url):
    proc = subprocess.run(
        ["curl", "-fsS", "--max-time", timeout, "-H", "Accept: application/json", url],
        capture_output=True, text=True)
    if proc.returncode != 0:
        return None, (proc.stderr or "").strip().splitlines()[-1][:200] if proc.stderr.strip() else "fetch failed"
    try:
        return json.loads(proc.stdout), ""
    except Exception:
        return None, "response is not valid JSON"

# 1. Issuer shape: same rule as the gateway's discovery validation.
iu = urllib.parse.urlparse(issuer)
if not iu.scheme or not iu.hostname or iu.username or iu.query or iu.fragment:
    check("issuer-shape", "FAIL", "issuer must be an absolute URL without user info, query, or fragment")
elif endpoint_ok(issuer, issuer):
    check("issuer-shape", "PASS", sanitize(issuer))
else:
    check("issuer-shape", "FAIL", "issuer must be HTTPS (loopback HTTP only with --allow-loopback)")

# 2. Callback shape: the gateway serves exactly /admin/auth/oidc/callback.
cu = urllib.parse.urlparse(callback)
if not cu.scheme or not cu.hostname or cu.username or cu.query or cu.fragment or cu.path != "/admin/auth/oidc/callback":
    check("callback-shape", "FAIL", "callback must be an absolute URL whose path is exactly /admin/auth/oidc/callback, with no query or fragment")
elif endpoint_ok(callback, issuer):
    check("callback-shape", "PASS", sanitize(callback))
else:
    check("callback-shape", "FAIL", "callback must be HTTPS (loopback HTTP only with --allow-loopback)")

# 3. Discovery document: issuer pinning + endpoint validity.
discovery_url = issuer.rstrip("/") + "/.well-known/openid-configuration"
doc, err = fetch(discovery_url)
if doc is None:
    check("discovery-fetch", "FAIL", "discovery fetch failed: " + err)
    check("discovery-issuer-pinning", "FAIL", "discovery document unavailable")
    check("discovery-endpoints", "FAIL", "discovery document unavailable")
    check("discovery-pkce-s256", "FAIL", "discovery document unavailable")
    check("jwks-fetch", "FAIL", "discovery document unavailable, JWKS URI unknown")
else:
    check("discovery-fetch", "PASS", "HTTP 200, valid JSON from " + sanitize(discovery_url))
    if doc.get("issuer") == issuer:
        check("discovery-issuer-pinning", "PASS", "discovery issuer exactly equals the configured issuer")
    else:
        check("discovery-issuer-pinning", "FAIL", "discovery issuer does not exactly equal the configured issuer")
    bad = [k for k in ("authorization_endpoint", "token_endpoint", "jwks_uri")
           if not endpoint_ok(str(doc.get(k, "")), issuer)]
    if not bad:
        check("discovery-endpoints", "PASS", "authorization, token, and JWKS endpoints are absolute HTTPS (loopback HTTP only with --allow-loopback)")
    else:
        check("discovery-endpoints", "FAIL", "invalid endpoint(s): " + ", ".join(bad))
    methods = doc.get("code_challenge_methods_supported")
    if methods is None:
        check("discovery-pkce-s256", "PASS", "provider does not advertise code_challenge_methods_supported; S256 must still be confirmed during the live login step")
    elif "S256" in methods:
        check("discovery-pkce-s256", "PASS", "provider advertises S256 PKCE")
    else:
        check("discovery-pkce-s256", "FAIL", "provider does not advertise S256 PKCE")
    jwks_uri = str(doc.get("jwks_uri", ""))
    if not endpoint_ok(jwks_uri, issuer):
        check("jwks-fetch", "FAIL", "JWKS URI is not a valid endpoint")
    else:
        jwks, jerr = fetch(jwks_uri)
        if jwks is None:
            check("jwks-fetch", "FAIL", "JWKS fetch failed: " + jerr)
        else:
            keys = jwks.get("keys")
            if not isinstance(keys, list) or not keys:
                check("jwks-fetch", "FAIL", "JWKS document contains no keys")
            else:
                rs256 = sum(1 for k in keys if isinstance(k, dict) and k.get("alg") == "RS256")
                kids = [str(k.get("kid", "")) for k in keys if isinstance(k, dict)][:20]
                check("jwks-fetch", "PASS",
                      "JWKS has %d key(s), %d labelled RS256; kids: %s" % (len(keys), rs256, ", ".join(kids) if kids else "(none labelled)"))

# 4. Config shape (optional): values are compared, never printed, except
# generic role names and counts. A value-bearing client_secret field is
# a FAIL: secrets belong only in the environment variable named by
# client_secret_env, never in the config file.
if config_path:
    try:
        with open(config_path, "r", encoding="utf-8") as fh:
            cfg = json.load(fh)
    except Exception as exc:
        check("config-shape", "FAIL", "config file is not valid JSON: %s" % type(exc).__name__)
        cfg = None
    if cfg is not None:
        oidc = (cfg.get("admin") or {}).get("oidc") or {}
        problems = []
        if oidc.get("enabled") is not True:
            problems.append("admin.oidc.enabled is not true")
        if oidc.get("issuer_url") != issuer:
            problems.append("admin.oidc.issuer_url does not equal --issuer")
        if oidc.get("redirect_url") != callback:
            problems.append("admin.oidc.redirect_url does not equal --callback")
        if not isinstance(oidc.get("client_id"), str) or not oidc["client_id"]:
            problems.append("admin.oidc.client_id is missing")
        env_name = oidc.get("client_secret_env")
        if not isinstance(env_name, str) or not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", env_name or ""):
            problems.append("admin.oidc.client_secret_env is not a valid environment variable name")
        if "client_secret" in oidc:
            problems.append("admin.oidc.client_secret must not appear in the config file; use client_secret_env only")
        if audience and oidc.get("audience") != audience:
            problems.append("admin.oidc.audience does not equal --audience")
        if not isinstance(oidc.get("audience"), str) or not oidc.get("audience"):
            problems.append("admin.oidc.audience is missing")
        if not isinstance(oidc.get("role_claim"), str) or not oidc.get("role_claim"):
            problems.append("admin.oidc.role_claim is missing")
        mappings = oidc.get("role_mappings")
        if not isinstance(mappings, dict) or not (1 <= len(mappings) <= 100):
            problems.append("admin.oidc.role_mappings must map between 1 and 100 claim values")
        else:
            bad_roles = sorted({v for v in mappings.values() if v not in ("viewer", "operator", "admin")})
            if bad_roles:
                problems.append("admin.oidc.role_mappings contains roles outside viewer/operator/admin")
            bad_keys = [k for k in mappings if not isinstance(k, str) or not k or k.strip() != k]
            if bad_keys:
                problems.append("admin.oidc.role_mappings contains empty or untrimmed claim values")
        if problems:
            check("config-shape", "FAIL", "; ".join(problems))
        else:
            check("config-shape", "PASS",
                  "admin.oidc shape valid: role_claim present, %d role mapping(s) to viewer/operator/admin, secret referenced by environment name only (value never read)" % len(mappings))

import datetime
overall = "PASS" if all(c["status"] == "PASS" for c in checks) else "FAIL"
report = {
    "schema": "nexaroute.oidc-preflight.v1",
    "generated_utc": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "issuer": sanitize(issuer),
    "callback": sanitize(callback),
    "overall": overall,
    "checks": checks,
    "note": "Preflight only: discovery/JWKS/config-shape. Not live acceptance evidence; see docs/reports/OIDC_OPERATOR_RUNBOOK_647.md.",
}
print(json.dumps(report, indent=2))
sys.exit(0 if overall == "PASS" else 1)
PYEOF
)

if [[ -n "$OUT" ]]; then
	mkdir -p "$(dirname "$OUT")"
	printf '%s\n' "$REPORT" > "$OUT"
	chmod 0600 "$OUT"
fi
printf '%s\n' "$REPORT"
# The python process above already decided PASS/FAIL; re-derive the exit
# code from the report so a write failure above cannot mask a FAIL.
if printf '%s' "$REPORT" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["overall"] == "PASS" else 1)'; then
	exit 0
else
	exit 1
fi
