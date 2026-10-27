# Issue #662 — scoped coverage evidence

Base: `main` at `e6c3cdf3fe0a193dda0c5857e91e46b541d04729` (the commit named in the issue).
Scope: `internal/video/runtime`, `internal/video/providers`, `cmd/gateway`, plus tests.

## Baseline (before changes)

Command:

```sh
go test ./internal/video/runtime ./internal/video/providers ./cmd/gateway \
  -coverprofile=.artifacts/issue662/baseline.out -covermode=atomic
```

| Package | Coverage | Statements |
| --- | ---: | ---: |
| `internal/video/runtime` | 63.8% | 134/210 |
| `internal/video/providers` | 67.6% | 602/891 |
| `cmd/gateway` | 81.0% | 2626/3243 |
| scoped total | 77.4% | 3362/4344 |

## After changes

Command (fresh, uncached):

```sh
go test -count=1 ./internal/video/runtime ./internal/video/providers ./cmd/gateway \
  -coverprofile=.artifacts/issue662/after.out -covermode=atomic
```

| Package | Coverage | Statements | Delta |
| --- | ---: | ---: | ---: |
| `internal/video/runtime` | 91.4% | 192/210 | +27.6 pts |
| `internal/video/providers` | 86.4% | 770/891 | +18.8 pts |
| `cmd/gateway` | 82.2% | 2665/3243 | +1.2 pts |
| scoped total | 83.5% | 3627/4344 | +6.1 pts |

Raw artifacts (generated locally; the large `.out` profiles are not committed):

- `.artifacts/issue662/baseline.out`
- `.artifacts/issue662/after.out`

Committed summaries:

- `.artifacts/issue662/after.log`
- `.artifacts/issue662/after-functions.txt`
- `.artifacts/issue662/verify.log`

## Representative function coverage

| Function | Before | After |
| --- | ---: | ---: |
| `runtime.New` | 65.5% | 95.5% |
| `runtime.Start` | 43.2% | 89.2% |
| `runtime.pruneOnce` | 0.0% | 100.0% |
| `runtime.Close` | 82.4% | 100.0% |
| `providers.(*Aggregator).ValidateRequest` | 82.9% | 100.0% |
| `providers.(*Aggregator).DownloadOutputs` | 78.9% | 100.0% |
| `providers.(*Aggregator).doJSON` | 82.8% | 96.6% |
| `providers.(*CodeRender).ValidateRequest` | 72.0% | 100.0% |
| `providers.(*CodeRender).resolvePlan` | 0.0% | 88.9% |
| `providers.(*CodeRender).GetJob` | 45.5% | 100.0% |
| `providers.(*CodeRender).DownloadOutputs` | 37.0% | 100.0% |
| `resolveCanaryTarget` | 59.6% | 100.0% |
| `externalProviderCredentialReady` | 45.5% | 100.0% |
| `runExternalProviderCheck` | 60.9% | 73.9% |
| `checkJournalOwnership` | 66.7% | 100.0% |
| `writeCanaryReport` | 63.2% | 68.4% |
| `writeExternalAcceptReport` | 65.2% | 69.6% |

## Tests added

- `internal/video/runtime/coverage_gaps_test.go`
  - job-store/aggregator/asset-store/planner/work-root construction failures
  - code-render model dedupe/prepend, default duration, retention modes, ffmpeg override, relative/default work roots
  - `Start` recovery failure cleanup, double-start rejection, startup retention prune, code-render prune branch, ledger closer on `Close`, nil receiver safety
- `internal/video/providers/issue662_coverage_test.go`
  - aggregator constructor/validation matrix, submit body, job vocabulary, output extraction, video-container signatures, download guards, truncation/decode errors, webhook fail-closed
  - ComfyUI missing-inputs validation, shot-prompt injection/frame clamp, empty prompt ID, output collection/queue parsing, content-type parameter handling, decode errors
  - code-render narration/capability, validation matrix, planner resolution, preflight failures, run fail-closed/cancellation, status/cancel/download guards, retention edge case, helper behavior
- `cmd/gateway/coverage_issue662_test.go`
  - canary target resolution errors/selection/overrides, report atomic write and permission behavior
  - external-accept credential readiness matrix and blocked-before-network checks, exit-code mapping, report write/permissions
  - journal manifest durability, ownership checks

## Verification

```sh
go vet ./internal/video/runtime ./internal/video/providers ./cmd/gateway
go test -race ./internal/video/runtime ./internal/video/providers -count=1
go test -race ./cmd/gateway -run 'TestIssue662' -count=1
./scripts/verify.sh
```

Results:

- `go vet`: PASS
- race tests: PASS
- `./scripts/verify.sh`: PASS (`VERIFY PASS`), including full unit, vet, race, release-artifact/SBOM checks, fuzz smoke, and linux amd64/arm64 builds.
- Browser acceptance: skipped by the script because Chromium + Python Playwright are unavailable on this runner.
- `ffmpeg`/`ffprobe` are not installed on this runner, so code-render success-path rendering remains uncovered here; failure/cancellation paths are covered.
- PostgreSQL-backed job store/ledger and live provider canaries were not run in this environment (live canaries remain tracked in #646).
