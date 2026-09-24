# Known gaps — v0.4

These are explicit boundaries of the current code, not hidden assumptions.

## Protocol scope

Implemented runtime protocol classes are:

- OpenAI-compatible Chat Completions
- Anthropic-compatible Messages

Not implemented as native protocol classes:

- OpenAI Responses API
- Gemini native API
- Bedrock
- Vertex AI
- Azure-specific deployment semantics
- embeddings/rerank

## Cross-protocol fidelity

Common Claude Code text/tool/stream flows are implemented and regression-tested, including a mocked two-turn tool round trip and parallel streamed tool calls. That does not mean every future or provider-specific Anthropic/OpenAI extension has a lossless equivalent.

In particular:

- reasoning/thinking formats differ across providers. NexaRoute does not silently translate those controls: when a request explicitly asks for reasoning/thinking, routing is constrained to the matching native ingress protocol;
- prompt-cache metadata does not always have an OpenAI-compatible equivalent;
- provider-specific beta fields are safest on native Anthropic passthrough;
- a committed broken stream is not transparently resumed on another provider.

## Token counting

`POST /v1/messages/count_tokens` first tries the native token-count endpoint of an eligible Anthropic-compatible upstream. If no native count can be obtained, NexaRoute returns a local estimate and explicitly includes `"estimated": true`.

The estimate is not a substitute for model-specific tokenization when exact billing/context calculations matter.

## Secrets

Implemented:

- literal secret
- environment-variable reference
- multiple credentials per provider
- credential rotation/failover/cooldown
- rewritten config mode `0600`
- secret-preserving provider edit

Not implemented:

- encrypted-at-rest secret vault / OS keyring integration

The Web UI can reveal a resolved provider credential to an authorized local/admin user because edit visibility was an explicit project requirement. Do not expose that admin surface to untrusted networks.

## Admin security

Implemented:

- loopback-only admin mode by default
- optional admin API key
- constant-time admin-key comparison
- session-only browser storage for the entered admin key
- optional data-plane client API keys (`client_auth`) with constant-time digest comparison and an optional per-key RPM ceiling

Not implemented as a full internet-facing control plane:

- per-client quotas beyond the RPM ceiling, key lifecycle UI (create/revoke from the dashboard), hashed-at-rest client keys;
- built-in TLS
- RBAC/multi-user accounts
- CSRF session framework
- enterprise SSO

## Model discovery

Discovery parses several common result shapes, but model-list APIs are not standardized. Providers that omit or customize discovery may require manual model IDs.

## State and HA

Runtime and health state are single-process/in-memory. Provider configuration is persisted atomically to the active JSON file without creating backup copies. Distributed state, Redis/Postgres coordination, and multi-node breaker synchronization are not implemented.


## Response cache boundaries

The exact-match response cache is opt-in and deliberately narrow: non-streaming, deterministic-sampling requests only, bounded by entries/bytes/TTL, invalidated on every config swap. It is not a semantic cache; two requests that differ by one byte are different keys. Streaming responses are never cached, and cached entries are replayed with the deployment label that produced them so the dashboard stays honest about provenance.

## Probe cadence and model quality

Health probing is selective and event-driven. Startup establishes readiness, new/unverified deployments are probed, and failed deployments move into dedicated recovery loops. Successful real Claude traffic refreshes a deployment's ready-health lease, so actively used models are not needlessly synthetic-probed. A healthy deployment that remains idle past `probe.ready_lease_seconds` is micro-probed before its health proof is trusted indefinitely. The sweep interval remains configurable (minimum 1 second) without turning health checks into a quota/rate-limit attack.

Micro-probes measure availability and latency. They do not measure model intelligence/answer quality. Model strength is expressed through configured deployment `priority` and `weight`; automatic quality benchmarking is outside the current v0.4 scope.


## Routing boundaries after Ready Mesh

Implemented routing intelligence is deterministic and observable: session affinity, capability filtering, priority/weight policy, live concurrency pressure, latency/failure evidence, provider-level P2C selection, credential-level P2C selection, scoped capability circuits, and supervised recovery.

Not implemented yet:

- complete provider-reported TPM/RPM accounting and predictive quota-reset scheduling (common remaining/reset headers are observed, but provider semantics vary);
- cost-aware routing based on current provider pricing/billing and exact cross-provider usage accounting;
- shared/distributed affinity and breaker state across multiple NexaRoute processes;
- an online learned semantic router that sends every prompt through another model/encoder.

The last item is deliberate for the current data plane: a learned router would add latency, cost and a new failure mode. Model/task specialization should currently be expressed with aliases plus explicit capability metadata until a separately evaluated routing model can prove a measurable benefit.
