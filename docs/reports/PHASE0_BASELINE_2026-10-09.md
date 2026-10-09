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
python3 scripts/bench/gateway_overhead.py --requests 1000 --output /tmp/nexaroute-benchmark-v2.json
```

Method and hardware:

- Manus Sandbox, Linux `x86_64`, **6 CPUs**, Python **3.12.3**, Go at `/usr/local/go/bin/go`.
- Compiled Go `net/http` mock upstream with fixed 15 ms delay.
- Mock connections use `TCP_NODELAY`; each non-streaming response is written in one write.
- Python client uses persistent HTTP/1.1 connections and two warm-up requests per connection.
- 1,000 measured requests at concurrency **1, 16, and 64**.
- Streaming test retained with three chunks and 10 ms inter-chunk gap.
- Direct and gateway paths use the same mock upstream.

The prior 100-request Python-server result was discarded. Its approximately +44 ms result was a harness delayed-ACK artifact caused by separate header/body writes, not a NexaRoute latency claim.

| Concurrency | Direct p50/p95/p99 ms | Gateway p50/p95/p99 ms | Added p50/p95/p99 ms | Direct RPS | Gateway RPS |
|---:|---:|---:|---:|---:|---:|
| 1 | 15.576 / 15.809 / 15.989 | 15.983 / 16.320 / 16.685 | 0.407 / 0.511 / 0.696 | 63.96 | 62.27 |
| 16 | 16.186 / 17.263 / 17.726 | 16.495 / 17.622 / 18.233 | 0.309 / 0.358 / 0.507 | 939.57 | 919.66 |
| 64 | 16.320 / 18.364 / 19.553 | 31.975 / 34.125 / 35.024 | 15.655 / 15.761 / 15.470 | 3037.51 | 1688.76 |

The concurrency-64 result shows gateway saturation/queueing under this local configuration; it is not collapsed into a single latency claim. Streaming result:

- Direct total/TTFT: **46.219 / 46.217 ms**.
- Gateway total/TTFT: **46.446 / 46.445 ms**.
- Streaming TTFT overhead: **0.228 ms**.
- Gateway process VmHWM: **29,168 KB**.

The JSON artifact contains the raw measurements. These are local reproducible baselines, not universal performance claims or competitor comparisons.

## 6. Competitor matrix and unverified cells

The matrix remains conservative. The following competitor cells are explicitly **unverified** because the reviewed official documentation was insufficient for a defensible claim:

- **Portkey AI Gateway:** per-deployment health and capability filtering; release artifacts/checksums.
- **Kong AI Gateway:** semantic cache.
- **Bifrost:** multi-user RBAC/SSO; exact response cache; semantic cache.
- **Envoy AI Gateway:** multi-user RBAC/SSO; exact response cache; semantic cache; encrypted secrets at rest; MCP/tool gateway.

No unverified cell is treated as evidence that a competitor lacks the capability.

## 7. What was not done

- The PR was **not merged**.
- No Phase 1 work was started.
- No `.github/workflows/*` file was changed.
- `scripts/verify.sh` was not changed.
- No competitor performance benchmark was run.
- No universal performance, parity, or superiority claim was made.
- No release/tag was created.
- No production deployment, account change, or external destructive action was performed.

## 8. Next phase boundary

Phase 1 remains out of scope for this follow-up. Its planned work is encrypted-at-rest secrets, runtime integration of durable stores, and identity-model expansion, subject to a separate user instruction.
