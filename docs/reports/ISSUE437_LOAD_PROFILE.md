# Issue #437 — local load profile and candidate-pool scaling baseline

Scope of this slice, what it completes, and what remains. This report is
deliberately partitioned from #435 (cost/quota/billing), #413
(frontend/backend contract acceptance) and #418 (policy canary/game-days).

## 1. What this slice completes

- A reproducible, local, mixed-workload load-profile harness that drives
  the real gateway handler against loopback `httptest` upstreams.
- Candidate-pool scaling evidence at **10 / 100 / 1000 deployments** for
  both the isolated router selection hot path and the end-to-end gateway.
- Gateway overhead reported **separately** from the fixed upstream delay
  (short-call p50/p95 and streamed TTFT p50/p95).
- One always-on deterministic contract test in ordinary CI proving a
  saturated set of slow dashboard subscribers cannot block inference.
- A runner (`scripts/load.sh`) that captures environment, seed and raw
  output under `benchmarks/load/`.

## 2. What remains (not claimed complete)

Issue #437 is broad; this is a subset. Still open:

- Sustained soak sampling of file descriptors, ledgers, caches, video
  queue state and event backlog over long horizons (link to #405).
- Fault-injection breadth not covered here: malformed streams,
  mid-stream disconnect (covered elsewhere), hot config reload under
  load (covered by `TestSoakConcurrentTrafficAndHotReload`), database /
  Redis outage, outage storms.
- Committed performance SLOs (intentionally deferred until repeated
  baselines exist; this is a first local baseline).
- CI wiring of the gated profile (kept manual to avoid shared-runner
  wall-clock flakiness).

## 3. Existing assets inspected first (no duplication)

| Asset | Purpose | Reused / not duplicated |
|---|---|---|
| `scripts/bench.sh` | Hot-path micro-benchmarks, default/tuned profiles | Extended bench set (1000 pool) picked up by same regex |
| `scripts/stress.sh` / `scripts/soak.sh` | Bounded stress + same-process soak | Left as-is |
| `internal/router/bench_test.go` | `Candidates10/100` | Added `Candidates1000` |
| `internal/router/stress_test.go` | 10k-deployment concurrent routing | Left as-is |
| `internal/probe/scale_acceptance_test.go` | 50/100/200 probe scheduler | Left as-is |
| `internal/events/sse_bounds_test.go` | Bounded subscriber buffer/eviction | Complements at unit level only |
| `internal/httpapi/routing_bench_test.go` | Gateway vs direct upstream | Reused measurement idea; the new harness adds mixed categories |
| `internal/httpapi/soak_test.go` | Flapping-upstream recovery soak | Left as-is |
| `internal/httpapi/stress_test.go` | Global admission / overload | Left as-is |
| `internal/httpapi/budget_leak_*` | Cancellation/ledger invariants (#405) | Left as-is (owned by #405) |

## 4. New files

- `internal/httpapi/loadprofile_test.go`
  - `TestSlowDashboardSubscribersDoNotBlockInference` — always-on,
    deterministic, no wall-clock thresholds.
  - `TestLoadProfileMixedWorkload` — gated by `NEXAROUTE_LOAD=1`.
- `scripts/load.sh` — environment capture + profile runner.
- `internal/router/bench_test.go` — `BenchmarkCandidates1000`.
- `benchmarks/load/*.txt` — raw captured output for this report.

## 5. Deterministic invariants (ordinary CI)

`TestSlowDashboardSubscribersDoNotBlockInference` attaches 32 slow
dashboard SSE subscribers to `/admin/api/events/stream`, then drives 128
inference requests concurrently. It asserts only bounded-state
invariants (no timing thresholds):

- every inference request returns `200`;
- global in-flight admission returns to `0`;
- every provider's `ActiveRequests`/`WaitingRequests` return to `0`.

This pins the #437 acceptance shape *"a slow UI must not block
inference"* without introducing a flaky timing assertion. Bounded event
buffers and slow-subscriber eviction remain asserted at the unit level
in `internal/events/sse_bounds_test.go`.

## 6. Mixed workload profile (gated)

`TestLoadProfileMixedWorkload` builds a deterministic priority-strategy
gateway and runs a seeded operation mix:

| Category | Alias | Upstream behaviour | Assertion |
|---|---|---|---|
| short | `short` | 200, fixed `3ms` | 200 |
| streaming coding | `coding` (pool) | SSE, fixed `2ms` first-chunk | 200 + `[DONE]` |
| failover chain | `failover` | primary `409`, fallback `200` | 200 + `FALLBACK` |
| provider 429 | `ratelimit` | `429` + `Retry-After` | `429`/`502`/`503` (bounded) |
| client timeout | `slow` | sleeps `400ms`; client deadline `60ms` | any bounded outcome |

Gateway overhead = observed latency − configured upstream delay, so local
routing/handler cost is reported separately from provider timing. The
profile asserts bounded state and liveness only (all requests resolve, no
in-flight/capacity leaks, failover + 429 paths actually exercised, bounded
goroutine growth). It never asserts a latency threshold.

## 7. Environment of record

- Machine: `Linux`, `AMD EPYC 9V45 96-Core Processor`, `nproc=4`, shared CI-class VM
- Go: `go1.25.0 linux/amd64`
- Base commit at work start: `4e91dbb` (`origin/main` tip)
- Deterministic seed: `1`

## 8. Exact commands

```sh
# Candidate-pool scaling (isolated router hot path)
go test -run xxx -bench 'BenchmarkCandidates' -benchtime 1s -count 5 -benchmem ./internal/router/

# End-to-end mixed workload at pool sizes 10 / 100 / 1000
NEXAROUTE_LOAD_POOL=10   ./scripts/load.sh scale-10
./scripts/load.sh baseline-20261018
NEXAROUTE_LOAD_POOL=1000 ./scripts/load.sh scale-1000

# Always-on deterministic contract
go test -count=1 -run 'TestSlowDashboardSubscribersDoNotBlockInference' ./internal/httpapi/
```

## 9. Raw baseline output

### 9.1 Candidate-pool scaling (`benchmarks/load/candidates-baseline.txt`)

```
cpu: AMD EPYC 9V45 96-Core Processor
BenchmarkCandidates10-4     	  267951	      4237 ns/op	    5423 B/op	       4 allocs/op
BenchmarkCandidates10-4     	  281901	      4290 ns/op	    5423 B/op	       4 allocs/op
BenchmarkCandidates10-4     	  279758	      4272 ns/op	    5423 B/op	       4 allocs/op
BenchmarkCandidates10-4     	  267264	      4243 ns/op	    5422 B/op	       4 allocs/op
BenchmarkCandidates10-4     	  275742	      4214 ns/op	    5423 B/op	       4 allocs/op
BenchmarkCandidates100-4    	   24829	     46095 ns/op	   49708 B/op	       4 allocs/op
BenchmarkCandidates100-4    	   25498	     46725 ns/op	   49708 B/op	       4 allocs/op
BenchmarkCandidates100-4    	   25888	     46767 ns/op	   49708 B/op	       4 allocs/op
BenchmarkCandidates100-4    	   25526	     46418 ns/op	   49708 B/op	       4 allocs/op
BenchmarkCandidates100-4    	   26266	     47665 ns/op	   49708 B/op	       4 allocs/op
BenchmarkCandidates1000-4   	    2365	    499953 ns/op	  467500 B/op	       4 allocs/op
BenchmarkCandidates1000-4   	    2379	    520944 ns/op	  467500 B/op	       4 allocs/op
BenchmarkCandidates1000-4   	    2269	    503000 ns/op	  467500 B/op	       4 allocs/op
BenchmarkCandidates1000-4   	    2265	    508731 ns/op	  467500 B/op	       4 allocs/op
BenchmarkCandidates1000-4   	    2281	    509750 ns/op	  467500 B/op	       4 allocs/op
PASS
ok  	github.com/ali-shortcuts/nexaroute/internal/router	20.586s
```

### 9.2 End-to-end profile, pool = 10 (`benchmarks/load/scale-10.txt`)

```
seed=1 pool=10 ops=1000 workers=16 subscribers=2
elapsed=400ms
upstream_calls short=408 stream=291 fail=2 fallback=154 rate=2 slow=44
gateway_overhead short p50=691µs p95=1.323ms n=408
gateway_ttft stream p50=742µs p95=1.365ms n=291
goroutines before=11 after=109
heap_alloc_mb before=1.54 after=3.15
sessions=0
```

### 9.3 End-to-end profile, pool = 100 (`benchmarks/load/baseline-20261018.txt`)

```
seed=1 pool=100 ops=1000 workers=16 subscribers=2
elapsed=396ms
upstream_calls short=408 stream=291 fail=3 fallback=154 rate=2 slow=44
gateway_overhead short p50=613µs p95=1.271ms n=408
gateway_ttft stream p50=685µs p95=1.414ms n=291
goroutines before=11 after=114
heap_alloc_mb before=1.69 after=3.40
sessions=0
```

### 9.4 End-to-end profile, pool = 1000 (`benchmarks/load/scale-1000.txt`)

```
seed=1 pool=1000 ops=1000 workers=16 subscribers=2
elapsed=416ms
upstream_calls short=408 stream=291 fail=3 fallback=154 rate=3 slow=44
gateway_overhead short p50=613µs p95=1.569ms n=408
gateway_ttft stream p50=1.317ms p95=3.223ms n=291
goroutines before=11 after=115
heap_alloc_mb before=3.21 after=4.91
sessions=0
```

## 10. Interpretation

- Router candidate selection is linear in pool size (≈0.43µs/candidate at
  10, ≈0.46µs/candidate at 100, ≈0.50µs/candidate at 1000) with a fixed
  `4 allocs/op` regardless of pool size — allocation count is
  deterministic and the safest cross-run signal.
- End-to-end gateway overhead for non-streaming short calls is
  effectively pool-independent (p50 `613–691µs`), because a healthy
  priority strategy only dispatches the first candidate after selection.
- Streaming TTFT overhead rises with pool size (p50 `0.69ms → 1.32ms`)
  because the coding alias is served by the whole pool and candidate
  ordering cost is paid before dispatch. This is the intended
  large-pool signal and is the number to watch for regressions.
- Short-call and TTFT overheads are reported after subtracting the fixed
  upstream delay, keeping local gateway overhead separate from provider
  timing.

## 11. Limitations (honest)

- Single shared CI-class VM, `nproc=4`; treat `ns/op`/percentiles as
  same-session comparisons only. Alloc counts are deterministic.
- Loopback `httptest` includes HTTP + JSON overhead but no real network
  or provider behaviour.
- The profile asserts invariants, not SLOs; no SLO is set here.
- The gated profile is not wired into CI, to avoid shared-runner
  wall-clock flakiness. Run it locally / on a dedicated host.
- Seed `1` fixes the category sequence; changing the seed changes the mix
  but not the harness. Record the seed with any future baseline.

## 12. Non-overlap

- No changes to cost/quota/budget ledger behaviour (#435).
- No changes to protocol/UI contracts or browser E2E (#413).
- No changes to policy lifecycle/canary (#418).
- No change to `docs/reports/COMPLETION_CHECKLIST.md`.
