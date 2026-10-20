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

## Follow-up — operator-duration qualification path (2026-10-20)

The earlier remainder said the dispatchable workflow was bounded to stress
rounds only. That gap is now closed without claiming a multi-hour run:

- `scripts/soak.sh` accepts `NEXAROUTE_SOAK_DURATION` in `s`, `m` or `h`
  (maximum `6h`; `0s` keeps the fixed-iteration shape). The HTTP soak test
  deadline is raised automatically above the requested duration.
- The same-session mixed workload now runs until the requested duration
  instead of a fixed operation count, while the configuration hot-reload loop
  continues alongside it.
- `TestSoakConcurrentTrafficAndHotReload` samples goroutines, open file
  descriptors and heap allocation every 250 ms and fails on a
  worker-proportional growth bound. The samples are written as JSON lines;
  no wall-clock latency assertion was added.
- `scripts/soak.sh` now emits a machine-readable
  `summary.json` (schema `nexaroute.soak.qualification.v1`) alongside the
  Markdown summary. It records the exact commit, branch, workflow refs,
  worktree state, environment, stress rounds, requested duration, per-stage
  exit/duration/max RSS and the resource-sample path.
- `.github/workflows/soak.yml` exposes `duration` and `timeout_minutes`
  `workflow_dispatch` inputs and still uploads the raw directory as an
  artifact.
- The precise operator path, evidence contract and live-environment boundary
  are documented in `docs/reports/ISSUE437_OPERATOR_RUNBOOK.md`.

### Actual bounded run (not a multi-hour soak)

Environment: Linux, AMD EPYC 7763, 4 vCPU, Go 1.25.0 linux/amd64. Clean
worktree at commit `d474a040a4cc` (the run started from the completed
operator-path commit; the evidence was added afterwards).

```sh
NEXAROUTE_SOAK_ROUNDS=3 NEXAROUTE_SOAK_DURATION=2m \
  NEXAROUTE_SOAK_OUT=benchmarks/soak ./scripts/soak.sh
```

Result: `SOAK PASS` (exit 0), total 273 s. All three stress rounds, the
2-minute sustained mixed workload, the recovery soak, the race-enabled soak
and the budget-leak slope passed.

| Stage | Duration | Max RSS |
|---|---|---|
| stress rounds 1–3 | 3 s each | 303788–303836 KB |
| sustained mixed workload (requested 2m) | 130 s | 304440 KB |
| recovery soak | <1 s | 144264 KB |
| race-enabled soak | 132 s | 343340 KB |
| budget-leak slope | 2 s | 305644 KB |

The sustained workload executed 701,414 requests with 701,414 upstream calls
and 9,082 hot reloads. Across 482 samples (120.0 s):

| Metric | Minimum | Peak | Last |
|---|---|---|---|
| goroutines | 40 | 109 | 79 |
| open file descriptors | 57 | 60 | 59 |
| heap allocation | 3.35 MB | 15.23 MB | 9.17 MB |

The worker-proportional bounds were not exceeded, in-flight admission and
provider capacity returned to zero, and session affinity stayed within its
bound. Raw evidence:
`benchmarks/soak/20261020T023234Z-d474a040a4cc/` (summary, per-stage logs,
GNU `time -v` files, `http-hot-reload-samples.jsonl` and the full transcript in `soak.raw.txt`).

### Real-service outage drills re-run

The disposable PostgreSQL 16 / Redis 7 drills from the section above were
re-run at commit `d474a040a4cc`:

```sh
NEXAROUTE_TEST_POSTGRES_DSN='postgres://nexaroute:nexaroute-ci-only@127.0.0.1:15432/nexaroute_test?sslmode=disable' \
NEXAROUTE_TEST_REDIS_URL='redis://127.0.0.1:16379/15' \
  go test -tags integration ./internal/controlplane/ \
  -run 'TestPostgresIntegrationOutageRecoveryCancellationBackpressure|TestRedisIntegrationOutageRecoveryCancellationBackpressure' \
  -count=3 -v
```

Result: 6/6 PASS, plus both drills again PASS under `-race`. Raw evidence:
`benchmarks/soak/drills/outage-drill-20261020T023728Z.raw.txt` and
`benchmarks/soak/drills/outage-drill-race-20261020T023742Z.raw.txt`.

### Verification of this follow-up

`./scripts/verify.sh` passed at commit `43d20c0` (`VERIFY PASS`, exit 0):
shell syntax, flake-hunt harness self-checks, formatting, clean
unit/integration suite, `go vet`, clean race suite, release/SBOM
trust-chain checks, JavaScript syntax, short fuzz checks and the Linux
amd64/arm64 builds. Chromium plus Python Playwright were unavailable on this
runner, so the script used its documented browser-acceptance warning path;
browser acceptance was not run here. Raw log:
`benchmarks/soak/verify-43d20c0.raw.txt`.

Focused reruns on the same tree:

- `NEXAROUTE_SOAK=1 go test ./internal/probe/ -run '^TestSoak' -count=10 -v`
  — 10/10 PASS.
- `go test ./internal/httpapi -run 'TestSlowDashboardSubscribersDoNotBlockInference|TestLoadProfileMixedWorkload' -count=1 -v`
  — slow-subscriber contract PASS; the gated load profile skipped without
  `NEXAROUTE_LOAD=1` as designed.
