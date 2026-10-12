# M1-1 — current-main coverage measurement

**State:** DONE.
**Measured source:** `main` at `dcb9f9b3c0352d23f1dfa599ea58450b072406c0` (requested starting head; this evidence branch changes documentation only).
**Toolchain:** `go version go1.26.8 linux/amd64`; the repository verification script prepends `/usr/local/go/bin` to `PATH`.

## Required measurement

Command (profile is outside the repository at `/tmp/c.out`):

```sh
go test -count=1 -coverprofile=/tmp/c.out ./...
go tool cover -func=/tmp/c.out | tail -1
```

The exact total line is in [`m1-1-coverage.raw.txt`](m1-1-coverage.raw.txt): **85.1% statements**. All packages passed. No coverage profile was committed.

## Priority package results

| Package | Coverage | Observation |
|---|---:|---|
| `cmd/gateway` | 74.9% | Remains a substantial target |
| `internal/probe` | 89.5% | Below 90%; no package was introduced by this item |
| `internal/httpapi` | 83.3% | Remains a substantial target |
| `internal/config` | 89.9% | Just below 90% |
| `internal/decision` | 87.6% | Remaining gap |
| `internal/compat` | 86.6% | Remaining gap |
| `internal/guardrail` | 100.0% | Above target |
| `internal/core` | 100.0% | Above target |

No new package was introduced by this item. Behavior-asserting suites already present in the measured main include:

- `cmd/gateway/coverage_behavior_test.go`: `TestRunConfigCommandsAndStableExitContracts`, `TestRunSecretsRejectsUnsafeOrUnknownOperations`;
- `internal/probe/coverage_behavior_test.go`: `TestCoverageBehaviorReadyLeaseExpiryBoundaries`, `TestCoverageBehaviorAcquireProbeCancellationReleasesNothing`;
- `internal/httpapi/coverage_pure_test.go`: `TestCoverageRoutingHelpersAndPolicies`, `TestCoverageRequestInspectionAndHeaderSafety`, `TestCoverageStreamWriterAndCanonicalErrors`;
- `internal/config/coverage_behavior_test.go`: `TestCoverageBehaviorCredentialResolutionAndDurations`, `TestCoverageBehaviorConfigValidationMatrix`;
- `internal/decision/coverage_behavior_test.go`: `TestCoverageBehaviorRegistryListSnapshotAndResolve`, `TestCoverageBehaviorReasonCodeContract`;
- `internal/compat/coverage_behavior_test.go`: `TestCoverageBehaviorSanitizeAndRepairMatrix`, `TestResponsesAndAnthropicCapabilitySuitesUseProtocolAdapters`;
- `internal/guardrail/scan_test.go`: `TestScanFindsConfiguredRiskClassesWithoutReturningMatchedText`, `TestScanFailClosedBoundariesAndNilInput`.

These tests predate this evidence PR; no synthetic coverage tests were added. The committed history identifies focused coverage work at `95924c5` (`test: expand Phase 1 behavior coverage (#238)`) and related follow-on tests.

## Historical figures and commit context

The reported percentages refer to different source trees, not repeat measurements of one commit:

1. **79.5%** — [`PHASE2_COVERAGE_REPORT.md`](PHASE2_COVERAGE_REPORT.md) records branch `phase1-coverage-on-1b`, based on `phase1b-on-1a` (77.4%). This historical coverage branch is not current `main`.
2. **84.9%** — [`FINAL_COMPLETION_REPORT.md`](FINAL_COMPLETION_REPORT.md) records `main` SHA `032c394336586e77233e6a96e93f368cdd0d6346` on 2026-10-10 and the result of its then-current run.
3. **85.1%** — this fresh, uncached run measures current starting `main` SHA `dcb9f9b3c0352d23f1dfa599ea58450b072406c0` on 2026-10-11. Between `032c394` and `dcb9f9b`, main gained further behavior tests, including the focused coverage expansion `#238` (`95924c5`), plus other feature/test commits. The +0.2 percentage-point difference is attributed to that full intervening tree; it is not attributed to a single commit without an isolated per-commit coverage run.

## PR gate

`./scripts/verify.sh` passed on the evidence PR head. Its full output is committed in [`m1-1-verify.raw.txt`](m1-1-verify.raw.txt). The raw log records formatting, clean unit/integration tests, `go vet`, race tests, browser control-plane and Live Visual Agent acceptance, short HTTP API/core fuzz checks, Linux amd64/arm64 builds, and final `VERIFY PASS`. The test regenerated unrelated browser evidence locally; those files were restored and are not part of this PR.

## Acceptance

- Fresh total: **85.1%**, meeting the **85.0%** target.
- No new package was created by this item; the table identifies remaining weak packages for subsequent work.
- Raw coverage and verification output are committed; no coverage profile or unrelated regenerated evidence is included.
