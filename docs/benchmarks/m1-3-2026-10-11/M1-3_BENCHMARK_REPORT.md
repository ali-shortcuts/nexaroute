# M1-3 — Benchmark Harness Repair and Proof

**Date:** 2026-10-11

**Repository revision under test:** branch `completion/m1-3-benchmark-harness`

**State:** DONE (subject to PR CI)

## Root cause and repair

The prior harness launched NexaRoute with `go run` and then terminated the `go run` parent PID. The Go tool had spawned a separate gateway executable; terminating the parent left that child alive. A lifecycle reproduction confirmed this: the parent exited with signal 15 while a gateway process using the same config remained alive (`go-run-lifecycle-repro.txt`). The old harness also placed each generated config under one temporary directory and relied on the implicit security-store path, so those overlapping gateway runs contended for the same bbolt file. The reproduced second startup failed with:

> `cannot initialize admin security store: open security store: timeout`

The original persistent gateway log is `initial-failure-gateway.log`. The fix is to compile the gateway once and launch the executable directly (so the PID sampled and terminated is the actual server), and to give each config/cap combination its own explicit `admin.security_store_path`. The benchmark config explicitly selects decision mode `off`; it has no configured credentials/secrets, so no encrypted-secrets key is needed. The security-store package creates its parent directory.

The harness now retains per-process gateway stdout/stderr under `benchmark-logs/` and prints the last 80 lines if startup or a measured request fails. The original failure log and a repeatable lifecycle reproduction are committed beside the results.

## Method

- Compiled Go `net/http` mock upstream; fixed 15 ms non-streaming response delay; TCP_NODELAY; local loopback.
- Persistent HTTP/1.1 connections; two warm-up requests per connection, completed before the timed window.
- 1,000 measured requests per client-concurrency level: **1, 16, 64, 256**.
- Direct baseline plus 16 gateway combinations: `ready_mesh` with startup/capability probes enabled and tuned `adaptive` (`max_attempts=1`, probes disabled), each at provider concurrency **32 explicit** and **256 default**. For the default-cap run the `max_concurrency` field is omitted, allowing the application default of 256 to apply. The default global in-flight limit remains 128.
- Streaming TTFT measured separately with the mock's multi-chunk SSE response.
- Gateway RSS is the server process's `/proc/<pid>/status` `VmHWM`, recorded after all four concurrency levels for each config/cap process.
- Every HTTP response is counted. JSON includes all-response p50/p95/p99 and throughput, plus success-only p50/p95/p99 and RPS, so overload responses are not hidden.

## Direct baseline

| Client concurrency | HTTP 200 | p50 / p95 / p99 (ms) | Success RPS |
|---:|---:|---:|---:|
| 1 | 1,000 | 16.02 / 16.38 / 17.72 | 62.1 |
| 16 | 1,000 | 16.39 / 18.06 / 19.28 | 951.0 |
| 64 | 1,000 | 17.08 / 19.60 / 21.18 | 3,299.0 |
| 256 | 1,000 | 20.02 / 28.10 / 33.14 | 2,651.8 |

## Gateway results

Percentiles below are for HTTP 200 responses; the status column and success RPS expose errors separately. RPS is completed HTTP responses per second. Gateway HWM is repeated per process and therefore covers the full four-level run for that config/cap.

| Config | Provider cap | Client c | HTTP statuses | p50 / p95 / p99 successful (ms) | Total RPS | Success RPS | Gateway HWM (MiB) |
|---|---:|---:|---|---:|---:|---:|---:|
| `ready_mesh_default` | 32 (explicit) | 1 | 200:1,000 | 16.84 / 17.31 / 19.41 | 59.1 | 59.1 | 33.06 |
| `ready_mesh_default` | 32 (explicit) | 16 | 200:1,000 | 17.38 / 19.42 / 20.96 | 895.1 | 895.1 | 33.06 |
| `ready_mesh_default` | 32 (explicit) | 64 | 200:1,000 | 32.02 / 34.37 / 36.29 | 1,909.6 | 1,909.6 | 33.06 |
| `ready_mesh_default` | 32 (explicit) | 256 | 200:1,000 | 29.30 / 63.99 / 100.38 | 1,242.3 | 1,242.3 | 33.06 |
| `ready_mesh_default` | 256 (default) | 1 | 200:1,000 | 16.70 / 17.42 / 18.57 | 59.5 | 59.5 | 34.35 |
| `ready_mesh_default` | 256 (default) | 16 | 200:1,000 | 16.94 / 18.41 / 19.51 | 922.0 | 922.0 | 34.35 |
| `ready_mesh_default` | 256 (default) | 64 | 200:1,000 | 17.81 / 33.22 / 41.99 | 2,949.6 | 2,949.6 | 34.35 |
| `ready_mesh_default` | 256 (default) | 256 | 200:1,000 | 20.35 / 27.95 / 31.16 | 2,680.1 | 2,680.1 | 34.35 |
| `tuned_adaptive_no_probe` | 32 (explicit) | 1 | 200:1,000 | 16.82 / 17.72 / 20.04 | 58.9 | 58.9 | 39.28 |
| `tuned_adaptive_no_probe` | 32 (explicit) | 16 | 200:1,000 | 17.09 / 18.63 / 20.29 | 917.5 | 917.5 | 39.28 |
| `tuned_adaptive_no_probe` | 32 (explicit) | 64 | 200:1,000 | 32.16 / 36.55 / 38.88 | 1,895.2 | 1,895.2 | 39.28 |
| `tuned_adaptive_no_probe` | 32 (explicit) | 256 | 200:881, 503:119 | 60.44 / 71.42 / 76.76 | 2,146.8 | 1,891.3 | 39.28 |
| `tuned_adaptive_no_probe` | 256 (default) | 1 | 200:1,000 | 16.74 / 17.24 / 17.72 | 59.6 | 59.6 | 35.47 |
| `tuned_adaptive_no_probe` | 256 (default) | 16 | 200:1,000 | 16.86 / 18.39 / 19.63 | 929.6 | 929.6 | 35.47 |
| `tuned_adaptive_no_probe` | 256 (default) | 64 | 200:1,000 | 17.52 / 28.45 / 38.40 | 3,073.8 | 3,073.8 | 35.47 |
| `tuned_adaptive_no_probe` | 256 (default) | 256 | 200:1,000 | 20.32 / 27.36 / 32.71 | 3,007.0 | 3,007.0 | 35.47 |

One case—tuned adaptive, explicit provider cap 32, concurrency 256—returned 119 HTTP 503 responses. They are included in the JSON's all-response metrics and are not represented as successful requests in the table's percentiles or success RPS. The other 15 gateway cases returned 1,000 HTTP 200 responses each.

## Streaming TTFT

| Path | TTFT (ms) | Over direct |
|---|---:|---:|
| Direct mock | 46.79 | — |
| `ready_mesh_default`, provider cap default 256 | 47.26 | +0.47 ms |
| `tuned_adaptive_no_probe`, provider cap default 256 | 47.31 | +0.52 ms |

## CPU profile attempt

During the default `ready_mesh` / default provider-cap run at client concurrency 256, the harness requested `/debug/pprof/profile?seconds=5`. The gateway returned HTTP **404** (`404 page not found`); `perf` is unavailable in this sandbox. No CPU profile or hotspot list could be captured. Per the checklist requirement, the result is **`cause not determined`**. The exact attempt result is in `benchmark.json` and `benchmark.stdout.txt`.

## Verification and evidence

- `python3 -m py_compile scripts/bench/gateway_overhead.py` — PASS.
- `git diff --check` — PASS.
- `./scripts/verify.sh` — **VERIFY PASS**; includes Go tests, `go vet`, mandatory clean race pass, JS syntax, real-browser control-plane and Live Visual Agent acceptance, short fuzz checks, and linux/amd64 + linux/arm64 builds.
- No product runtime behavior or dependencies changed; this item repairs the benchmark harness and publishes reproducible evidence.

Evidence files in this directory:

- `benchmark.json` — structured full measurements and pprof result.
- `benchmark.stdout.txt` — raw benchmark stdout.
- `verify.stdout.txt` — raw `verify.sh` output.
- `benchmark-logs/` — persistent mock/gateway stdout/stderr for all final runs.
- `initial-failure-gateway.log` — original startup failure, including the bbolt timeout.
- `go-run-lifecycle-repro.txt` — proof that killing the `go run` parent left its gateway child alive.
