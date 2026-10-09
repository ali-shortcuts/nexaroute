# Phase 0 baseline report — 2026-10-09

## 1. Phase and PR status

- **Phase:** 0 — baseline and honest capability matrix.
- **PR:** not opened; the phase was executed on a local feature branch only.
- **Implementation status:** no product feature was implemented. Phase 0 produced documentation and a reproducible benchmark harness.

## 2. Acceptance matrix

| Acceptance item | Status | Evidence |
|---|---|---|
| Run `./scripts/verify.sh` | **DONE** | Baseline job `job_TeCyTvNL`; verify printed `VERIFY PASS`. |
| Measure package coverage | **DONE** | Baseline job `job_VH4YLlUG`; package coverage recorded below. |
| Create capability matrix | **DONE with conservative evidence** | `docs/CAPABILITY_MATRIX.md`; competitor cells link official docs and use `unverified` where evidence was insufficient. |
| Create reproducible gateway benchmark | **DONE** | `scripts/bench/gateway_overhead.py`; result `docs/benchmarks/phase0-2026-10-09.json`. |
| Publish comparable competitor performance numbers | **NOT DONE** | Deliberately not attempted; no same-hardware competitor runs were performed. |

## 3. Verification output summary

- `gofmt -l .`: pass.
- `go vet ./...`: pass.
- `go test -count=1 ./...`: pass.
- `go test -race -count=1 ./...`: pass inside `scripts/verify.sh`.
- JavaScript syntax checks: pass.
- Browser control-plane acceptance: pass where Chromium/Playwright were available.
- Short fuzz checks: pass.
- Linux amd64 and arm64 builds: pass.
- Final repository baseline before Phase 0 artifacts: `v0.16.3`, commit `f30fc8b403df4e13cdd3cc6133164a54e67e7835`, clean `main`.

## 4. Coverage baseline

The requested 85% overall target is **not met** by the current repository baseline. The measured package values were:

```text
cmd/gateway 22.9%
internal/budget 86.2%
internal/cache 87.4%
internal/compat 64.5%
internal/config 70.5%
internal/controlplane 40.2%
internal/core 100.0%
internal/decision 68.0%
internal/decision/jev 80.8%
internal/decision/policy 83.9%
internal/decision/providerstate 85.1%
internal/decision/remote 85.2%
internal/desktop 70.9%
internal/eval 88.0%
internal/evallive 81.5%
internal/events 94.3%
internal/feature 79.6%
internal/guardrail 0.0%
internal/health 83.2%
internal/httpapi 73.4%
internal/logging 72.7%
internal/probe 71.8%
internal/protocol/canonical 83.9%
internal/providers 64.0%
internal/route 94.8%
internal/router 81.6%
internal/scorecards 88.6%
internal/taskprofile 84.6%
internal/translate 80.9%
internal/usage 65.3%
```

The low or zero coverage packages are recorded as a Phase 0 gap, not hidden by averaging or by excluding packages.

## 5. Benchmark run

Command:

```bash
python3 scripts/bench/gateway_overhead.py --requests 100 --output /tmp/nexaroute-benchmark.json
```

Method: local loopback mock upstream, fixed 15 ms response delay, sequential OpenAI Chat Completions requests, plus a three-chunk streaming response with 10 ms inter-chunk delay. The direct path and gateway path used the same mock. Hardware reported by the harness: Linux x86_64, 6 CPUs, Python 3.12.3.

Results from `docs/benchmarks/phase0-2026-10-09.json`:

- Direct p50/p95/p99: **15.93 / 16.16 / 18.16 ms**.
- Through NexaRoute p50/p95/p99: **59.95 / 60.07 / 60.61 ms**.
- Added p50/p95/p99: **44.02 / 43.91 / 42.46 ms** for this specific cold local mock configuration.
- Direct throughput: **61.75 RPS**; gateway throughput: **16.66 RPS** in the sequential workload.
- Streaming TTFT overhead: **0.39 ms** in this run.
- Gateway process VmHWM: **28,296 kB**; this is process high-water memory, not a complete system memory profile.

These values are a reproducible local baseline, not a universal performance claim. Competitor comparisons require running the same harness against each product and configuration.

## 6. Documentation and gap delta

- Added `docs/CAPABILITY_MATRIX.md` with NexaRoute evidence and official competitor links.
- Added `scripts/bench/gateway_overhead.py` and its JSON result artifact.
- Added `docs/benchmarks/phase0-2026-10-09.json`.
- Added the prioritized gap list in the capability matrix. Existing `docs/KNOWN_GAPS.md` already records the major security, HA, protocol, economics and observability boundaries; no product gap was silently marked fixed.

## 7. Not done or not verifiable

- The structured competitor workflow completed the LiteLLM subtask but four competitor subtasks were stopped by the session credit limit. Existing official links from the repository's prior comparison were reused where available; otherwise matrix cells are explicitly `unverified`.
- No Phase 1 implementation was started. In particular, there is no claim that encrypted secret storage, RBAC/SSO, TLS/mTLS, durable runtime adapters, or audit-chain security is complete.
- No competitor performance benchmark was run.

## 8. Recommended next phase

Phase 1 should be split into small concerns, beginning with encrypted-at-rest secrets and migration tests. It must preserve zero-dependency default builds, add fuzz/property tests for parsers and migration logic, update `docs/KNOWN_GAPS.md` and `docs/CAPABILITY_MATRIX.md`, and run the full verification gate before any release decision.
