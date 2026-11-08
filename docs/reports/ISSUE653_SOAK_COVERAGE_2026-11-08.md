# Issue #653 coverage and soak evidence — 2026-11-08

## Scope

This record covers the repository-wide Go coverage gate and the requested
three-round, two-hour CI soak on the exact current `main` commit. It does not
claim production traffic, production failover, external-provider, multi-host,
or live operator evidence.

## Exact runs

- Commit: `56c7ddfdcdb4869562d4b53187354d0033f688f0`
- Coverage workflow: [42435913579](https://github.com/ali-shortcuts/nexaroute/actions/runs/42435913579)
- Soak workflow: [42435915263](https://github.com/ali-shortcuts/nexaroute/actions/runs/42435915263)
- Toolchain: Go `1.27.3`, GitHub-hosted `ubuntu-24.04`

## Coverage result

The canonical command `go test -count=1 -coverprofile=.artifacts/coverage/coverage.out ./...`
completed successfully and the enforced 95.0% gate passed. The downloaded
`go tool cover -func` report recorded **99.3% statement coverage** on the exact
commit above. The workflow uploaded the reproducible profile and package
breakdown as an artifact.

## Soak result

The workflow was manually dispatched with `rounds=3`, `duration=2h`, and
`timeout_minutes=150`. The validated summary reported:

- Result: `PASS`
- Requested sustained duration: `7,200` seconds
- Observed hot-reload stage: `7,209` seconds
- Total run duration: `7,417` seconds
- Stress rounds: 3/3 PASS
- Planned stages: 10/10 PASS
- Resource samples: 28,802
- Peak goroutines: 120
- Peak open file descriptors: 62
- Peak heap allocation: 22,911,912 bytes
- Commit binding: exact match to `56c7ddfdcdb4869562d4b53187354d0033f688f0`

The raw stage logs, summary, and resource samples are preserved in the
workflow artifact `soak-56c7ddfdcdb4869562d4b53187354d0033f688f0-42435915263-attempt-1`.

## Explicit limits

The soak summary correctly leaves live provider canaries, live OIDC/Vault/
Kubernetes/ComfyUI/multi-host HA, production PostgreSQL/Redis failover,
production SLOs, and a live game-day exercise as `BLOCKED` or `NOT_RUN`.
Those are separate acceptance scopes and are not silently promoted by this
report.
