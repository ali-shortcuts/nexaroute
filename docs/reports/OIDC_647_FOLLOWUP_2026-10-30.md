# OIDC follow-up re-audit (#647) — 2026-10-30

**Status: local follow-up complete; live-tenant gate still BLOCKED.**

This report is the sanitized record for the 2026-10-30 follow-up on issue
[#647](https://github.com/ali-shortcuts/nexaroute/issues/647). It covers
local deterministic tests and documentation only. **No live tenant was
exercised, no live IdP call was made, and nothing in this report upgrades
the live-tenant gate.** Every live checklist item stays **BLOCKED/NOT_RUN**
until an operator-owned tenant is authorized and exercised item by item
(see `OIDC_LIVE_TENANT_ACCEPTANCE_647.md`).

## Base and scope

- Base: main `7a2623a9252d14943ebad64b8945bbebafd4eef2` (merge of #778).
- Branch: `supervisor/issue647-followup-20261030`.
- Change: 1 new test file (`internal/httpapi/oidc_followup_647_test.go`,
  5 tests), 1 test-fixture mode (`nested-role-claim` in
  `internal/httpapi/oidc_test.go`), 3 documentation updates (live-tenant
  checklist follow-up section + §9 top-level-claim requirement, operator
  runbook follow-up note, `EXTERNAL_VERIFICATION.md` row).
- Production code: **unchanged**. No protocol behavior was redesigned;
  no assertion was weakened; no security check was disabled.
- Prior work on main, not duplicated: PR #699 (consolidated OIDC
  tests/runbook), PR #767 (gap tests), PR #772 (callback-binding
  negatives). The sibling #647 session that produced #767/#772 had
  finished before this follow-up started; no open #647 PR existed.

## Tested configuration shape (local only, test values only)

- In-process loopback IdP fixture (no network IdP, no real tenant).
- `admin.oidc`: issuer = loopback fixture URL, client ID
  `nexaroute-test-client`, audience `nexaroute-control-plane`, role claim
  `groups`, mappings `nr-viewers → viewer`, `nr-operators → operator`,
  `nr-admins → admin`, callback
  `http://localhost/admin/auth/oidc/callback`.
- Client secret: test-only value from the test environment, never a real
  credential. No secret, token, code, or session value appears in this
  report, the PR body, or any committed file.

## Local cases — PASS (all deterministic, in-process IdP)

| Case | Test | Result |
|---|---|---|
| Nested role claim denied, no session (top-level-only rule pinned fail-closed) | `TestOIDC647NestedRoleClaimDenied` | PASS |
| Short state denied; real transaction still completes | `TestOIDC647ShortStateDeniedAndTransactionPreserved` | PASS |
| Session-store read outage → protected requests 503, no data; session survives recovery | `TestOIDC647SessionStoreReadFailureFailsClosed` | PASS |
| Cross-host HTTPS discovery endpoints accepted by design (Google-compatible) | `TestOIDC647DiscoveryCrossHostHTTPSEndpointsAccepted` | PASS |
| Login-success audit event carries the subject as actor, outcome success, status 303 | `TestOIDC647LoginSuccessAuditCarriesSubject` | PASS |

Covered on main and deliberately not re-tested: PKCE challenge mismatch
(denied, no leak); issuer/discovery pinning (mismatch, trailing slash,
malformed/insecure endpoints, fixed trusted issuer); state/nonce binding
and uniqueness; duplicate callback parameters; expired transactions;
audience and azp; unsigned/tampered tokens; JWKS rotation, overlap, and
outage; duplicate/mixed and unmapped/ambiguous/malformed top-level role
claims; write-path session/audit persistence failures; policy-mismatch
revocation; logout success and revocation failure; callback-binding
negatives (no/malformed flow cookie, wrong method, provider error);
audit action presence and credential-canary non-leakage.

Considered and rejected, with reason: a same-host rule for discovery
endpoints (issuer host must equal endpoint hosts). It would break real
providers — Google's issuer is `accounts.google.com` while its token
endpoint lives on `oauth2.googleapis.com` — so no such rule was added.
Only the issuer string (exact match) and the endpoint scheme (HTTPS, or
loopback HTTP for development issuers) are pinned.

## Live-tenant cases — BLOCKED / NOT_RUN (all of them)

Provider/version: **none** (no tenant supplied).
Discovery/issuer pinning, redirect URI, Authorization Code + PKCE,
token signature/JWKS, key rotation, role mapping, denied unmapped user,
session/logout, structured audit events, provider-specific claims, and
clock-skew behavior on a real tenant: all **BLOCKED** — not executed,
not observed, not inferred from local tests.

Unexercised behaviors stay **UNVERIFIED**: every provider-specific
claim shape (including nested `realm_access.roles` on a real Keycloak
tenant, Entra group ids/overage, Okta/Auth0 custom claims, Google's
absence of a group claim), real-tenant clock skew, real-tenant key
rotation, and any browser-login behavior on a real tenant.

## Tenant/authorization check (names only, no values)

Tenant/secret environment-name count: **0 of 13 non-empty**
(`NEXAROUTE_OIDC_CLIENT_SECRET`, `OIDC_CLIENT_SECRET`, `KEYCLOAK_URL`,
`KEYCLOAK_REALM`, `OKTA_DOMAIN`, `OKTA_ISSUER`, `AUTH0_DOMAIN`,
`AUTH0_ISSUER`, `AZURE_TENANT_ID`, `ENTRA_TENANT_ID`, `GOOGLE_CLIENT_ID`,
`OIDC_ISSUER`, `NEXAROUTE_ADMIN_OIDC_ISSUER`). No repository config
contains an `admin.oidc` tenant block outside docs/tests, and no #647
comment supplies an operator-owned tenant, test users, or run
authorization. **Zero live IdP calls were made.**

## Local verification (commands and real outcomes)

- `go test ./internal/httpapi -run 'TestOIDC|TestAdminAuth' -count=1`
  (pre-change baseline): PASS, 107.297s, Go 1.27.0.
- `go test ./internal/httpapi -run 'TestOIDC647' -count=1 -v` (5 new +
  5 existing #647 tests): all PASS, 14.958s.
- `go test ./internal/httpapi -run 'TestOIDC|TestAdminAuth' -count=1`
  (with changes): PASS, 55.681s.
- `go test -race` on the 5 new tests: PASS, 12.650s.
- `go vet ./internal/httpapi`: PASS (no findings).
- `gofmt -l` on both touched Go files: clean.
- `git diff --check`: clean.
- `bash scripts/test-oidc-live-preflight.sh`: PASS (loopback fixtures
  only; not live acceptance evidence).
- `./scripts/verify.sh`: exit 1 SOLELY on its own 3-minute per-package
  cap (`go test -timeout=3m`, script line 25): `internal/httpapi`
  hit exactly 180.113s with no failing test assertion. The test in
  flight at the cap was `TestWave2GeminiUpstream429`, blocked in
  bbolt `Fsync` during test-server construction — a disk-contention
  stall on this 2-vCPU VM, unrelated to this change (which touches no
  Gemini, wave2, telemetry, or bbolt path; the new tests pass focused
  in 14.958s). This is the same 3m-cap artifact recorded for the #650
  work. Confirmation, same tree: `TestWave2GeminiUpstream429` alone
  PASSES in 0.05s, and the full `internal/httpapi` package with
  `-timeout=10m` PASSES in 276.752s (4m37s — over the 3m cap, which is
  why the capped run failed). CI on the PR head is the deciding gate;
  this PR is not merged on local evidence alone.

## Exact operator inputs still required (names only, never values)

1. Provider kind + exact version (Keycloak / Entra ID / Okta / Auth0 /
   Google) and issuer URL.
2. Confidential client ID + registered callback URL
   (`https://<gateway>/admin/auth/oidc/callback`).
3. Client secret ONLY via the environment variable named by
   `admin.oidc.client_secret_env` (operator's secret injector).
4. Expected audience + provider-specific role/group claim + explicit
   claim-value → viewer/operator/admin mappings. If the tenant's roles
   are nested-only, map them to a top-level claim on the tenant side
   first (see checklist §9); the gateway reads one top-level claim.
5. One mapped test user per role + one unmapped user.
6. Authority for a signing-key rotation window (or an agreed window).
7. Written authorization to run runbook §3–§7 + approval of the §8
   evidence-redaction policy.

## Close rule

#647 stays OPEN. It closes only after live-tenant evidence is committed
item by item, or after a narrow, explicit, owner-approved exclusion is
recorded and reviewed. This follow-up PR is not merged by its author;
it awaits full green checks and lead review.
