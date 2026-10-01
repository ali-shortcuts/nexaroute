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

Not implemented:

- encrypted-at-rest secret vault / OS keyring integration

Saved provider keys are now write-only, including literal, pool, and environment keys. Headers and proxy URLs are also write-only. Editing a credential field replaces the whole credential set; individual saved pool keys cannot be revealed. Do not store credentials in base URLs or other public metadata. Do not expose the admin surface to untrusted networks.

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

## Audit follow-up record (top-15 audit #57, recorded via #178)

Status labels used below: `open` means an unresolved defect/boundary with no
fix on `main`; `intentional boundary` means the behavior is deliberate and is
not a bug; `verified fixed` would require a merged fix plus test evidence and
is not claimed for any item here. Nothing below is marked fixed merely because
it is documented. Evidence snapshot date: 2026-09-30. Parent audit: #57.
Audit follow-up set: #169, #170, #171, #172, #173, #174, #175, #176, #177.
Full re-verification report: `docs/reports/audit-top15-issue57.md`.

### Panic boundary contract — `intentional boundary`

`internal/httpapi/server.go` recovers handler panics, emits an `internal_panic`
bus event plus a `request_id=... handler_panic` log line, and returns HTTP 500
(`"internal gateway error"`) — except for `http.ErrAbortHandler`, which is
re-panicked as required by the `net/http` contract. Converting the abort path
to a 500 would break connection handling, so the re-panic is deliberate policy,
not a bug.

- Impact: ordinary handler panics are fail-closed 500s with an audit event; the
  abort path never produces a response by design.
- Mitigation/workaround: none required; operators monitor `internal_panic`
  events and `handler_panic` log lines.
- Owner: audit finding F3; follow-up set #169–#177.

### Provider/proxy URL credential and egress/SSRF policy — `partially fixed`

Finding F4, tracked by #125. `ValidateProviderConfig` now rejects embedded
`user:pass@` URL credentials in provider `base_url`, plus fragments and
malformed/hostless endpoint URLs. Proxy URL userinfo remains accepted for
backwards compatibility with existing authenticated proxy configurations and
is write-only on admin read surfaces. Private-IP and cloud-metadata targets
remain intentionally permitted for localhost/self-hosted providers and are
still an operator trust-boundary concern.

- Impact: a proxy can still intentionally point at an internal target when an
  authorized operator configures it.
- Mitigation/workaround: store credentials only in the dedicated credential
  fields or environment references (write-only, never revealed on admin read
  surfaces); do not embed `user:pass@` in URLs; do not expose the admin surface
  to untrusted networks.
- Owner: #125 (finding F4); follow-up set #169–#177.

### Stress/soak tests opt-in — `intentional boundary` (CI budget)

The bounded stress checks (`NEXAROUTE_STRESS=1`) and long-form soak checks
(`NEXAROUTE_SOAK=1`) in `internal/router`, `internal/events`,
`internal/probe`, `internal/logging`, `internal/eval`, and `internal/httpapi`
skip by default so routine `go test ./...` and CI stay within budget. Opting
the whole suite into CI would require a workflow change and is deliberately
out of scope here.

- Impact: default test runs do not exercise the stress/soak paths.
- Mitigation/workaround: run them explicitly, e.g.
  `NEXAROUTE_STRESS=1 go test ./...`, `NEXAROUTE_SOAK=1 go test ./...`, or via
  `scripts/stress.sh` / `scripts/soak.sh`.
- Owner: audit finding F11; follow-up set #169–#177.

### Dashboard missing-DOM resilience and CLI-snippet injection — `verified fixed`

Findings F1/F2, in `internal/httpapi/web/app.js`, are covered by the dashboard
section-isolation helpers and `dom_isolation.test.mjs`. CLI snippet values are
HTML-escaped before insertion into the code block by `cliSnippet`, and missing
non-critical nodes no longer abort unrelated sections.

- Evidence: `go test ./...` plus `node --test internal/httpapi/web/dom_isolation.test.mjs`.

### Package coverage gaps and targets — `open`

Finding F7. Measured on `main` 2026-09-30 via
`go test ./internal/compat/ ./internal/route/ ./internal/feature/ ./internal/core/ ./cmd/gateway/ -cover`:

- `internal/compat`: 51.6%
- `internal/route`: 56.3%
- `internal/feature`: 59.4%
- `internal/core`: 30.4%
- `cmd/gateway`: 12.9%

- Impact: thinner regression protection in the listed packages, notably
  `internal/core` and `cmd/gateway`.
- Mitigation/workaround: none in-product; re-measure with the same `-cover`
  command when raising coverage per package.
- Owner: audit finding F7; follow-up set #169–#177.
