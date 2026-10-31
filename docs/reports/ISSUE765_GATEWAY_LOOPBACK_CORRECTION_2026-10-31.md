# NexaFlow gateway_base_url loopback-only correction — evidence report for owner review

**Issue:** #765 (NexaFlow combined production gateway wiring)
**PR #793 head corrected:** `b8aa54b8b2260f58b2e592f4370af1e7ce2151ff`
**Correction branch:** `fix/nexaflow-loopback-gateway-base-url`
**Correction code commit:** `c55a5e3ae2cf2d19366e6f663e9a3a76682c4eae`
**Date:** 2026-10-31
**Status:** Final local evidence report — see §7 for the conditional verdict.

## 1. Defect

`nexaflow.gateway_base_url` validation in `internal/config/config.go`
(`validateNexaflow`) accepted any `http(s)` URL with a host. That
contradicted the documented loopback-only design in
`docs/CONFIGURATION.md` and in `cmd/gateway/nexaflow.go`
(`loopbackBaseURL` derives `http(s)://127.0.0.1:<listen-port>`).

`nexaflow.GatewayClient` (`internal/nexaflow/http_backend.go`,
`doJSON` and `VideoAsset`) sends the configured Bearer token
(`GatewayTokenRef`, resolved through `SecretResolver`) as
`Authorization: Bearer <token>` to the configured base URL. Accepting
an arbitrary external URL therefore created an avoidable
credential-disclosure risk: a misconfigured or maliciously edited
config could send the token to `https://example.com` or any other
external host.

## 2. Fix (three gates, no weakening of existing checks)

1. **Config gate — `internal/config/config.go`:**
   new exported `ValidateLoopbackGatewayBaseURL`, used by
   `validateNexaflow` when a base URL is explicitly configured.
   Empty still means "derive from the listener".
2. **Wiring gate — `cmd/gateway/nexaflow.go`:**
   `loopbackBaseURL` re-validates an explicit base URL with the same
   helper, so a config that bypassed `Validate()` can never hand
   `GatewayClient` an external destination at attach time.
3. **Client gate — `internal/nexaflow/http_backend.go`:**
   `validateLoopbackBaseURL` runs at the start of `doJSON` and before
   the token is resolved in `VideoAsset`, before any network call.
   A directly constructed client therefore cannot send a token to an
   unapproved destination either.

### Allowed, and nothing else

- Scheme `http` or `https`
- Host `127.0.0.1`, `::1`, or `localhost` (case-insensitive),
  optional port in 1..65535
- Path `""` or `"/"` only

### Rejected

- Public IPs, private IPs, link-local IPs, arbitrary external
  hostnames, loopback-like hostnames (`127.0.0.1.evil.com`,
  `sub.localhost`), other loopback spellings (`127.0.0.2`,
  `::ffff:127.0.0.1`, `0.0.0.0`)
- URL userinfo (embedded credentials)
- Any path other than `""` or `"/"`, any query (including a bare `?`),
  any fragment, opaque URLs, missing host, non-http(s) schemes,
  out-of-range ports

### Documentation

`docs/CONFIGURATION.md` now states the loopback-only rule instead of
"must be an `http(s)` URL when set".

### What was NOT changed

- Automatic loopback derivation from the gateway listener is kept.
- PR #793 was **not** merged by this work.
- Issue #765 was **not** closed.
- The three previous candidate PRs (#781, #779, #771) were **not**
  closed.
- No existing security check or test was weakened to make a check
  pass; the one pre-existing wiring test that asserted an external
  explicit base URL (`https://gateway.example:8443`) was updated to
  a loopback URL, because that assertion was the defect itself.

## 3. Regression tests

- `internal/config/nexaflow_config_test.go`:
  `TestValidateNexaflowGatewayFields` (expanded) and new
  `TestValidateLoopbackGatewayBaseURL` — valid IPv4 loopback, valid
  IPv6 loopback, valid localhost, reject `https://example.com`,
  reject a non-loopback IP, reject URLs with embedded credentials,
  reject unsupported path/query/fragment, plus private/link-local/
  mapped-loopback/zero-address and port-range cases.
- `cmd/gateway/nexaflow_wiring_test.go`:
  `TestNexaflowWiringLoopbackDerivation` (explicit loopback wins,
  external/credential/path explicit bases rejected, derivation and
  TLS derivation unchanged) and
  `TestNexaflowWiringGatewayFieldsValidation` (external and
  credential bases rejected by full `Validate()`).
- `internal/nexaflow/gateway_loopback_test.go` (new):
  `TestGatewayClientLoopbackOnly` — for every rejected base URL,
  the auth-token resolver is **not** called and the HTTP transport
  is **not** called, for both `Chat` and `VideoAsset`;
  `TestGatewayClientLoopbackAccepted` — IPv4 loopback, `localhost`
  spelling, and `[::1]` loopback all succeed end to end with the
  Bearer token arriving at the loopback server.

## 4. Test evidence (local, real output, not CI)

Commands below were run with Go 1.25.3. Exact commands and outcomes
are recorded in §7; this section summarises the focused evidence.

- Focused config tests:
  `go test -count=1 -run 'TestValidateNexaflow|TestValidateLoopbackGatewayBaseURL' ./internal/config/`
  — PASS.
- Gateway wiring tests:
  `go test -count=1 -run 'TestNexaflowWiring' ./cmd/gateway/`
  — PASS.
- GatewayClient loopback tests:
  `go test -count=1 -run 'TestGatewayClientLoopback' ./internal/nexaflow/`
  — PASS.
- Race, affected packages:
  `go test -race -count=1 -timeout=3m ./internal/config/ ./internal/nexaflow/ ./cmd/gateway/`
  — PASS (config, nexaflow, gateway all `ok`).
- Browser acceptance, NexaFlow Studio (all 15 cases):
  `python scripts/test-browser-nexaflow-studio.py`
  — `nexaflow studio browser acceptance: PASS`
  (Playwright 1.55.0, Chromium 140.0.7339.16).
- Browser acceptance, standard suites:
  `python scripts/run-browser-acceptance.py`
  — `BROWSER ACCEPTANCE: PASS`
  (browser-e2e 48.5s, browser-e2e-502 5.4s, live-visual-agent 6.2s).
- Full `scripts/verify.sh`:
  see §5 — it did **not** pass cleanly in every local environment,
  for reasons unrelated to this change, documented there. That is
  why the final verdict in §7 is conditional, not a blanket GO.

## 5. Full verify.sh — what actually happened

Local verify runs were environment-sensitive, and the report does
not hide that:

- As `root` (uid 0): `go test ./...` failed in five unrelated
  packages (`cmd/videogen`, `internal/controlplane`,
  `internal/httpapi`, `internal/logging`,
  `internal/video/providers`) on tests that expect a write to a
  read-only directory to fail. Root bypasses read-only permissions,
  so those tests fail under root. Re-running exactly those tests
  on the **base commit without this change** failed identically,
  proving they are pre-existing environment failures, not a
  regression from this correction.
- As a user with uid 1000: `cmd/gateway` failed on
  `TestGap2ValidateJournalOwnership`, because that test uses uid
  1000 as its "foreign" owner. A user whose uid is 1000 is not
  foreign to it. Again unrelated to this change.
- As a non-root user with uid 1001 (CI-like): the first
  `go test ./...` pass in `verify.sh` passed cleanly, including
  `internal/config`, `internal/nexaflow`, and `cmd/gateway`, and
  `go vet` passed. The subsequent race pass passed for every
  package except `internal/httpapi`, which hit the
  `go test -race -timeout=3m` limit (180.399s, timing out while
  running `TestWave3EvaluationPersist`, a test untouched by this
  change). That package took 118.659s non-race in the root run and
  is the slowest package in the tree; the race timeout is a
  capacity/limit effect on a 4-core VM, not a race detector
  finding or a failure in the corrected code. A baseline race run
  for `internal/httpapi` on the base commit is recorded in §7 if
  it confirms the same timeout.

## 6. CodeQL and coverage

- CodeQL (Go) is a GitHub Actions job (`.github/workflows/security.yml`)
  that runs on push/PR and uploads results with `security-events:
  write` permission. It cannot be faithfully reproduced by a local
  command, and this report does **not** claim a local CodeQL pass.
  CodeQL must run in CI on the correction PR before merge.
- Coverage: the repository coverage gate is
  `scripts/coverage-gate.sh` over a full
  `go test -count=1 -coverprofile=.artifacts/coverage/coverage.out ./...`
  profile, with the CI artifact authoritative (see the script's
  own toolchain caveat). A local coverage total, if measured, must
  not be quoted as a CI total. Coverage evidence, if completed
  locally, is recorded in §7; otherwise it remains a CI gate.

## 7. GO / NO-GO — evidence-based verdict for owner review

### Correction commit

- Code commit: `c55a5e3ae2cf2d19366e6f663e9a3a76682c4eae`
  (branch `fix/nexaflow-loopback-gateway-base-url`, parent
  `b8aa54b8b2260f58b2e592f4370af1e7ce2151ff`, the PR #793 head)
- This report was added in a follow-up commit on the same branch;
  the branch head at publication is the published SHA in the
  delivery message, and its tree contains the code commit above
  unchanged.

### Additional evidence completed after §5

- Race, `internal/httpapi` alone, base commit (no correction):
  `go test -race -count=1 -timeout=3m ./internal/httpapi/`
  — `ok 152.887s`.
- Race, `internal/httpapi` alone, with correction:
  same command — `ok 154.879s`.
  The ~2s difference is normal variance. The full-verify race
  timeout for this package is therefore a parallel-load effect
  on a 4-core VM, not a race failure introduced by this change,
  and not a failure in the corrected code.
- Coverage, local (Go 1.25.3, **not** the CI Go 1.27.3 toolchain,
  so this total must not be quoted as a CI total):
  `go test -count=1 -coverprofile=.artifacts/coverage/coverage.out ./...`
  — total **95.5%** (threshold 95.0%),
  `ValidateLoopbackGatewayBaseURL` 96.4%,
  `validateLoopbackBaseURL` (nexaflow) 95.8%,
  `loopbackBaseURL` 100.0%.
  That coverage run itself hit one flaky timing test in
  `internal/httpapi`
  (`TestClaudeStableAnthropicRouteFailoverDeadlineExpiry`,
  "expected server A to be attempted before timeout; contacted=[]"),
  a test that passed in the earlier full non-race run as uid 1001.
  It is unrelated to `gateway_base_url` and is recorded here
  rather than hidden.
- CodeQL: **not run locally, pending CI.** CodeQL (Go) is a
  GitHub Actions job, not a step in `scripts/verify.sh`, and it
  requires GitHub `security-events: write` upload. No local
  CodeQL pass is claimed.

### Verdict: CONDITIONAL GO — do not merge on local evidence alone

**GO for the security correction itself:** the defect is fixed at
three independent gates, the regression tests pass, the Bearer
token is proven not to be resolved or sent for any rejected base
URL, loopback derivation is unchanged, and the focused, race
(affected packages), browser (15 studio cases + 3 standard
suites), and coverage (95.5% local) evidence above is real.

**NO-GO for merging until CI completes on the correction PR:**
1. Full `scripts/verify.sh` did not complete locally — its race
   pass timed out on `internal/httpapi` under parallel load on
   this 4-core VM (isolated race passes for base and fix, §5/§7).
2. CodeQL (Go) has not run yet — it runs in CI on push/PR.
3. Coverage must be re-measured in CI with Go 1.27.3, the
   authoritative toolchain for the 95.0% gate.

**Owner actions:**
- Review this branch and this report.
- Let CI run `verify.sh`, CodeQL (Go), and the coverage gate on
  the correction PR.
- If CI is green, merge the **correction branch**, not PR #793
  as it stood, and only then decide on closing issue #765 and
  the three candidate PRs (#781, #779, #771) — none of those
  were closed by this work, as instructed.

**What this work did not do, by instruction:**
- Did not merge PR #793.
- Did not close issue #765.
- Did not close PRs #781, #779, or #771.
- Did not weaken any existing security check or test.
