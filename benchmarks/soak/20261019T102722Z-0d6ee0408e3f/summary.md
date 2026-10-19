# Soak qualification evidence

- commit: `0d6ee0408e3f47dadc092e13f65d8770c57979fc`
- git branch: `opencode/issue437-20261019101449`
- workflow ref: `main`
- workflow head ref: `n/a`
- worktree entries (porcelain): 0
- started: 2026-10-19T10:27:22Z
- finished: 2026-10-19T10:28:00Z
- total duration: 38s
- stress rounds: 3
- result: PASS (exit 0)
- output directory: `benchmarks/soak/20261019T102722Z-0d6ee0408e3f`

## Environment

```
go version go1.25.0 linux/amd64
Linux runnervm8k3b6 6.17.0-1022-azure #22-Ubuntu SMP Mon Jul 27 17:24:03 UTC 2026 x86_64 x86_64 x86_64 GNU/Linux
nproc: 4
cpu: AMD EPYC 7763 64-Core Processor
GOMAXPROCS: auto
```

## Stages

| Stage | Exit | Duration | Max RSS | Raw log |
|---|---|---|---|---|
| stress-round-1 | 0 | 3s | 302844 KB | `benchmarks/soak/20261019T102722Z-0d6ee0408e3f/01-stress-round-1.txt` |
| stress-round-2 | 0 | 3s | 302124 KB | `benchmarks/soak/20261019T102722Z-0d6ee0408e3f/02-stress-round-2.txt` |
| stress-round-3 | 0 | 3s | 304308 KB | `benchmarks/soak/20261019T102722Z-0d6ee0408e3f/03-stress-round-3.txt` |
| http-hot-reload-soak | 0 | 11s | 300600 KB | `benchmarks/soak/20261019T102722Z-0d6ee0408e3f/04-http-hot-reload-soak.txt` |
| recovery-soak | 0 | 1s | 146964 KB | `benchmarks/soak/20261019T102722Z-0d6ee0408e3f/05-recovery-soak.txt` |
| race-soak | 0 | 14s | 342340 KB | `benchmarks/soak/20261019T102722Z-0d6ee0408e3f/06-race-soak.txt` |
| budget-leak-slope | 0 | 3s | 312348 KB | `benchmarks/soak/20261019T102722Z-0d6ee0408e3f/07-budget-leak-slope.txt` |

## Fault classes exercised

| Class | Proving suite |
|---|---|
| Provider refused / hang / mid-stream cut / half-open trial | `internal/httpapi/failure_injection_test.go` |
| Outage storm | `internal/httpapi/gameday_test.go` |
| Flapping upstream recovery + hot config reload | `TestSoakFlappingUpstreamFailureSemantics`, `TestSoakConcurrentTrafficAndHotReload` |
| Budget leak / disconnect / cancellation | `TestSoakBudgetLeakSlope` and the budget leak disconnect matrix |
| Recovery probe flood + repeated recovery cycles | `TestStressRecoveryFloodFiveThousandDeploymentsStaysBounded`, `TestSoakRepeatedRecoveryCyclesReleaseState` |
| Event flood / slow subscribers / overload admission | `TestStressConcurrentEventAndSnapshotFlood`, `TestStressGlobalAdmissionKeepsControlPlaneResponsive` |
| 10k-deployment routing | `TestStressTenThousandDeploymentsConcurrentIndexedRouting` |

## Reproduce

```sh
NEXAROUTE_SOAK_ROUNDS=3 NEXAROUTE_SOAK_OUT=benchmarks/soak ./scripts/soak.sh
```

Per-stage GNU time output is preserved next to each raw log. The
real PostgreSQL/Redis outage drills are separate CI integration
jobs (`TestPostgresIntegrationOutageRecoveryCancellationBackpressure`,
`TestRedisIntegrationOutageRecoveryCancellationBackpressure`) run
against ephemeral service containers.

## Scope / UNVERIFIED

- Multi-hour sustained soak: this workflow is bounded (<= 20 rounds) and is not a multi-hour operator soak.
- Live production PostgreSQL/Redis failover drill: the CI drills use disposable local service containers; production failover remains UNVERIFIED.
- Production-traffic SLOs: the local regression bands in `docs/PERFORMANCE_SLO.md` are not production SLOs.
- Live operator/game-day exercise: remains UNVERIFIED unless separately run with evidence.
