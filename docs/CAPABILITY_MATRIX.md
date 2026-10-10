# Capability matrix — Phase 0 baseline, updated through Phase 2 WP2b

**Baseline snapshot:** NexaRoute `v0.16.3` (`f30fc8b`, 2026-10-09). This matrix preserves Phase 0's dated competitor-evidence baseline; NexaRoute capability cells and prioritized gaps are updated as subsequent phases close. Phase 1 now includes encrypted secrets, built-in TLS/mTLS, browser CSRF checks, and the read-only configuration preflight CLI.

## Reading the table

- **done** means the capability is implemented and has a repository reference or test reference.
- **partial** means a bounded subset exists, or the capability is opt-in/advisory rather than a complete enterprise implementation.
- **missing** means the current NexaRoute code does not claim the capability.
- **unverified** means the reviewed official documentation was not a deep, capability-specific page sufficient to support the cell. It is not a claim that the competitor lacks the capability.

Generic landing pages do not count as evidence. Competitor cells marked verified link to official pages whose titles and content are capability-specific.

## Matrix

| Capability | NexaRoute | LiteLLM | Portkey AI Gateway | Kong AI Gateway | Bifrost | Envoy AI Gateway |
|---|---|---|---|---|---|---|
| OpenAI/Anthropic-compatible ingress | **done** — `internal/httpapi`, `docs/COMPATIBILITY.md` | **verified** — [supported endpoints](https://docs.litellm.ai/docs/supported_endpoints) | **unverified** — generic gateway landing page | **unverified** — generic AI Gateway landing page | **verified** — [gateway streaming](https://docs.getbifrost.ai/quickstart/gateway/streaming) | **unverified** — generic AI Gateway landing page |
| Per-deployment health and capability filtering | **done** — `internal/health`, `internal/router` | **unverified** — routing page does not explicitly establish this combined capability | **unverified** — generic gateway landing page | **unverified** — generic AI Gateway landing page | **unverified** — routing governance page does not explicitly establish this combined capability | **unverified** — traffic page does not explicitly establish this combined capability |
| Deterministic routing and explainable fallback | **done** — `internal/router`, decision tests | **verified** — [routing strategies](https://docs.litellm.ai/docs/routing) | **unverified** — generic gateway landing page | **unverified** — generic AI Gateway landing page | **verified** — [retries and fallbacks](https://docs.getbifrost.ai/docs/features/retries-and-fallbacks) | **unverified** — generic AI Gateway landing page |
| Virtual keys / consumer authentication | **done** — `internal/httpapi/clientauth.go`, `client_keys.go`; tenant/project/team policy intersection is fail-closed and tested | **verified** — [virtual keys](https://docs.litellm.ai/docs/proxy/virtual_keys) | **verified** — [virtual keys](https://portkey.ai/docs/product/ai-gateway/virtual-keys) | **unverified** — generic security page | **unverified** — governance page does not explicitly establish virtual keys | **unverified** — generic security page |
| Multi-user RBAC / SSO | **partial** — server-side permission classification for Admin API route families, default-deny unknown routes, and break-glass owner only; user identities and OIDC sessions are not implemented (`internal/authz`, `internal/httpapi`) | **verified** — [security best practices](https://docs.litellm.ai/docs/proxy/security_best_practices) | **unverified** — generic access-control/gateway page | **unverified** — generic AI Gateway page | **unverified** — generic security page | **unverified** — generic AI Gateway page |
| Budgets, spend tracking and cost routing | **partial** — budget ledger in `internal/budget`; bounded/advisory cost routing | **unverified** — linked page does not explicitly establish the complete combined capability | **unverified** — generic gateway page | **unverified** — rate limiting is not evidence of spend/cost routing | **unverified** — generic governance page | **unverified** — rate limiting is not evidence of spend/cost routing |
| Exact response cache | **done** — `internal/cache`, cache tests | **verified** — [caching](https://docs.litellm.ai/docs/proxy/caching) | **verified** — [caching](https://portkey.ai/docs/product/ai-gateway/caching) | **unverified** — generic AI Gateway page | **unverified** — generic feature landing page | **unverified** — generic AI Gateway page |
| Semantic cache | **missing** — `docs/KNOWN_GAPS.md` | **verified** — [semantic caching](https://docs.litellm.ai/docs/proxy/caching) | **unverified** — linked cache page is not retained as explicit semantic-cache evidence here | **unverified** — generic AI Gateway page | **unverified** — generic feature landing page | **unverified** — generic AI Gateway page |
| Streaming and tool-call handling | **done** — `internal/translate`, tool fidelity tests | **unverified** — supported-endpoints page does not explicitly establish tool-call fidelity | **unverified** — generic gateway page | **unverified** — generic AI Gateway page | **verified** — [streaming](https://docs.getbifrost.ai/quickstart/gateway/streaming) | **unverified** — generic AI Gateway page |
| OpenTelemetry / Prometheus observability | **partial** — Prometheus-style metrics and W3C propagation; OTLP exporter not claimed | **verified** — [observability](https://docs.litellm.ai/docs/proxy/observability) | **unverified** — generic gateway page | **unverified** — generic AI Gateway page | **verified** — [default observability](https://docs.getbifrost.ai/features/observability/default) | **unverified** — generic AI Gateway page |
| Durable control plane / HA state | **partial** — versioned store contracts in `internal/controlplane`; adapters not runtime default | **unverified** — deployment page does not explicitly establish durable HA state | **unverified** — generic deployment page | **unverified** — generic AI Gateway page | **unverified** — generic overview page | **unverified** — generic AI Gateway page |
| Encrypted secrets at rest | **done** — AES-256-GCM secret envelopes, field-bound AAD, atomic plaintext migration with encrypted backup, fail-closed key errors, crash-safe auto-managed key rotation/recovery, redacted Admin/API/log/metric surfaces; `internal/secrets`, `internal/config`, `cmd/gateway` tests and `nexaroute secrets` CLI. See [`docs/SECURITY.md`](SECURITY.md) | **verified** — [secret managers](https://docs.litellm.ai/docs/secret_managers/overview) | **unverified** — generic security page | **unverified** — generic security page | **unverified** — generic security page | **unverified** — generic AI Gateway page |
| Built-in TLS / mTLS | **done** — optional TLS 1.2+ on shared listener, reloadable certificate files, route-scoped client certificate enforcement (`internal/transport`, `internal/httpapi`) | **unverified** — security page does not explicitly establish built-in TLS/mTLS | **unverified** — generic security page | **unverified** — generic security page | **unverified** — generic security page | **unverified** — generic security page |
| MCP/tool gateway | **missing** — roadmap item | **verified** — [MCP support](https://docs.litellm.ai/docs/completion/mcp) | **unverified** — generic integrations page | **unverified** — generic AI Gateway page | **unverified** — generic feature page | **unverified** — generic AI Gateway page |
| Asynchronous video gateway | **partial** — opt-in `/v1/video/` runtime; required bearer-token environment variable; bounded single-process queue; atomic local JSON jobs with restart recovery; output persistence and fail-closed cost admission; fake provider is development-only; shared-listener TLS is available. No real external provider adapter is claimed verified. | **unverified** — no retained capability-specific evidence | **unverified** — no retained capability-specific evidence | **unverified** — no retained capability-specific evidence | **unverified** — no retained capability-specific evidence | **unverified** — no retained capability-specific evidence |
| Single-binary zero-dependency default | **done** — `README.md`, `go.mod`, release workflow | **unverified** — no capability-specific deployment-model evidence retained here | **unverified** — gateway deployment model not established by a capability-specific page | **unverified** — deployment model not established by a capability-specific page | **unverified** — official binary/npx fact requires a capability-specific deployment page not retained here | **unverified** — deployment model not established by a capability-specific page |
| Release artifacts/checksums | **done** — `.github/workflows/release.yml`, v0.16.3 assets | **verified** — [signed Docker images](https://docs.litellm.ai/docs/proxy/docker_image_security) | **unverified** — no capability-specific official release-process page retained | **unverified** — generic AI Gateway page | **verified** — [Bifrost releases](https://github.com/maximhq/bifrost/releases) | **verified** — [Envoy releases](https://github.com/envoyproxy/ai-gateway/releases) |

## Verified vs unverified cell counts

Counts cover the **16 competitor capability cells per competitor**. `verified` means the cell has a retained deep, capability-specific official link. `unverified` is conservative and does not mean the capability is absent.

| Competitor | Verified cells | Unverified cells | Total |
|---|---:|---:|---:|
| LiteLLM | 10 | 6 | 16 |
| Portkey AI Gateway | 2 | 14 | 16 |
| Kong AI Gateway | 0 | 16 | 16 |
| Bifrost | 5 | 11 | 16 |
| Envoy AI Gateway | 1 | 15 | 16 |

## Phase 0 benchmark and baseline

The reproducible benchmark is [scripts/bench/gateway_overhead.py](../scripts/bench/gateway_overhead.py). It compiles a local Go `net/http` mock upstream with fixed 15 ms response latency, `TCP_NODELAY`, and one write per non-streaming response. The Python client uses persistent HTTP/1.1 connections, two warm-up requests per connection, and 1,000 measured requests. It compares the explicit `adaptive_no_probe` configuration with the **default** `ready_mesh_default` configuration (`strategy=ready_mesh`, default `max_attempts=4`, probes enabled and run on start). It reports concurrency 1/16/64, provider caps 32/128/256, and streaming TTFT. Every measured response must be HTTP 200.

The old 100-request Python mock result was discarded because its separate header/body writes produced a delayed-ACK artifact; it must not be used as a gateway-latency claim.

## Prioritized gap list from Phase 0

1. **P0 security:** multi-user RBAC/SSO and cookie-backed Admin identity remain incomplete; Admin API route permission mapping and default-deny unknown paths are implemented. Strict provider egress policy, OS keyring integration, and externally managed key-rotation workflows remain future work; built-in TLS/mTLS and browser CSRF checks are implemented.
2. **P0 operations:** make durable store adapters runtime-integrated and define multi-replica consistency and tested backup/restore; `config validate/diff/dry-run` is implemented as a local read-only CLI.
3. **P1 economics:** invoice-accurate price book, hard budget enforcement and durable cost/usage settlement across processes.
4. **P1 protocol breadth:** native Bedrock, Vertex, Azure semantics, embeddings, rerank, image and audio endpoints.
5. **P1 intelligence:** semantic cache, quality-aware routing, canary/shadow/weighted rollout and explainable hedging budgets.
6. **P1 platform:** MCP gateway with deny-by-default tool policy and per-tenant audit.
7. **P2 observability:** OTLP exporter, GenAI semantic conventions, cardinality limits and packaged Grafana assets.
8. **P2 quality:** continue raising overall coverage from the measured baseline and close low-coverage packages before adding broad feature surface.

## Baseline limitations

The matrix intentionally does not infer missing competitor capabilities from generic landing pages. It records only deep-link evidence that was retained and marks the rest `unverified`.
