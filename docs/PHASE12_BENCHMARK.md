# Phase 12 benchmark report — adaptive routing / decision-plane latency overhead (#412)

This report satisfies the Phase 12 acceptance item *"benchmark
reports include latency overhead and test corpus"*. It measures
what the adaptive decision plane costs per request on top of the
deterministic router baseline, on a defined corpus, with real
measured numbers. It makes **no quality claims**: nothing here
shows the decision plane routes *better*, only what it *costs*.
Online learned adaptation remains unimplemented and unclaimed —
per the issue's design point 11 it stays gated behind proof of
quality benefit against latency, cost, availability, privacy and
prompt-injection risk; shadow mode plus this corpus are the
measurement foundation for any such future proposal.

## Corpus

| Benchmark | What it measures | Pool |
|---|---|---|
| `BenchmarkCandidates10/100/1000` (`internal/router`) | Deterministic baseline: eligibility + scoring + ranking for one request | 10 / 100 / 1000 deployments, one model, all healthy (25 ms recorded latency), tools+streaming requirement |
| `BenchmarkOrchestrator_OffMode` (`internal/decision`) | Orchestrator present, mode `off`: the decision plane's floor cost | 8 eligible candidates |
| `BenchmarkOrchestrator_Local` (`internal/decision`) | Local decision engine evaluation (Phase 12 policy scoring) | 8 eligible candidates |
| `BenchmarkOrchestrator_Local_100` (`internal/decision`, added by this slice) | Same, at the large end of the pool ladder | 100 eligible candidates |

Reproduce with the curated harness (the decision package and the
`Orchestrator` name filter were added to it by this slice):

```sh
scripts/bench.sh default phase12
# or directly:
go test ./internal/router/   -run xxx -bench BenchmarkCandidates    -benchtime 1s -count 5 -benchmem
go test ./internal/decision/ -run xxx -bench BenchmarkOrchestrator   -benchtime 1s -count 5 -benchmem
```

## Environment of record

AMD EPYC 9D25 (2 vCPUs visible), Go 1.27.0, linux/amd64, shared
CI-class VM — the same environment class as `docs/BENCHMARKS.md`.
Shared machines are noisy; the table reports **medians of 5 runs**
and the observed spread matters more than the third digit.

## Results (measured 2026-10-18, medians of 5 × 1 s runs)

| Benchmark | ns/op (median) | spread | B/op | allocs/op |
|---|---|---|---|---|
| Candidates10 (baseline) | 11,097 | 10,338–12,823 | 5,421 | 4 |
| Candidates100 (baseline) | 119,920 | 103,790–155,843 | 49,708 | 4 |
| Candidates1000 (baseline) | 1,227,139 | 1,170,238–1,502,462 | 467,500 | 4 |
| Orchestrator OffMode (8) | 559 | 461–793 | 1,824 | 2 |
| Orchestrator Local (8) | 4,609 | 3,635–6,044 | 5,984 | 11 |
| Orchestrator Local_100 (100) | 25,224 | 21,581–26,478 | 67,208 | 14 |

## Reading the overhead

- **Decision plane off:** the HTTP wiring short-circuits before
  the orchestrator when the mode is off, so the production cost
  with the feature disabled is effectively zero; the 0.56 µs
  OffMode figure is only the orchestrator's own mode check when
  called directly.
- **Local decision at 8 candidates:** ≈ 4.6 µs per request on top
  of an ≈ 11.1 µs baseline scoring step (~+40 % of the *scoring
  step*, not of the request, which is dominated by upstream I/O).
- **Local decision at 100 candidates:** ≈ 25.2 µs on top of an
  ≈ 119.9 µs baseline (~+21 % of the scoring step). Decision cost
  grows much more slowly than the baseline's linear candidate
  scan, so its relative share *shrinks* as pools grow.
- Allocation pressure follows the same pattern (14 allocs at 100
  candidates vs the baseline's flat 4, but on a ~7× larger byte
  footprint in the baseline itself).

## What this does not show

- No end-to-end request latency claim: these are in-process
  scoring benchmarks without network, TLS or upstream time.
- No routing-quality claim: decision quality is evaluated through
  shadow mode (`decision_shadow` events) and deterministic replay,
  not by this report.
- No learned/adaptive behavior: the local engine is a bounded,
  deterministic policy scorer with versioned policies, minimum
  evidence hysteresis and classified failover triggers (PR #495).
  Anything beyond that requires the proof programme described in
  the issue, design point 11.
