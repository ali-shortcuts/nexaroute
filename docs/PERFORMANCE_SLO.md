# Performance baseline & initial SLOs (#437)

This document closes the last step of the #437 execution prompt:
*set initial performance SLOs only after a measured baseline.* The
baseline below was measured with the reproducible load profile
(`scripts/load.sh`, `internal/httpapi/loadprofile_test.go`) — never
estimated — and the SLOs are regression bands for rerunning that
profile in a comparable environment, **not** wall-clock assertions
in ordinary CI tests (the profile itself asserts only bounded-state
and liveness invariants, by design).

## Measured baseline

Command (per pool size): `NEXAROUTE_LOAD_POOL=<n> scripts/load.sh
slo-baseline-pool<n>` — 1000 ops, 16 workers, seed 1, 2 slow
dashboard subscribers, loopback fake upstreams with a fixed delay;
gateway overhead is measured separately from upstream latency.

| Environment | Commit | Pool | Overhead p50 | Overhead p95 | Stream TTFT p50 | Stream TTFT p95 | Goroutines after | Heap growth |
|---|---|---|---|---|---|---|---|---|
| GH-class runner, AMD EPYC 9V45, 4 vCPU, Go 1.25 (#498) | pre-`fa3a2d81` tree | 100 | — | 1.271 ms | — | 1.414 ms | 114 | +1.71 MB |
| Sandbox, AMD EPYC 9D25, 2 vCPU, Go 1.27 | `6c332fa5` | 10 | 1.85 ms | 6.636 ms | 1.844 ms | 7.403 ms | 102 | +1.54 MB |
| Sandbox, AMD EPYC 9D25, 2 vCPU, Go 1.27 | `6c332fa5` | 100 | 1.702 ms | 5.554 ms | 1.664 ms | 5.802 ms | 114 | +1.63 MB |
| Sandbox, AMD EPYC 9D25, 2 vCPU, Go 1.27 | `6c332fa5` | 1000 | 3.611 ms | 18.71 ms | 5.534 ms | 20.1 ms | 102 | +1.47 MB |

Raw output: `benchmarks/load/baseline-20261018.txt` (#498) and,
for this baseline, `benchmarks/load/slo-baseline-pool10.txt`,
`benchmarks/load/slo-baseline-pool100.txt` and
`benchmarks/load/slo-baseline-pool1000.txt`. Absolute numbers are environment-bound — the 2-vCPU
sandbox is several times slower than the 4-vCPU runner — which is
exactly why the SLOs below are relative bands plus generous
absolute ceilings, not tight absolutes.

Two structural facts the table proves:

- **Pool size does not leak resources**: goroutines after the
  profile sit at ~102–114 and heap growth stays under ~1.7 MB
  whether the candidate pool is 10 or 1000 deployments.
- **Overhead scales gently**: p95 gateway overhead grows from
  ~5.5–6.6 ms (pools 10–100) to ~18.7 ms at pool 1000 on the slow
  sandbox; on the faster runner pool-100 p95 was 1.271 ms.

## Initial SLOs (regression bands)

For a rerun of the standard profile (1000 ops, 16 workers, seed 1)
in an environment comparable to the recorded one:

1. **Gateway overhead p95** ≤ 3× the same-pool recorded baseline
   **and** ≤ 25 ms (pools ≤ 100) / ≤ 60 ms (pool 1000) on a
   2-vCPU-class machine.
2. **Stream TTFT overhead p95** ≤ 3× the same-pool recorded
   baseline and ≤ 25 ms / ≤ 60 ms on the same pool split.
3. **Bounded resources (hard invariants, already asserted by the
   harness)**: goroutines after ≤ 150; heap growth ≤ 16 MB; zero
   leaked provider capacity; zero leaked sessions.
4. **Correctness under the mixed profile**: every operation ends
   in success or a bounded typed failure (429 from the provider,
   502/503 from the gateway) — no hangs, no panics, no unbounded
   retries; a saturated set of 32 slow dashboard subscribers does
   not block inference (`TestSlowDashboardSubscribersDoNotBlockInference`).

A rerun breaching a band is a regression to investigate, not a
flaky test to retry: record the new raw output under
`benchmarks/load/`, compare against the table, and root-cause the
delta before moving the baseline.

## Fault-injection inventory (issue item 5)

Deterministic suites already in the tree; this slice verified the
mapping rather than duplicating coverage:

- Provider refused / failover from a refused primary / upstream
  hang bounded / mid-stream cut / security-store lock fails closed
  / half-open trial recovery — `internal/httpapi/failure_injection_test.go`.
- Outage-storm game day — `internal/httpapi/gameday_test.go`.
- Flapping-upstream failure semantics, hot-reload soak, budget
  leak slope + disconnect matrix — `internal/httpapi/soak_test.go`,
  `budget_leak_*_test.go` (settle/release exactly once; no
  duplicate billing under disconnects).
- Slow subscribers / event flood / recovery flood / 10k-deployment
  routing — the `TestStress*` suites driven by `scripts/soak.sh`.

## UNVERIFIED / remaining

- **Long-duration soak** (hours of sustained mixed load with
  periodic resource sampling): the profiles above run ~1 s per
  pool; no multi-hour soak evidence exists yet.
- **Real database/Redis outage injection**: bounded CI drills now run
  against disposable real PostgreSQL/Redis services
  (`TestPostgresIntegrationOutageRecoveryCancellationBackpressure`,
  `TestRedisIntegrationOutageRecoveryCancellationBackpressure`) using a
  TCP fault proxy that cuts live connections, rejects new ones, and then
  restores the service. They verify last-known-good reads, cancellation,
  bounded concurrent outage reads, state preservation and durable
  recovery. A live production failover/outage drill has not been run and
  remains UNVERIFIED. See
  `docs/reports/ISSUE437_SOAK_QUALIFICATION.md`.
- **Soak evidence**: `scripts/soak.sh` records commit, rounds,
  durations, peak RSS per stage, raw logs, a machine-readable
  `summary.json` and JSON-lines goroutine/FD/heap samples from the
  sustained workload. `NEXAROUTE_SOAK_DURATION` (up to 6h) and the Soak
  workflow's `duration`/`timeout_minutes` inputs support an
  operator-triggered longer run. The workflow publishes the summary and
  uploads the raw directory; any duration is only evidence when a PASS
  summary for that duration actually exists. See
  `docs/reports/ISSUE437_OPERATOR_RUNBOOK.md`.
- **Production SLOs**: latency/error budgets for a real deployment
  must be set from that deployment's traffic, not from loopback
  profiles. The bands above are regression detectors for the
  codebase, nothing more.
