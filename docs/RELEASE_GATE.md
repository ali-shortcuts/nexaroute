# Release Gate

The release gate answers one question, for one exact commit SHA: did
every required check run on *that* SHA, and did it pass? A result from
any other SHA is never substituted, and a gate with no check run on
the SHA is NOT_RUN, never PASS.

## Checking a SHA (no local toolchain needed)

```sh
# From any machine with `gh` authenticated; no Go, Docker, or test
# dependencies are installed on the operator's machine. CI runs the
# tests; this script only reads their recorded check runs.
bash scripts/release-gate-status.sh <full-40-hex-sha>
# Release-candidate verdict (also requires the manual-only gates):
bash scripts/release-gate-status.sh <sha> --rc
```

The script exits non-zero unless every required gate is PASS, and
prints `Release Gate Passed` only in that case. A gate that is
queued or running prints RUNNING and fails the gate until it
finishes; an unreadable Checks API response prints BLOCKED. Its own
regression tests (`bash scripts/test-release-gate-status.sh`) run
offline against a fake `gh` and cover pass, one-failed, missing
(= NOT_RUN), running, API-error (= BLOCKED), non-SHA refusal, the
latest-run rule (old success must not hide a newer failure, old
failure must not hide a newer success or a newer running run, ties
broken by check-run id), and `--rc` given without a repo argument.

Note on invocation: the release-gate scripts are invoked with
`bash` throughout this document because their Git executable bit
is not guaranteed on every push path (a snapshot-style push lands
them at mode 644); `bash scripts/...` works regardless of that
bit, while a bare `scripts/...` invocation does not. Only
`scripts/verify.sh` is executed directly by CI, so its mode
100755 must be preserved on every push.

## PR-required gates

These run on `pull_request` and are required for any PR verdict:

| Gate | Owning workflow / job |
|---|---|
| verify.sh, stress, browser acceptance, container build/smoke | CI: `verify`, `container` |
| Fuzz smoke, soak smoke | CI: `fuzz-smoke`, `ci-soak-smoke` |
| PostgreSQL and Redis integration | CI: `integration-postgres`, `integration-redis` |
| Vulnerability scan | Security: `Go vulnerability scan` (govulncheck) |
| Static analysis (SAST) | Security: `CodeQL (Go)` |
| Coverage, 95.0% inclusive, no exclusions | Coverage: `Fresh Go coverage` |
| Two real Gateway processes against PostgreSQL; real keyring round-trip | Real-system acceptance |

Existing PR coverage already includes `gofmt`, `go vet`, race
(`go test -race ./...`), short fuzz, browser acceptance (Playwright,
pinned Chromium; a skipped suite fails the CI job, it is never
reported as a pass), and the final-release-gate regression script,
all inside `scripts/verify.sh` / CI. This document does not add a
second copy of those runs.

## Release-candidate-only gates

These do not run on a normal PR and are required only for a release
candidate (`--rc`):

| Gate | How it runs |
|---|---|
| golangci-lint, staticcheck, gosec, deadcode | Architecture & Quality Audit workflow, `workflow_dispatch` only |
| Extended fuzz (45 s per target by default) | Audit workflow, `fuzz-extended` |
| Soak, 2 h sustained mixed workload | Soak workflow, `workflow_dispatch` with `duration: 2h` and a timeout above 2 h 30 m (see `docs/reports/ISSUE664_SOAK_PROCEDURE.md`); the soak report must name the same SHA |
| Browser evidence on main | Browser evidence workflow, `workflow_dispatch` / main push |

## Gaps this gate does not hide

- **Mutation testing: NOT_RUN.** No mutation tooling is wired into
  CI. The loopback validator is mutation-sensitive in practice (the
  PR #794 Go 1.27 failure was a real mutant-like escape: a
  normalization step silently changed the checked value), but no
  mutation score is claimed until a tool is added and run.
- **Visual regression: NO BASELINE.** The browser workflows produce
  evidence and run behavioral suites; no committed visual baseline
  exists, so no new baseline may be recorded without human approval
  and no visual difference may be reported as a pass or a failure
  on baseline grounds alone.
- **Real external systems: NOT_RUN unless separately recorded.**
  Real Vault Transit, real Kubernetes/HA, and live-provider runs are
  manual, operator-authorized, and recorded in
  `docs/EXTERNAL_VERIFICATION.md`; they are never implied by mock
  tests. No test in the normal gates spends real provider quota or
  touches production.
- **Branch protection / rulesets:** not changed by this PR. If the
  owner wants the gates above enforced by GitHub (rather than by
  this script and reviewer discipline), that is a settings change
  for the repository owner to make and verify; it is documented
  here, not silently applied.

## What "Release Gate Passed" does and does not mean

It means: on the named SHA, every gate above ran and passed, and
no critical security issue is known-open against that SHA. It does
not mean zero bugs, and it does not cover the manual-only gates
unless `--rc` was used. The per-SHA evidence (workflow run links
and artifacts) is the verdict; this document and the script are
only the procedure for reading it.
