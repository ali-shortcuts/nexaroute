# OIDC organizational-tenant operator runbook (#647)

**Status: BLOCKED — not executed on any real tenant.**

Date opened: 2026-10-27. Issue:
[#647](https://github.com/ali-shortcuts/nexaroute/issues/647).
Companion checklist (local coverage + the item-by-item live checklist):
PR [#666](https://github.com/ali-shortcuts/nexaroute/pull/666)
(`docs/reports/OIDC_LIVE_TENANT_ACCEPTANCE_647.md` once that PR lands).
Automated harness reference:
`docs/reports/verification-slices/live-acceptance-harness.md`
(`nexaroute external-accept`).

This document is the **procedure** an operator follows, in order, on
an operator-owned staging tenant. The #666 checklist is the
**scorecard**; this runbook is the **execution order**. Neither
document, and no local test, upgrades the live tenant from
**UNVERIFIED**. A local run of any step below (loopback fixture,
in-process IdP) is local evidence and must be labeled as such.

Consolidation note (issue #647 completion): this runbook, as updated
here, is the **single documented acceptance process**. It
consolidates the #666 checklist (scorecard, carried unchanged as
`docs/reports/OIDC_LIVE_TENANT_ACCEPTANCE_647.md`) and the #647
security review, and corrects four inaccuracies found in the
original #671 text: the §0 cross-reference now points at the live
cases in §5–§6 (not §7, which is the harness cross-check only); §1
now states the `nbf` 5-minute leeway the original omitted; §3 no
longer overstates the preflight PASS conditions (S256-absent and
kid-less JWKS caveats are stated); and the clock wording is unified
with the #666 checklist (implementation rule: zero `exp` leeway;
the operator records the observed offset, never a tolerance the
gateway does not have). No second runbook is created by this
consolidation.

2026-10-30 re-audit (main `7daa36d`): no operator-owned tenant,
credentials, test users, or run authorization were supplied, and no
live IdP calls were made. The local suite gained four gap tests
(exact trailing-slash issuer rejection, cross-transaction nonce,
token-exchange client/redirect/grant, duplicate/mixed role values).
Every live step in §5–§7 remains **BLOCKED/NOT_RUN**; the exact
operator inputs are listed in `OIDC_LIVE_TENANT_ACCEPTANCE_647.md`.

2026-10-30 follow-up re-audit (main `7a2623a`): still no
operator-owned tenant, credentials, test users, or run authorization,
and zero live IdP calls. The local suite gained five further negatives
(nested-claim denial, short-state denial with transaction preserved,
session-read-failure fail-closed, cross-host HTTPS discovery accepted,
login-audit subject attribution); see
`OIDC_LIVE_TENANT_ACCEPTANCE_647.md` §"2026-10-30 follow-up re-audit"
and `OIDC_647_FOLLOWUP_2026-10-30.md`. Every live step in §5–§7
remains **BLOCKED/NOT_RUN**. Note the top-level-claim requirement
recorded in the checklist §9: a tenant whose roles are nested-only
must map them to a top-level claim on the tenant side, or the
mapped-role cases cannot pass — by gateway design, not by defect.

## 0. What blocks this run, and the rules that do not bend

No operator-owned tenant, client, test user, or signing-key access
was supplied for this work. Until an operator supplies them, every
live case in §5–§6 (Steps C–D) and the harness cross-check in §7
stays **BLOCKED** and issue #647 must not be closed
on the strength of local tests.

Rules for the whole run:

- Staging tenant only. Never point this procedure at a production
  tenant, and never use a real operator's personal account as a test
  user. Use dedicated test users: one per mapped role, one outside
  every mapped group.
- Secrets only through the operator's secret injector or environment.
  The client secret lives only in the environment variable named by
  `admin.oidc.client_secret_env`. Never paste a client secret, access
  token, ID token, authorization code, session cookie, PKCE verifier,
  state, or nonce into an issue, PR, config file, command line, shell
  history, or committed evidence. The preflight script (§3) accepts
  no credential argument at all, by design.
- Never weaken a control to make a case pass: do not loosen the
  issuer pin, audience, role mappings, callback URL, or CSRF check,
  and do not enable an emergency-access fallback to work around a
  failed OIDC login. A FAIL is a finding, not a reason to relax the
  config. If a case fails, stop, record it, and fix the tenant or
  gateway configuration through the normal change path.
- Review every report before attaching it anywhere. Hostnames,
  tenant ids, client ids, and model ids are metadata that can
  themselves be sensitive, even after values are redacted.
- Rollback at any point: restore the previously known-good gateway
  config, restart, and confirm admin access through the previously
  working method. Do not leave a half-configured OIDC client as the
  only admin path.

## 1. Host preparation (do this first; it decides clock cases)

1. Synchronize the gateway host clock (NTP) and record `date -u`
   before the run and after any clock-sensitive case.
2. Record the gateway build SHA (`git rev-parse HEAD` on the deployed
   checkout, or the release version) and the tenant's provider name
   and version in the evidence record (§8).
3. Know this implementation fact before interpreting clock results
   (verified in `internal/httpapi/oidc.go` against go-oidc v3.15.0):
   the verifier is built with `oidc.Config{ClientID: ...}` only — no
   expiry leeway and no custom clock. An ID token whose `exp` is
   already past is rejected, with **zero tolerance** for provider
   clock skew. `nbf`, when present, is enforced by go-oidc with a
   hard-coded **5-minute leeway** (a token whose `nbf` is up to 5
   minutes in the future is accepted; beyond that it is rejected) —
   this is library behaviour the gateway does not configure, and
   the original #671 text omitted it. Separately, `iat` is not
   independently enforced at all — not even a far-future `iat` is a
   rejection reason on its own (pinned by local test, recorded here
   as a deliberate limitation, never as "iat validated"); only
   `exp`, signature, issuer, audience, and nonce are checked (plus
   the gateway's own role-claim rules). So a provider clock running
   ahead of the gateway fails logins closed rather than being
   absorbed by a tolerance, and a future `iat` alone is not a
   rejection reason. Record the observed host/IdP offset (compare
   the gateway `date -u` with the IdP response `Date` header); do
   not invent a skew policy the gateway does not have, and do not
   add a leeway to the test config to make a case pass.
4. Know the verifier and session limits before the run. The
   verifier is effectively **RS256-only**: no `SupportedSigningAlgs`
   is configured, so go-oidc defaults to RS256; a tenant signing
   only with ES256/PS256/EdDSA fails closed until the verifier is
   configured otherwise — confirm RS256 on the tenant before
   starting. Pending OIDC login transactions are **process-local
   in-memory** even when sessions/audit are shared via
   PostgreSQL/Redis: the callback must return to the instance that
   started the login (single instance or sticky routing for the
   whole flow), and a gateway restart drops pending logins
   fail-closed. Admin sessions are **not bound to IP or User-Agent**:
   the login flow is bound by a browser-binding cookie, but a
   stolen session cookie replays until TTL/idle expiry, logout, or
   revocation — mitigations are the opaque random token,
   `HttpOnly`/`Secure`/`SameSite=Lax` cookie flags, rotation on
   privilege transition, and policy/issuer-change revocation. Do
   not record a session as "device-bound".

## 2. Tenant registration (per provider; confirm on the tenant)

Create a **confidential** client (Authorization Code flow, PKCE
S256). Register exactly one callback URL:

```
https://<gateway-host>/admin/auth/oidc/callback
```

That path is the gateway's route (`internal/httpapi/server.go`:
`/admin/auth/oidc/callback`); any other path is a different
registration and must be rejected by the provider, not accepted by
the gateway. Record the provider's actual issuer URL — it must
match `admin.oidc.issuer_url` character-for-character, including
trailing-slash behavior.

| Provider | Issuer shape to confirm on the tenant | Role-claim starting point (confirm, do not assume) |
|---|---|---|
| Keycloak | `https://<host>/realms/<realm>` | `groups`, or `realm_access.roles` (nested — record whether the configured `role_claim` reaches it; the gateway reads one claim name, string or array of strings) |
| Entra ID | `https://login.microsoftonline.com/<tenant>/v2.0` | `groups` (may arrive as ids; optional-claims / overage behavior must be recorded from the actual token shape) |
| Okta | `https://<org>.okta.com/oauth2/<auth-server>` | `groups` |
| Auth0 | `https://<domain>/` | Provider-specific roles claim (custom claim or Authorization extension output — record the exact claim name the tenant emits) |
| Google | `https://accounts.google.com` | Google issues no group claim; if the tenant cannot emit a claim the gateway can map, the mapped-role cases cannot pass on that tenant — record that as BLOCKED for that provider shape, not as a gateway pass |

If the tenant's actual claim shape differs from the table, the table
is wrong, not the tenant: record the actual shape and set
`admin.oidc.role_claim` / `role_mappings` to match it explicitly.

## 3. Step A — secret-free preflight (no login, no credentials)

Run this before any browser login. It fetches only the public
discovery document and JWKS and validates the config shape:

```bash
./scripts/oidc-live-preflight.sh \
  --issuer 'https://<tenant-issuer>' \
  --callback 'https://<gateway-host>/admin/auth/oidc/callback' \
  --audience '<configured-audience>' \
  --config /etc/nexaroute/config.json \
  --out /tmp/oidc-preflight-$(date -u +%Y%m%dT%H%M%SZ).json
```

Expected: exit 0, every check PASS — issuer shape, callback shape
(exact path, HTTPS), discovery fetch, discovery issuer exactly
equal to the configured issuer, authorization/token/JWKS endpoints
absolute HTTPS, S256 advertised **or the
`code_challenge_methods_supported` field absent (the script PASSes
that case with a caveat — S256 must still be confirmed during the
live login step, a preflight PASS does not guarantee it)**, JWKS
non-empty **(key ids listed when the keys carry a `kid`; keys
without a `kid` still PASS and are reported as "(none labelled)")**,
config shape (enabled, issuer/callback/audience match, secret
referenced by environment name only, 1–100 mappings, values only
`viewer`/`operator`/`admin`). A config that carries a literal
`client_secret` value fails the preflight and the value is never
echoed. The report file is written mode `0600`.

A preflight PASS is not acceptance: it proves the tenant publishes
a conformant discovery document, nothing about login, roles, or
logout. If it FAILs, fix the tenant/config and re-run; do not
proceed to browser login on a failing preflight.

## 4. Step B — sanitized config shape

Commit and share only this shape, with placeholders, never values:

```json
{
  "admin": {
    "oidc": {
      "enabled": true,
      "issuer_url": "https://<tenant-issuer>",
      "client_id": "<staging-client-id>",
      "client_secret_env": "NEXAROUTE_OIDC_CLIENT_SECRET",
      "redirect_url": "https://<gateway-host>/admin/auth/oidc/callback",
      "audience": "<configured-audience>",
      "role_claim": "<tenant-role-claim>",
      "role_mappings": {
        "<tenant-viewer-claim-value>": "viewer",
        "<tenant-operator-claim-value>": "operator",
        "<tenant-admin-claim-value>": "admin"
      }
    }
  }
}
```

Validation rules enforced by `internal/config/admin_security.go`
and checked by the preflight: issuer and callback absolute HTTPS
(loopback HTTP only for loopback development issuers), audience
and role claim required, 1–100 explicit mappings, mapping values
only `viewer`/`operator`/`admin`. Start the gateway and confirm
startup discovery succeeds; a discovery failure at startup must
fail closed (no admin access via a fallback) — verify that, do not
work around it.

## 5. Step C — browser login matrix (interactive, in this order)

For each mapped test user, in a clean browser profile:

1. Open `/admin/auth/login`. Confirm the redirect to the tenant
   carries `response_type=code`, a fresh `state`, a fresh `nonce`,
   `code_challenge_method=S256`, and the exact registered
   `redirect_uri`. Record that the values are fresh per login (the
   local suite pins uniqueness over 64 transactions; on the live
   tenant, confirm at least two consecutive logins differ).
2. Complete the login. Confirm an opaque server-side session is
   created: the session cookie is `HttpOnly`, `Secure`,
   `SameSite=Lax`, `Path=/admin/` (verified in
   `internal/httpapi/admin_auth.go`), and the audit log shows
   `auth.login.started` then `auth.login.succeeded` with the mapped
   role. Confirm the user reaches only that role's permissions —
   notably, only `admin` reads the audit API and manages client
   keys; `viewer` and `operator` attempts at those endpoints must
   produce `admin.access.denied` and no data.
3. Confirm the token exchange used the PKCE verifier: the gateway
   sends it (locally pinned by the in-process IdP); on the live
   tenant, confirm indirectly — a login started in one profile and
   completed in another (no flow-binding cookie) must be rejected,
   and the same callback URL replayed a second time must be
   rejected (single-use state).

## 6. Step D — negative and lifecycle cases (all must fail closed)

Run each, record the exact HTTP status and audit record, and
confirm **no** session cookie is issued on denial:

- **Unmapped user:** test user outside every mapped claim value.
  Expected: callback denied, `auth.login.failed` recorded.
- **Conflicting roles:** user in two mapped groups (e.g. viewer +
  admin). Expected: denied, not granted either role.
- **Missing/malformed role claim:** user with no role claim, or a
  claim that is neither a string nor an array of strings.
  Expected: denied.
- **Mismatched callback registration:** a client whose registered
  callback differs from the gateway's. Expected: provider rejects
  the authorization request; no gateway session.
- **Token-level negatives (via the harness, §7):** wrong audience,
  wrong issuer, expired token, tampered signature. The gateway
  rejects all of these locally; on the live tenant, exercise them
  through an operator-issued test ID token where the tenant allows
  it, and record which ones the tenant actually permitted you to
  exercise — the rest stay UNVERIFIED, not assumed.
- **Session expiry:** wait out the absolute TTL and idle timeout
  (or use the configured test values on a disposable gateway) and
  confirm the old cookie is rejected.
- **Policy-change revocation:** change a role mapping, issuer,
  audience, or client, then reuse an existing session. Expected:
  revoked on next use (the gateway hashes the OIDC policy into the
  session; a policy change invalidates it).
- **Logout + CSRF:** log out with `POST /admin/auth/logout` and
  the double-submit CSRF token. Expected: session revoked, old
  cookie replay rejected, `auth.logout` recorded. Then repeat
  without the CSRF token/header, and with a cross-origin request.
  Expected: `403`, session still active (the logout did not
  happen). Logout revocation that fails to persist must report
  `503` and keep the session active rather than clearing the
  browser cookie — that is the fail-closed behavior, not a bug.
- **JWKS rotation:** during the overlap window (both keys
  published) logins keep working; after the old key leaves JWKS,
  new logins still work and a token signed with the retired key is
  rejected. The gateway fetches the current JWKS on every callback
  (no process-lifetime cache), so retirement takes effect
  immediately. A JWKS outage must fail the callback closed.
  Rotation requires tenant authority or an agreed rotation window;
  if neither is available, mark rotation BLOCKED, not passed.
- **Audit persistence failure (disposable copy only):** make the
  audit/security store unavailable on a disposable gateway copy
  and attempt login start, failed login, successful login, and
  logout. Expected: `503` in each case, no session issued, no
  `auth.login.succeeded` / `auth.logout` record written for the
  failed operation. Never do this against the staging tenant's
  real gateway.

## 7. Step E — harness cross-check (does not replace §5–§6)

```bash
nexaroute external-accept \
  --config /etc/nexaroute/config.json \
  --oidc --confirm-live \
  --out docs/reports/verification-slices/oidc-staging-$(date -u +%Y%m%dT%H%M%SZ).json
```

With `--oidc-id-token-env <NAME>` holding an operator-issued test
ID token, the harness additionally verifies that token's signature,
issuer, and audience against the live JWKS. Limits, stated here so
they are not overstated later: the harness does **not** perform the
interactive login, does **not** check nonce (it did not initiate
the authorization request), does **not** check role mapping,
logout, or audit. A harness PASS is one row of the evidence record,
never the whole acceptance. Review the report (mode `0600`) before
attaching it; it records statuses and counts, never secret values,
tokens, subjects, or group values.

## 8. Evidence record (fill in, per tenant run)

Copy this table into the run's evidence note. One row per case;
`Result` is exactly one of `PASS` / `FAIL` / `BLOCKED` /
`UNVERIFIED`. Leave a case BLOCKED when its input was missing and
UNVERIFIED when it was not actually exercised — never upgrade
either from a local test.

| # | Case | Expected (short) | Observed (redacted) | Result |
|---|---|---|---|---|
| 1 | Discovery + issuer pinning | Discovery issuer == configured issuer; endpoints HTTPS | | |
| 2 | Callback + client config | Exact callback path registered; config shape valid; secret by env name only | | |
| 3 | Authorization Code + PKCE | `code`, fresh state/nonce, S256 challenge; verifier on exchange | | |
| 4 | State/nonce single-use | Callback replay rejected; cross-profile callback rejected | | |
| 5 | Signature + JWKS rotation | Overlap logins work; retired-key token rejected; JWKS outage fails closed | | |
| 6 | Audience | Wrong/missing audience rejected | | |
| 7 | Expiry + clock skew | Expired token rejected; `nbf` +5-minute leeway confirmed (§1); far-future `iat` alone not a rejection (limitation, §1); host/IdP offset recorded; zero `exp` leeway confirmed (§1) | | |
| 8 | Explicit role mapping | Each mapped user gets exactly its role's permissions | | |
| 9 | Denied unmapped/ambiguous/malformed | No session; `auth.login.failed` recorded | | |
| 10 | Session + logout + CSRF | Opaque session; logout revokes; CSRF-less logout `403` and session survives | | |
| 11 | Audit records | `auth.login.started/succeeded/failed`, `auth.logout`, `admin.access.denied` present, redacted | | |
| 12 | Audit persistence failure | Disposable copy: `503`, no session, no success/logout record | | |

Header fields for the same note (all required before the run can
move out of BLOCKED):

- Provider name and exact version; tenant type (staging).
- Test date (UTC) and gateway SHA.
- Redacted issuer URL and callback URL (mask tenant ids if the
  operator marks them sensitive).
- Sanitized config shape (§4) with the secret environment variable
  name only.
- Role claim name, mapping count, and redacted claim values.
- Redacted audit excerpts for case 11 (confirm no excerpt contains
  a secret, code, token, state, nonce, verifier, session cookie, or
  full claim payload).
- Preflight report path and overall result; harness report path
  and per-check results.
- Every behavior not exercised, listed explicitly as UNVERIFIED —
  including, until run, provider back-channel logout, which the
  gateway does not consume: never assume a provider-side revocation
  reaches the gateway.

## 9. Unblock condition

This runbook moves from BLOCKED only when an operator supplies the
§0 inputs, executes §3–§7 on the real staging tenant, and files the
§8 record with item-by-item results. Until then, organizational-IdP
acceptance for NexaRoute remains **UNVERIFIED**, issue #647 remains
open, and no PR description, report, or release note may claim
external OIDC interoperability from local evidence.
