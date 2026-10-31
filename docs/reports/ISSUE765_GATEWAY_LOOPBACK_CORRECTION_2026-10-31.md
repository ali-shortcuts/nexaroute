# NexaFlow gateway_base_url loopback-only correction — evidence report for owner review

**Issue:** #765 (NexaFlow combined production gateway wiring)
**PR #793 head corrected:** `b8aa54b8b2260f58b2e592f4370af1e7ce2151ff`
**Correction branch:** `fix/nexaflow-loopback-gateway-base-url`
**Date:** 2026-10-31
**Status:** Final local evidence report — see §7 for the conditional verdict, and §8 for the SHA reconciliation that fixes the discrepancy in the first version of this report.

## 0. SHA reconciliation — discrepancy fixed in this revision

The first version of this report named a local-only commit as
the "correction code commit". That was a discrepancy, because that
commit was never published to GitHub. The authoritative published
history is:

- Main at review time: `3347608516a066ecfd193320735671d8c9f13ad5`
  (merge of PR #793; its tree is identical to the PR #793 head
  `b8aa54b8b2260f58b2e592f4370af1e7ce2151ff`).
- First published snapshot commit on the correction branch:
  `48a5ab282054bc6d9386283fdd35e9dec5d9fd61`
  (branch `fix/nexaflow-loopback-gateway-base-url`, parent = main
  `3347608…`, verified with `git ls-remote` and `git fetch`).
- Local-only commits that produced the same tree and were **not**
  published as GitHub commits: code commit
  `c55a5e3ae2cf2d19366e6f663e9a3a76682c4eae` and report commit
  `5066f72c66505547cf1caedba6a9ca23c12ce21f`.
  Verified: `git diff 5066f72 origin/fix/nexaflow-loopback-gateway-base-url`
  is empty — the published snapshot tree is identical to the local
  tree, only the commit SHA and parent differ (snapshot parent =
  main, local parent chain = PR #793 head → code → report).

Therefore, for every later step in this supervisor cycle, the
authoritative head is **the PR head at the time CI runs**, not any
SHA printed inside this report, and not the local-only SHAs above.
This revision does not invent a future SHA: the correction to this
report itself is published as a new snapshot commit on the same
branch, and that new head — not this file's text — is what the
dedicated PR and its CI checks bind to. The final status report
for this cycle states that new head exactly, with its PR link and
workflow runs.

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

### Correction commit — corrected, see §0

- Published branch head at first publication:
  `48a5ab282054bc6d9386283fdd35e9dec5d9fd61`
  (branch `fix/nexaflow-loopback-gateway-base-url`, parent = main
  `3347608516a066ecfd193320735671d8c9f13ad5`).
- The local-only code commit `c55a5e3…` named in the first version
  of this report is **not** a GitHub commit and must not be quoted
  as the branch head. Its tree is identical to the published
  snapshot tree (verified, §0).
- The head that CI must bind to is the dedicated PR's head after
  this §0/§7 correction is pushed — stated exactly in the
  supervisor-cycle status report, not pre-printed here.

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
  the dedicated correction PR (not on the local-only SHAs).
- If CI is green, merge the **correction PR**, not PR #793
  as it stood, and only then decide on closing issue #765 —
  issue #765 is not closed by this report, and the three
  candidate PRs (#781, #779, #771) were already closed
  (not merged) in GitHub before this cycle and were not closed
  by this work.

**What this work did not do, by instruction:**
- Did not merge PR #793.
- Did not close issue #765.
- Did not close PRs #781, #779, or #771.
- Did not weaken any existing security check or test.

## 8. PR #794 CI failure and fix (Go 1.27 mapped-loopback bug)

PR #794 head `3a48592ecc3d50884025dfde9b5672bd7d5189b0` failed CI
and Coverage in GitHub Actions, on the same two tests, for
`http://[::ffff:127.0.0.1]:8080`:

- CI run 41172171898, job `verify`, step `Full verification`:
  `TestValidateLoopbackGatewayBaseURL/reject_mapped_loopback`
  (accepted, want rejection) and
  `TestGatewayClientLoopbackOnly/http://[::ffff:127.0.0.1]:8080`
  (auth token resolver was called).
- Coverage run 41172171858, job `Fresh Go coverage`, step
  `Measure package and repository coverage`: same two failures;
  the coverage gate step did not run, so no coverage total is
  claimed from that run.

Root cause: both validators used `net.ParseIP(host).String()`.
In Go 1.25.3, `url.Parse` rejects the mapped spelling as an
invalid IPv6 host, so the local test passed via the parse error.
In CI Go 1.27.3, `url.Parse` accepts it and `ParseIP` normalizes
it to `127.0.0.1`, so the validator accepted it and the client
resolved the token — the exact disclosure path this correction
closes.

Fix in this revision: both validators now use an exact-spelling
check only (`127.0.0.1`, `::1`, or `localhost`, exactly as
written), with no `ParseIP` normalization. This is a
strengthening, not a weakening: the mapped form is now rejected
on every toolchain, not only where `url.Parse` happens to reject
it. Focused local tests pass after the fix; CI must go green on
the new PR head before any merge, and that new head — not any SHA
printed here — is the authoritative head for the next CI run.
