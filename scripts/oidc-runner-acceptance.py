#!/usr/bin/env python3
"""Runner-only OIDC acceptance for issue 647, against a disposable Keycloak.

This script is NOT a real organizational tenant. It runs a Keycloak
container on the SAME runner, over loopback HTTP, with disposable
test users created inside the run. Its evidence class is
runner-keycloak: single-runner Keycloak container, loopback only,
disposable test users only. It is NOT an organizational tenant,
NOT Entra ID / Okta / Auth0 / Google, NOT production, and NOT a
claim about provider-specific claim shapes or clock-skew policy
beyond what this run actually exercised.

What it does, in order:
  1. Start Keycloak (start-dev) in a container, wait for ready.
  2. Via the Keycloak Admin REST API, create realm "nexaroute",
     a confidential client, two groups, three users, and a
     group-membership protocol mapper that puts a top-level
     "groups" claim into ID tokens. The gateway reads exactly
     one claim name (admin.oidc.role_claim); a nested claim
     alone is denied by the gateway, so the mapper is required
     and its shape is recorded in the summary.
  3. Start the real gateway binary with admin.oidc pointing at
     that Keycloak issuer, over loopback HTTP (permitted for
     loopback issuers by the gateway validator).
  4. Drive the real Authorization Code + PKCE login flow with
     plain HTTP (no browser): gateway /admin/auth/login ->
     Keycloak login form -> POST credentials -> gateway
     /admin/auth/oidc/callback -> session cookie. Cookies are
     handled manually, because the gateway sets Secure cookies
     that a cookie jar would not send back over loopback HTTP;
     the script sends the session cookie explicitly, exactly as
     a browser would over HTTPS.
  5. Checks, all with status codes only (no token, code, or
     session value is written to any report):
       admin user (group nexaroute-admins -> admin):
         login succeeds, /admin/api/audit returns 200,
         /admin/api/snapshot returns 200
       viewer user (group nexaroute-viewers -> viewer):
         login succeeds, /admin/api/snapshot returns 200,
         /admin/api/audit returns 403
       unmapped user (no group):
         login ends with 401 and no session cookie
       logout (admin user, fresh login):
         POST /admin/auth/logout with a CSRF token returns
         200 or 303, and /admin/api/audit afterwards is 401

Secrets: every password and the client secret are generated
inside the run, kept only in a temporary directory with mode
0700 (files 0600), and destroyed with the container when the
script exits. The summary contains usernames and group names
of disposable test accounts only, never passwords, secrets,
codes, tokens, or session values.

Exit codes: 0 all checks PASS; 1 a check FAIL; 2 usage error.
On failure the summary is still written when possible, with
the failing check marked FAIL and the error recorded (redacted
of any secret-looking value).
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import re
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

REALM = "nexaroute"
CLIENT_ID = "nexaroute-control-plane"
ADMIN_GROUP = "nexaroute-admins"
VIEWER_GROUP = "nexaroute-viewers"
ROLE_CLAIM = "groups"


def redact(text: str) -> str:
    text = re.sub(r"(?i)([?&](?:password|passwd|pwd|token|secret|key|code|session)=)[^&\s]+", r"\1***", text)
    return text


def redact_url(url: str) -> str:
    """Keep only scheme, host, and path; drop query and fragment.

    Flow URLs carry one-time state, session codes, and tab ids. They are
    not credentials, but the summary is published as evidence, so it
    records only the endpoint path.
    """
    parts = urllib.parse.urlsplit(url or "")
    return urllib.parse.urlunsplit((parts.scheme, parts.netloc, parts.path, "", ""))


def login_error(body: bytes) -> str:
    """Extract a short, non-secret Keycloak form error for diagnostics."""
    text = body.decode("utf-8", "replace")
    for pattern in (
        r'id="input-error"[^>]*>([^<]{0,160})',
        r'id="kc-error-message"[^>]*>.*?<p[^>]*>([^<]{0,160})',
        r'class="pf-c-alert__title"[^>]*>[^<]*<span[^>]*>([^<]{0,160})',
        r'<title>([^<]{0,120})</title>',
    ):
        m = re.search(pattern, text, re.S)
        if m and m.group(1).strip():
            return re.sub(r"\s+", " ", m.group(1)).strip()
    return ""


class Fail(Exception):
    pass


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Return every 3xx to the caller instead of following it.

    The login flow must be driven step by step: automatic redirects hide
    the intermediate Set-Cookie headers and turn the form POST straight
    into the gateway callback, which is why the flow needs manual
    handling here.
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


_OPENER = urllib.request.build_opener(_NoRedirect)


def http(method: str, url: str, data=None, headers=None, timeout: float = 30.0):
    body = None
    hdrs = dict(headers or {})
    if data is not None:
        if isinstance(data, dict):
            body = urllib.parse.urlencode(data).encode()
            hdrs.setdefault("Content-Type", "application/x-www-form-urlencoded")
        elif isinstance(data, (bytes, str)):
            body = data.encode() if isinstance(data, str) else data
        else:
            body = json.dumps(data).encode()
            hdrs.setdefault("Content-Type", "application/json")
    req = urllib.request.Request(url, data=body, headers=hdrs, method=method)
    try:
        with _OPENER.open(req, timeout=timeout) as resp:
            return resp.status, resp.headers, resp.read()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.headers, exc.read()
    except Exception as exc:  # connection refused, timeout, DNS
        return None, {}, str(exc).encode()


def get_header(headers, name: str) -> str:
    try:
        return headers.get(name, "") or ""
    except Exception:
        return ""


def parse_cookies(headers) -> dict:
    out = {}
    try:
        raw = headers.get_all("Set-Cookie") or []
    except Exception:
        raw = []
    for line in raw:
        part = line.split(";", 1)[0].strip()
        if "=" in part:
            name, value = part.split("=", 1)
            out[name.strip()] = value.strip()
    return out


def cookie_header(cookies: dict) -> str:
    return "; ".join(f"{k}={v}" for k, v in cookies.items())


class Session:
    """Manual redirect + cookie handling for the login flow."""

    def __init__(self):
        self.cookies: dict = {}

    def request(self, method: str, url: str, data=None, headers=None):
        hdrs = dict(headers or {})
        if self.cookies:
            hdrs["Cookie"] = cookie_header(self.cookies)
        status, resp_headers, body = http(method, url, data=data, headers=hdrs)
        self.cookies.update(parse_cookies(resp_headers))
        return status, resp_headers, body

    def follow(self, url: str, max_steps: int = 20):
        """Follow redirects; return (final_status, final_url, body, history)."""
        history = []
        current = url
        for _ in range(max_steps):
            status, headers, body = self.request("GET", current)
            history.append((current, status))
            if status in (301, 302, 303, 307, 308):
                location = get_header(headers, "Location")
                if not location:
                    return status, current, body, history
                current = urllib.parse.urljoin(current, location)
                continue
            return status, current, body, history
        raise Fail(f"too many redirects following {url}")


def wait_for(url: str, timeout: float, want: int = 200) -> None:
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        status, _, _ = http("GET", url, timeout=5.0)
        if status == want:
            return
        last = status
        time.sleep(1.0)
    raise Fail(f"timed out waiting for {url} to return {want} (last={last})")


def kc_admin(api: str, token: str, method: str, path: str, payload=None):
    # The Keycloak Admin API expects JSON. Passing a dict straight to
    # http() form-encodes it and Keycloak answers 415 Unsupported Media
    # Type (observed in the first local run of this harness).
    headers = {"Authorization": f"Bearer {token}"}
    data = None
    if payload is not None:
        data = json.dumps(payload).encode()
        headers["Content-Type"] = "application/json"
    status, resp_headers, body = http(method, f"{api}/admin{path}",
                                      data=data, headers=headers)
    return status, resp_headers, body


def kc_location(headers) -> str:
    loc = get_header(headers, "Location")
    return loc.rstrip("/").split("/")[-1] if loc else ""


def resolve_work_dir() -> pathlib.Path:
    """Return a private work directory, never the checkout.

    An unset OIDC_RUNNER_WORKDIR must fall back to a fresh temp dir.
    The older `Path(os.environ.get(..., "")) or None` treated Path(".")
    as truthy, so the script used and later removed the whole cwd.
    """
    work_env = os.environ.get("OIDC_RUNNER_WORKDIR", "").strip()
    if work_env:
        work = pathlib.Path(work_env).resolve()
        work.mkdir(parents=True, exist_ok=True)
    else:
        import tempfile
        work = pathlib.Path(tempfile.mkdtemp(prefix="oidc-runner-"))
    os.chmod(work, 0o700)
    return work


def selftest() -> int:
    """Deterministic checks for the two runner harness regressions.

    No Docker, Keycloak, gateway, or network is used. This proves the
    work-directory resolution and the management-port readiness wiring
    only; it is not OIDC acceptance evidence.
    """
    import tempfile

    errors = []
    source = pathlib.Path(__file__).read_text(encoding="utf-8")
    saved = os.environ.pop("OIDC_RUNNER_WORKDIR", None)
    try:
        work = resolve_work_dir()
        if work.resolve() == pathlib.Path.cwd().resolve():
            errors.append("default work directory resolved to the checkout cwd")
        if not str(work).startswith(tempfile.gettempdir()):
            errors.append("default work directory is not under the temp directory")
        work.rmdir()
        os.environ["OIDC_RUNNER_WORKDIR"] = "   "
        blank = resolve_work_dir()
        if blank.resolve() == pathlib.Path.cwd().resolve():
            errors.append("blank OIDC_RUNNER_WORKDIR resolved to the checkout cwd")
        blank.rmdir()
    finally:
        os.environ.pop("OIDC_RUNNER_WORKDIR", None)
        if saved is not None:
            os.environ["OIDC_RUNNER_WORKDIR"] = saved

    # Split the needle so this checker does not match its own source.
    bad_workdir = ('pathlib.Path(os.environ.get("OIDC_RUNNER_' +
                   'WORKDIR", "")) or None')
    if bad_workdir in source:
        errors.append("blank-env Path('') bug reintroduced")
    if '"-p", f"127.0.0.1:{kc_mgmt_port}:9000"' not in source:
        errors.append("Keycloak management port 9000 is not published")
    if 'wait_for(f"{kc_mgmt_base}/health/ready"' not in source:
        errors.append("readiness is not polled on the Keycloak management port")
    if "if work.resolve() != pathlib.Path.cwd().resolve():" not in source:
        errors.append("cleanup guard against deleting the checkout is missing")
    if 'headers["Content-Type"] = "application/json"' not in source:
        errors.append("Keycloak Admin API request is not sent as JSON")
    if "urllib.request.build_opener(_NoRedirect)" not in source:
        errors.append("HTTP redirects are followed automatically, hiding flow cookies")
    if redact_url("http://127.0.0.1:1/admin/auth/oidc/callback?state=x&code=y") != "http://127.0.0.1:1/admin/auth/oidc/callback":
        errors.append("summary URLs would leak flow query parameters")
    for field in ('"firstName"', '"lastName"', '"emailVerified": True'):
        if field not in source:
            errors.append(f"Keycloak test user is missing required profile field {field}")

    if errors:
        for err in errors:
            print(f"FAIL: oidc-runner-selftest: {err}")
        return 1
    print("PASS: oidc-runner-selftest: workdir resolution and management-port readiness")
    return 0


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="Runner-only OIDC acceptance, issue 647")
    parser.add_argument("--out-dir", default="oidc-runner-reports")
    parser.add_argument("--gateway-binary", default="")
    parser.add_argument("--keycloak-image", default="quay.io/keycloak/keycloak:26.0")
    parser.add_argument("--keycloak-port", type=int, default=18081)
    # Keycloak 25+ exposes /health/* on its management port (9000 in the
    # container), not on the main HTTP port. Publish it separately and
    # poll readiness there; polling 8080/health/ready always times out.
    parser.add_argument("--keycloak-management-port", type=int, default=18082)
    parser.add_argument("--gateway-port", type=int, default=18080)
    parser.add_argument("--self-test", action="store_true",
                        help="deterministic harness checks only; no Docker or network")
    args = parser.parse_args(argv)
    if args.self_test:
        return selftest()
    if not args.gateway_binary:
        parser.error("--gateway-binary is required unless --self-test is used")

    out_dir = pathlib.Path(args.out_dir).resolve()
    out_dir.mkdir(parents=True, exist_ok=True)
    work = resolve_work_dir()

    kc_port = args.keycloak_port
    kc_mgmt_port = args.keycloak_management_port
    gw_port = args.gateway_port
    kc_base = f"http://127.0.0.1:{kc_port}"
    kc_mgmt_base = f"http://127.0.0.1:{kc_mgmt_port}"
    gw_base = f"http://127.0.0.1:{gw_port}"
    issuer = f"{kc_base}/realms/{REALM}"
    redirect_url = f"{gw_base}/admin/auth/oidc/callback"
    container = f"nexaroute-oidc-acceptance-{secrets.token_hex(4)}"

    checks = []

    def check(check_id: str, name: str, ok: bool, detail: str = ""):
        checks.append({"id": check_id, "name": name, "status": "PASS" if ok else "FAIL",
                       "detail": redact(detail)})
        print(f"{'PASS' if ok else 'FAIL'}: {check_id} {name}")
        return ok

    def write_summary(keycloak_version, extra=None):
        doc = {
            "schema": "nexaroute.oidc-runner-acceptance.v1",
            "issue": 647,
            "evidence_class": "runner-keycloak",
            "keycloak_image": args.keycloak_image,
            "keycloak_version_recorded": keycloak_version,
            "issuer": issuer,
            "client_id": CLIENT_ID,
            "role_claim": ROLE_CLAIM,
            "role_mappings": {ADMIN_GROUP: "admin", VIEWER_GROUP: "viewer"},
            "test_users": ["oidc-admin-user", "oidc-viewer-user", "oidc-unmapped-user"],
            "checks": checks,
            "limitations": [
                "Keycloak container on the same runner, loopback HTTP, disposable test users only.",
                "NOT a real organizational tenant, NOT Entra ID / Okta / Auth0 / Google, NOT production.",
                "Provider-specific claim shapes and clock-skew policy beyond this run are NOT tested.",
                "JWKS key rotation, session expiry, and audit-chain integrity are NOT tested here; "
                "they are covered by the gateway unit tests, not by this runner run.",
            ],
        }
        if extra:
            doc.update(extra)
        (out_dir / "oidc-runner-summary.json").write_text(
            json.dumps(doc, indent=2, sort_keys=True), encoding="utf-8")
        return doc

    gateway_proc = None
    keycloak_version = "NOT_RECORDED"
    try:
        # --- secrets, disposable only, never printed ---
        kc_admin_user = "kc-admin"
        kc_admin_pw = "kc-" + secrets.token_hex(16)
        client_secret = "cs-" + secrets.token_hex(16)
        passwords = {u: "pw-" + secrets.token_hex(12)
                     for u in ("oidc-admin-user", "oidc-viewer-user", "oidc-unmapped-user")}

        # --- Keycloak container ---
        proc = subprocess.run(["docker", "run", "-d", "--name", container,
                               "-p", f"127.0.0.1:{kc_port}:8080",
                               "-p", f"127.0.0.1:{kc_mgmt_port}:9000",
                               "-e", f"KC_BOOTSTRAP_ADMIN_USERNAME={kc_admin_user}",
                               "-e", f"KC_BOOTSTRAP_ADMIN_PASSWORD={kc_admin_pw}",
                               "-e", "KC_HEALTH_ENABLED=true",
                               args.keycloak_image, "start-dev"],
                              capture_output=True, text=True)
        if proc.returncode != 0:
            raise Fail(f"docker run keycloak failed: {redact(proc.stderr[-500:])}")
        wait_for(f"{kc_mgmt_base}/health/ready", 180.0)
        check("keycloak.started", "Keycloak container ready on loopback", True)

        status, _, body = http("POST", f"{kc_base}/realms/master/protocol/openid-connect/token",
                               data={"grant_type": "password", "client_id": "admin-cli",
                                     "username": kc_admin_user, "password": kc_admin_pw})
        if status != 200:
            raise Fail(f"keycloak admin token failed: status={status}")
        admin_token = json.loads(body)["access_token"]

        status, _, body = http("GET", f"{kc_base}/admin/serverinfo",
                               headers={"Authorization": f"Bearer {admin_token}"})
        if status == 200:
            try:
                keycloak_version = json.loads(body)["systemInfo"]["version"]
            except Exception:
                keycloak_version = "UNKNOWN"
        check("keycloak.version", "Keycloak version recorded from serverinfo, not from the image tag",
              keycloak_version not in ("NOT_RECORDED", "UNKNOWN"), f"version={keycloak_version}")

        status, _, _ = kc_admin(kc_base, admin_token, "POST", "/realms",
                                {"realm": REALM, "enabled": True})
        if status not in (201, 409):
            raise Fail(f"create realm failed: status={status}")
        status, _, _ = kc_admin(kc_base, admin_token, "POST", f"/realms/{REALM}/clients",
                                {"clientId": CLIENT_ID, "secret": client_secret, "enabled": True,
                                 "standardFlowEnabled": True, "directAccessGrantsEnabled": False,
                                 "serviceAccountsEnabled": False, "publicClient": False,
                                 "redirectUris": [redirect_url], "webOrigins": []})
        if status not in (201, 409):
            raise Fail(f"create client failed: status={status}")
        status, _, body = kc_admin(kc_base, admin_token, "GET",
                                   f"/realms/{REALM}/clients?clientId={CLIENT_ID}")
        clients = json.loads(body)
        if not clients:
            raise Fail("client not found after creation")
        client_uuid = clients[0]["id"]
        check("keycloak.client", "Realm, confidential client, and redirect URI created", True,
              f"redirect={redirect_url}")

        group_ids = {}
        for group in (ADMIN_GROUP, VIEWER_GROUP):
            status, headers, _ = kc_admin(kc_base, admin_token, "POST",
                                          f"/realms/{REALM}/groups", {"name": group})
            if status == 201:
                group_ids[group] = kc_location(headers)
            elif status == 409:
                status2, _, body2 = kc_admin(kc_base, admin_token, "GET",
                                             f"/realms/{REALM}/groups?search={group}")
                group_ids[group] = json.loads(body2)[0]["id"]
            else:
                raise Fail(f"create group {group} failed: status={status}")
        status, _, _ = kc_admin(kc_base, admin_token, "POST",
                                f"/realms/{REALM}/clients/{client_uuid}/protocol-mappers/models",
                                {"name": "groups", "protocol": "openid-connect",
                                 "protocolMapper": "oidc-group-membership-mapper",
                                 "consentRequired": False,
                                 "config": {"claim.name": ROLE_CLAIM, "full.path": "false",
                                            "id.token.claim": "true", "access.token.claim": "true",
                                            "userinfo.token.claim": "true"}})
        if status not in (201, 409):
            raise Fail(f"create groups mapper failed: status={status}")
        check("keycloak.mapper", "Top-level groups claim mapper created (gateway reads one claim name)",
              True, f"claim={ROLE_CLAIM}")

        user_ids = {}
        for username in passwords:
            # Keycloak 26's default user profile requires email,
            # firstName, and lastName; without them the login flow stops
            # at a VERIFY_PROFILE required action instead of the callback.
            status, headers, _ = kc_admin(kc_base, admin_token, "POST",
                                          f"/realms/{REALM}/users",
                                          {"username": username, "enabled": True, "emailVerified": True,
                                           "email": f"{username}@example.invalid",
                                           "firstName": "OIDC", "lastName": username,
                                           "credentials": [{"type": "password", "value": passwords[username],
                                                            "temporary": False}]})
            if status == 201:
                user_ids[username] = kc_location(headers)
            elif status == 409:
                status2, _, body2 = kc_admin(kc_base, admin_token, "GET",
                                             f"/realms/{REALM}/users?username={username}")
                user_ids[username] = json.loads(body2)[0]["id"]
            else:
                raise Fail(f"create user {username} failed: status={status}")
        for username, group in (("oidc-admin-user", ADMIN_GROUP), ("oidc-viewer-user", VIEWER_GROUP)):
            status, _, _ = kc_admin(kc_base, admin_token, "PUT",
                                    f"/realms/{REALM}/users/{user_ids[username]}/groups/{group_ids[group]}")
            if status not in (204, 409):
                raise Fail(f"assign {username} to {group} failed: status={status}")
        check("keycloak.users", "Three test users created; admin and viewer assigned to their groups", True)

        # --- gateway config and process ---
        security_dir = work / "security"
        security_dir.mkdir(mode=0o700, exist_ok=True)
        config = {
            "listen": f"127.0.0.1:{gw_port}",
            "providers": [{"id": "acceptance", "type": "openai_compatible",
                           "base_url": "http://127.0.0.1:9", "api_key": "acceptance-plaintext",
                           "enabled": False}],
            "admin": {
                "bind_local_only": True,
                "emergency_access_enabled": False,
                "security_store_path": str(security_dir / "nexaroute-security.db"),
                "session_ttl_seconds": 28800,
                "idle_timeout_seconds": 1800,
                "audit_retention_days": 365,
                "audit_max_events": 100000,
                "oidc": {
                    "enabled": True,
                    "issuer_url": issuer,
                    "client_id": CLIENT_ID,
                    "client_secret_env": "NEXAROUTE_OIDC_CLIENT_SECRET",
                    "redirect_url": redirect_url,
                    "audience": CLIENT_ID,
                    "role_claim": ROLE_CLAIM,
                    "role_mappings": {ADMIN_GROUP: "admin", VIEWER_GROUP: "viewer"},
                },
            },
        }
        config_path = work / "config.json"
        config_path.write_text(json.dumps(config, indent=2), encoding="utf-8")
        os.chmod(config_path, 0o600)
        env = dict(os.environ)
        env["NEXAROUTE_OIDC_CLIENT_SECRET"] = client_secret
        log_path = work / "gateway.log"
        with open(log_path, "wb") as log_fh:
            gateway_proc = subprocess.Popen([args.gateway_binary, "-no-browser", "-config", str(config_path)],
                                            stdout=log_fh, stderr=subprocess.STDOUT, env=env)
        try:
            wait_for(f"{gw_base}/healthz", 60.0)
        except Fail:
            tail = ""
            try:
                tail = log_path.read_text(encoding="utf-8", errors="replace")[-2000:]
            except Exception:
                pass
            raise Fail(f"gateway did not become healthy: {redact(tail)}")
        check("gateway.booted", "Gateway healthy on loopback with OIDC config", True)

        def do_login(username: str):
            sess = Session()
            status, final_url, body, history = sess.follow(f"{gw_base}/admin/auth/login")
            text = body.decode("utf-8", "replace")
            form = re.search(r'<form[^>]+action="([^"]+)"', text)
            if not form:
                raise Fail(f"keycloak login form not found for {username}: final={final_url} status={status}")
            action = urllib.parse.urljoin(final_url, form.group(1).replace("&amp;", "&"))
            # POST credentials, then follow redirects manually.
            status, headers, body = sess.request("POST", action,
                                                 data={"username": username,
                                                       "password": passwords[username],
                                                       "credentialId": ""})
            for _ in range(20):
                if status in (301, 302, 303, 307, 308):
                    loc = get_header(headers, "Location")
                    if not loc:
                        break
                    # urljoin with the current URL is correct for both
                    # absolute and relative Location values.
                    nxt = urllib.parse.urljoin(action, loc)
                    status, headers, body = sess.request("GET", nxt)
                    action = nxt
                    continue
                break
            return sess, status, body, action, login_error(body)

        def api(sess: Session, path: str, method: str = "GET", extra_headers=None):
            return sess.request(method, f"{gw_base}{path}", headers=extra_headers or {})

        # --- admin user ---
        sess_admin, status, _, url_admin, err_admin = do_login("oidc-admin-user")
        admin_session = "nexaroute_admin_session" in sess_admin.cookies and sess_admin.cookies["nexaroute_admin_session"] != ""
        check("oidc.login_admin", "Mapped admin user completes PKCE login and holds a session cookie",
              status in (200, 303) and admin_session, f"final_status={status} session={'present' if admin_session else 'absent'} err={err_admin} url={redact_url(url_admin)}")
        if admin_session:
            st, _, _ = api(sess_admin, "/admin/api/audit?limit=1")
            check("oidc.admin_audit", "Admin user can read the audit API", st == 200, f"status={st}")
            st, _, _ = api(sess_admin, "/admin/api/snapshot")
            check("oidc.admin_snapshot", "Admin user can read the snapshot API", st == 200, f"status={st}")
        else:
            check("oidc.admin_audit", "Admin user can read the audit API", False, "no session")
            check("oidc.admin_snapshot", "Admin user can read the snapshot API", False, "no session")

        # --- viewer user ---
        sess_viewer, status, _, url_viewer, err_viewer = do_login("oidc-viewer-user")
        viewer_session = "nexaroute_admin_session" in sess_viewer.cookies and sess_viewer.cookies["nexaroute_admin_session"] != ""
        check("oidc.login_viewer", "Mapped viewer user completes PKCE login and holds a session cookie",
              status in (200, 303) and viewer_session, f"final_status={status} session={'present' if viewer_session else 'absent'} err={err_viewer} url={redact_url(url_viewer)}")
        if viewer_session:
            st, _, _ = api(sess_viewer, "/admin/api/snapshot")
            check("oidc.viewer_snapshot", "Viewer user can read the snapshot API", st == 200, f"status={st}")
            st, _, _ = api(sess_viewer, "/admin/api/audit?limit=1")
            check("oidc.viewer_audit_denied", "Viewer user is denied the audit API (admin only)", st == 403, f"status={st}")
        else:
            check("oidc.viewer_snapshot", "Viewer user can read the snapshot API", False, "no session")
            check("oidc.viewer_audit_denied", "Viewer user is denied the audit API (admin only)", False, "no session")

        # --- unmapped user ---
        sess_unmapped, status, _, url_unmapped, err_unmapped = do_login("oidc-unmapped-user")
        unmapped_session = "nexaroute_admin_session" in sess_unmapped.cookies and sess_unmapped.cookies["nexaroute_admin_session"] != ""
        check("oidc.unmapped_denied", "Unmapped user is denied at the callback (401, no session cookie)",
              status == 401 and not unmapped_session,
              f"final_status={status} session={'present' if unmapped_session else 'absent'} err={err_unmapped} url={redact_url(url_unmapped)}")

        # --- logout (fresh admin login) ---
        sess_logout, status, _, _, _err_logout = do_login("oidc-admin-user")
        logout_ok = False
        after_status = None
        if "nexaroute_admin_session" in sess_logout.cookies:
            st, headers, body = api(sess_logout, "/admin/api/csrf-token")
            token = ""
            try:
                token = json.loads(body).get("token", "")
            except Exception:
                token = ""
            if token:
                st2, _, _ = api(sess_logout, "/admin/auth/logout", method="POST",
                                extra_headers={"X-NexaRoute-CSRF": token})
                logout_ok = st2 in (200, 303)
                st3, _, _ = api(sess_logout, "/admin/api/audit?limit=1")
                after_status = st3
        check("oidc.logout", "Logout revokes the session (audit API is 401 afterwards)",
              logout_ok and after_status == 401,
              f"logout_status_ok={logout_ok} audit_after={after_status}")

        failed = [c for c in checks if c["status"] != "PASS"]
        write_summary(keycloak_version, {"result": "FAIL" if failed else "PASS",
                                         "failed_checks": [c["id"] for c in failed]})
        return 1 if failed else 0
    except Fail as exc:
        check("run.aborted", "Run completed without an infrastructure error", False, str(exc))
        try:
            write_summary(keycloak_version, {"result": "FAIL", "error": redact(str(exc))})
        except Exception:
            pass
        return 1
    except Exception as exc:  # never leak a traceback with secrets
        check("run.aborted", "Run completed without an infrastructure error", False, type(exc).__name__)
        try:
            write_summary(keycloak_version, {"result": "FAIL", "error": type(exc).__name__})
        except Exception:
            pass
        return 1
    finally:
        if gateway_proc is not None:
            try:
                gateway_proc.terminate()
                gateway_proc.wait(timeout=10)
            except Exception:
                try:
                    gateway_proc.kill()
                except Exception:
                    pass
        subprocess.run(["docker", "rm", "-f", container], capture_output=True)
        # Work directory (config, security DB, logs, generated secrets)
        # is deliberately NOT in the out dir, and is removed here. Never
        # delete the checkout if OIDC_RUNNER_WORKDIR points at it: the
        # earlier `Path("") or None` bug made an unset/blank value resolve
        # to "." and rmtree removed the whole workspace, including the
        # summary the workflow then tried to upload.
        try:
            import shutil
            if work.resolve() != pathlib.Path.cwd().resolve():
                shutil.rmtree(work, ignore_errors=True)
        except Exception:
            pass


if __name__ == "__main__":
    sys.exit(main())
