# Issue #653 refresh — 2026-10-30 (main `7daa36d`)

## Scope of this refresh

This record refreshes the stale values in issue #653 (which still cited
main `02a43ac`, 95.63%, PR #759 as open, and two soaks in progress) with
fresh first-hand measurements on current main, a per-package risk review,
a validated soak reconciliation, and the branch-protection ruleset that
turns the coverage gate from a job that merely reports into a merge
blocker. It separates four layers that must never be conflated: code
coverage, bounded CI smoke, long soak, and live external acceptance.

## 1. Code coverage — MET by measurement, gate enforced

- Base: main `7daa36da962bf3031b353487f760506cd60aedb2` (merge of #764).
- Canonical command (from `.github/workflows/coverage-report.yml`):
  `go test -count=1 -coverprofile=coverage.out ./...`
- Toolchain: Go 1.27.3, the version pinned by the coverage workflow
  (#756). Statement counts are toolchain-dependent, so a Go 1.27.0
  measurement is not comparable and is not used here.
- Retained CI artifact (authoritative for the gate): Coverage report
  run 41013985974, artifact
  `coverage-7daa36da962bf3031b353487f760506cd60aedb2-41013985974`
  (id 12959051528), Go 1.27.3: **95.51% (41,895 / 43,865)**,
  margin +224 above the 95% bar.
- Fresh local recomputation, same commit, same toolchain, exit 0:
  **95.61% (41,939 / 43,865)**, margin +268. The denominator is
  identical; the +44 covered-statement difference is run-to-run
  variation in timing-sensitive branches (largest:
  `internal/video/providers` +34, `internal/decision` +10, all others
  within ±2). Both profiles pass `scripts/coverage-gate.sh` at the
  inclusive 95.0% threshold (local PASS, CI-profile PASS, exit 0).
- Supersession: this replaces the `02a43ac` figure (95.63%,
  41,706 / 43,612). The delta is the #764 NexaFlow Studio merge:
  +253 statements, +189 covered.
- Preserved, not weakened: the fail-closed 95.0% gate merged via #756
  and the count-reporting refinement merged via #759. No threshold
  was lowered, no package or statement was excluded, no coverage was
  padded. The gate script itself is regression-tested through
  `verify.sh` (`scripts/test-coverage-gate.sh`).

## 2. Per-package review — no per-package requirement, no test added

22 of 62 statement-bearing packages are below 95% (same count as on
`02a43ac`). Largest uncovered counts (CI artifact):

- `internal/nexaflow`: 81.6%, 712 uncovered — PostgreSQL-store code
  exercised only under the integration tag with a real database,
  plus the Phase-6-undecided engines (durable, in-memory, Phase 3,
  Phase 4, studio). Forcing this package to 95% would pre-empt open
  architecture decisions.
- `cmd/gateway`: 92.9%, 350 uncovered — the uncovered half of the
  PostgreSQL control-plane opener is its success path, which needs a
  real database; its fail-closed branches (missing env name, empty
  DSN, unreachable host) already have unit tests
  (`TestOpenPostgresControlPlaneUnreachable`,
  `TestOpenPostgresControlPlaneConfigErrors`). The remaining gateway
  partials are mostly admin/acceptance report writers and backup /
  restore helpers.
- `internal/video/storage`: 84.7%, 29 uncovered.
- `internal/video/tts`: 91.4%, 17 uncovered.
- `internal/events`: 94.0%, 18 uncovered (two zero-coverage snapshot
  accessors, `Epoch` / `Cursor`, are the only whole functions at 0%).
- `internal/video/render`: 92.1% on CI, 94.3% on the local rerun
  (timing-sensitive); `internal/video/composer` 94.9%,
  `cmd/videogen` 94.9% (its only zero-coverage function is `main`).

A whole-function scan found no important uncovered path that is a
defect: the only other zero-coverage functions are the NexaFlow
PostgreSQL store/delivery functions (integration-only by design),
`cmd/videogen`'s `main`, and one small error helper in `internal/evallive`.
The issue's target is repository-wide, not per-package, so **no
behavior test was added for coverage in this refresh**. Adding tests
to chase the lowest packages would be padding or architecture
pre-emption, not evidence.

## 3. Bounded CI smoke — distinct from soak

The CI `ci-soak-smoke` job, the fuzz smoke, and the Postgres/Redis
integration jobs run on every pull request and are part of the
required-check set below. They are bounded, short checks. They are
not the long soak and are not live acceptance.

## 4. Long soak — validated only for `ccce752`; current main in flight

- Validated PASS, bound to `ccce752` only: Soak run 41011189189
  (workflow_dispatch, Go 1.27.3, 2026-10-30T05:56:34Z–08:00:13Z,
  total 7419s, requested sustained duration 2h / 7200s, sustained
  stage 7209s, 10/10 stages PASS, 28,802 resource samples, peak 118
  goroutines / 62 open FDs / ~19.5 MB heap). The summary was read
  first-hand from artifact
  `soak-ccce752453c9760af62f7a6042ea341f376155d9-41011189189-attempt-1`.
  This PASS is NOT evidence for current main.
- Current main `7daa36d`: two long soaks are in flight and no third
  was dispatched. Run 41040550445 (dedicated branch
  `soak-run-7daa36d` at the exact SHA, rounds=3, duration=2h,
  timeout 180m, dispatched 2026-10-30T10:24:45Z, expected completion
  ~12:40Z) is the validation vehicle; a 30-minute monitor validates
  its artifact against the commit and 7200s before any PASS is
  recorded. Run 41040133996 (same SHA, main ref, 10:21:24Z) is also
  in flight but is NOT relied on: its dispatch inputs are not
  verifiable from here, and it sits under the soak workflow's
  cancel-in-progress rule for main.
- Until that validation lands, the current-main long soak is
  IN FLIGHT, not PASS. Production SLOs, production failover drills,
  and the live operator game-day exercise remain NOT_RUN (the
  `ccce752` summary itself lists them as NOT_RUN/BLOCKED).

## 5. Live external acceptance — still BLOCKED / NOT_RUN / OPEN

#646 (live provider canaries), #647 (live OIDC tenant), #648
(production-like Vault KV), #649 (Vault Transit live), #650 (live
Kubernetes/Helm; the safe-failure harness merged via #762, but the
live run still needs a kind-capable authorized runner), #651 (real
ComfyUI inference; reopened after a premature close), and #652
(multi-host HA, failover, measured RPO/RTO) are all still open, and
none of them is evidenced by anything in this refresh. Umbrella
context only (not a #653 dependency): the #654 release gate now also
includes #765 (NexaFlow production AttachNexaflow wiring).

## 6. Branch protection — applied, in two steps, without deadlock

Previous state (verified live): `main` was unprotected (API 404) and
no ruleset existed, so the coverage gate job alone could not block
a merge.

Ruleset `main-required-checks` (id 26200123, enforcement active,
target branch `main`) was created and verified back through both
the ruleset endpoint and the branch-rules endpoint. Its rules:

- `deletion` — `main` cannot be deleted.
- `non_fast_forward` — force pushes to `main` are blocked.
- `required_status_checks`, strict=false — the branch need not be
  up to date before merging, so a fast-moving main cannot deadlock
  merges. No review count is required: the owner is the sole
  collaborator, so a review requirement would deadlock merges;
  owner review remains a human step, not a GitHub gate.
- Bypass: the admin role only, and only via pull request. An admin
  can merge a PR past a broken check in an audited emergency, but
  direct pushes to `main` stay blocked for everyone.
- Rollback: disable or delete ruleset 26200123.

Required contexts were taken from fresh runs, never guessed. Step 1
(applied 2026-10-30, before this PR merges) requires the eight
checks that already report on every pull request:

- `verify`, `ci-soak-smoke`, `fuzz-smoke`, `integration-postgres`,
  `integration-redis`, `container` (CI workflow, unfiltered PR
  trigger; `integration-redis` is reported by two workflows, CI and
  the Redis integration workflow, both of which run on PRs)
- `Go vulnerability scan`, `CodeQL (Go)` (Security workflow, PRs
  targeting `main`)

Step 2 (applied immediately after this PR merges) adds
`Fresh Go coverage`. Adding it before this PR merges would have
blocked in-flight docs-only PRs forever: under the old path filter,
their coverage job never reported, and a required check that never
reports blocks the merge. This PR removes that path filter (see
below), so from the merge onward every PR reports the coverage
context and the requirement is deadlock-free. Deliberately NOT
required: the long Soak workflow (manual, hours-long), the
real-system acceptance jobs, the live canary/conformance workflows,
the path-filtered quality-gate job, and the manual audit workflow —
none of them is a stable per-PR check, and requiring any of them
for ordinary merges would be a deadlock or a live-acceptance
requirement in disguise.

Proof the step-1 ruleset does not deadlock ordinary PRs: while its
checks were pending, sibling docs PR #768 already showed CLEAN
under it (its eight contexts pass); this PR showed BLOCKED only
while its own checks were still running.

## 7. What this PR changes

- `.github/workflows/coverage-report.yml`: the `pull_request`
  trigger is unfiltered. Rationale is the deadlock above: the
  coverage job carries the fail-closed gate and must report on
  every PR once it becomes a required check. Cost is a few minutes
  of CI on docs-only PRs; benefit is that PRs touching only
  workflow files are gated too.
- This report. No Go code, no threshold change, no package
  exclusion, no new behavior test (see §2), no KNOWN_GAPS edit
  (the umbrella lane owns that document this mission; a soak PASS
  on current main, when validated, is recorded there by its own
  monitor path).

## 8. What remains for #653

Issue #653 stays open. It closes only when its complete scope is
satisfied — a validated long soak on current main plus the external
gates #646–#652 evidenced — or when the owner records an explicit
reviewed release exclusion. Coverage above 95% alone does not
close it, and this PR's merge does not close it either.
