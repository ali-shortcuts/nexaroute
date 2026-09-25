# Known gaps — v0.6.1-beta.2

These are explicit boundaries of the current code, not hidden assumptions.

## Protocol scope

Implemented runtime protocol classes are:

- OpenAI-compatible Chat Completions
- Anthropic-compatible Messages
- Stateless OpenAI Responses subset
- Gemini GenerateContent upstream (not Gemini ingress)

Not implemented as native protocol classes:

- Stateful/background Responses, hosted tools and response retrieval/deletion
- Bedrock
- Vertex AI
- Azure-specific deployment semantics
- embeddings/rerank

## Cross-protocol fidelity

Common Claude Code text/tool/stream flows are implemented and regression-tested, including a mocked two-turn tool round trip and parallel streamed tool calls. That does not mean every future or provider-specific Anthropic/OpenAI extension has a lossless equivalent.

In particular:

- reasoning/thinking formats differ across providers. native-protocol candidates are preferred for Chat/Messages reasoning requests, but the existing router can relax that preference when no native candidate is healthy. Foreign reasoning has no valid Anthropic signature and is not emitted as Anthropic thinking. Exact reasoning parity is not guaranteed;
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

Micro-probes measure availability and latency. They do not measure model intelligence/answer quality. Model strength is expressed through configured deployment `priority` and `weight`; automatic quality benchmarking is outside the current v0.5.2 scope.


## Routing boundaries after Ready Mesh

Implemented routing intelligence is deterministic and observable: session affinity, capability filtering, priority/weight policy, live concurrency pressure, latency/failure evidence, provider-level P2C selection, credential-level P2C selection, scoped capability circuits, and supervised recovery.

Not implemented yet:

- proactive quota headroom pressure and in-flight local reservations are implemented when provider quota evidence exists, but reservations are deliberately advisory: NexaRoute does not yet persist rolling-window quota debt after a completed response without fresh headers, hard-throttle traffic from inferred quota, or coordinate reservations across multiple gateway processes;
- `cost_aware` routing uses configured base input/output prices and a bounded request estimate, but it is not invoice-perfect billing optimization (cache discounts, credits, batch pricing, taxes, and provider-specific billing rules remain outside the router);
- shared/distributed affinity and breaker state across multiple NexaRoute processes;
- an online learned semantic router that sends every prompt through another model/encoder.

The last item is deliberate for the current data plane: a learned router would add latency, cost and a new failure mode. Model/task specialization should currently be expressed with aliases plus explicit capability metadata until a separately evaluated routing model can prove a measurable benefit.

## Universal Compatibility Engine boundaries (v0.6)

Implemented but bounded by design:

- Level B probing sends OpenAI-shaped or Anthropic-shaped probe payloads per
  provider family. Gemini upstreams are served through the canonical path and
  learn from real traffic and error classification; a dedicated
  Gemini-shaped probe suite is future work.
- The repair engine mutates OpenAI-style and Anthropic-style payload keys.
  Gemini-native payloads carry parameters inside `generationConfig`, so
  capability rejections there fail over instead of being repaired inline.
- Runtime learning upgrades UNKNOWN to SUPPORTED conservatively and never
  flips verified UNSUPPORTED back; only a contract reset (identity change or
  `/admin/api/compat/reset`) re-opens the question.
- The capability cache is in-memory, matching the single-process state model
  described above; multi-process deployments re-probe after restart.

## Beta audit boundaries

- Responses ingress uses serial failover; Chat/Messages hedging and response caching are not wired into this ingress.
- Responses client streaming retains output for the final protocol object, bounded to 8 MiB and 4096 items. Exceeding the bound ends the stream with an error.
- `previous_response_id`, `store=true`, and non-function hosted tools are rejected, rather than silently discarding conversation state or requested tools.
- The canonical formats represent a subset. Audio/video, encrypted reasoning, Gemini thought signatures, provider-specific extension fields and arbitrary future tool types are not universally preserved.
- Linux amd64 was runtime-tested locally. Linux arm64, macOS amd64/arm64 and Windows amd64 were cross-compiled; native runtime certification on those targets is pending.
- No live paid-provider credentials were supplied for this audit. Mocked integration coverage does not establish current Claude Code, Codex CLI or every provider/model compatibility.
- No comparative throughput benchmark, penetration test, long-duration production soak or distributed deployment certification was performed.

## Transport and bounded parsing (beta.2)

Provider redirects may only stay on the exact same scheme/host/port origin.
Configure the final URL explicitly for services that redirect elsewhere.
Canonical SSE frames are limited to 16 MiB and 65536 data lines per event.
Responses native requests use store=false; this does not make a claim about a
provider's retention policies. File/reference input and unknown content types
are not implemented and now fail explicitly.
