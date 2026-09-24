# Current verification contract — NexaRoute v0.3

This document describes the current verification contract, not historical CI snapshots. Old run-specific reports were removed because they become stale as soon as the code changes.

## Required source gates

- repository-cleanliness checks: no tracked backup/rollback artifacts or forbidden legacy copies;
- `gofmt` check;
- repeated and shuffled Go tests;
- `go vet ./...`;
- race detector;
- short HTTP/API fuzzing;
- short core protocol fuzzing;
- JavaScript syntax check when Node is available;
- Linux amd64 and arm64 builds.

## Bounded stress gate

Every normal CI run also executes one bounded stress pass covering:

- 10,000-deployment indexed routing and concurrent session pressure;
- 5,000-deployment supervised recovery pressure inside valid per-provider limits;
- concurrent event-ring/counter flooding;
- global HTTP admission overload while `/healthz` remains responsive;
- concurrent rotating-log writes with disk-retention bounds.

## Manual soak gate

`.github/workflows/soak.yml` is a long-form gate. It can be started manually and also runs automatically only when the stress/soak workflow, scripts, or stress/soak test files change. It repeats the bounded stress suite, then runs same-process concurrent traffic + hot reload and repeated recovery cycles, followed by race-enabled soak checks. Ordinary application pushes do not pay this extra CI cost.

## Required runtime gates

- local runtime smoke test;
- installable-package / installer smoke test;
- one-command installer (`scripts/install.sh`) default-path smoke test;
- Docker image build;
- Docker runtime smoke test including config persistence and permissions.

## High-risk regression coverage

- verified-ready routing and readiness;
- session affinity and bounded session state;
- large deployment/model indexing;
- provider and credential concurrency lifetime;
- credential cooldown and environment-key rotation;
- hot reload adapter reuse and stale-health invalidation;
- total failover budgets and actual upstream-attempt accounting;
- cancellation-neutral provider health;
- malformed/truncated JSON and SSE;
- translated stream terminal validation and client-write failure;
- strict health-probe response validation;
- bounded request/config/discovery/upstream payloads;
- global admission/overload behavior;
- config resource limits and invalid negative values;
- request-ID sanitization and header validation;
- secret redaction across credential rotation;
- embedded Web UI control wiring;
- rotating log disk bounds, backup cleanup and console rate limiting;
- concurrent event/session flood bounds;
- bounded recovery-queue behavior and worker retry recovery;
- fault injection with fake providers: hung-provider timeout failover, mid-stream upstream close, garbage-200 pre-commit failover, all-`429` capped `Retry-After` surfacing, connection-refused classification, flapping-provider cooldown isolation, credential redaction in failure bodies;
- upstream logical-error fault injection: quota/auth/throttle/paywall bodies, HTTP 200 with error envelopes or error finish reasons, and mid-stream error chunks/errors — all fail over (or surface a precise typed `502`) without recording success;
- route preview read-only API: ordering, explanations, session pin, alias/capability eligibility, empty-result notes, method guard.
- upstream logical-error detection: 200 error envelopes / paywall content / error finish reasons fail over without recording success; streamed paywall/error tails correct accounting post-commit; quota/auth/throttle classes cool only the serving credential; `429 insufficient_quota` earns the long quota cooldown; provider-test/probe surface the classified cause.

## Measured routing performance (linux/amd64, CI-class vCPU)

`go test ./internal/router -bench . -benchmem` (1000 iterations):

| Benchmark | Scale | ns/op | B/op | allocs/op |
|---|---|---:|---:|---:|
| BenchmarkCandidates_10Providers | 10 deployments | ~580 | 352 | 1 |
| BenchmarkCandidates_100Models | 100 deployments | ~570 | 352 | 1 |
| BenchmarkCandidates_1000Models | 1000 deployments | ~510 | 352 | 1 |
| BenchmarkCandidates_3000Models | 3000 deployments | ~600 | 352 | 1 |
| BenchmarkCandidates_AliasVirtualScan | 1020 deployments (catch-all `auto`) | ~1.6M | ~566K | 6 |
| BenchmarkEligibleSingle | single eligibility check | ~330 | 0 | 0 |

Targeted model lookups stay sub-microsecond and allocation-flat up to 3000 deployments. The catch-all virtual scan is linear in the registry (one health-lock acquisition total, ~6 allocations beyond the returned candidate slice); routing a specific model never scans the registry.

## Release rule

A build or tag is not considered current merely because it compiles. The authoritative version is the commit on `main` for which the complete GitHub Actions workflow succeeds. If any gate fails, that commit is not an accepted release baseline.
