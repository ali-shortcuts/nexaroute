# Phase 0 baseline report — follow-up 2026-10-09

## 1. Phase and PR status

- **Phase:** 0 — baseline and honest capability matrix.
- **Branch:** `phase0-baseline`.
- **PR:** [#210](https://github.com/ali-shortcuts/nexaroute/pull/210); not merged.
- **Implementation status:** no Phase 1 product feature was implemented. This follow-up corrected the benchmark harness and its evidence.

## 2. Acceptance matrix

| Acceptance item | Status | Evidence |
|---|---|---|
| Push Phase 0 branch and open one PR | **DONE** | PR link/number is recorded in the PR and final commit metadata. No merge performed. |
| Correct benchmark transport and workload | **DONE** | `scripts/bench/gateway_overhead.py`; Go mock, TCP_NODELAY, persistent connections, warm-up, 1/16/64 concurrency, 1,000 requests per level. |
| Replace obsolete benchmark JSON | **DONE** | `docs/benchmarks/phase0-2026-10-09.json`. |
| Report total and per-package coverage | **DONE** | `go tool cover -func=/tmp/nexaroute-cover.out | tail -1` returned `total: (statements) 75.0%`. |
| Exact browser-test evidence | **DONE** | Tests ran in the Manus Sandbox; command and result are recorded below. |
| Conservative competitor matrix | **DONE** | Every cell not supported by sufficient reviewed official documentation remains `unverified`; list is recorded below. |

## 3. Verification output summary

All commands below ran in the Manus Sandbox checkout `/home/ubuntu/nexaroute` with `/usr/local/go/bin` on `PATH`.

### Raw formatting output

```text
$ gofmt -l .
<no output>
exit 0
```

### Raw vet output

```text
$ go vet ./...
<no output>
exit 0
```

### Race-test summary

```text
$ go test -race -timeout=3m -count=1 ./...
ok   github.com/ali-shortcuts/nexaroute/cmd/gateway
ok   github.com/ali-shortcuts/nexaroute/internal/budget
ok   github.com/ali-shortcuts/nexaroute/internal/cache
ok   github.com/ali-shortcuts/nexaroute/internal/compat
ok   github.com/ali-shortcuts/nexaroute/internal/config
ok   github.com/ali-shortcuts/nexaroute/internal/controlplane
ok   github.com/ali-shortcuts/nexaroute/internal/core
ok   github.com/ali-shortcuts/nexaroute/internal/decision
ok   github.com/ali-shortcuts/nexaroute/internal/decision/jev
ok   github.com/ali-shortcuts/nexaroute/internal/decision/policy
ok   github.com/ali-shortcuts/nexaroute/internal/decision/providerstate
ok   github.com/ali-shortcuts/nexaroute/internal/decision/remote
ok   github.com/ali-shortcuts/nexaroute/internal/desktop
ok   github.com/ali-shortcuts/nexaroute/internal/doccheck
ok   github.com/ali-shortcuts/nexaroute/internal/eval
ok   github.com/ali-shortcuts/nexaroute/internal/evallive
ok   github.com/ali-shortcuts/nexaroute/internal/events
ok   github.com/ali-shortcuts/nexaroute/internal/feature
?    github.com/ali-shortcuts/nexaroute/internal/guardrail [no test files]
ok   github.com/ali-shortcuts/nexaroute/internal/health
ok   github.com/ali-shortcuts/nexaroute/internal/httpapi
ok   github.com/ali-shortcuts/nexaroute/internal/logging
ok   github.com/ali-shortcuts/nexaroute/internal/probe
ok   github.com/ali-shortcuts/nexaroute/internal/protocol/canonical
ok   github.com/ali-shortcuts/nexaroute/internal/providers
ok   github.com/ali-shortcuts/nexaroute/internal/route
ok   github.com/ali-shortcuts/nexaroute/internal/router
ok   github.com/ali-shortcuts/nexaroute/internal/scorecards
ok   github.com/ali-shortcuts/nexaroute/internal/taskprofile
ok   github.com/ali-shortcuts/nexaroute/internal/translate
ok   github.com/ali-shortcuts/nexaroute/internal/usage
exit 0
```

The full `./scripts/verify.sh` gate also passed, including JavaScript checks, fuzz checks, browser tests, and Linux amd64/arm64 builds: `VERIFY PASS`.

### Browser test location, commands, and results

These tests **ran in the Manus Sandbox** (not the user's local browser):

```text
$ python3 scripts/test-browser-e2e.py
BROWSER E2E PASS

$ python3 scripts/test-live-visual-agent.py
LIVE VISUAL AGENT E2E PASS
```

They were invoked by `./scripts/verify.sh` after confirming both `chromium` and the Python `playwright` module were available.

## 4. Coverage

Repository-wide coverage command and raw total:

```text
$ export PATH="/usr/local/go/bin:$PATH"
$ go test ./... -coverprofile=/tmp/nexaroute-cover.out
... all packages passed ...
$ go tool cover -func=/tmp/nexaroute-cover.out | tail -1
total:                                          (statements)                  75.0%
```

The required 85% overall target is **not met**. Per-package measurements:

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
internal/decision/policy 83.7%
internal/decision/providerstate 85.1%
internal/decision/remote 85.2%
internal/desktop 70.9%
internal/doccheck no statements
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

Packages below 70% are explicitly listed: **`cmd/gateway` (22.9%), `internal/compat` (64.5%), `internal/controlplane` (40.2%), `internal/decision` (68.0%), `internal/providers` (64.0%), `internal/usage` (65.3%), and `internal/guardrail` (0.0%)**. `internal/guardrail` has no test files and is not excluded from the gap list.

## 5. Corrected benchmark

Command:

```bash
python3 scripts/bench/gateway_overhead.py --requests 1000 --output /tmp/nexaroute-benchmark-v4.json
```

Method and hardware:

- Manus Sandbox, Linux `x86_64`, **6 CPUs**, Python **3.12.3**, Go at `/usr/local/go/bin/go`.
- Compiled Go `net/http` mock upstream with fixed 15 ms delay.
- Mock connections use `TCP_NODELAY`; each non-streaming response is written in one write.
- Python client uses persistent HTTP/1.1 connections and two warm-up requests per connection.
- 1,000 measured requests at concurrency **1, 16, and 64**.
- Provider `max_concurrency` is explicit in every generated config: **32, 128, and 256**.
- Streaming test retained with three chunks and 10 ms inter-chunk gap.
- Every measured request now requires HTTP 200; failed/error responses are rejected rather than timed.

The benchmark compares these configurations:

1. `adaptive_no_probe`: explicit `strategy=adaptive`, `max_attempts=1`, probes disabled.
2. `ready_mesh_default`: **default** `strategy=ready_mesh`, default `max_attempts=4`, probes enabled and run on start.

The prior 100-request Python-server result was discarded. Its approximately +44 ms result was a harness delayed-ACK artifact caused by separate header/body writes, not a NexaRoute latency claim.

Direct baseline across the run: p50/p95/p99 was **15.607 / 15.725 / 15.794 ms** at concurrency 1, **16.283 / 17.704 / 18.369 ms** at 16, and **16.876 / 19.272 / 20.476 ms** at 64.

### Concurrency 64: all provider caps

| Routing config | Provider cap | Gateway p50/p95/p99 ms | Added p50/p95/p99 ms | Gateway RPS |
|---|---:|---:|---:|---:|
| `adaptive_no_probe` | 32 | 31.914 / 35.785 / 37.460 | 15.038 / 16.513 / 16.984 | 1676.34 |
| `adaptive_no_probe` | 128 | 17.688 / 21.597 / 25.098 | 0.812 / 2.326 / 4.622 | 2599.71 |
| `adaptive_no_probe` | 256 | 18.310 / 24.078 / 28.878 | 1.434 / 4.806 / 8.402 | 2521.68 |
| `ready_mesh_default` (**default**) | 32 | 31.748 / 35.018 / 36.805 | 14.871 / 15.746 / 16.329 | 1708.86 |
| `ready_mesh_default` (**default**) | 128 | 17.611 / 20.790 / 39.771 | 0.734 / 1.518 / 19.295 | 2536.88 |
| `ready_mesh_default` (**default**) | 256 | 18.899 / 25.313 / 29.967 | 2.023 / 6.041 / 9.491 | 2452.49 |

At concurrency 1 and 16 with provider cap 32, the corresponding gateway p50/p95/p99 values were **16.154 / 16.346 / 16.625 ms** and **16.652 / 18.136 / 18.907 ms** for `adaptive_no_probe`; for `ready_mesh_default` they were **16.190 / 16.436 / 16.869 ms** and **16.596 / 17.831 / 18.484 ms**.

Streaming result at provider cap 32:

- Direct total/TTFT: **46.345 / 46.344 ms**.
- `adaptive_no_probe` gateway total/TTFT: **46.811 / 46.809 ms**; TTFT overhead **0.465 ms**.
- `ready_mesh_default` gateway total/TTFT: **46.917 / 46.915 ms**; TTFT overhead **0.572 ms**.

### CPU profiling and interpretation

The harness attempted to use `perf` during the gateway run. `perf` was not available in the Manus Sandbox, so no CPU profile or hotspot list was produced: **cause not determined**. The observed cap-dependent differences are reported as measurements, not as a proven causal explanation. In particular, the prior wording “saturation/queueing” has been removed; provider-cap-constrained latency and remaining unexplained variance are the supported statements.

The JSON artifact contains the raw measurements and records the profiling result. These are local reproducible baselines, not universal performance claims or competitor comparisons.

## 6. Competitor matrix and cell counts

The matrix now treats generic landing pages as insufficient evidence and avoids negative claims about competitors. Counts cover 16 competitor capability cells per competitor:

| Competitor | Verified | Unverified | Total |
|---|---:|---:|---:|
| LiteLLM | 10 | 6 | 16 |
| Portkey AI Gateway | 2 | 14 | 16 |
| Kong AI Gateway | 0 | 16 | 16 |
| Bifrost | 5 | 11 | 16 |
| Envoy AI Gateway | 1 | 15 | 16 |

The complete unverified-cell list and the deep-link evidence are in `docs/CAPABILITY_MATRIX.md`. In particular, every competitor cell in **Single-binary zero-dependency default** is `unverified`.

## 7. What was not done

- The PR was **not merged**.
- No Phase 1 work was started.
- No `.github/workflows/*` file was changed.
- `scripts/verify.sh` was not changed.
- No competitor performance benchmark was run.
- No CPU hotspot was claimed because profiling was unavailable; cause remains not determined.
- No universal performance, parity, or superiority claim was made.
- No release/tag was created.
- No production deployment, account change, or external destructive action was performed.

## 8. Next phase boundary

Phase 1 remains out of scope for this follow-up. Its planned work is encrypted-at-rest secrets, runtime integration of durable stores, and identity-model expansion, subject to a separate user instruction.
