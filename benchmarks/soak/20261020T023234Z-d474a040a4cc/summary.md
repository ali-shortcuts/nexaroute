# Soak qualification evidence

- commit: `d474a040a4cc68375a016d0f71cb0b4677eaf5e2`
- git branch: `opencode/issue437-20261020021920`
- workflow ref: `main`
- workflow head ref: `n/a`
- worktree entries (porcelain): 0
- started: 2026-10-20T02:32:34Z
- finished: 2026-10-20T02:37:07Z
- total duration: 273s
- stress rounds: 3
- sustained duration: 2m (120s)
- result: PASS (exit 0)
- output directory: `benchmarks/soak/20261020T023234Z-d474a040a4cc`
- JSON evidence: `benchmarks/soak/20261020T023234Z-d474a040a4cc/summary.json`
- resource samples: `benchmarks/soak/20261020T023234Z-d474a040a4cc/http-hot-reload-samples.jsonl`

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
| stress-round-1 | 0 | 3s | 303788 KB | `benchmarks/soak/20261020T023234Z-d474a040a4cc/01-stress-round-1.txt` |
| stress-round-2 | 0 | 3s | 303808 KB | `benchmarks/soak/20261020T023234Z-d474a040a4cc/02-stress-round-2.txt` |
| stress-round-3 | 0 | 3s | 303836 KB | `benchmarks/soak/20261020T023234Z-d474a040a4cc/03-stress-round-3.txt` |
| http-hot-reload-soak | 0 | 130s | 304440 KB | `benchmarks/soak/20261020T023234Z-d474a040a4cc/04-http-hot-reload-soak.txt` |
| recovery-soak | 0 | 0s | 144264 KB | `benchmarks/soak/20261020T023234Z-d474a040a4cc/05-recovery-soak.txt` |
| race-soak | 0 | 132s | 343340 KB | `benchmarks/soak/20261020T023234Z-d474a040a4cc/06-race-soak.txt` |
| budget-leak-slope | 0 | 2s | 305644 KB | `benchmarks/soak/20261020T023234Z-d474a040a4cc/07-budget-leak-slope.txt` |

## Fault classes exercised

| Class | Proving suite |
|---|---|
| Provider refused / hang / mid-stream cut / half-open trial | `internal/httpapi/failure_injection_test.go` |
| Outage storm | `internal/httpapi/gameday_test.go` |
| Flapping upstream recovery + hot config reload | `TestSoakFlappingUpstreamFailureSemantics`, `TestSoakConcurrentTrafficAndHotReload` |
| Sustained mixed workload / resource sampling | `TestSoakConcurrentTrafficAndHotReload` with `NEXAROUTE_SOAK_DURATION` |
| Budget leak / disconnect / cancellation | `TestSoakBudgetLeakSlope` and the budget leak disconnect matrix |
| Recovery probe flood + repeated recovery cycles | `TestStressRecoveryFloodFiveThousandDeploymentsStaysBounded`, `TestSoakRepeatedRecoveryCyclesReleaseState` |
| Event flood / slow subscribers / overload admission | `TestStressConcurrentEventAndSnapshotFlood`, `TestStressGlobalAdmissionKeepsControlPlaneResponsive` |
| 10k-deployment routing | `TestStressTenThousandDeploymentsConcurrentIndexedRouting` |

## Reproduce

```sh
NEXAROUTE_SOAK_ROUNDS=3 NEXAROUTE_SOAK_DURATION=2m NEXAROUTE_SOAK_OUT=benchmarks/soak ./scripts/soak.sh
```

Per-stage GNU time output and the JSON-lines goroutine/FD/heap
samples are preserved next to each raw log. The real
PostgreSQL/Redis outage drills are separate CI integration jobs
(`TestPostgresIntegrationOutageRecoveryCancellationBackpressure`,
`TestRedisIntegrationOutageRecoveryCancellationBackpressure`) run
against ephemeral service containers.

## Scope / UNVERIFIED

- Multi-hour sustained soak: the duration mode is operator-triggered and bounded (<= 6h); no multi-hour run is claimed unless this summary reports one.
- Live production PostgreSQL/Redis failover drill: the CI drills use disposable local service containers; production failover remains UNVERIFIED.
- Production-traffic SLOs: the local regression bands in `docs/PERFORMANCE_SLO.md` are not production SLOs.
- Live operator/game-day exercise: remains UNVERIFIED unless separately run with evidence.
