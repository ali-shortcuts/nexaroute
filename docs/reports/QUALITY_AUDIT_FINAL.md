# Quality Audit Final Report - Issue #593

Date: 2026-10-21
Branch: `opencode/issue593-20261021073246`
Base: `7a815a4ba5a951a1be5e3cd591e58e3ca7125e00`
Implementation checkpoint: [`6144fc028da766ed5cf3d50dc21a1542356178aa`](https://github.com/ali-shortcuts/nexaroute/commit/6144fc028da766ed5cf3d50dc21a1542356178aa)

## Outcome

The cleanup batch is implemented and locally verified. It fixes two test-visible defects, removes proven dead code and no-op assignments, tightens provider-report permissions, and adds report-only gosec/deadcode audit jobs. It does not attempt the four architecture refactors, does not triage every gosec finding, and does not certify production readiness.

| Result | Value |
| --- | --- |
| Confirmed defects fixed | 2 (forwarded-scheme cookie control; world-readable report directory) |
| Files changed at checkpoint | 44 (Go code and tests, JS, workflow) |
| Diff size at checkpoint | +183 / -407 lines |
| Documentation added after checkpoint | 4 files (`docs/SECURITY.md` plus these 3 audit reports) |
| Unreachable functions removed | 21 |
| Other unused Go symbols removed | 10 |
| Unused web i18n lines removed | 39 (13 keys x 3 locales) |
| `SA4006` no-op assignments fixed | 16 |
| `SA4000` weak assertion fixed | 1 |
| Coverage, baseline `7a815a4` | 84.3% statements |
| Coverage, final checkpoint | 84.5% statements |
| Targeted staticcheck (`SA4006,SA4000,U1000`) | 48 lines before, 0 after |
| Deadcode | 25 before, 4 protected after |
| Gosec | 159 before, 158 after; report-only |
| Local verification | `./scripts/verify.sh` exit 0, `VERIFY PASS` |

The `86.2%` figure in the manual report is `internal/budget` package coverage from older reports, not the repository total. See `QUALITY_AUDIT_BASELINE.md` for the exact reconciliation.

## Implemented changes

### Security and permissions

- `internal/httpapi/server.go`: `requestIsSecure` now uses `r.TLS != nil` only. Caller-supplied `X-Forwarded-Proto` and similar headers no longer influence `Secure` cookies or the same-origin scheme check.
- `internal/httpapi/transport_security_test.go`: added `TestAdminCookiesIgnoreSpoofedForwardedProto`; it failed before the fix and passes after.
- `cmd/gateway/provider_accept.go`: report directory is created with `0700` instead of `0755`; the report file remains `0600`.
- `cmd/gateway/provider_accept_test.go`: added `TestProviderAcceptReportDirectoryIsPrivate`; it failed before the fix and passes after.
- `docs/SECURITY.md`: documents that forwarding headers are not trusted for cookie security and that TLS-terminating proxies in front of plain HTTP do not produce `Secure` cookies.

### Dead code and no-op cleanup

- Removed 21 unreachable private/test-only functions and 10 unused constants, fields, types, and variables.
- Removed 13 unused `BODY_I18N` keys from all three locales in `internal/httpapi/web/app.js`.
- Fixed all 16 targeted `SA4006` findings, including the misleading empty loop and unused `delta` block in `internal/decision/policy/provider.go`.
- Fixed the `SA4000` tautological assertion in `internal/httpapi/helpers_test.go`.
- Retained exactly four dead functions as owner-decision candidates: `CloneForHotReload`, `IsUnavailable`, `NewFileBudgetAlertStore`, and `FeedbackTracker.Cards`.

### Audit workflow

`.github/workflows/audit.yml` now includes report-only `gosec` and `deadcode` jobs:

- `gosec v2.29.0`
- `deadcode` from `golang.org/x/tools v0.52.0`
- outputs uploaded as artifacts
- no ratchet, no baseline-fail gate, and no CI failure on findings

The YAML parses and both jobs have the expected checkout/setup/install/run/upload steps. A workflow run on this checkpoint does not exist yet; the only official run is on the base commit.

## Verification evidence

### Project verification

Command:

```bash
./scripts/verify.sh > .artifacts/verify-final.log 2>&1
```

Result: exit `0`, final line `VERIFY PASS`.

Covered by that run:

| Gate | Result |
| --- | --- |
| Formatting | PASS |
| Mandatory unit/integration test pass (57 packages) | PASS |
| `go vet ./...` | PASS |
| Mandatory race pass (57 packages) | PASS |
| Release artifact manifest checks | PASS |
| SBOM generator trust-chain checks | PASS |
| Web UI JavaScript syntax | PASS |
| Browser control-plane acceptance | Skipped: Chromium + Python Playwright unavailable |
| Short fuzz checks (`FuzzPatchJSONModel`, `FuzzParseAnthContent`) | PASS |
| Linux amd64 build | PASS |
| Linux arm64 build | PASS |

### Coverage

```bash
go test -timeout=40m -count=1 -coverprofile=.artifacts/audit-baseline/coverage-final.out ./...
go tool cover -func=.artifacts/audit-baseline/coverage-final.out
```

Final total: `84.5%` of statements. Baseline at `7a815a4`: `84.3%`. The increase is consistent with removing uncovered dead code, not with adding broad new test coverage.

### Static analysis and dead code

```bash
"$(go env GOPATH)/bin/staticcheck" -checks 'SA4006,SA4000,U1000' ./...
"$(go env GOPATH)/bin/deadcode" -test ./...
"$(go env GOPATH)/bin/gosec" -fmt=text -out=.artifacts/gosec-final.txt ./...
```

- Targeted staticcheck: exit `0`, no output.
- Deadcode: only the four protected functions remain.
- Gosec: exit `1`, 158 issues, 232 files, 75,485 lines, 1 `#nosec`. This is report-only and is not a CI failure.
- Full staticcheck (all checks) still reports 46 style/other findings. They are outside this task's targeted cleanup and remain report-only.

## Pull request and CI status

- Branch pushed: `opencode/issue593-20261021073246` at `6144fc0`.
- PR: not created by this environment; GitHub reported no pull request for the branch at report time.
- Official audit run: [39559283242](https://github.com/ali-shortcuts/nexaroute/actions/runs/39559283242), `success`, HEAD `7a815a4` (base, not this checkpoint).
- No statement is made that PR CI or a new audit run on `6144fc0` is green. Those runs must be observed after the PR is opened.

## Remaining limitations

- No live browser acceptance.
- No live PostgreSQL, Redis, OIDC IdP, KMS/keyring, or upstream provider verification.
- No multi-replica test of breaker, budget, session, or video state.
- Artifact byte download required authentication and returned HTTP 401 unauthenticated.
- 146 gosec findings were not individually triaged; 12 inspected findings were false positives (6 `G124`, 6 `G101`).
- Full staticcheck style findings (46) remain.
- TLS-terminating reverse proxies in front of a plain-HTTP gateway will not get `Secure` cookies without a future trusted-proxy option. This is documented, not resolved.

## Owner decisions

1. Whether to refactor `Config.Validate` (complexity 644) into domain validators.
2. Whether to deduplicate virtual-key create/rotate/revoke mutation paths.
3. Whether distributed circuit-breaker/cooldown state is required, and if so which consistency/failure model to use.
4. Whether video jobs need a durable shared queue and asset store.
5. Whether to remove or keep the four protected dead functions.
6. Whether to add an explicit trusted-proxy configuration for `Secure` cookies behind TLS termination.

## Recommendation

Ready for review as a scoped cleanup and hardening PR after the PR is opened and CI/Security plus a fresh audit run are observed on the checkpoint SHA. Do not treat this as a production certification. The architecture items and the trusted-proxy decision should be handled separately.

---

## Earlier audit pass (PR #595, branch `opencode/issue593-20261021064216`)

The following section records the independent findings and fixes from the earlier audit pass that was rebased and merged alongside this report; it is preserved verbatim for traceability.

# Post-release quality audit final report (Issue #593)

Date: 2026-10-21
Audited commit: `05003db9326b01464a368723d8ab31671b40dc8f`
Branch: `opencode/issue593-20261021064216`

## Outcome

Nine confirmed defects were fixed with regression coverage. The complete local
verification gate passes. No release, tag, or published artifact was modified.

| Metric | Result |
| --- | --- |
| Confirmed defects | 9 |
| Fixed with regression tests | 9 |
| P1 findings | 6 (`AUD-01` through `AUD-06`) |
| P2 findings | 3 (`AUD-07` through `AUD-09`) |
| Remaining confirmed defects in audited scope | 0 |
| Local verification | `VERIFY PASS` |

Finding details are in `docs/reports/QUALITY_AUDIT_FINDINGS.md`.

## Code changes

| File | Change |
| --- | --- |
| `internal/egress/policy.go` | Stop pinning TLS `ServerName`; permit the validated proxy hop at dial time |
| `internal/egress/policy_test.go` | Proxy/allow-list regression test |
| `internal/egress/tls_servername_test.go` | Real TLS handshake/SNI regression test |
| `internal/httpapi/media.go` | Apply privacy requirement; read payload before committing status |
| `internal/httpapi/media_test.go` | Truncated JSON and audio response regression tests |
| `internal/httpapi/privacy_p2_test.go` | Media `no_training` regression test |
| `internal/httpapi/admin_auth.go` | Reject emergency credentials whenever OIDC is enabled and healthy |
| `internal/httpapi/oidc_test.go` | Healthy-OIDC emergency-key regression test |
| `internal/httpapi/compat_engine.go` | Propagate structured-output requirement into the capability profile |
| `internal/compat/capabilities.go` | Treat verified `StructuredOutput` unsupported as ineligible |
| `internal/httpapi/structured_output_gate_test.go` | Fail-closed structured-output regression test |
| `internal/httpapi/openai.go`, `internal/httpapi/anthropic.go` | Apply capability gate to hedge partners |
| `internal/httpapi/hedging_test.go` | Ineligible hedge-partner regression test |
| `internal/protocol/canonical/anthropic.go` | Raise `max_tokens` above the thinking budget |
| `internal/protocol/canonical/anthropic_budget_test.go` | Thinking-budget regression test |
| `internal/protocol/canonical/stream.go` | Preserve and emit Anthropic cache-token usage |
| `internal/protocol/canonical/stream_cache_usage_test.go` | Cache-token decoding/emission regression tests |

## Verification evidence

All commands below were run in the repository checkout on the audit branch after
the fixes. Outcomes are actual command results, not inferred.

| Command | Result |
| --- | --- |
| `go test ./internal/egress -count=1` | pass |
| `go test ./internal/httpapi -run 'TestMedia' -count=1` | pass |
| `go test ./internal/httpapi -run 'TestOIDC|TestAdmin|TestSession|TestEmergency' -count=1` | pass |
| `go test ./internal/httpapi -run TestHedgeSkipsCapabilityIneligiblePartner -count=1` | pass |
| `go test ./internal/httpapi -run TestStructuredOutputUnsupportedFailsClosed -count=1` | pass |
| `go test ./internal/protocol/... ./internal/compat ./internal/egress -count=1` | pass |
| `go test -count=1 -timeout=10m ./...` | exit 0 |
| `go vet ./...` | exit 0 |
| `go test -race -count=1 -timeout=15m ./...` | exit 0 |
| `./scripts/verify.sh` | exit 0, `VERIFY PASS` |

`scripts/verify.sh` also passed shell syntax checks, the flake-hunt harness
checks, gofmt, release artifact manifest checks, SBOM trust-chain checks, the
web UI JavaScript syntax checks, short fuzz checks, and linux/amd64 plus
linux/arm64 builds.

Regression-test evidence before each fix is recorded in
`docs/reports/QUALITY_AUDIT_FINDINGS.md` and in `.artifacts/*-prefix.log`
(git-ignored local logs).

## CI and external verification

- The audited SHA had green CI (`39529391279`) and Security (`39529391318`)
  runs.
- Post-change GitHub Actions CI/Security were not observed in this environment;
  they are expected to run on the pull request.
- `gh` is not authenticated, so the auditor could not create or merge the pull
  request directly. The branch and commits are the handoff artifact.
- Browser acceptance (`scripts/test-browser-e2e.py` and related scripts) was
  skipped because Chromium and Python Playwright are unavailable.
- No live OIDC provider, PostgreSQL, Redis, or external model provider was
  exercised.
- `golangci-lint`, `staticcheck`, `govulncheck`, and `gosec` are not installed;
  their checks are UNVERIFIED, not passed.
- `v0.23.0` was not modified, deleted, or re-tagged. No release was created.

## Residual risks and follow-ups

1. **Media buffering:** successful media payloads are now read into memory
   before the response is committed. The existing 32 MiB JSON and 64 MiB binary
   caps bound each request, but operators with very high concurrent audio
   traffic should monitor memory.
2. **Live provider behavior:** all protocol fixes are validated against
   dialect-faithful fixtures and local test servers, not live Anthropic/OpenAI
   endpoints. The `max_tokens` and cache-usage changes should still be confirmed
   against a live Anthropic-compatible provider during rollout.
3. **Proxy scope:** the dial-scoped allow-list deliberately permits only the
   configured proxy host. Combined proxy plus `allowed_hosts` deployments
   should be exercised once in staging to confirm the expected network path.
4. **Unavailable static analysis:** run `golangci-lint`/`staticcheck`/
   `govulncheck`/`gosec` in CI or a developer environment before merge if they
   are part of the release policy.

## Recommended next actions

1. Open the pull request from `opencode/issue593-20261021064216` to `main` and
   confirm post-change CI and Security runs are green.
2. Exercise the media, proxy, and Anthropic-thinking paths against staging
   infrastructure with a real upstream and a live OIDC provider.
3. Add the unavailable static analyzers and browser acceptance to the
   environment used for release qualification so those gates are no longer
   environment-dependent.
