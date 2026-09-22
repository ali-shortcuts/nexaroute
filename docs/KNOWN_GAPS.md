# Known gaps — v0.3

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

- reasoning/thinking formats differ across providers;
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

Not implemented as a full internet-facing control plane:

- built-in TLS
- RBAC/multi-user accounts
- CSRF session framework
- enterprise SSO

## Model discovery

Discovery parses several common result shapes, but model-list APIs are not standardized. Providers that omit or customize discovery may require manual model IDs.

## State and HA

Runtime and health state are single-process/in-memory. Provider configuration is persisted atomically to JSON with one `.bak` last-known-good copy. Distributed state, Redis/Postgres coordination, and multi-node breaker synchronization are not implemented.


## Probe cadence and model quality

Health probing is periodic and event-driven, not a sub-second broadcast to every configured model. The interval is configurable (minimum 1 second), and real traffic also updates health immediately. This avoids turning health checks into a quota/rate-limit attack against the configured providers.

Micro-probes measure availability and latency. They do not measure model intelligence/answer quality. Model strength is expressed through configured deployment `priority` and `weight`; automatic quality benchmarking is outside the current v0.3 scope.
