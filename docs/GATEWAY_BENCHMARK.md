# AI Gateway Benchmark: NexaRoute Context

## Measured routing hot-path benchmark (2026-09-29)

Loopback rig, no external providers. Method: `go test ./internal/httpapi/ -run NONE
-bench 'BenchmarkRoutingHotPath|BenchmarkDirectUpstreamCall|BenchmarkGatewayChatCompletionsE2E'
-benchtime 5000x -count=5 -benchmem`. Benchmarks live in
`internal/httpapi/routing_bench_test.go`. Upstream is an `httptest` stub
returning a canned OpenAI chat completion; the gateway rig uses two
`openai_compatible` providers x three models. Environment:
`linux/amd64, AMD EPYC 9V74 (4 test threads)`.

| Benchmark | Median (5 x 5000 ops) | Range | Allocs |
|---|---|---|---|
| `BenchmarkRoutingHotPath` (pure routing decision: candidate filter + score + order, no I/O) | ~1.6 µs/op | 1397–1842 ns/op | 1628 B/op, 5 allocs/op |
| `BenchmarkDirectUpstreamCall` (direct POST to loopback upstream, no gateway) | ~69 µs/op | 60196–70709 ns/op | ~7250 B/op, 84 allocs/op |
| `BenchmarkGatewayChatCompletionsE2E` (full gateway data plane: admission + routing + upstream round-trip + validation/rewrite) | ~168 µs/op | 165711–168744 ns/op | ~40415 B/op, 428 allocs/op |

Raw output (median run of each group shown; full runs all passed):

```text
BenchmarkRoutingHotPath-4              5000   1557 ns/op   1628 B/op   5 allocs/op
BenchmarkDirectUpstreamCall-4          5000  68996 ns/op   7246 B/op  84 allocs/op
BenchmarkGatewayChatCompletionsE2E-4   5000 168271 ns/op  40413 B/op 428 allocs/op
```

Reading: on loopback the gateway adds roughly ~99 µs per non-streaming chat
request over a direct upstream call (~168 µs vs ~69 µs). The pure routing
decision is ~1.6 µs — about 1% of that overhead — so the gateway cost is
dominated by the extra HTTP hop, JSON decode/validate/rewrite, and
observability bookkeeping, not by candidate selection. Against real
upstreams (10–1000+ ms model latency) the relative overhead is negligible.
Re-run with `go test ./internal/httpapi/ -run NONE -bench
'BenchmarkRoutingHotPath|BenchmarkDirectUpstreamCall|BenchmarkGatewayChatCompletionsE2E'
-benchmem .` to reproduce on your hardware.

---

> **Scope:** a concise product-architecture comparison of NexaRoute with four widely used AI gateways. This is not a feature-scorecard or pricing comparison; it highlights operational tradeoffs relevant to multi-provider LLM routing. Sources were reviewed on 2026-09-28.

| Gateway | Routing and resilience model | Observability and deployment | Best fit | Material tradeoff |
|---|---|---|---|---|
| **NexaRoute** | Self-hosted, deterministic deployment-level routing with bounded pre-stream failover, provider/model health isolation, supervised five-probe recovery, and fail-closed mid-stream behavior. | Local admin snapshot plus authenticated bounded live event stream; privacy-safe event fields; one Go binary with embedded UI. | Operators who need explainable, locally controlled OpenAI/Anthropic/Responses routing and predictable failure semantics. | Fewer hosted-control-plane integrations and provider catalog conveniences than managed gateways. |
| [LiteLLM Proxy](https://docs.litellm.ai/docs/routing) | Model-group aliases, weighted/RPM/TPM/latency/least-busy/cost strategies, retries, cooldowns, and ordered fallbacks. | Open-source self-hosted proxy with callback-based observability; production multi-instance deployment commonly adds PostgreSQL and Redis. | Teams wanting a broad OpenAI-compatible self-hosted proxy and a large integration ecosystem. | Operational complexity rises for horizontally scaled production deployments. |
| [Portkey AI Gateway](https://github.com/Portkey-AI/gateway) | Config-driven weighted and conditional routing, provider/model fallbacks, and retries with exponential backoff. | Hosted, local open-source, or enterprise VPC/Kubernetes deployment; OpenTelemetry tracing and hosted analytics. | Production apps valuing provider portability, policy configuration, and enterprise private deployment. | Private/VPC mode requires more control-plane and Kubernetes setup; some observability is hosted-app oriented. |
| [OpenRouter](https://openrouter.ai/docs/guides/routing/provider-selection) | Managed provider routing with price/availability weighting, explicit provider preferences, and opt-in ordered model fallbacks. It documents that transparent mid-stream failover is impossible after output begins. | Managed API; generation metadata and optional broadcast integrations for tracing, usage, and cost. | Applications that prefer a unified managed model marketplace and low gateway-operating burden. | Default routing is intentionally dynamic; reproducibility and privacy require explicit provider/data-policy constraints. |
| [Helicone AI Gateway](https://docs.helicone.ai/gateway/overview) | Cheapest-available routing by default, BYOK preference, provider chains, automatic failover, and transient-error retries. | Hosted gateway with optional Docker Compose/Kubernetes/cloud self-hosting and a unified usage/cost/performance dashboard. | Teams that prioritize cross-provider analytics plus hosted or optional self-hosted routing. | Automatic cost/availability optimization reduces direct provider-selection control unless chains or provider locks are configured. |

## Source notes

- **LiteLLM:** [routing](https://docs.litellm.ai/docs/routing), [reliability](https://docs.litellm.ai/docs/proxy/reliability), [logging](https://docs.litellm.ai/docs/proxy/logging), [deployment](https://docs.litellm.ai/docs/proxy/deploy).
- **Portkey:** [Gateway repository](https://github.com/Portkey-AI/gateway), [automatic retries](https://docs.portkey.ai/docs/product/ai-gateway/automatic-retries), [traces](https://docs.portkey.ai/docs/aigw/product/observability/traces), [hybrid deployment](https://docs.portkey.ai/docs/enterprise/hybrid2).
- **OpenRouter:** [provider selection](https://openrouter.ai/docs/guides/routing/provider-selection), [model fallbacks](https://openrouter.ai/docs/guides/routing/model-fallbacks), [errors and debugging](https://openrouter.ai/docs/api_reference/errors-and-debugging), [broadcast](https://openrouter.ai/docs/guides/features/broadcast).
- **Helicone:** [gateway overview](https://docs.helicone.ai/gateway/overview), [provider routing](https://docs.helicone.ai/gateway/provider-routing), [error handling](https://docs.helicone.ai/gateway/concepts/error-handling), [self-hosting](https://docs.helicone.ai/getting-started/self-host/overview).

## Design takeaway

The surrounding market validates a common baseline: multi-provider aliases, retries/fallbacks, and observability. NexaRoute differentiates by keeping the data plane **local, bounded, and explainable**: routing separates capability, health, and provider evidence; attempts are capped; recovery is explicitly supervised; and no gateway attempts to splice a second provider into a client-visible stream.
