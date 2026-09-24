# Current verification contract — NexaRoute v0.4.2

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
- provider-wide incident circuits requiring distinct deployment evidence and guarding active cooldowns from stale in-flight observations;
- model-specific versus provider-wide error classification;
- streaming TTFT observation and provider quota-header telemetry;
- exact routed JSON/SSE usage accounting with unknown-coverage semantics;
- per-model base pricing validation and estimated-cost telemetry;
- multi-frame Anthropic SSE usage merging and empty-usage false-positive rejection;
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
- bounded recovery-queue behavior and worker retry recovery.

## Release rule

A build or tag is not considered current merely because it compiles. The authoritative version is the commit on `main` for which the complete GitHub Actions workflow succeeds. If any gate fails, that commit is not an accepted release baseline.
