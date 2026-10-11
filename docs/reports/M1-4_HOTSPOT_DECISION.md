# M1-4 — Hotspot Decision

**Date:** 2026-10-11

**State:** DONE — none actionable

## Decision

**None actionable.** No product-code optimization was selected or applied.

The M1-3 measurement run attempted Go CPU profiling at client concurrency 256, but the gateway's `/debug/pprof/profile` endpoint returned HTTP 404; `perf` is unavailable in the sandbox. No function-level profile or hotspot list was produced, so there is no measured top hotspot to target. The aggregate measurements do show that one configuration (tuned adaptive, explicit provider cap 32, concurrency 256) returned 119 HTTP 503 responses, but this is not CPU-profile evidence and does not identify a safe code-level optimization. Changing routing/admission behavior on this aggregate result alone would be speculative.

The M1-3 benchmark remains the before-measurement record, but there is no after-measurement because no optimization was made. This follows the checklist's explicit alternative: record `none actionable` when no clear and safe hotspot is available.

## Evidence

- M1-3 profile attempt and complete measurements: `docs/benchmarks/m1-3-2026-10-11/benchmark.json` (PR #257).
- M1-3 analysis, including status counts and pprof limitation: `docs/benchmarks/m1-3-2026-10-11/M1-3_BENCHMARK_REPORT.md` (PR #257).
- Source scan on baseline `dcb9f9b` for `net/http/pprof`, `runtime/pprof`, and `StartCPUProfile` in `cmd/` and `internal/` found no registered profiling implementation.
- `./scripts/verify.sh` — **VERIFY PASS**; raw output: `docs/reports/m1-4-verify.raw.txt`.

No product code, runtime behavior, dependencies, `scripts/verify.sh`, or workflow files are changed by M1-4.
