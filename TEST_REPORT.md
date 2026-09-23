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
- cancellation-neutral provider health;
- malformed/truncated JSON and SSE;
- translated stream terminal validation and client-write failure;
- strict health-probe response validation;
- bounded request/config/discovery/upstream payloads;
- global admission/overload behavior;
- config resource limits and invalid negative values;
- request-ID sanitization and header validation;
- secret redaction across credential rotation;
- embedded Web UI control wiring.

## Release rule

A build or tag is not considered current merely because it compiles. The authoritative version is the commit on `main` for which the complete GitHub Actions workflow succeeds. If any gate fails, that commit is not an accepted release baseline.
