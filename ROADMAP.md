# Roadmap — after the current v0.5.2 baseline

The current v0.5 baseline includes everything v0.4 shipped (verified-ready routing, session affinity, provider/credential P2C selection, capability-aware circuits, supervised recovery, bounded admission, hot reload, provider discovery/testing, embedded UI, Linux/Docker packaging) plus provider incident circuits, quota hints, TTFT telemetry, request hedging, an opt-in exact-match response cache, opt-in client API keys, context-window-aware pre-routing, and usage/cost accounting.

Future work should be added only as separately scoped, tested capabilities.

## Protocol expansion

- native OpenAI Responses API;
- Gemini native `generateContent` / streaming;
- Azure OpenAI deployment-specific semantics;
- AWS Bedrock;
- Google Vertex AI;
- embeddings/rerank only if required by real clients;
- broader Claude Code fixture coverage for evolving beta fields;
- safe additional reasoning/thinking and prompt-cache mappings where the destination protocol can faithfully represent them.

## Routing intelligence

- explicit named fallback chains/pools;
- ~~context-window-aware routing~~ (shipped in v0.5 with a conservative chars/4 estimator and unknown-window exemption);
- ~~proactive quota headroom pressure~~ (shipped in v0.5.1 when providers report limit + remaining + resource-specific reset headers; the router starts deprioritizing below 25% headroom and preserves last-resort availability);
- ~~in-flight local quota reservation~~ (shipped in v0.5.2: data-plane upstream attempts temporarily subtract one request plus a conservative token bound from effective headroom until fresh provider evidence or response-body completion);
- durable rolling-window RPM/TPM debt, hard throttling, and cross-process quota coordination are still open;
- ~~price-aware candidate ordering~~ (shipped in v0.5.1 as the opt-in `cost_aware` verified-ready strategy; priority tiers remain authoritative and incomplete request-cost estimates fall back safely);
- richer per-request route explanations;
- ~~carefully evaluated hedged requests~~ (shipped in v0.5 for the first attempt with a single partner, zero-health-signal abandonment, and route-context bounding);
- learned/semantic routing only if it proves measurable benefit without unacceptable latency, cost, or failure amplification.

## Operations and security

- encrypted-at-rest secret vault / OS keyring integration;
- explicit SSRF allow/deny policy for arbitrary provider/proxy URLs;
- first-class client keys/authentication and per-client limits;
- request RPM/TPM controls;
- CSRF/session protection for intentional non-loopback Web UI deployments;
- durable audit/request history;
- richer Prometheus metrics and OpenTelemetry;
- persistent cost/token accounting;
- shared/distributed health, breaker and affinity state if multi-instance HA is required.

## Release discipline

A later version number should be created only after:

1. the current `main` baseline passes the complete CI/runtime gate;
2. the new scope has explicit compatibility and failure-mode tests;
3. old contradictory docs/config examples/artifacts are removed rather than left beside the new behavior;
4. the resulting release package is independently smoke-tested.
