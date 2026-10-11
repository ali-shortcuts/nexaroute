# M1-1 — Current aggregate coverage

Repository: `ali-shortcuts/nexaroute`
Measured base: `dcb9f9b` (the latest `main` observed at task start)
Toolchain: `go version go1.26.8 linux/amd64`
Profile: `/tmp/c.out` (local temporary file; not committed)

## Command and raw result

The required command was run from the repository root, with `/usr/local/go/bin` added to `PATH` as `scripts/verify.sh` does:

```sh
go test -count=1 -coverprofile=/tmp/c.out ./... && go tool cover -func=/tmp/c.out | tail -1
```

Raw final line:

```text
total:							(statements)				85.0%
```

All packages passed. Aggregate coverage meets the `>=85.0%` threshold in this run. No new package was introduced by M1-1. The prioritized package measurements from the same run were: `internal/probe` 89.5%, `internal/httpapi` 83.3%, `internal/config` 89.9%, `cmd/gateway` 75.1%, `internal/decision` 87.6%, `internal/compat` 86.6%, and `internal/guardrail` 100.0%. These are measured values, not claims that every listed package independently reaches 90%.

Behavior-oriented tests already on the measured `main` include those introduced by `95924c572031f91311bbf817f970704d924e9386` (PR #238), including `TestRunConfigCommandsAndStableExitContracts`, `TestRunSecretsRejectsUnsafeOrUnknownOperations`, the `TestCoverageBehavior*` suites in `internal/compat`, `internal/config`, and `internal/decision`, and routing/request-safety behavior tests in `internal/httpapi`. These are existing tests; no coverage-only tests were added for this measurement.

## Reconciling earlier figures

- `79.5%` is recorded in `docs/reports/PHASE2_COVERAGE_REPORT.md` for the earlier `phase1-coverage-on-1b` snapshot. It predates the behavior-test expansion in commit `95924c5`.
- `84.9%` is recorded in `docs/reports/HANDOFF.md` and `docs/reports/FINAL_COMPLETION_REPORT.md` for handoff commit `032c394336586e77233e6a96e93f368cdd0d6346`, before that coverage expansion.
- The prior `85.1%` report identifies the same source snapshot (`dcb9f9b`) as this run. Therefore that 0.1 percentage-point difference cannot be attributed to a source commit. This sandbox run used Go 1.26.8; the earlier report's runtime environment has not been independently reproduced here, so the cause of the same-snapshot variation is **undetermined**. The exact command in this run yields 85.0%, which satisfies the threshold without rounding up.

The source-history explanation for 79.5% and 84.9% is the addition of meaningful behavior tests before `dcb9f9b`; the exact cause of 85.1% versus 85.0% on that same commit remains unresolved and is not presented as a commit-driven change.

## Verification boundary

This evidence establishes the requested command and result on the task sandbox and the `dcb9f9b` source snapshot. It does not claim an independent rerun under the earlier report's toolchain. The generated coverage profile remains only at `/tmp/c.out` and is intentionally excluded from version control.
