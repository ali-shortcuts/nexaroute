# Issue #437 — soak/recovery and real-service outage qualification

This is the supervisor-qualification slice requested after PR #538 (measured
baseline + initial SLO bands) and PR #566 (recovery-soak fixture fix). It
does not repeat the 10/100/1000 load baselines or the existing
fault-injection/game-day coverage.

## What this slice completes

1. Re-ran the fixed recovery soak and the full soak matrix on the current
   tree, and added one deterministic fixture assertion: the recovery soak
   now fails if it stops exercising failed probes or observes fewer
   successful probes than the 3-of-5 re-admission threshold requires. No
   wall-clock threshold was added.
2. Added real PostgreSQL and Redis outage/recovery drills to the existing
   disposable-service integration jobs. A TCP fault proxy cuts live
   connections and refuses new ones against the real service, then restores
   it. The drills check:
   - last-known-good reads during the outage (state preserved, revision not
     advanced);
   - context cancellation still returns `context.Canceled`;
   - 32 concurrent outage reads resolve within bounded contexts and the
     connection pool stays at its configured bound;
   - after restore, reconciliation reports the durable source, the original
     revision/payload is intact, and the next compare-and-set write
     advances exactly one revision.
3. Fixed the real-store error classification that made those drills
   meaningful: PostgreSQL/Redis transport failures now wrap
   `ErrUnavailable` (while context cancellation and store sentinels remain
   unchanged). Before this, `last_known_good` worked only with the
   in-memory fake because real adapters returned an unclassified transport
   error.
4. Made the soak workflow reproducible and evidence-bearing:
   `scripts/soak.sh` now records the exact commit, branch, worktree state,
   environment, stress rounds, total/stage durations, peak RSS per stage,
   fault-class inventory, raw per-stage logs and a Markdown summary under a
   repository-owned path. The workflow publishes that summary and uploads
   the raw directory as an artifact.

## Existing coverage reused (not duplicated)

| Area | Existing evidence |
|---|---|
| Provider refused/hang/mid-stream cut/half-open | `internal/httpapi/failure_injection_test.go` |
| Outage storm | `internal/httpapi/gameday_test.go` |
| Flapping provider recovery | `TestSoakFlappingUpstreamFailureSemantics` |
| Hot config reload under load | `TestSoakConcurrentTrafficAndHotReload` |
| Budget leak/cancellation/disconnect | `TestSoakBudgetLeakSlope`, budget-leak matrix |
| Recovery flood | `TestStressRecoveryFloodFiveThousandDeploymentsStaysBounded` |
| 10k-deployment routing | `TestStressTenThousandDeploymentsConcurrentIndexedRouting` |
| Slow subscribers/backpressure | `TestSlowDashboardSubscribersDoNotBlockInference`, `TestStressGlobalAdmissionKeepsControlPlaneResponsive` |

## Measured local evidence

Environment: Linux, AMD EPYC 7763, 4 vCPU, Go 1.25.0 linux/amd64. The
soak run recorded commit `0d6ee04` with a clean worktree.

Command:

```sh
NEXAROUTE_SOAK_ROUNDS=3 NEXAROUTE_SOAK_OUT=benchmarks/soak ./scripts/soak.sh
```

Result: `SOAK PASS` (exit 0). All stages passed:

| Stage | Duration | Max RSS |
|---|---|---|
| stress round 1 | 3s | 302844 KB |
| stress round 2 | 3s | 302124 KB |
| stress round 3 | 3s | 304308 KB |
| HTTP hot-reload soak | 11s | 300600 KB |
| recovery soak | 1s | 146964 KB |
| race-enabled soak | 14s | 342340 KB |
| budget leak slope | 3s | 312348 KB |

Raw evidence: `benchmarks/soak/20261019T102722Z-0d6ee0408e3f/` (summary,
per-stage logs and GNU `time -v` output). The race-enabled recovery soak
also passed separately with `-count=3` before this fit-for-purpose run.

Real-service drill command (disposable local containers, no secrets):

```sh
docker run -d --rm --name nexaroute-drill-redis -p 127.0.0.1:16379:6379 redis:7-alpine
docker run -d --rm --name nexaroute-drill-pg \
  -e POSTGRES_USER=nexaroute -e POSTGRES_PASSWORD=nexaroute-ci-only \
  -e POSTGRES_DB=nexaroute_test -p 127.0.0.1:15432:5432 postgres:16-alpine

NEXAROUTE_TEST_POSTGRES_DSN='postgres://nexaroute:nexaroute-ci-only@127.0.0.1:15432/nexaroute_test?sslmode=disable' \
NEXAROUTE_TEST_REDIS_URL='redis://127.0.0.1:16379/15' \
  go test -tags integration ./internal/controlplane/ \
  -run 'TestPostgresIntegrationOutageRecoveryCancellationBackpressure|TestRedisIntegrationOutageRecoveryCancellationBackpressure' \
  -count=5 -v
```

Result: 10/10 drill executions passed (5 PostgreSQL + 5 Redis), and both
drills passed again under `-race`. Raw output:
`benchmarks/soak/drills/outage-drill.raw.txt` and
`benchmarks/soak/drills/outage-drill-race.raw.txt`.

CI executes the same drills through the existing `integration-postgres`
and `integration-redis` jobs against their service containers, so no new
workflow dependency or secret is required.

## Verification

`./scripts/verify.sh` passed locally on this evidence tree (`VERIFY PASS`,
exit 0): formatting, clean unit/integration suite, `go vet`, clean race
suite, release artifact and SBOM trust-chain checks, JavaScript syntax,
short fuzz checks and the Linux amd64/arm64 builds. Chromium plus Python
Playwright were not installed on this runner, so the script's documented
browser-acceptance warning path was used; browser acceptance was not run
here. Raw log: `benchmarks/soak/verify-791a131.raw.txt`.

```
== mandatory clean unit/integration pass ==   PASS
== mandatory clean race pass ==               PASS
== release artifact manifest checks ==        PASS
== SBOM generator trust-chain checks ==       PASS
== short fuzz checks ==                       PASS
== linux amd64 build == / arm64 build ==      PASS
VERIFY PASS
```

## Remaining / explicitly UNVERIFIED

- **Multi-hour sustained soak**: the dispatchable workflow is bounded
  (1–20 rounds, currently minutes). No multi-hour operator soak has been
  run, so multi-hour resource drift remains UNVERIFIED.
- **Live production PostgreSQL/Redis failover**: the new drill is real but
  local/disposable. A production failover, replica promotion or managed
  service outage has not been run and remains UNVERIFIED.
- **Production-traffic SLOs**: `docs/PERFORMANCE_SLO.md` still contains
  local regression bands only. Production latency/error budgets require
  production traffic and remain UNVERIFIED.
- **Live operator game-day exercise**: remains UNVERIFIED unless separately
  run with evidence.

#437 is therefore not marked complete by this slice.
