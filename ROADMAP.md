# Roadmap — after the current v0.3 baseline

The current v0.3 baseline already includes verified-ready routing, session affinity, provider/credential P2C selection, capability-aware circuits, supervised recovery, bounded admission, hot reload, provider discovery/testing, embedded UI, and Linux/Docker packaging.

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

- cache/revision-invalidate the virtual catch-all candidate scan so `auto` becomes O(revision) instead of O(registry) on large fleets (currently ~1.6 ms at 1020 deployments, measured);

- explicit named fallback chains/pools;
- context-window-aware routing;
- provider-reported RPM/TPM headroom and reset-aware routing;
- cost/budget-aware routing;
- richer per-request route explanations;
- carefully evaluated hedged requests only for operations where duplicate work is safe;
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
