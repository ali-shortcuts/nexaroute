# M1-1 — current-main coverage measurement

**State at this evidence checkpoint:** measurement complete; acceptance threshold met.  
**Measured source:** `main` at `dcb9f9b3c0352d23f1dfa599ea58450b072406c0` (the requested starting head; the only change in this evidence branch is documentation).  
**Toolchain:** `go version go1.26.8 linux/amd64`; `scripts/verify.sh` prepends `/usr/local/go/bin` to `PATH`.

## Required measurement

Command (profile remains outside the repository at `/tmp/c.out`):

```sh
go test -count=1 -coverprofile=/tmp/c.out ./...
go tool cover -func=/tmp/c.out | tail -1
```

The exact total line from the fresh run is:

```text
total:                                          (statements)              85.1%
```

The complete raw `go test` package output and total line are committed in [`m1-1-coverage.raw.txt`](m1-1-coverage.raw.txt). No coverage profile is committed. All packages passed in the run.

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

No new package was introduced in this item. Coverage expansion already present in the measured main includes behavior-asserting suites such as:

- `cmd/gateway/coverage_behavior_test.go`: `TestRunConfigCommandsAndStableExitContracts`, `TestRunSecretsRejectsUnsafeOrUnknownOperations`;
- `internal/probe/coverage_behavior_test.go`: `TestCoverageBehaviorReadyLeaseExpiryBoundaries`, `TestCoverageBehaviorAcquireProbeCancellationReleasesNothing`;
- `internal/httpapi/coverage_pure_test.go`: `TestCoverageRoutingHelpersAndPolicies`, `TestCoverageRequestInspectionAndHeaderSafety`, `TestCoverageStreamWriterAndCanonicalErrors`;
- `internal/config/coverage_behavior_test.go`: `TestCoverageBehaviorCredentialResolutionAndDurations`, `TestCoverageBehaviorConfigValidationMatrix`;
- `internal/decision/coverage_behavior_test.go`: `TestCoverageBehaviorRegistryListSnapshotAndResolve`, `TestCoverageBehaviorReasonCodeContract`;
- `internal/compat/coverage_behavior_test.go`: `TestCoverageBehaviorSanitizeAndRepairMatrix`, `TestResponsesAndAnthropicCapabilitySuitesUseProtocolAdapters`;
- `internal/guardrail/scan_test.go`: `TestScanFindsConfiguredRiskClassesWithoutReturningMatchedText`, `TestScanFailClosedBoundariesAndNilInput`.

These tests were already in the supplied current main; this measurement adds no synthetic coverage tests. The committed history identifies focused coverage work at `95924c5` (`test: expand Phase 1 behavior coverage (#238)`) and related follow-on tests. The measured tree is the complete current `dcb9f9b` tree, not a cherry-picked test-only snapshot.

## Why previous reports differ

The figures describe different source trees and cannot be read as repeated measurements of the same commit:

1. **79.5%** — [`PHASE2_COVERAGE_REPORT.md`](PHASE2_COVERAGE_REPORT.md) labels this as the final result on branch `phase1-coverage-on-1b`, based on `phase1b-on-1a` (77.4%). It is a historical coverage branch/report, not current `main`.
2. **84.9%** — [`FINAL_COMPLETION_REPORT.md`](FINAL_COMPLETION_REPORT.md) explicitly records `main` SHA `032c394336586e77233e6a96e93f368cdd0d6346` on 2026-10-10 and the result from its older raw run.
3. **85.1%** — this fresh, uncached run measures the requested current starting `main` SHA `dcb9f9b3c0352d23f1dfa599ea58450b072406c0` on 2026-10-11. Between `032c394` and `dcb9f9b`, main acquired additional behavior tests, including the focused `#238` coverage expansion (`95924c5`), alongside other feature/test commits. The observed +0.2 percentage-point change is for that complete commit range; no claim is made that one commit alone accounts for all of it.

## Acceptance

- The required total is **85.1%**, above the **85.0%** threshold.
- The measured current tree has no newly added package from this item; the per-package table identifies the remaining low-coverage packages for later work.
- The total, package figures, and zero-failure package output are reproducible from the committed raw output; the profile is not tracked.
