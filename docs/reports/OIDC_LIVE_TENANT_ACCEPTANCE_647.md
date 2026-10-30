# OIDC live-tenant acceptance checklist (#647)

**Status: BLOCKED — not executed.**

Date opened: 2026-10-27. Last re-audited: 2026-10-30 on main
`7daa36d` (see §"2026-10-30 re-audit").
Issue: [#647](https://github.com/ali-shortcuts/nexaroute/issues/647)

No operator-owned Keycloak, Entra ID, Okta, Auth0, or Google tenant,
client credentials, test users, or signing-key access were supplied to
this workflow. This report is therefore **not** organizational-IdP
acceptance evidence. The deterministic in-process IdP tests described
below are local protocol coverage only; they do not upgrade the live
tenant from **UNVERIFIED**.

## Required operator inputs (none supplied)

| Input | Required | Status |
|---|---|---|
| Provider kind and exact version (Keycloak / Entra ID / Okta / Auth0 / Google) | Yes | BLOCKED |
| Issuer URL for the tenant | Yes | BLOCKED |
| Confidential client ID and registered callback URL | Yes | BLOCKED |
| Client secret supplied only through the environment variable named by `admin.oidc.client_secret_env` | Yes | BLOCKED |
| Expected audience value and provider-specific role/group claim | Yes | BLOCKED |
| Explicit mapping from provider claim values to `viewer`, `operator`, `admin` | Yes | BLOCKED |
| One user in a mapped group and one user outside every mapped group | Yes | BLOCKED |
| Authority to rotate the provider signing key (or an agreed rotation window) | Yes | BLOCKED |
| Approval for the redaction policy used in the evidence report | Yes | BLOCKED |

Never paste a client secret, access token, ID token, authorization code,
session cookie, or private tenant identifier into the issue, PR, config
file, command line, or committed evidence. Supply secrets through the
operator's secret injector/environment only.

## Deterministic local-IdP coverage (executed, not live acceptance)

Command:

```bash
go test ./internal/httpapi -run 'OIDC|AdminAuth|Session' -count=1
```

Observed 2026-10-27: PASS (exit 0). The broader relevant package run
`go test ./internal/httpapi ./internal/config ./internal/authz ./internal/securitystore -count=1`
also passed.

| Area | Local coverage |
|---|---|
| Discovery issuer pinning | `TestOIDCDiscoveryIssuerMismatchFailsClosed`, `TestOIDCProviderDiscoveryUsesFixedTrustedIssuerAndNoOpenRedirect`, wrong-issuer case in `TestOIDCRejectsInvalidTokensAndTamperedResponses`, `TestOIDCDiscoveryRejectsIssuerThatDiffersOnlyByTrailingSlash` |
| Callback URL | authorization request must carry the configured `redirect_uri`; `TestOIDCTokenExchangeUsesConfiguredClientRedirectAndGrant` pins the configured client authentication, callback, and `authorization_code` grant on the token exchange; callback state is bound to the initiating browser cookie |
| State / nonce / PKCE | `TestOIDCAuthorizationCodePKCEAndSessionLifecycle`, `TestOIDCRejectsInvalidTokensAndTamperedResponses`, `TestOIDCLoginTransactionsIssueUniqueStateNonceAndPKCE`, `TestOIDCNonceIsBoundToItsTransaction` (a valid nonce from another pending login is denied) |
| JWKS signature and rotation | `TestOIDCJWKSKeyRotation` (old key during overlap, new key rollover, retired key rejected), `TestOIDCJWKSOutageRejectsCallback` |
| Explicit role mapping | lifecycle role matrix plus `TestOIDCRolePermissionsReachActualAdminHandlers`; `TestOIDCRoleMappingAcceptsDuplicateAndMixedMappedValues` pins that duplicate copies of one mapped value collapse and unmapped/empty siblings are ignored |
| Denied unmapped / ambiguous roles | `TestOIDCUnmappedAmbiguousAndMalformedRolesDenied` (unmapped, missing claim, conflicting mapped roles, malformed claim) |
| Logout / session revocation | `TestOIDCAuthorizationCodePKCEAndSessionLifecycle`, `TestOIDCLogoutRevocationFailureKeepsSessionActive`, `TestSessionPolicyChangeRevokesExistingSession`, `TestSessionExpiryAndIdleTimeoutAreEnforcedByMiddleware` |
| Audit failure behavior | `TestOIDCLoginFailureAuditStoreUnavailableFailsClosed`, `TestCoverageAdminAuthLoginAuditFailureFailsClosed`, `TestOIDCSuccessfulLoginPersistenceFailureIssuesNoSession`, `TestOIDCLogoutRevocationFailureKeepsSessionActive` |

These tests use an in-process `httptest` IdP with real discovery, JWKS,
token, and RSA-signed ID-token behavior. They are deterministic and
repeatable in CI, but they are not a real organizational tenant and do
not exercise provider-specific claim shapes or clock-skew policy.

## 2026-10-30 re-audit of main (local only; live gate BLOCKED)

Inspected main `7daa36d` and the merged #647 work (PR #699, merge
`63c75d4`). The merged suite already covers the listed acceptance
areas; this re-audit did not duplicate them. Four local gaps were
closed in this PR, with no production-code change:

| Gap | New deterministic test |
|---|---|
| Issuer equality was only tested against an obviously hostile issuer | `TestOIDCDiscoveryRejectsIssuerThatDiffersOnlyByTrailingSlash` |
| Nonce was only tested against an arbitrary wrong value | `TestOIDCNonceIsBoundToItsTransaction` (valid nonce from another pending login denied) |
| Only the authorization request's `redirect_uri` was checked | `TestOIDCTokenExchangeUsesConfiguredClientRedirectAndGrant` (token exchange sends configured client auth, callback, and `authorization_code` grant) |
| Declared duplicate/mixed role behavior had no test | `TestOIDCRoleMappingAcceptsDuplicateAndMixedMappedValues` |

The in-process IdP fixture now records the token-endpoint `client_id`
(including HTTP Basic client authentication), `redirect_uri`, and
`grant_type`, so those assertions exercise the real exchange path. This
remains local protocol evidence, not live-tenant acceptance.

Environment check for this re-audit (variable **names** only; values
were never read): `NEXAROUTE_OIDC_CLIENT_SECRET`, `OIDC_CLIENT_SECRET`,
`KEYCLOAK_URL`, `KEYCLOAK_REALM`, `OKTA_DOMAIN`, `OKTA_ISSUER`,
`AUTH0_DOMAIN`, `AUTH0_ISSUER`, `AZURE_TENANT_ID`, `ENTRA_TENANT_ID`,
`GOOGLE_CLIENT_ID`, `OIDC_ISSUER`, `NEXAROUTE_ADMIN_OIDC_ISSUER` — all
unset. `OIDC_BASE_URL` exists only as an empty value in
`.github/workflows/opencode.yml`, not as a tenant. No repository config
contains an `admin.oidc` tenant block outside docs/tests, and no comment
on #647 supplies an operator-owned tenant, test users, or authorization.
**Zero live IdP calls were made.**

Local verification for this re-audit (all PASS unless stated):

- `go test ./internal/httpapi -run 'TestOIDC|TestCoverageAdminAuth|TestSession|TestAdminSession' -count=1` — PASS (6.543s).
- `go test ./internal/httpapi ./internal/config ./internal/authz ./internal/securitystore -count=1` — PASS (`internal/httpapi` 91.519s; config/authz/securitystore PASS).
- `go vet ./internal/httpapi` — PASS.
- `go test -race ./internal/httpapi -run 'TestOIDCDiscoveryRejectsIssuerThatDiffersOnlyByTrailingSlash|TestOIDCNonceIsBoundToItsTransaction|TestOIDCTokenExchangeUsesConfiguredClientRedirectAndGrant|TestOIDCRoleMappingAcceptsDuplicateAndMixedMappedValues' -count=1` — PASS (3.170s).
- `bash scripts/test-oidc-live-preflight.sh` — PASS (loopback fixtures only; not live acceptance evidence).
- The four new tests verbose-PASS individually; this is local evidence, never a live `PASS`.

The live checklist below applies unchanged to an operator-owned
Keycloak, Entra ID, Okta, Auth0, or Google tenant. Provider-specific
registration and starting claim shapes are in
`OIDC_OPERATOR_RUNBOOK_647.md` §2; record the tenant's actual claim
shape, never the assumed one. Every live item remains
**BLOCKED/NOT_RUN**. #647 stays open until this checklist is executed on
a real tenant with item-by-item evidence, or an owner-approved exclusion
is reviewed.

## Live-tenant checklist (all items BLOCKED)

Mark an item `PASS` only from a live observation on the operator-owned
tenant. Mark it `FAIL` when the observation contradicts the expected
result. Leave it `BLOCKED` when required credentials, configuration, or
access are missing. Do not infer a live `PASS` from the local tests.

### 1. Discovery and issuer pinning — BLOCKED

- Configure `admin.oidc.issuer_url` with the exact tenant issuer.
- Confirm startup discovery succeeds and `issuer` in the discovery
  document exactly equals the configured issuer.
- Confirm the authorization, token, and JWKS endpoints are the tenant's
  real HTTPS endpoints.
- Negative check: point a disposable copy of the config at a different
  issuer and confirm startup/Admin access fails closed (no emergency-key
  fallback).

### 2. Callback URL — BLOCKED

- Register exactly `https://<gateway>/admin/auth/oidc/callback` at the
  provider.
- Confirm the provider accepts the gateway's authorization request and
  returns to that exact path.
- Confirm a mismatched callback registration is rejected by the
  provider and does not create a gateway session.
- If the operator can inspect the token request (provider log, approved
  proxy, or disposable test client), confirm it carries the configured
  `redirect_uri`, the expected client authentication, and
  `grant_type=authorization_code`. If that request cannot be observed,
  keep this sub-item **UNVERIFIED**; do not infer it from the local test.

### 3. State, nonce, and PKCE — BLOCKED

- Confirm the authorization redirect contains a fresh `state`, `nonce`,
  `code_challenge`, and `code_challenge_method=S256`.
- Complete a normal login and confirm the token request carries the
  matching PKCE verifier.
- Replay the same callback URL and confirm it is rejected (single-use
  state).
- Confirm a callback from a different browser profile without the
  flow-binding cookie is rejected.
- If the provider supports a token-inspection view, confirm the nonce in
  the issued ID token matches the authorization request.

### 4. JWKS signature and key rotation — BLOCKED

- Complete a login with the tenant's current signing key.
- Capture a pre-rotation ID token through an approved operator procedure
  (do not commit it) and confirm a callback replay after the old key is
  removed is rejected.
- Rotate the provider signing key, or wait for its scheduled rotation.
- During overlap, confirm logins keep working while both keys are
  published.
- After the old key leaves JWKS, confirm new logins still work and the
  retired-key token is rejected.
- Confirm a JWKS outage fails the callback closed.

### 5. Explicit role mapping — BLOCKED

- Record the provider claim name and the exact redacted claim values
  mapped to `viewer`, `operator`, and `admin`.
- Confirm each mapped user reaches only the permissions documented for
  that role (notably: only `admin` reads the audit API and manages client
  keys).
- Confirm the provider-specific claim shape is accepted (string or array
  of strings at `admin.oidc.role_claim`).

### 6. Denied unmapped users — BLOCKED

- Log in with a test user outside every mapped claim value.
- Confirm the callback is denied, no admin session cookie is issued, and
  an `auth.login.failed` audit record is present.
- Confirm a user with conflicting mapped roles is denied rather than
  granted either role.
- Confirm a missing or malformed role claim is denied.

### 7. Session and logout behavior — BLOCKED

- Confirm a successful login creates an opaque server-side session with
  the mapped role.
- Confirm logout with the CSRF double-submit token revokes the session;
  replaying the old cookie is rejected.
- Confirm a policy change (role mapping, issuer, audience, or client)
  revokes existing sessions on their next use.
- Confirm absolute TTL and idle timeout expiry are enforced.
- If the provider offers back-channel logout, record that the gateway
  does not consume it and keep that behavior **UNVERIFIED** rather than
  assuming revocation.

### 8. Structured audit entries — BLOCKED

- Capture redacted audit excerpts for:
  - `auth.login.started`
  - `auth.login.succeeded` with the mapped role
  - `auth.login.failed` for the unmapped user
  - `auth.logout`
  - `admin.access.denied` for a role without the requested permission
- Confirm no audit record contains the client secret, authorization
  code, access token, ID token, state, nonce, PKCE verifier, session
  cookie, or full claim payload.
- With a disposable copy, make the audit store unavailable and confirm
  login start, failed login, successful login persistence, and logout
  fail with HTTP 503 rather than silently proceeding.

### 9. Provider-specific claims and clock skew — BLOCKED

- Record how the tenant represents groups/roles (for example nested
  `realm_access.roles`, `groups`, or app roles) and whether the configured
  `role_claim` reaches it.
- Confirm behavior when the ID token is near expiry and when the
  provider's clock differs from the gateway host. Record the observed
  tolerance; do not assume a provider-specific skew policy.
- Keep any provider claim shape or clock behavior not actually exercised
  as **UNVERIFIED**.

## Evidence to record

- Provider name and exact version; test date in UTC; operator.
- Redacted issuer URL (tenant IDs may be masked) and callback URL.
- Config shape with the secret value removed; the secret environment
  variable name may be recorded, never its value.
- Role claim, redacted claim values, and role mappings.
- Redacted audit excerpts for each action listed in §8.
- Exact command output from the live checks and the result per checklist
  item (`PASS` / `FAIL` / `BLOCKED`).
- Any behavior not exercised, explicitly retained as **UNVERIFIED**.

## Automation boundary

`nexaroute external-accept --confirm-live --oidc ...` can check live
discovery, JWKS, and optionally an operator-issued ID token. It does not
perform the interactive Authorization Code + PKCE browser login,
role-mapping checks, logout/revocation, or audit inspection. The exact
invocations are in
[`verification-slices/live-acceptance-harness.md`](verification-slices/live-acceptance-harness.md).
A harness `PASS` alone does not close this checklist.

## Repository verification (2026-10-27)

- `go test ./internal/httpapi -run 'OIDC|AdminAuth|Session' -count=1` — PASS.
- `go test ./internal/httpapi ./internal/config ./internal/authz ./internal/securitystore -count=1` — PASS.
- `./scripts/verify.sh` — PASS (`VERIFY PASS`). The script's browser
  acceptance step was skipped because Chromium and Python Playwright are
  unavailable in this runner; the script reported that warning explicitly.
  This does not affect the local OIDC protocol tests or the live-tenant
  **BLOCKED** status.

## Unblock condition

This report may move from **BLOCKED** only when an operator supplies the
inputs above and a live run produces item-by-item evidence on the real
tenant. Until then, organizational-IdP acceptance remains **UNVERIFIED**
and this issue must not be closed on the strength of local tests.
