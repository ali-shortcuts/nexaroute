# Known gaps

These are explicit boundaries of the current code, not hidden assumptions.

## Protocol scope

Implemented runtime protocol classes are:

- OpenAI-compatible Chat Completions
- Anthropic-compatible Messages
- OpenAI Responses (explicit `/v1/responses` path)
- Gemini upstreams through the canonical adapter

Not implemented as native protocol classes:

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
- credentials stripped on all admin read surfaces (never revealed after save, even to admin GET / snapshot / metrics)

The `api_key_env` NAME (not its value) is intentionally visible on admin-only
surfaces (`GET /admin/api/providers*`): operators need to know which
environment variable feeds a provider. It never appears on non-admin surfaces
(`/v1/*`, `/healthz`, `/readyz`, `/metrics`), and no log or error line ever
pairs it with a secret value. A guard test
(`TestAPIKeyEnvNameIsAdminOnlyAndNeverPairedWithValue`) locks this in.

Not implemented:

- encrypted-at-rest secret vault / OS keyring integration

Saved provider keys are now write-only, including literal, pool, and environment keys. Headers and proxy URLs are also write-only. Editing a credential field replaces the whole credential set; individual saved pool keys cannot be revealed. Do not store credentials in base URLs or other public metadata. Do not expose the admin surface to untrusted networks.

## URL validation (SSRF/credential enforcement)

`Config.Validate` (and therefore `ValidateProviderConfig`) now enforces what
was previously docs-only guidance:

- `base_url` and `proxy_url` must not embed `user:pass@` credentials;
- cloud-metadata targets (`169.254.169.254`, `metadata.google.internal`, …)
  are rejected for both `base_url` and `proxy_url`;
- non-loopback private-IP proxy targets are rejected (loopback stays allowed
  for local development and tests; LAN `base_url` targets such as on-prem
  inference servers remain allowed).

Transport-layer dial guards remain the second line of defense; validation is
the first.

## Panic policy

The HTTP middleware is fail-closed: any handler panic other than
`http.ErrAbortHandler` is contained as HTTP 500 plus an `internal_panic` bus
event (visible in the console and the failure radar). `http.ErrAbortHandler`
is deliberately re-raised per the `net/http` contract — the server owns that
response path. A regression test (`TestMiddlewarePanicIsFailClosed`) pins
this behavior.

## Config strictness

Silent defaults are now surfaced: `LoadWithWarnings` reports empty
routing/probe sections, `probe.max_tokens==0`, empty `routing.strategy`,
missing `probe.interval_seconds`, and legacy `routing.public_model`
auto-migration. Warnings go to stderr on every load; `--strict-config`
(or `NEXAROUTE_STRICT_CONFIG=1`) turns them into hard errors.

## Frontend hardening

The dashboard no longer trusts element presence: the `$` helper returns a
detached absorbing stub (with a one-time console warning) for missing
selectors, and every render section is isolated so one bad panel cannot kill
the render loop. CLI snippets escape all interpolated values (`veModel`,
`veList`, origin) before `innerHTML` insertion. The live-event consumer uses
capped exponential backoff with jitter, an error counter with operator
toasts, and a surfaced malformed-frame counter
(`window.NexaRoute.sseStats()`), instead of a fixed silent 1500 ms retry.

## Test-coverage posture

Audit-driven coverage work (see the `audit_coverage_test.go` files):
`internal/core` 30.4% → 100%, `internal/route` 56.3% → 91.4%,
`internal/feature` 59.4% → 71%+, `internal/compat` 51.6% → 63%+,
`cmd/gateway` 12.9% → 20%+ (dashboard-URL, strict-config, UI-fallback
branches). Stress/soak checks (`NEXAROUTE_STRESS=1`, `NEXAROUTE_SOAK=1`)
remain opt-in and are intended for nightly CI rather than every push;
workflow files are owned by a separate process and were not touched here.

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

Micro-probes measure availability and latency. They do not measure model intelligence/answer quality. Model strength is expressed through configured deployment `priority` and `weight`; automatic quality benchmarking is outside current scope.


## Routing boundaries

Implemented routing intelligence is deterministic and observable: session affinity, capability filtering, priority/weight policy, live concurrency pressure, latency/failure evidence, provider-level P2C selection, credential-level P2C selection, scoped capability circuits, and supervised recovery.

Not implemented yet:

- proactive quota headroom pressure and in-flight local reservations are implemented when provider quota evidence exists, but reservations are deliberately advisory: NexaRoute does not yet persist rolling-window quota debt after a completed response without fresh headers, hard-throttle traffic from inferred quota, or coordinate reservations across multiple gateway processes;
- `cost_aware` routing uses configured base input/output prices and a bounded request estimate, but it is not invoice-perfect billing optimization (cache discounts, credits, batch pricing, taxes, and provider-specific billing rules remain outside the router);
- shared/distributed affinity and breaker state across multiple NexaRoute processes;
- an online learned semantic router that sends every prompt through another model/encoder.

The last item is deliberate for the current data plane: a learned router would add latency, cost and a new failure mode. Model/task specialization should currently be expressed with aliases plus explicit capability metadata until a separately evaluated routing model can prove a measurable benefit.

## Compatibility engine boundaries

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

## Model intelligence and evaluation boundaries

The evaluation plane is an admin-only observation surface and is
explicitly bounded:

- evaluation is **offline replay**: the admin endpoint accepts recorded
  artifacts and never prompts a model, calls an upstream or spends provider
  quota. Live, in-band quality measurement is not implemented;
- no judge implementation ships. Deterministic evaluators decide every case; the
  judge path exists and is proven never to override a deterministic verdict, but
  the HTTP surface cannot enable it;
- the suite catalog is built-in and versioned. Operator-defined suites and
  custom case packs are not configurable yet;
- `production_telemetry` provenance exists as a validated constructor
  (`FromTelemetry`) but nothing ingests telemetry automatically; operational
  values are only produced from a completed evaluation run (availability,
  failure rate, and latency/TTFT when a target is configured);
- scorecards are single-process state. `evaluation.state_path` gives one process
  durable runs/scorecards, but there is no shared/distributed scorecard store,
  no multi-node coordination and no history beyond the bounded version ring;
- scorecard values are evidence records, not routing inputs. The evaluation plane does not
  order candidates, change weights/priorities, gate failover or alter health by
  scorecard content, and a structural guard test keeps the dependency direction
  that way.
