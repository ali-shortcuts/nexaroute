# Release-gate audit, 2026-10-31

Base for this branch: PR #794 branch head
`c654c1a6bde6e0c2682d7980a14553bf05436993` (the exact-spelling
loopback fix, landed by a parallel cycle on the PR #794 branch
during this work; this branch builds on it and does not re-land
it). Main at audit time: `3347608516a066ecfd193320735671d8c9f13ad5`
(PR #793 merged). PR #794 is open and unmerged. Issues #765, #653,
#654 and #646-#652 remain open; nothing in this branch closes any
of them, and its PR body must not contain auto-close wording.

Status labels: PASS = verifiable evidence on the named SHA;
FAIL = a test or acceptance criterion failed; NOT_RUN = not
executed on this SHA; PARTIAL = only part of the criteria are
evidenced; BLOCKED = a named obstacle prevents the test. A
capability is never marked PASS merely because its name appears
in the README, a workflow, or `scripts/verify.sh`.

## A. Current project state

- NexaFlow production wiring (#765) landed via PR #793, then
  corrected by PR #794: `nexaflow.gateway_base_url` is loopback-only
  at config, wiring, and client layers.
- The PR #794 head `3a48592` failed CI and Coverage on Go 1.27
  for `http://[::ffff:127.0.0.1]:8080`: `url.Parse` accepts the
  mapped spelling in Go 1.27 and `net.ParseIP` normalizes it to
  `127.0.0.1`, so the validator accepted it and the GatewayClient
  resolved the Bearer token. Local Go 1.25 tests passed only via
  a parse error, which is why the defect reached CI.
- Fix `c654c1a` (parallel cycle, already on the PR #794 branch):
  both validators now accept only the exact spellings `127.0.0.1`,
  `::1`, `localhost`. This branch adds the remaining same-address
  spellings, a validator fuzz/property test, and the release-gate
  status script; it changes no validator logic.

## B. Gap and risk table

| Area | Implementation | Test present | Gap / risk | Priority | Action |
|---|---|---|---|---|---|
| NexaFlow gateway_base_url SSRF / token disclosure | Exact-spelling validators, 3 layers | Yes, incl. no-resolver/no-transport assertions | Same-address spellings beyond the one CI case were untested before this branch | P0 | Done in this branch: 9 more spellings in both tables + fuzz property |
| URL validation, other surfaces (provider base URLs, egress) | Egress policy + per-surface validators | Yes, incl. egress fuzz | Egress policy is a separate rule from the gateway rule; do not conflate their evidence | P1 | None in this branch; keep separate |
| LLM protocol translation (4 paths) | Anthropic/OpenAI both directions, canonical IR | Yes: 34 tests in translate, tool-fidelity, round-trip fuzz | No single enumerated 4-path x behavior matrix (stream/cancel/timeout/retry/budget per path) | P1 | PARTIAL; matrix test is follow-up, not claimed here |
| Routing, failover, circuit breaker, quota/budget | Implemented | Yes, mocked-provider and stress suites | Budget bypass via retry/failover is asserted in targeted tests, not as a full property suite | P1 | PARTIAL; follow-up property suite |
| gofmt, go vet, race, unit, integration (Postgres/Redis) | In CI `verify` job | Yes, on PR | Nothing missing on the PR path | — | PASS (on PR #794 head, to be re-proven on this branch's SHA) |
| golangci-lint, staticcheck, gosec, deadcode | Audit workflow only, manual dispatch | Yes, but not on PR | A PR can pass CI while lint findings sit unrun | P2 | Report-only in this branch via `--rc` gates; enabling them on PR is an owner decision (workflow trigger change) |
| govulncheck, CodeQL, dependency scan | Security workflow, on PR | Yes | — | — | PASS (PR path) |
| Coverage 95.0% | Coverage workflow, on PR, no exclusions | Yes | Lowest packages: nexaflow 81.6%, cmd/gateway 92.9% on the #653 baseline; total passes only by margin | P2 | Keep 95.0% threshold unchanged; per-package figures must come from this SHA's artifact, not the #653 baseline |
| Fuzz, short + extended | verify.sh smoke + CI fuzz-smoke + audit extended | Yes | Gateway validator had no fuzz target before this branch | P2 | Done: `FuzzValidateLoopbackGatewayBaseURL` (audit extended-fuzz auto-discovers it; verify.sh/CI wiring is a follow-up, see section C) |
| Property-based testing | Scattered (chain, egress) | Partial | No property suite for routing/quota/translation invariants | P2 | PARTIAL; validator property added here, wider suite is follow-up |
| Mutation testing | None wired into CI | No | No killed/survived counts exist | P3 | NOT_RUN; do not claim a score |
| Stress, race, concurrency | CI stress + soak-smoke jobs | Yes | Bounded, not sustained | P2 | PARTIAL |
| Soak 2 h on this SHA | Soak workflow, manual dispatch | Procedure exists; prior soak evidence is on other SHAs | A soak on another SHA is not evidence for this SHA | P2 | NOT_RUN for this branch; operator dispatch required for a release candidate |
| Browser acceptance, accessibility | CI browser job (Playwright, pinned Chromium); skip fails the job | Yes | Behavioral suites only; no committed visual baseline | P3 | Visual regression: NOT_RUN / NO BASELINE; never auto-record a baseline |
| Cloud/isolated execution without local install | GitHub Actions + Docker in CI; mock providers; no real keys in normal tests | Yes | Local `verify.sh` still needs Go locally; the no-install path is CI + the gate script, not local verify | P2 | Documented in `docs/RELEASE_GATE.md`; local Go absence on the owner's Ubuntu is not a blocker |
| Real Vault Transit / real K8s-HA / live providers | Implemented behind flags; fake-API tests only | No real run recorded | Production claims must not be made from mock evidence | P2 | NOT_RUN; recorded separately in `docs/EXTERNAL_VERIFICATION.md` when run |
| This branch's own CI | New tests + scripts | Not yet run at report time | Until CI finishes, every row above for this SHA is NOT_RUN | P0 | CI on the new PR head; report updated only from its runs |

## C. Changes in this branch

- `internal/config/nexaflow_config_test.go`: 10 more rejection
  cases (mapped hex/expanded/padded, expanded/padded `::1`,
  padded/hex/decimal IPv4, `localhost.` trailing dot).
- `internal/nexaflow/gateway_loopback_test.go`: same 9
  same-address spellings added to the no-resolver/no-transport
  client table (the client table already contained the original
  mapped case).
- `internal/config/nexaflow_loopback_fuzz_test.go`: new
  `FuzzValidateLoopbackGatewayBaseURL` with seeds for every
  mapped spelling and the invariant that any accepted URL
  re-parses to exactly an approved host, http(s), and no
  credentials/path/query/fragment.
- No workflow or `verify.sh` wiring in the pushed branch, by
  tooling necessity, not by choice: the connector push used here
  writes files at mode 644 (which would break CI's
  `./scripts/verify.sh` execution) and cannot push workflow files
  at all (403). The target is still auto-discovered by the audit
  extended-fuzz job and runnable manually (see the test file
  header); wiring it into `verify.sh` / CI fuzz-smoke is a
  follow-up for a mode-preserving, workflows-permitted push.
- `scripts/release-gate-status.sh`: SHA-bound gate reader (see
  `docs/RELEASE_GATE.md`); `scripts/test-release-gate-status.sh`:
  offline regression tests for it (pass, fail, NOT_RUN, RUNNING,
  BLOCKED, non-SHA refused). Verified locally without Go:
  `bash scripts/test-release-gate-status.sh` passes.
- `docs/RELEASE_GATE.md`: gate definition, PR vs
  release-candidate gates, no-install procedure, and the gaps
  the gate does not hide.
- This report.

No validator, routing, translation, or security-check logic was
changed or weakened in this branch, and no test assertion was
loosened. The only production-code-adjacent change is the
Audit extended-fuzz auto-discovery for the new target (see section C).

## D. Tests run (at report time)

| Test | Where | SHA | Result |
|---|---|---|---|
| `scripts/test-release-gate-status.sh` | Local, no Go | Working tree (uncommitted, pre-push) | PASS |
| `bash -n` on new/changed scripts | Local | Working tree | PASS |
| Focused config / client loopback tests, race, full verify.sh, CI, Security, Coverage, browser, soak | GitHub Actions on this branch's head | (to be filled from the new PR's runs) | NOT_RUN |

Go tests cannot run on the owner's Ubuntu without Go installed,
by design of this mission; they run in CI, and no local result
is claimed here.

## E. Failed / not-run tests

- Everything in section D except the script tests: NOT_RUN until
  the new PR's CI completes. That is the honest state of this
  report at push time, not a pass.
- Mutation testing, 2 h soak on this SHA, visual regression:
  NOT_RUN (see table). They are release-candidate gates, not
  PR gates, and must not block or pass this PR on their own.
- Real Vault / K8s / live-provider acceptance: NOT_RUN and
  operator-authorized only.

## F. Security

- Fixed (parallel cycle, `c654c1a`): IPv4-mapped loopback
  acceptance via ParseIP normalization, in both validators.
- Hardened (this branch): same-address spellings, fuzz property.
- Unresolved: none known in the gateway-base-URL rule after
  this branch, pending CI proof. The wider egress/provider-URL
  rules are separate and unchanged; no new token path was added.

## G. UI / browser

Behavioral browser acceptance runs in CI and fails on skip.
Accessibility suites are part of that job. Visual regression
has no committed baseline: NOT_RUN, and no baseline is recorded
by this branch.

## H. Stability

Bounded stress and soak-smoke run in CI on the PR path. The
2 h sustained soak is manual-only and NOT_RUN for this SHA;
prior soak evidence (e.g. the #664 procedure and its runs) is
on other SHAs and is not reused as this SHA's evidence.

## I. Release gate (this SHA)

NOT passed at report time: the PR-required gates are NOT_RUN
for this branch's head until its CI finishes. The gate script
is the instrument for the verdict, not the verdict itself.
`Release Gate Passed` may only be printed by that script, on
the final head SHA, after every required check run exists and
succeeds on it.

## J. Next steps, in order

1. Push this branch, open the PR (review-ready, unmerged), and
   let CI/Security/Coverage run on its exact head.
2. If CI is green, run `bash scripts/release-gate-status.sh <head>`
   and record the run links/artifacts in the PR; do not merge
   on script output alone without review.
3. For a release candidate: dispatch Audit (lint/staticcheck/
   gosec/deadcode + extended fuzz) and a 2 h Soak on the same
   head, then re-run the gate script with `--rc`.
4. Follow-ups, not blockers for this PR: 4-path behavior matrix,
   routing/quota property suite, mutation tooling, visual
   baseline decision (human-approved), and per-package coverage
   recovery for nexaflow/cmd-gateway.
5. Owner decisions (not made here): branch protection/ruleset
   enforcement, enabling audit lint on PR, and the #765/#653
   reconciliation after PR #794 merges.
