# Roadmap — after the current v0.5 baseline

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
- proactive request scheduling against provider-reported RPM/TPM budgets (v0.5 observes remaining quotas and reset times and deprioritizes exhausted providers; predictive throttling against a known per-minute budget is still open);
- price-aware candidate ordering on top of the v0.5 usage/pricing accounting (accounting shipped; routing remains cost-neutral);
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
