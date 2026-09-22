# Roadmap — v0.3 validation branch

The project remains **v0.3** until the user tests this exact package on the target Ubuntu/Claude Code/Chat2API setup. The items below are not new version numbers; they are gates for deciding what to do after that real-world validation.

## Current v0.3 validation gate

Must be proven on the user's machine:

- ULG starts from the prebuilt Linux binary
- Web UI opens
- Chat2API can be added and re-opened for editing without losing Base URL or credentials
- Detect Models works where Chat2API exposes a model endpoint
- manual model IDs work where discovery is unavailable
- model tests and Probe All show health correctly
- Claude Code can call ULG through `/v1/messages`
- a Chat2API/OpenAI-compatible model can complete a normal Claude Code text request
- common Claude Code tool-use/tool-result cycle works
- streaming works without malformed SSE
- one failed provider/model falls back to another eligible deployment
- repeated failures produce cooldown and later half-open recovery
- multiple keys on one provider rotate/fail over as expected
- hot provider edits do not interrupt unrelated in-flight requests

## Hardening backlog after real user validation

Only prioritize these if the v0.3 field test shows they are needed:

### Protocol conformance

- broader Claude Code fixture corpus across current beta headers and edge cases
- more reasoning/thinking cross-protocol mappings where the destination protocol can represent them safely
- more prompt-cache metadata mappings
- malformed/truncated/abrupt SSE corpus
- long tool-result and large-schema cases
- very long coding-session soak tests

### Provider coverage

- OpenAI Responses API as a native provider/ingress
- Gemini native `generateContent` / stream API
- Azure OpenAI deployment-specific semantics
- AWS Bedrock
- Google Vertex AI
- native local-engine adapters when compatibility endpoints are insufficient
- embeddings/rerank only if actually required

### Routing intelligence

- explicit named fallback chains/pools
- session-sticky routing
- context-window-aware routing
- quota/headroom-aware scoring
- cost/budget-aware scoring
- richer per-request route explanation
- safe hedged requests only for eligible operations

### Operations / security

- encrypted-at-rest secret vault
- explicit SSRF policy for arbitrary remote Base URLs
- CSRF/session protection for non-loopback UI deployments
- virtual client keys and per-client limits
- request RPM/TPM controls
- durable request/audit history
- richer Prometheus metrics and OpenTelemetry
- persistent cost/token accounting
- graceful multi-instance shared state if HA is ever required

## Release discipline

A later version number should be created only after:

1. the user has run this v0.3 package;
2. real defects from that test are recorded and fixed;
3. the supported behavior is re-tested;
4. the user explicitly agrees the v0.3 baseline is good enough to advance.
