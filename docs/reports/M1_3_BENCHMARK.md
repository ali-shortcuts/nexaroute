# M1-3 — Benchmark Harness Repair and Proof

Date: 2026-10-11. These are local reproducible measurements for the current NexaRoute checkout, not universal performance claims or competitor comparisons.

## Result

The harness now keeps gateway stdout/stderr, generated configurations, the compiled mock, the compiled gateway, raw JSON, and the CPU profile under a persistent artifacts directory. On failure it prints the last 80 gateway-log lines. It requires at least 1,000 measured requests at every requested concurrency and rejects every response other than HTTP 200.

## Root cause and correction

The historical `process exited with 1` run cannot be attributed with certainty: its log was deleted on exit, as noted in the completion prompt. A fresh reproduction with persistent diagnostics did identify a concrete harness/configuration failure at c=256:

- Gateway startup and health checks succeeded; missing encrypted-secret key, control-plane store, and decision-chain fields were **not** startup requirements for this synthetic configuration (no encrypted credentials, external control plane, or non-default decision mode is used).
- The benchmark config omitted `routing.max_inflight_requests`, so the gateway applied its default global admission limit of **128** while the client attempted 256 in-flight requests. The gateway returned `503 gateway is at capacity`; 24 such responses were observed before the harness aborted.
- The failure config, gateway log, and error are retained in [`m1-3-attempt-01-global-admission-failure`](../benchmarks/m1-3-attempt-01-global-admission-failure/).

The harness now sets `max_inflight_requests=512` in both benchmark profiles. This is intentionally above c=256 to isolate the provider-cap comparison (explicit 32 versus the omitted-field default 256). **This is a load-test override, not the product default**; the product default remains 128. The default routing profile still uses `ready_mesh` and probes enabled/on-start, and the tuned profile uses `adaptive`, one attempt, and probes disabled. The benchmark records this override in the raw JSON.

## Method and environment

- Sandbox: Linux x86_64, 6 CPUs; Python 3.12.3; Go 1.26.8.
- Local compiled Go `net/http` mock; 15 ms fixed non-streaming response delay; TCP_NODELAY; each non-streaming response written in one write.
- Persistent HTTP/1.1 client connections, two warm-up requests per connection **before** timed measurement, and 1,000 measured requests at each concurrency: 1, 16, 64, and 256.
- Direct baseline plus 16 gateway measurement cells: 2 routing profiles × 2 provider caps × 4 client concurrency levels. Explicit provider cap is 32; default provider cap is measured by omitting the field (effective default 256).
- Streaming is a separate 3-chunk SSE probe (10 ms gap); TTFT is measured from request start to first response bytes.
- Gateway peak RSS is Linux `VmHWM` (high-water RSS), collected from `/proc/<pid>/status` after each gateway sequence. It is a per-run high-water mark, not a c-specific allocation measurement.
- pprof is captured in a separate default-profile, default-provider-cap c=256 run using opt-in `-cpuprofile`; top output is saved in the artifacts directory.

Reproduce from the repository root:

```bash
python3 scripts/bench/gateway_overhead.py \
  --requests 1000 \
  --output docs/benchmarks/m1-3-2026-10-11.json \
  --artifacts-dir docs/benchmarks/m1-3-2026-10-11-artifacts
```

## Direct baseline

| Client concurrency | p50 ms | p95 ms | p99 ms | RPS |
|---:|---:|---:|---:|---:|
| 1 | 15.776 | 15.939 | 16.011 | 63.4 |
| 16 | 16.087 | 17.355 | 18.187 | 967.2 |
| 64 | 16.375 | 19.920 | 24.500 | 3467.4 |
| 256 | 40.769 | 90.390 | 115.147 | 4177.8 |

## Gateway measurements

Each row contains 1,000 successful measured responses. Peak RSS shown in the last column is the high-water mark for that gateway process over its run sequence.

| Routing profile | Provider cap | Client concurrency | p50 ms | p95 ms | p99 ms | RPS | Peak RSS MB |
|---|---:|---:|---:|---:|---:|---:|---:|
| tuned adaptive | 32 | 1 | 16.415 | 16.684 | 16.963 | 60.8 | 45.9 |
| tuned adaptive | 32 | 16 | 16.843 | 18.460 | 19.594 | 932.1 | 45.9 |
| tuned adaptive | 32 | 64 | 31.952 | 34.806 | 37.142 | 1938.2 | 45.9 |
| tuned adaptive | 32 | 256 | 127.905 | 132.846 | 136.170 | 1897.1 | 45.9 |
| tuned adaptive | 256 (default) | 1 | 16.580 | 16.877 | 17.086 | 60.3 | 43.9 |
| tuned adaptive | 256 (default) | 16 | 16.665 | 18.213 | 19.701 | 936.4 | 43.9 |
| tuned adaptive | 256 (default) | 64 | 17.540 | 23.196 | 27.838 | 3192.1 | 43.9 |
| tuned adaptive | 256 (default) | 256 | 50.890 | 128.660 | 175.366 | 3134.3 | 43.9 |
| ready_mesh + probes | 32 | 1 | 16.617 | 17.074 | 17.430 | 59.9 | 45.6 |
| ready_mesh + probes | 32 | 16 | 16.944 | 18.452 | 19.716 | 927.2 | 45.6 |
| ready_mesh + probes | 32 | 64 | 32.025 | 37.392 | 39.685 | 1894.8 | 45.6 |
| ready_mesh + probes | 32 | 256 | 128.268 | 132.943 | 135.097 | 1917.4 | 45.6 |
| ready_mesh + probes | 256 (default) | 1 | 16.553 | 16.815 | 17.020 | 60.3 | 44.5 |
| ready_mesh + probes | 256 (default) | 16 | 16.805 | 18.471 | 19.674 | 931.8 | 44.5 |
| ready_mesh + probes | 256 (default) | 64 | 17.701 | 23.924 | 30.015 | 3129.1 | 44.5 |
| ready_mesh + probes | 256 (default) | 256 | 48.939 | 127.412 | 183.962 | 3485.2 | 44.5 |

## Streaming TTFT

Direct TTFT was **46.384 ms** (total 46.387 ms). Each gateway cell used the same mock and stream format.

| Routing profile | Provider cap | Gateway TTFT ms | Added TTFT ms |
|---|---:|---:|---:|
| tuned adaptive | 32 | 47.136 | 0.752 |
| tuned adaptive | 256 (default) | 47.315 | 0.931 |
| ready_mesh + probes | 32 | 47.134 | 0.750 |
| ready_mesh + probes | 256 (default) | 47.194 | 0.809 |

## CPU profile

The c=256 Go `runtime/pprof` CPU profile was captured successfully. In the 770 ms of samples, the largest entry was `internal/runtime/syscall/linux.Syscall6` at 19.48% flat; `runtime.futex` was 5.19%. The largest visible application-owned entry was `internal/httpapi.cloneConfig` at 1.30% flat in this run. The detailed top-20 output and raw profile are retained with the benchmark artifacts.

## Evidence files

- Raw numeric measurements: [`m1-3-2026-10-11.json`](../benchmarks/m1-3-2026-10-11.json).
- Persistent gateway stdout/stderr, mock source/log, generated configs, profile and pprof top: [`m1-3-2026-10-11-artifacts`](../benchmarks/m1-3-2026-10-11-artifacts/).
- Reproduced first failure and its 503 gateway diagnostics: [`m1-3-attempt-01-global-admission-failure`](../benchmarks/m1-3-attempt-01-global-admission-failure/).
- The harness deliberately changes no competitor claims; the capability matrix is unaffected.
