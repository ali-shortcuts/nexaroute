# OIDC security review — issue #647 (independent verification)

**Review date:** 2026-10-28. **Base:** main `e6c3cdf3`
(`e6c3cdf3fe0a193dda0c5857e91e46b541d04729`, verified live as the
remote `main` head at review time).
**Scope:** the gateway's OIDC admin authentication only
(`internal/httpapi/oidc.go`, `internal/httpapi/admin_auth.go`,
`internal/config/admin_security.go`, `internal/securitystore`).
This review changes no RBAC rule, weakens no check, and skips no
token validation. It is **not** live-tenant evidence: every result
below comes from code inspection against the go-oidc v3.15.0 module
source and from tests against the in-process IdP fixture. A local
mock proves the gateway's checks, never interoperability with a
real organization's IdP.

## Live state at review time

| Item | State |
|---|---|
| Issue #647 — verify OIDC against a real organizational IdP | OPEN |
| PR #666 — OIDC local tests expanded; live-tenant checklist BLOCKED (`opencode/issue647-20261027145434`, head `5800beda`) | OPEN, MERGEABLE, all listed CI checks pass (incl. `verify`) |
| PR #671 — OIDC operator runbook + secret-free live preflight (`supervisor/issue647-oidc-operator-runbook`, head `a455b8ce`) | OPEN, MERGEABLE, all listed CI checks pass (incl. `verify`) |
| Real organizational tenant (Entra ID, Okta, Auth0, Google, operator Keycloak) | **BLOCKED_EXTERNAL** — no tenant, client, test user, or authorization was supplied for this review; zero live calls were made |

**Post-merge update (2026-10-30, main `7daa36d`):** #666 and #671 were
superseded by PR #699 (merge `63c75d4`), which also carried the `azp`
change below; #647 was closed then reopened because no live evidence
exists. The 2026-10-30 re-audit added four local gap tests (exact
issuer trailing-slash, cross-transaction nonce, token-exchange
client/redirect/grant, duplicate/mixed role values). No production
change and no live accepted tenant. The real tenant remains
**BLOCKED/UNVERIFIED**.

## Verification matrix

Each row was checked first-hand in code and, where a test is
named, is pinned by that test.

| Property | Implementation | Evidence |
|---|---|---|
| Exact issuer pinning | Discovery issuer must equal `admin.oidc.issuer_url` character-for-character (`validateOIDCDiscovery`); the verifier is built with that issuer; `Complete` re-checks `idToken.Issuer`. A Google scheme-less `iss` accepted by go-oidc is still rejected by the gateway's own check (stricter, fail-closed). | `TestOIDCDiscoveryIssuerMismatchFailsClosed`, `TestOIDCRejectsInvalidTokensAndTamperedResponses` (wrong issuer) |
| Client ID and audience | go-oidc requires `aud` to contain the client ID; the gateway additionally requires the configured `audience` (config requires it non-empty, so there is no audience-less fallback). **New:** when `azp` is present it must equal the client ID (finding F1). | Wrong-audience and missing-configured-audience cases in the tampered-responses test; `TestOIDCReviewAzpValidation` (new) |
| Signed ID-token verification | Signature is verified first, exactly one signature is required, and the JWKS key set is fetched fresh on **every** callback — no process-lifetime cache, so a retired key stops verifying immediately. A JWKS outage fails the callback closed. | `TestOIDCJWKSKeyRotation` (original → overlap → completed → retired-key rejection), `TestOIDCJWKSOutageRejectsCallback`, bad-signature case |
| JWKS caching and rotation | Deliberately **no cache**: `freshJWKSKeySet` builds a new remote key set per verification (10 s HTTP timeout). Rotation is honored immediately; the price is that a JWKS outage blocks logins (fail-closed, not degraded). | Rotation test above; see also F2 for the RS256-only consequence |
| State and nonce | 32-byte random state and nonce per transaction; state is stored only as its SHA-256 hash, single-use (deleted on take), 10-minute expiry, per-IP cap 32, global cap 4096. Nonce is compared constant-time and must match exactly. | Tampered-state, wrong-browser, replayed-state, wrong/missing-nonce cases; `TestOIDCReviewExpiredPendingTransactionDenied` (new); uniqueness pinned by PR #666 |
| PKCE S256 | `S256ChallengeOption` on the authorization request; the verifier is held server-side in the pending transaction (never sent to the browser) and presented on exchange. | `TestOIDCAuthorizationCodePKCEAndSessionLifecycle`, PKCE-mismatch case, PR #666 uniqueness test |
| Callback URL and origin | `redirect_url` must be an absolute HTTPS URL (loopback HTTP only for loopback issuers), with no query, fragment, userinfo, or opaque part (config validation). The login handler ignores any caller-supplied `next` value: the authorization redirect always carries the configured callback, and the post-login redirect is the fixed path `/`. State-changing admin calls under a session require same-origin **and** the double-submit CSRF token (below). | `TestOIDCProviderDiscoveryUsesFixedTrustedIssuerAndNoOpenRedirect`, config validation; `TestSessionMutationsRequireStrictDoubleSubmitCSRF` |
| Session binding and expiry | The session cookie is an opaque 32-byte random token (SHA-256 hash stored, value never stored); cookie flags are `HttpOnly`, `Secure` (unconditional), `SameSite=Lax`, `Path=/admin/`. Absolute TTL 8 h and idle timeout 30 min by default; the absolute cap is enforced even if idle touches slide expiry. A session is revoked on next use when the OIDC policy hash or issuer no longer matches the live config. Binding is to the token plus policy — **not** to IP or User-Agent (finding F5). | `TestOIDCAuthorizationCodePKCEAndSessionLifecycle` (rotation on privilege transition, old session dead), `TestSessionExpiryAndIdleTimeoutAreEnforcedByMiddleware`, `TestSessionPolicyChangeRevokesExistingSession`, `TestAdminSessionCookieHasSecureScopedAttributes`, `TestOIDCReviewSessionSurvivesGatewayRestart` (new) |
| Logout and revocation | Logout is POST-only with strict CSRF; revocation deletes the server-side session and clears the cookie. If revocation fails to persist, logout reports 503 and the session **stays active** — it never claims a logout it did not persist. Provider back-channel logout is **not** consumed: a provider-side revocation does not reach the gateway (also stated in the #671 runbook §8). | Lifecycle test (replay after logout rejected), PR #666 logout-revocation-failure test |
| CSRF protections | Login flow: server-side state + HttpOnly flow-binding cookie (a cross-profile or cross-browser callback is rejected). Mutations under a session: same-origin check plus a double-submit token (`HttpOnly`, `Secure`, `SameSite=Strict` CSRF cookie, 12 h, header must equal cookie, constant-time compare). Logout additionally requires an authenticated session. | `TestSessionMutationsRequireStrictDoubleSubmitCSRF`, `TestCoverageAdminAuthLogoutCSRFAndAuditQueryValidation`, wrong-browser case |
| Explicit role mapping | 1–100 explicit mappings, claim values trimmed and non-empty, mapped values restricted to `viewer`/`operator`/`admin` at config validation. No default role, no inference from any other claim. | Config validation; role-permission tests |
| Rejection of missing, ambiguous, malformed roles | Missing claim, unmapped values only, two distinct mapped roles, and a claim that is neither a string nor an array of strings are all denied with no session issued and an `auth.login.failed` audit record. A stored session with an unknown role or != 1 role is likewise denied. | PR #666 `TestOIDCUnmappedAmbiguousAndMalformedRolesDenied`; `TestCoverageAdminAuthSessionRejectsInvalidStoredRoleClaims` |

## Findings

### F1 — `azp` was not validated (hardened in this branch)

go-oidc v3.15.0 checks audience membership but explicitly does
**not** check the authorized party (`azp`): its own comment says
the check "DOES NOT ensure that the ClientID is the party to
which the ID Token was issued". The gateway therefore accepted a
token minted for another client that merely listed this client in
`aud`. The realistic exposure is low (it requires an IdP to mint
such a token), but it is a genuine OIDC Core gap: when `aud`
carries multiple values, `azp` is the claim that says who the
token was issued to.

**Change (this branch, `internal/httpapi/oidc.go`):** when the
token carries `azp`, it must be a string exactly equal to the
client ID; otherwise login is denied. Tokens without `azp` are
still accepted (single-audience tokens need not carry it), and
the gateway itself is the OAuth client (it performs the code
exchange), so legitimate flows always carry its own client ID as
`azp` or omit it. Pinned by `TestOIDCReviewAzpValidation`
(another client denied, absent allowed, own client allowed).

### F2 — The verifier is RS256-only (fail-closed interop note)

The verifier is built with `oidc.Config{ClientID: ...}` and no
`SupportedSigningAlgs`, so go-oidc defaults to RS256 only. An IdP
that signs ID tokens exclusively with another algorithm (ES256,
PS256, EdDSA) will fail logins closed — never bypassed, but also
never working until the verifier is configured for that
algorithm. Operators must confirm the tenant signs with RS256
(the default for Entra ID, Okta, Auth0, Google, and Keycloak) or
treat that tenant shape as unsupported. This is a limitation to
record on the tenant, not a reason to loosen verification.

### F3 — Clock behavior, corrected against the module source

The #671 runbook §1 says only `exp` is time-checked. Verified
against go-oidc v3.15.0 `verify.go`, the full picture is:

- `exp` in the past is rejected with **zero leeway** and no custom
  clock (the gateway sets neither). A provider clock ahead of the
  gateway fails logins closed; it is not absorbed.
- `nbf`, when present, is enforced with a **5-minute leeway**
  (a token whose `nbf` is more than 5 minutes in the future is
  rejected).
- `iat` is **not** independently checked.

Pinned by `TestOIDCReviewClockClaimsPinned` (near-future `iat`
accepted, `nbf` within leeway accepted, `nbf` beyond leeway
rejected). The runbook's §1 paragraph should gain the `nbf`
sentence; the zero-leeway `exp` guidance stands.

### F4 — Google issuer special case is neutralized (fail-closed)

go-oidc accepts a scheme-less `accounts.google.com` issuer claim
when the expected issuer is `https://accounts.google.com`. The
gateway's own `idToken.Issuer != a.issuer` check rejects it
anyway, so the library's leniency never reaches a session. The
operator consequence is only that the configured issuer must
match the token's `iss` exactly.

### F5 — Session binding is token-plus-policy, not device binding

Nothing binds a live session to an IP address, User-Agent, or
browser fingerprint. The mitigations are the ones that matter
most in practice: the token is random and opaque, the cookie is
`HttpOnly` + `Secure` + `SameSite=Lax`, sessions rotate on
privilege transition, and policy/issuer changes revoke sessions.
A stolen cookie (which would require defeating those layers)
replays until TTL, idle expiry, logout, or revocation. This is
standard bearer-session behavior, recorded here so it is not
mistaken for device binding.

### F6 — What survives a restart, and what does not

See the classification below. The split is already documented in
`docs/SECURITY.md` and `docs/KNOWN_GAPS.md` and is now pinned at
the HTTP layer by `TestOIDCReviewSessionSurvivesGatewayRestart`:
durable sessions and audit records survive a same-host restart;
a pending login transaction does not.

## Persistence classification (task 3)

| Capability | Classification | Notes |
|---|---|---|
| Admin sessions | Durable; single-host by default, shared on PG/Redis | bbolt file (mode 0600, next to the config) by default; survives process restart on the same host; must not be shared between hosts or over a network filesystem, and two processes cannot open one bbolt file. PostgreSQL/Redis backends share sessions across instances. |
| Audit chain | Durable; same backends as sessions | Hash-chained records in the same store; chain integrity is surfaced on the audit read path (status, counts, first problems), never silently served as trusted. |
| Pending OIDC login transactions | **Process-local, in-memory only** | State/nonce/verifier/binding live in `oidcAuthenticator.pending`. The callback must return to the instance that started the login: single instance, or sticky routing for the whole login flow, even when sessions/audit are shared via PG/Redis. A restart drops pending logins (they fail closed, they do not complete). |
| Rate-limit buckets, provider health, caches, in-flight quota reservations | Process-local by design | Per `internal/securitystore/backend.go`; not session or audit state. |
| JWKS | No cache by design | Fresh fetch per callback (see matrix). A JWKS outage blocks logins until the IdP recovers. |
| Backend selection | Fail-closed at startup | An unknown backend name or unreachable PG/Redis URL is a startup error, never a silent fallback to a local or in-memory store. |

## Audit fail-closed contract (verified in code and tests)

| Failure point | Behavior |
|---|---|
| Login start, audit write fails | 503, no redirect to the provider, no pending transaction left usable |
| Callback denial, audit write fails | 503 instead of 401 (the denial is not reported as a plain rejection while its record is missing) |
| Successful login, session persist fails | 503, **no session cookie issued** (PR #666) |
| Logout, revocation persist fails | 503, session **stays active**, browser cookie not cleared as if logged out (PR #666) |
| Protected admin request, denial audit fails | 503 instead of the intended 401/403 (`denyAdminRequest`) |
| Protected admin operation, pre-operation audit fails | Operation is blocked before the handler runs |
| Protected admin operation, post-operation audit fails | Logged only; the durable pre-operation record stands, so intent is not erased |
| Security store unavailable at request time | 503 everywhere admin auth is required; OIDC-enabled mode never downgrades to emergency API-key or loopback trust, even with a healthy-looking key configured |

## Operator procedure (task 4)

No second procedure is created here. The procedure for an
authorized organizational tenant is the #671 runbook,
`docs/reports/OIDC_OPERATOR_RUNBOOK_647.md` (§0 rules and
BLOCKED status, §1 host/clock preparation, §2 per-provider
tenant registration, §3 secret-free preflight,
§4 sanitized config shape, §5 browser login matrix,
§6 negative and lifecycle cases, §7 harness cross-check,
§8 evidence record, §9 unblock condition), executed on a
**staging** tenant only, with secrets only via the operator's
secret injector or masked environment variables, and with the
report reviewed before it is attached anywhere. This review
adds three annotations to that runbook, not a replacement:
apply F1 (azp) when checking token shapes, F2 (RS256-only)
when confirming the tenant's signing algorithm, and F3 (the
`nbf` 5-minute leeway) to its §1 clock paragraph. Until an
operator supplies the §0 inputs and files the §8 record, every
live case stays **BLOCKED** and issue #647 stays open.

## Local verification (this review)

Toolchain: Go 1.27.0, `GOPATH=$HOME/workspace/.gopath`,
`GOROOT=$HOME/go`. All commands below were run in this branch
against base `e6c3cdf3` plus the F1 change and the new tests.

- `gofmt -l` on the three touched Go files: clean (one
  formatting fix was applied to the fixture extension before
  committing; it was not a behavior change).
- `go vet ./internal/httpapi/ ./internal/securitystore/
  ./internal/config/`: clean.
- `go test -count=1 -v -run 'TestOIDCReview'
  ./internal/httpapi/`: **PASS** (14.560 s) — all six new tests,
  including every subtest: azp (another client denied, absent
  allowed, own client allowed), unsigned token denied, duplicate
  callback parameters denied, expired pending transaction
  denied, clock claims (near-future `iat` accepted, `nbf` within
  leeway accepted, `nbf` beyond leeway rejected), session
  survives restart while the pending login does not.
- `go test -count=1 -run 'TestOIDCReview|TestOIDC'
  ./internal/httpapi/`: **ok** (32.445 s).
- `go test -count=1 ./internal/securitystore/
  ./internal/config/`: **ok** (38.585 s / 0.674 s).
- Broader filtered run (`TestAdmin|TestSession|
  TestCoverageAdminAuth|TestOIDC` in `internal/httpapi`):
  exactly **one** failure,
  `TestAdminClientKeyRevokePersistenceFailureLeavesKeyActive`,
  which was **proven pre-existing** by running it on the
  pristine base (all changes stashed including untracked
  files): it fails on `e6c3cdf3` itself. It concerns
  client-key revoke persistence, not OIDC, and is untouched by
  this branch. It needs its own triage before any merge gate.
- **Not run locally:** `scripts/verify.sh` (the full gate) and
  GitHub CI (no PR was opened from this branch). The pre-existing
  failure above would appear in a full `verify.sh` run as well
  and must be triaged separately; it is not hidden by the
  filtered runs reported here.
- **Not run at all:** any live-tenant step (BLOCKED_EXTERNAL).
  No tenant, credential, or authorization was supplied, and no
  external IdP was contacted.

## Branch and merge notes

Branch `supervisor/issue647-oidc-security-review`, cut from main
`e6c3cdf3`, local only: **not pushed, no PR, no merge, no
release, issue #647 not closed**. The only behavior change is
F1 (azp); everything else is tests and this report. Merge-time
reconciliation note: PR #666 extends the same IdP fixture with
`missing-role-claim` / `malformed-role-claim` modes, while this
branch extends it with `azp` / `omitAzp` / `iatOffset` /
`nbfUnix` / `unsigned` fields — both extensions are additive
and combine mechanically, but they touch the same function and
will need one careful merge, whichever lands second.
