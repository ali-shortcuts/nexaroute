# Quality Audit Findings - Issue #593

Date: 2026-10-21
Base: `7a815a4ba5a951a1be5e3cd591e58e3ca7125e00`
Implementation checkpoint: `6144fc028da766ed5cf3d50dc21a1542356178aa`

## Summary counts

| Category | Count | Status |
| --- | --- | --- |
| Confirmed cookie-security defect | 1 | Fixed with regression test |
| Confirmed report-directory permission defect | 1 | Fixed with regression test |
| Other confirmed runtime/data-loss bugs | 0 | None found in the reviewed scope |
| Staticcheck `SA4006` no-op assignments removed | 16 | Fixed |
| Staticcheck `SA4000` weak test assertion corrected | 1 | Fixed |
| Unreachable Go functions removed | 21 | Fixed |
| Other unused Go symbols removed | 10 | Fixed |
| Unused web i18n lines removed | 39 (13 keys x 3 locales) | Fixed |
| CI/tooling gap closed | 1 | Fixed (report-only gosec/deadcode jobs) |
| Gosec false positives inspected | 12 | 6 `G124`, 6 `G101` |
| Architecture debt requiring an owner decision | 4 | Not implemented; proposals below |
| Gosec findings not yet individually triaged | 146 | Report-only; not confirmed bugs |
| Full staticcheck style findings remaining | 46 | Report-only; out of this task's scope |
| Dead code remaining | 4 protected functions | Intentionally retained |

The two test-visible defects are the only confirmed defects fixed here. The dead-code and no-op changes are behavior-preserving code-quality work. No claim is made that the remaining gosec or style findings are safe; they are untriaged signals, not a clean bill of health.

## Confirmed defects

### SEC-01: forwarded scheme header controlled cookie `Secure`

- Severity: medium security hardening / availability.
- Location: `internal/httpapi/server.go`, `requestIsSecure`.
- Before: the function trusted `X-Forwarded-Proto: https`. A plain-HTTP request could therefore cause admin session/CSRF cookies to be issued with `Secure`, and `sameOrigin` could treat an `https` origin as matching a plain-HTTP listener. The header is caller-controlled.
- Impact: the cookie security decision was not derived from the gateway's own connection state. This could lock a browser out of an HTTP admin session and weakened the origin/scheme invariant. It was not a direct credential bypass because the admin credential and CSRF token checks still applied.
- Fix: `requestIsSecure` now returns `r.TLS != nil` only. `X-Forwarded-Proto`, `Forwarded`, and similar headers are ignored.
- Regression test: `TestAdminCookiesIgnoreSpoofedForwardedProto` in `internal/httpapi/transport_security_test.go`.
- Pre-fix failure:

```text
spoofed X-Forwarded-Proto made cookie "nexaroute_admin_csrf" Secure over plain HTTP
```

- Post-fix: test passes; the existing plain-HTTP and direct-TLS cookie tests also pass.
- Deployment note: a TLS-terminating reverse proxy in front of a plain-HTTP gateway will no longer produce `Secure` cookies. This is now documented in `docs/SECURITY.md`. A future explicit trusted-proxy configuration would be required to restore that deployment mode safely.

### PERM-01: provider acceptance report directory was world-readable

- Severity: low confidentiality / defense in depth.
- Location: `cmd/gateway/provider_accept.go`, `writeProviderAcceptReport`.
- Before: `os.MkdirAll(dir, 0o755)` created the report directory with group/other execute and read access. The report file itself was already `0600`.
- Impact: directory traversal/listing by other local users; the redacted report can still reveal provider names, models, and acceptance results.
- Fix: create the directory with `0o700`; retain the existing `0600` file mode and explicit `Chmod`.
- Regression test: `TestProviderAcceptReportDirectoryIsPrivate` in `cmd/gateway/provider_accept_test.go`.
- Pre-fix failure:

```text
report directory mode=755, want no group/other access (0700)
```

- Post-fix: test passes.

## Dead code and no-op cleanup

### DEAD-01: 21 unreachable functions removed

Evidence: `deadcode -test ./...` reported 25 functions before and 4 protected functions after. The removed functions were private or test-only dead paths:

`State.valid`, `f64`, `MemoryStore.available`, `testTLSServer.port`, `codingCase`, `applyProviderHeaders`, `proxyValidatedJSONResponse`, `budgetModelServed`, `extractOpenAIUsage`, `extractAnthropicUsage`, `canonicalFromOpenAIRequest`, `conformanceCfg`, `evaluationPlane.stateModTime`, `hardCooldownStatus`, `redactProviderBody`, `Server.routeSnapshot`, `traceFromContext`, `decisionFromScored`, `socioDummy`, `mustJSON`, `writeValidWAVBytes`.

Removal was limited to symbols proven unused by both `deadcode` and targeted staticcheck U1000. No exported public API was removed.

### DEAD-02: 10 other unused Go symbols removed

- Constants: `redisMaxUint64Decimal`, `redisMaxInt64Decimal`, `budgetAlertNamespace`.
- Fields: `capabilities`, `taskAnalysisErrors`, `sawCR`, `anthropicEmitter.started`.
- Types: `clientAuthState`, `telemetrySample`.
- Variable: `keyAuditSeq`.

### DEAD-03: 13 unused web i18n keys removed

Removed from `internal/httpapi/web/app.js` in all three locales (EN, FA, PS; 39 lines):

`pol_auto_state`, `pg_response`, `pg_idle`, `pg_response_text`, `set_no_usage`, `pr_disabled`, `pr_configured`, `pr_no_provider_error`, `el_show`, `el_hide`, `req_empty`, `req_empty_d`, `wz_add_provider`.

### Protected dead code retained

`deadcode` still reports exactly four functions. They are intentionally retained as owner-decision candidates, not because they are proven live:

| Function | Location | Reason to retain |
| --- | --- | --- |
| `CloneForHotReload` | `internal/decision/jev/provider.go:441` | Possible future hot-reload seam; removing it would change a provider extension point. |
| `IsUnavailable` | `internal/decision/remote/errors.go:70` | Public error-classification helper; may be used by external packages or future wiring. |
| `NewFileBudgetAlertStore` | `internal/httpapi/budget_alerts.go:131` | Alternate budget-alert store constructor. |
| `FeedbackTracker.Cards` | `internal/scorecards/feedback.go:186` | Public scorecard inspection helper. |

No further dead-code removal is recommended without an explicit owner decision.

### NOOP-01: all 16 `SA4006` findings fixed

Files and semantics:

- `internal/decision/policy/provider.go`: removed an empty loop, a redundant `origPrimaryID` assignment, and an unused `delta` block. Existing `TestProvider_MinScoreDeltaAgainstOriginalPrimary` and the rest of the policy package cover the behavior.
- `internal/httpapi/decision_wiring.go`: removed a fallback loop whose body contained only comments. Existing virtual-key/fallback tests pass.
- `internal/httpapi/openai.go`, `internal/httpapi/anthropic.go`: removed overwritten adapter/start variables; declarations now reflect the actual use.
- `internal/probe/engine.go`: first deployment lookup ignores the unused adapter; the second keeps it.
- `internal/compat/sanitizer.go`, `internal/feature/extractor.go`, `internal/protocol/canonical/validation.go`, `internal/video/render/render.go`: removed no-op assignments/conditions.
- Test-only no-ops: `cmd/gateway/wavec_coverage_test.go`, `internal/decision/chain_test.go`, `internal/httpapi/virtual_test.go`, `internal/secrets/secrets_test.go`.

Evidence: targeted staticcheck `SA4006,SA4000,U1000` produced 48 lines before and zero lines after.

### TEST-01: `SA4000` tautological assertion corrected

`internal/httpapi/helpers_test.go` previously used `if !condition || !condition`. Staticcheck flagged identical expressions; the second call could not add coverage. The test now evaluates two explicit admission assertions in sequence.

## CI and audit-tooling finding

### CI-01: audit workflow lacked gosec and deadcode artifacts

- Fix: `.github/workflows/audit.yml` now has `gosec` and `deadcode` jobs, both report-only, with pinned tool versions and artifact upload.
- Local pins: `gosec v2.29.0`, `deadcode` from `golang.org/x/tools v0.52.0`.
- The jobs use the existing checkout/setup/upload action SHAs already present in the workflow.
- YAML parses; job step lists verified.
- No ratchet or baseline-fail mechanism was added, as required.
- The workflow run on this branch/PR is pending creation of the PR; do not claim a green run for this checkpoint until it exists.

## False positives inspected

- `G124` (6): cookie `Secure` values are computed by `requestIsSecure`; gosec cannot see through the function call and treats the argument as a possible hardcoded false. These are false positives after inspection.
- `G101` (6): flagged literals are provider URLs, reason/class strings, map keys, and environment-variable names (`NEXAROUTE_CANARY_API_KEY`, `NEXAROUTE_CANARY_SECRET_KEY`), not secret values. These are false positives after inspection.

The remaining gosec findings (`G304` 54, `G104` 33, `G703` 28, `G115` 14, `G204` 12, `G704` 4, `G705` 1) were not individually triaged in this task. They remain report-only and must not be described as confirmed vulnerabilities.

## Architecture debt and proposals

These are design decisions, not cleanup items. No large refactor was performed.

### ARCH-01: `config.Config.Validate` is extremely complex

- Evidence: `gocyclo v0.6.0` reports cyclomatic complexity `644` for `internal/config/config.go:1818` (`Config.Validate`). `ApplyDefaults` is also high (105).
- Risk: high review cost, hard-to-isolate validation regressions, and brittle error ordering.
- Proposal: keep the public `Validate` signature, but split it into domain validators (`validateTLS`, `validateAdmin`, `validateProviders`, `validateVideo`, `validateRouting`, ...) called in a fixed order. Add table-driven tests per domain before moving code. This is a multi-PR refactor and needs owner approval.

### ARCH-02: virtual-key HTTP create/rotate/revoke paths duplicate mutation logic

- Evidence: `internal/httpapi/client_keys.go` repeats key generation, config mutation, not-found handling, and response construction across create, rotate, and revoke.
- Risk: future changes (budget fields, audit events, validation) can drift between paths.
- Proposal: extract a single `mutateVirtualKey`/`applyVirtualKeyMutation` helper that owns lookup, mutation, error mapping, and response shaping while preserving the existing HTTP contract. Add contract tests for 201/200/204/404/405 before refactoring.

### ARCH-03: circuit-breaker and cooldown state are per instance

- Evidence: `internal/httpapi/deployment.go:92` documents breaker/cooldown state as per-instance; `docs/HA_STATE_INVENTORY.md` records the same boundary.
- Risk: multiple replicas can disagree about open circuits and cooldowns, causing uneven load and repeated probing of a failing provider.
- Options: (a) keep per-instance semantics and document them as an availability trade-off; (b) move breaker state to Redis with atomic transitions and TTLs; (c) use a lease/owner model where one replica probes and publishes state. Option (b) adds a dependency and needs fail-open/fail-closed rules. Owner decision required.

### ARCH-04: video jobs and assets are local to one process

- Evidence: `docs/HA_STATE_INVENTORY.md:26` and `docs/KNOWN_GAPS.md:115-116` state that video jobs and worker shutdown are single-process/local.
- Risk: no durable queue or shared asset store across replicas; process loss can strand jobs.
- Options: (a) keep single-instance as an explicit deployment constraint; (b) add a PostgreSQL job table with `FOR UPDATE SKIP LOCKED` claims plus shared object storage; (c) use an external queue. Option (b) requires schema, idempotency, and lease-expiry design. Owner decision required.

## Limitations and unverified areas

- No live browser acceptance: Chromium/Python Playwright unavailable.
- No live PostgreSQL, Redis, OIDC IdP, KMS/keyring, or upstream provider verification.
- No multi-replica test of breaker, budget, session, or video state.
- Artifact download required authentication and returned HTTP 401 unauthenticated.
- Full staticcheck after the changes reports 46 style/other findings. The targeted `SA4006,SA4000,U1000` set is clean, but the full style set was not part of the requested cleanup.
- The audit workflow's report-only jobs do not prove the absence of vulnerabilities.

---

## Earlier audit pass (PR #595, branch `opencode/issue593-20261021064216`)

The following section records the independent findings and fixes from the earlier audit pass that was rebased and merged alongside this report; it is preserved verbatim for traceability.

# Post-release quality audit findings (Issue #593)

Audited commit: `05003db9326b01464a368723d8ab31671b40dc8f`
Audit branch: `opencode/issue593-20261021064216`

## Summary

| ID | Severity | Area | Defect | Status |
| --- | --- | --- | --- | --- |
| AUD-01 | P1 | Egress TLS | SNI and certificate verification pinned to the provider base host | Fixed |
| AUD-02 | P1 | Egress proxy | `allowed_hosts` rejected the configured proxy hop | Fixed |
| AUD-03 | P1 | Privacy | Media ingress ignored `no_training` route profiles | Fixed |
| AUD-04 | P1 | Capability gating | Unsupported structured output was silently dropped and served as 200 | Fixed |
| AUD-05 | P1 | Anthropic protocol | `max_tokens` could be less than or equal to `thinking.budget_tokens` | Fixed |
| AUD-06 | P1 | Admin auth | Emergency API key remained valid while healthy OIDC was enabled | Fixed |
| AUD-07 | P2 | Media response | Truncated/oversized upstream media produced a silent 200 | Fixed |
| AUD-08 | P2 | Hedging | Hedge partner bypassed the capability-ineligible gate | Fixed |
| AUD-09 | P2 | Usage accounting | Anthropic stream cache-token usage was dropped | Fixed |

Nine defects were confirmed with failing tests or equivalent call-site proof.
All nine were fixed and covered by regression tests in this branch.

## AUD-01: egress TLS ServerName pinned to the base host

- Type: security/correctness
- Severity: P1
- Confidence: high
- Original location: `internal/egress/policy.go` (`NewTransport`, TLS config)
- Fix location: `internal/egress/policy.go:314`
- Regression test: `internal/egress/tls_servername_test.go`

The provider egress transport set `TLSClientConfig.ServerName` to the hostname of
the provider's base URL. Absolute-URL endpoint overrides to another host in
`allowed_hosts` are a supported operator feature, and the dialer correctly
resolved and policy-checked the override host, but TLS still sent SNI for and
verified the certificate of the base host.

Failure evidence before the fix:

```text
request to allow-listed HTTPS host failed TLS verification:
Get "https://localhost:41665": tls: failed to verify certificate:
x509: certificate is valid for localhost, not base.invalid
```

Impact: allow-listed HTTPS overrides to a different host failed closed in the
common case. The check also verified the wrong identity, so the transport was
not actually validating the certificate of the host it connected to.

Fix: leave `ServerName` empty and let `net/http` derive SNI and certificate
verification from the request URL host, matching the decision transport. The
regression test uses a CA-signed `localhost` certificate and asserts both the
handshake and the server-observed SNI.

## AUD-02: proxy host rejected by `allowed_hosts`

- Type: functional/security configuration
- Severity: P1
- Confidence: high
- Original location: `internal/egress/policy.go` (dial-time allow-list)
- Fix location: `internal/egress/policy.go:263`
- Regression test: `internal/egress/policy_test.go`
  (`TestNewTransportAllowsProxyHostOutsideAllowedHosts`)

`docs/CONFIGURATION.md` states that the dial-time transport additionally permits
the `proxy_url` host because it dials the proxy hop. The dial closure applied
`allowed_hosts` to every address, including the proxy. Any deployment that set
both `allowed_hosts` and `proxy_url` failed with:

```text
proxyconnect tcp: egress host "127.0.0.1" is not allow-listed
```

Fix: build a dial-scoped policy that appends the already-validated proxy host
when `allowed_hosts` is non-empty. Request building still validates target URLs
against the original allow-list, so the proxy host does not become a target
host. An empty allow-list remains unrestricted.

## AUD-03: media ingress bypassed `no_training`

- Type: privacy
- Severity: P1
- Confidence: high
- Original location: `internal/httpapi/media.go` (`serveMedia`)
- Fix location: `internal/httpapi/media.go:183` and `:200`
- Regression test: `internal/httpapi/privacy_p2_test.go`
  (`TestMediaHonorsNoTrainingRouteProfile`)

Embeddings, images, audio, and rerank requests built a router requirement
without calling `applyPrivacyRequirement`. A `no_training` route profile or
virtual-key requirement was therefore ignored and a training-enabled provider
could receive the prompt.

Failure evidence before the fix:

```text
training-enabled provider received 1 media request(s) for a no_training route
```

Fix: stamp the privacy requirement before candidate selection and map the
empty-candidate case to the same `privacy_unavailable` 503 and privacy-safe
event used by the chat ingresses. The regression test proves the
training-enabled deployment receives zero requests and the compliant deployment
serves the request.

## AUD-04: structured output silently removed

- Type: correctness/API contract
- Severity: P1
- Confidence: high
- Original locations: `internal/httpapi/compat_engine.go`,
  `internal/compat/capabilities.go`
- Fix locations: `internal/httpapi/compat_engine.go:164`,
  `internal/compat/capabilities.go:575`
- Regression test: `internal/httpapi/structured_output_gate_test.go`
  (`TestStructuredOutputUnsupportedFailsClosed`)

Two gaps combined:

1. `profileFromRequirement` did not copy `router.Requirement.StructuredOutput`
   into the compatibility requirement profile for legacy ingress paths.
2. `IneligibleFor` checked only `JSONObject`/`JSONSchema`, while runtime
   learning records `CapStructuredOutput`.

A deployment with a verified-unsupported structured-output capability was
therefore eligible. The sanitizer then removed `response_format` and the client
received a plain, unstructured 200 response despite requesting JSON.

Failure evidence before the fix: the upstream was called and returned 200 with
`response_format` removed.

Fix: propagate `StructuredOutput` into the profile and treat a verified
`StructuredOutput` or both unsupported JSON format capabilities as ineligible.
The request now fails closed with 503 before any upstream call.

## AUD-05: Anthropic `max_tokens` versus thinking budget

- Type: protocol correctness
- Severity: P1
- Confidence: high
- Original location: `internal/protocol/canonical/anthropic.go`
- Fix location: `internal/protocol/canonical/anthropic.go:430`
- Regression test: `internal/protocol/canonical/anthropic_budget_test.go`

`EncodeAnthropicRequest` defaulted `max_tokens` to 4096 but independently chose
thinking budgets of 8192 (default effort) or 16384 (high effort). Anthropic
requires `max_tokens > budget_tokens`, so these payloads were rejected
upstream. Codex-style `/v1/responses` reasoning requests routed to Anthropic
were affected.

Failure evidence before the fix:

```text
max_tokens=4096 <= budget_tokens=8192
max_tokens=4096 <= budget_tokens=16384
max_tokens=1024 <= budget_tokens=4096
```

Fix: when thinking is enabled and the ceiling is not greater than the budget,
raise `max_tokens` to `budget + 1024`, matching the legacy translator.

## AUD-06: emergency API key valid while OIDC is healthy

- Type: authentication bypass
- Severity: P1
- Confidence: high
- Original location: `internal/httpapi/admin_auth.go`
- Fix location: `internal/httpapi/admin_auth.go:87`
- Regression test: `internal/httpapi/oidc_test.go`
  (`TestOIDCHealthyRejectsEmergencyAPIKey`)

`docs/SECURITY.md` states that emergency API-key access is unavailable while
OIDC is enabled. `authenticateAdmin` blocked emergency access only when OIDC was
enabled and unavailable. With a healthy OIDC provider, a request carrying the
static emergency key fell through to `legacyAdminAuthorized` and received the
owner identity.

Failure evidence before the fix: `GET /admin/api/snapshot` with the emergency
key returned `200` and the full admin snapshot while healthy OIDC was enabled.

Fix: once OIDC is enabled and discovery is healthy, an absent session never
falls back to emergency credentials. OIDC outage behavior remains 503 and OIDC
disabled behavior remains unchanged.

## AUD-07: truncated media response became a silent 200

- Type: response integrity
- Severity: P2
- Confidence: high
- Original location: `internal/httpapi/media.go` (2xx response path)
- Fix location: `internal/httpapi/media.go:276`
- Regression tests: `internal/httpapi/media_test.go`
  (`TestMediaEmbeddingsTruncatedUpstreamIsNotSilent200`,
  `TestMediaSpeechTruncatedUpstreamIsNotSilent200`)

The handler wrote the upstream status before reading the body. For non-binary
responses, a failed body read left a committed `200` with an empty body and had
already recorded deployment success. Binary responses were truncated at
`maxMediaResponseBytes` (64 MiB) without any signal to the client.

Failure evidence before the fix: a hijacked upstream that advertised a longer
body and closed early produced `200` with an empty body; truncated audio also
returned `200`.

Fix: read the full payload (bounded by the existing limits) before committing
headers. Read failures and oversized payloads are recorded as failures, allow
failover, and return 502 if no candidate succeeds. The success health/telemetry
record now happens only after the payload is usable.

## AUD-08: hedge partner ignored capability ineligibility

- Type: routing
- Severity: P2
- Confidence: high
- Original locations: `internal/httpapi/openai.go`,
  `internal/httpapi/anthropic.go` (hedge builder closures)
- Fix locations: `internal/httpapi/openai.go:258`,
  `internal/httpapi/anthropic.go:230`
- Regression test: `internal/httpapi/hedging_test.go`
  (`TestHedgeSkipsCapabilityIneligiblePartner`)

The normal attempt loop calls `capabilityIneligible` before dispatch. The
hedging path built and launched the next candidate directly, so a deployment
with a verified-unsupported required capability could receive a hedged request
and win the race.

Failure evidence before the fix:

```text
capability-ineligible hedge partner received 1 request(s)
```

Fix: apply the same capability check in the hedge builder closure. If the
partner is ineligible, hedging does not launch and the primary attempt proceeds
normally.

## AUD-09: Anthropic stream cache usage dropped

- Type: usage/cost accounting
- Severity: P2
- Confidence: high
- Original location: `internal/protocol/canonical/stream.go`
- Fix locations: `internal/protocol/canonical/stream.go` (Anthropic stream
  decoder and emitter)
- Regression test: `internal/protocol/canonical/stream_cache_usage_test.go`

The Anthropic SSE decoder read only `input_tokens` and `output_tokens`. It
ignored `cache_read_input_tokens` and `cache_creation_input_tokens` in
`message_start` and `message_delta`. The Anthropic emitter also forwarded only
input/output tokens, so canonical traffic lost cache reads and writes used for
usage accounting and client-visible usage.

Failure evidence before the fix: a `message_start` with cache read 40 and cache
creation 7 decoded to zero cache tokens; the emitted `message_delta` reported
zero cache tokens.

Fix: decode both cache fields, propagate them through canonical `Usage`, retain
them in the Anthropic emitter, and include them in the final `message_delta`
usage when non-zero.

## Areas reviewed without a confirmed defect

- Router candidate filtering, virtual endpoint/pool/fallback resolution, and
  unknown-model fallback.
- Provider protocol fallback after commitment and attempt accounting.
- Admin route/method authorization matrix, session rotation/expiry, CSRF
  double-submit, mTLS gate, and temp-file permissions.
- Egress deny lists, loopback rules, embedded IPv4 (NAT64/6to4), DNS pinning,
  and proxy credential stripping outside the two proxy/TLS defects above.
- Canonical request/response translation outside the Anthropic budget and cache
  usage defects above.
- Video cost ledger transitions and provider aggregation.
- Config defaults and bounds.

No additional defect from these areas survived a failing-test or precise
call-site check during this audit.
