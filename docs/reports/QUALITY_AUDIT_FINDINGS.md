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
