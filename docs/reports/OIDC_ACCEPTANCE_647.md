# OIDC acceptance consolidation (#647)

**Status: LOCAL IdP acceptance complete (fixture tests, listed below).
LIVE organizational-tenant acceptance: BLOCKED — not executed.**

Issue: [#647](https://github.com/ali-shortcuts/nexaroute/issues/647).
This report consolidates three lines without closing any of them:
PR #666 (local negative-path tests + live checklist, scorecard),
PR #671 (operator runbook + secret-free preflight, the single
documented acceptance process), and the #647 security review
(`azp` validation + clock/restart pinning). This PR does **not**
close #647, #666, or #671, and must not be merged without the
owner's word. A local fixture pass is never live-tenant evidence.

## 1. Coverage map (code + test evidence)

Legend: COVERED = check in code + negative test in this tree.
PARTIAL = check in code with a named, documented gap or a
deliberate leniency. MISSING (by design) = no check, documented
as a limitation, not presented as available.

| Dimension | Verdict | Evidence |
|---|---|---|
| Issuer exactness (discovery + token `iss`) | COVERED | `internal/httpapi/oidc.go` discovery exact match + post-`Verify` re-check; `TestOIDCDiscoveryIssuerMismatchFailsClosed`, wrong-issuer subtest. Go-oidc's scheme-less Google `iss` leniency is neutralized by the gateway's own exact check. |
| Client ID | COVERED (new) | `oidc.Config{ClientID}`; go-oidc `aud` membership. New `TestOIDCCompleteAudienceWithoutClientIDDenied`: `aud` containing the configured audience but **not** the client ID is denied. |
| Audience membership | COVERED | Exact-string membership, not whole-list equality; wrong-audience and missing-configured-audience subtests. Multi-value `aud` passes, which is why the `azp` row matters. |
| `azp` (authorized party) | PARTIAL (deliberate leniency) | New in the review line: a present `azp` must equal the client ID exactly, else denied (`TestOIDCReviewAzpValidation`). An **absent** `azp` is allowed unconditionally, including for multi-value `aud` — strict OIDC Core would require `azp` for multi-value `aud`. Documented as a leniency, never as a pass. |
| Signature / JWKS (rotation + outage) | COVERED | Fresh JWKS key set per verification (10 s timeout), no process-lifetime cache; retired key stops verifying immediately. Tests: bad-signature, JWKS outage → 401, rotation original → overlap (old kid, both published; ported from #666) → rollover → completed → retired-key rejected. |
| Token expiration | COVERED | No `SkipExpiryCheck`/`Now`: go-oidc rejects past `exp` with **zero leeway**; expired-token subtest. |
| `nbf` leeway | COVERED | Gateway sets nothing; go-oidc enforces `nbf` only when present with a hard-coded **5-minute leeway** (`TestOIDCReviewClockClaimsPinned`: +2 min accepted, +10 min rejected). The original #671 runbook omitted this; §1 now states it. |
| `iat` handling | PARTIAL (deliberate non-enforcement) | Neither the gateway nor go-oidc checks `iat`. `TestOIDCReviewClockClaimsPinned` pins near-future `iat` (+2 min) accepted; new `TestOIDCCompleteFarFutureIatIsNotChecked` pins far-future `iat` (2 h, valid `exp`) accepted. Do not describe `iat` as "validated". |
| Nonce | COVERED | 32-byte random nonce per transaction, constant-time comparison in `Complete`; wrong-nonce and missing-nonce subtests. |
| State (stale / reused / single-use) | COVERED | 32 random bytes stored only as SHA-256, 10-minute lifetime, global cap 4096, per-IP cap 32; `takeState` deletes on success, so a state whose `Complete` later fails cannot be retried. Tests: tampered, wrong-browser, replayed-after-success, expired pending (forced server-side). Binding-mismatch attempts do **not** consume the state (delete happens only after the binding passes). |
| PKCE S256 (incl. mismatch) | COVERED | `S256ChallengeOption` on the authorization request; verifier lives only in the server-side pending transaction; exchange failure is a hard failure. Tests: `code_challenge_method=S256` + 43-char challenge + verifier reaching the token endpoint; PKCE-mismatch subtest (fixture IdP returns `invalid_grant`). Uniqueness over 64 transactions (ported from #666). |
| Callback validation | COVERED | GET-only; exactly one `state`, exactly one `code`; flow-binding cookie present and well-formed; provider `error` rejected; `Complete` rejects empty or >4096-byte code and missing/oversize (>64 KiB) `id_token`; fixed configured `RedirectURL`, caller `next` ignored. Tests: duplicate state/code, bad state/binding, provider-error, missing state, and the new empty/overlong-code tests (malformed codes never reach the token endpoint). Ordering note: `takeState` consumes the state **before** the provider-error check, so an error callback burns the transaction (fail-closed, requires a fresh login). |
| Browser binding | PARTIAL (split verdict) | Pending-login binding COVERED: 32-byte random binding in an `HttpOnly; Secure; SameSite=Lax; Path=/admin/` cookie, SHA-256 stored, constant-time compared; wrong-browser test denies. Session binding MISSING by design: sessions carry no IP/User-Agent and are never compared against them; a stolen session cookie replays until TTL (8 h absolute / 30 min idle defaults), logout, or revocation. Mitigations: opaque random token, cookie flags, rotation, policy revocation. |
| Session rotation | COVERED | Callback mints a fresh 32-byte session ID and `RotateSession`s in the same transaction as the `auth.login.succeeded` audit event. Lifecycle test rotates across viewer → operator → admin and asserts each old session then gets 401. Policy/issuer mismatch revokes on next use. |
| Logout / revocation (incl. failure) | COVERED | POST-only, strict CSRF first, revocation + `auth.logout` in one transaction, then cookies cleared. Revocation-persist failure → 503 **before** any cookie is cleared, session stays active (ported from #666: `TestOIDCLogoutRevocationFailureKeepsSessionActive`); no false logout is ever claimed. Provider back-channel logout and RP-initiated logout are MISSING by design: logout is local-only; an IdP-side revocation does not reach the gateway. |
| CSRF protection | COVERED | Same-origin check + double-submit token (`HttpOnly; Secure; SameSite=Strict` cookie mirrored in `X-NexaRoute-CSRF`, constant-time compare). The OIDC callback itself is a GET and correctly does not use this token — it relies on state + flow-binding cookie instead. |
| Role / claim mapping | COVERED | Only a string or an array of strings is accepted; null/missing → no values; unmapped values ignored; zero mapped roles → denied; two or more **distinct** mapped roles → denied; duplicates collapsing to one role are harmless. Config requires 1–100 mappings, values only `viewer`/`operator`/`admin`, no default role, no inference from other claims. Tests (ported from #666): unmapped, missing, conflicting, malformed role claims — each 401, no session, exactly 4 `auth.login.failed` records. |
| Audit behaviour (incl. persist failure) | COVERED | `auth.login.started`, `auth.login.failed` (actor always `anonymous`), `auth.login.succeeded` (same transaction as rotation), `auth.logout` (same transaction as revocation), `auth.session.policy_mismatch`, `admin.access.denied`. Fail-closed: login-start audit failure → 503 with no redirect/flow cookie; callback-denial audit failure → 503 instead of 401; success-persist failure → 503, no session (ported from #666); logout-persist failure → 503, session stays. Lifecycle test asserts no audit record contains any canary (state, nonce, challenge, code, verifier, session/binding values, client secret, access token). |
| Verifier algorithm scope | PARTIAL (deliberate) | Effectively **RS256-only**: no `SupportedSigningAlgs` is configured, so go-oidc defaults to RS256; a tenant signing only with ES256/PS256/EdDSA fails closed until the verifier is configured otherwise. Operators must confirm RS256 (runbook §1). |
| Pending-transaction locality | PARTIAL (documented) | Pending logins are process-local in-memory even on shared PG/Redis backends; the callback must return to the starting instance (single instance or sticky routing), and a restart drops pending logins fail-closed while sessions/audit survive (`TestOIDCReviewSessionSurvivesGatewayRestart`). |
| Other token claims | MISSING (by design) | `at_hash`, `auth_time`/`max_age`, token `typ`, and access-token validity are not checked; identity comes solely from the ID token after exchange. No refresh-token handling. |

## 2. What this PR adds over the review branch

Ported from #666 (tests + docs, no production change): the four
role-denial tests, the 64-transaction uniqueness test, the
session-persistence-failure test, the logout-revocation-failure
test, the JWKS-overlap step, `completeOIDCTestLogin` /
`oidcWriteFailureBackend` helpers, the `missing-role-claim` /
`malformed-role-claim` fixture modes, the live checklist
(scorecard), and the `EXTERNAL_VERIFICATION.md` /
`KNOWN_GAPS.md` updates. The single `signedIDToken` merge point
was resolved as: review-branch claims construction, then the
#666 mode-switch, then the `unsigned` early return after marshal.

Ported from #671 (no Go change): the operator runbook (single
acceptance process, with the four corrections listed in its
consolidation note) and both preflight scripts, unchanged except
for the runbook corrections.

New gap tests (this PR only): `TestOIDCCompleteAudienceWithoutClientIDDenied`,
`TestOIDCCompleteCallbackCodeLengthDenied` (empty + overlong code,
never reaching the token endpoint), and
`TestOIDCCompleteFarFutureIatIsNotChecked` (pins the deliberate
`iat` non-enforcement; not a validation claim).

No claim validation was weakened anywhere. The only production
change carried in is the review line's `azp` check.

## 3. LIVE tenant acceptance: BLOCKED

Tenant-check method (sanitized only, no secret handling): checked
for OIDC-related environment variable **names** only (none set),
for an `admin.oidc` configuration in the repository (none outside
docs/tests), and for public issue #647 text (its two comments
supply no tenant, no authorization, and no test account). The
#671 preflight self-test was run against loopback fixtures only
(PASS — loopback evidence, not live evidence). **Zero live IdP
calls were made.**

Exact missing input, all of it: an operator-owned disposable
non-production tenant (provider kind + exact version + issuer
URL), a registered confidential client with the exact callback
`https://<gateway>/admin/auth/oidc/callback` and its secret
referenced only by environment name, the expected audience and
role/group claim with claim-value → `viewer`/`operator`/`admin`
mappings, one mapped test user per role plus one unmapped user,
and the operator's authorization to run the runbook's §3–§7 on
that tenant. Until then, live cases stay BLOCKED, #647 stays OPEN,
and no document in this PR claims organizational-IdP
interoperability.

## 4. Verification (local)

- `gofmt` clean on `internal/httpapi`; `go vet ./internal/httpapi` clean.
- Focused suite `go test -count=1 -run 'TestOIDC|TestCoverageAdminAuth|TestSession|TestAdminSession' ./internal/httpapi` PASS (39 s).
- New + ported tests individually verbose-PASS (see PR body).
- Preflight self-test `scripts/test-oidc-live-preflight.sh` PASS
  (loopback fixtures only).
- `go test -race`, browser tests, and `scripts/verify.sh`:
  reported in the PR body on the final SHA (see there for the
  exact commands and outcomes; the known httpapi `-race`
  3-minute timeout artifact, if it recurs, is reported as a
  timeout artifact, never as a pass).
- Known pre-existing failure, NOT fixed silently here:
  `TestAdminClientKeyRevokePersistenceFailureLeavesKeyActive`
  was proven pre-existing on pristine main `e6c3cdf3` in the
  review line (unrelated to OIDC); it needs its own triage.
