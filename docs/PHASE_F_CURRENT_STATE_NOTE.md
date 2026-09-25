# Phase F — Current State Note (pre-implementation audit)

Date: 2026-09-25
Checkout: `ba91c53` ("Fix half-open health donut overwriting cooldown"), branch
`arena/01a0d9e5-nexaroute`.

This note is the Phase F §1 deliverable. It records the audited pre-Phase-F
state of the repository so the external-decision-provider work can be reviewed
against ground truth rather than assumptions.

## 1. Material finding: no decision plane exists yet

The Phase F brief assumes Phase D (decision architecture) and Phase E (policy
engine) artifacts:

- `internal/decision/provider.go`, `request.go`, `result.go`,
  `orchestrator.go`, `validator.go`, `registry.go`, `reason.go`
- `internal/decision/policy/*`
- `internal/httpapi/decision_wiring.go`
- `internal/route/*`, `internal/taskprofile/*`, `internal/feature/*`
- `docs/PHASE_D_DECISION_ARCHITECTURE.md`, `docs/PHASE_E_POLICY_ENGINE.md`,
  `docs/PHASE_E_IMPLEMENTATION_REPORT.md`

**None of these exist in this checkout.** The repository history is a single
squash commit, so there is no earlier Phase A–E baseline to diff against
either. The packages that do exist are:

```text
internal/cache internal/compat internal/config internal/core internal/events
internal/health internal/httpapi internal/logging internal/probe
internal/protocol/canonical internal/providers internal/router
internal/translate internal/usage
```

Consequence: Phase F must first establish the **minimal generic
DecisionProvider foundation owned by NexaRoute** (contract, registry,
orchestrator, validator, primary-selection guardrails, local/policy
built-ins), and only then add the external infrastructure plus the single Jev
adapter. The generic contract is deliberately kept small so it cannot become
architecturally dependent on Jev, TypeSafe, OpenAI, Anthropic, Gemini, or any
other external model. There are no Phase E strict tests to preserve; instead
the guardrail semantics described by the brief are implemented once, in shared
code, and pinned by new Phase F tests.

## 2. Current routing pipeline (the integration seam)

Per-request flow today (`internal/httpapi/openai.go`, `anthropic.go`,
`canonical_path.go` for OpenAI Responses):

```text
readJSON + inspectRequestJSON (vision/reasoning/session/token estimates)
  -> router.Requirement{Model, Tools, Vision, Streaming, Reasoning,
                        ProviderType?, MinContextWindow, EstimatedInputTokens,
                        MaxOutputTokens, SessionKey, SelectionKey, LoadForProvider}
  -> Server.routeSnapshot(req) -> Router.Candidates(req) -> []router.Scored
  -> bounded attempts (max_attempts) with failover / hedging / repair
```

`Router.Candidates` (`internal/router/router.go`):

1. Resolves the request model against deployment IDs, upstream model names,
   local IDs, and aliases; `auto` / `claude-auto` / empty match everything;
   optional fallback when the model is unknown.
2. Filters by provider type, provider incident circuit (`ProviderAvailable`),
   context-window pre-check (unknown windows never filtered), capability
   flags (tools/vision/streaming/reasoning), and health:
   - ready strategies (`ready_mesh`, `ready_queue`, `cost_aware`) require
     `Healthy` (plus scope readiness for `ready_mesh`);
   - other strategies exclude only `Cooldown`.
3. Orders by strategy. `ready_mesh` honors a healthy session pin by moving it
   to the front, otherwise power-of-two choice within the best priority tier;
   `cost_aware` honors an in-tier pin, otherwise cheapest-known-first within
   each priority tier; both diversify provider failover inside a tier.

**External integration seam:** the ordered eligible set `E`
(`[]router.Scored`) returned by `Router.Candidates`, after all hard
eligibility, and before the attempt loop. The decision plane may only reorder
the primary within a permitted band; it must never add, remove, resurrect, or
re-credential candidates, and must never change `max_attempts`. On any
external failure the router order is preserved verbatim (fail open).

## 3. Existing "primary-selection" semantics (what Phase F must generalize)

There is no `PrimarySelectionConstraints` type today. The closest existing
rules live inside strategy ordering:

- **Priority tiers are authoritative.** `ready_mesh`, `ready_queue`, and
  `cost_aware` sort by `Priority` ascending; power-of-two and cost ordering
  never cross a tier. There is no separate "pool" construct: priority tiers
  ARE the pools. Phase F therefore maps `PoolOrdinal := Priority` when
  adapting router candidates, while keeping `PoolOrdinal` and `Priority` as
  independent fields so a future pool construct can populate them separately.
- **Session affinity can be authoritative.** `Router.pinned()` resolves the
  session pin (SHA-256 bucket over session key + model + provider type +
  scopes, TTL-bounded, 10k-entry cap). `ready_mesh` moves any eligible pin to
  the front (even across tiers); `cost_aware` only honors in-tier pins.
  `ObserveSession` records the executed deployment after success.
- **No minimum-priority-tier rule exists as a named guardrail**, but tier
  ordering plus in-tier selection is the de-facto behavior the decision plane
  must not bypass.

Phase F extracts these into shared `ComputeConstraints` logic: earliest pool
→ eligible pin authoritative → else minimum priority tier competes. The full
candidate list stays intact for failover.

## 4. Current privacy boundary

- `router.Requirement` carries `SessionKey` (sensitive: client session IDs)
  and `SelectionKey`. These must never leave the process.
- Request inspection (`inspectRequestJSONFields`) walks only protocol-defined
  conversation fields and derives booleans plus a conservative token
  estimate; raw prompts stay in the handler.
- Provider auth (`ResolvedAPIKey`, credential pools) is applied at the
  adapter layer; client `Authorization`/`x-api-key`/cookies are never blindly
  forwarded (`blockedClientForwardHeader`).
- Admin snapshot redacts secrets (`reveal=1` is an explicit loopback-only
  exception for operator use); upstream error bodies are redacted via
  `RedactBody` before events/logging.

Phase F keeps this boundary and adds a stricter one for external decisions:
only derived, bounded, operational metadata may leave Nexaroute, addressed to
opaque per-request candidate IDs. No session keys, no prompts, no transcript,
no tool schemas/results, no headers/cookies, no upstream credentials, no
physical provider/model/deployment names.

## 5. Current timeout / fail-open behavior (no decision timeout exists)

- Data-plane requests run under `routeContext` (gateway request timeout for
  non-streaming) with client-disconnect and gateway-deadline handling per
  attempt; transport/5xx/429 failures fail over to the next candidate.
- Hedging races at most 2 legs; the loser records no health evidence.
- Probes, admin checks, and discovery all use bounded contexts and bounded
  readers (`io.LimitReader`).

There is no decision timeout. Phase F adds `decision.timeout_ms` (default
400ms) enforced via the `context.Context` supplied to `Decide`, with
fail-open to the router order on every failure class (DNS, connect, TLS,
timeout, HTTP error, invalid JSON, nonzero Jev code, unknown choice, bad
confidence, oversized bodies, missing key). `MaxProviderCalls = 1` per
request; no retries, no chains (Phase G).

## 6. Config / reload / observability seams

- `internal/config/config.go`: `Config` with `ApplyDefaults`, `Validate`
  (bounded strings/counts, strict enums), `SaveAtomic` (temp + rename +
  dir-sync). Defaults must gain `decision{mode:off, provider:local,
  timeout_ms:400}` and empty `decision_providers`; validation must enforce
  the §11 cross-field rules (unique IDs, no `local`/`policy` collision,
  supported type `jev` only, `metadata_only` only, mode/provider coherence,
  enabled-selected).
- Hot reload: `Server.applyConfigLocked` validates + persists, prepares a new
  provider registry, then swaps registry/router/health-config under
  `runtimeMu`. Decision adapters must be immutable snapshots swapped the same
  way (atomic pointer), so in-flight requests keep the old coherent adapter.
- Events: `internal/events.Bus` (bounded ring + bounded counters). New kind
  `external_decision` with safe fields only (provider id/type, action,
  local selected ID only, latency, confidence, reason codes, outcome class).
- Metrics: `metrics.go` renders Prometheus text. New bounded metrics
  `nexaroute_external_decision_requests_total{type,outcome}` and
  `nexaroute_external_decision_latency_seconds{type}` (count+sum, no
  high-cardinality labels).
- Admin snapshot: `adminSnapshot` gains `external_decision_providers[]` with
  `{id,type,enabled,health,key_configured,privacy_mode}` — never secrets,
  payloads, or URL credentials.

## 7. Security threats considered for the external seam

1. Raw user content exfiltration (prompt/system/transcript/files/tool data).
2. Credential exfiltration (Jev key anywhere but the Authorization header;
   upstream/client/admin keys anywhere at all).
3. SSRF via configurable base URL → Phase F exposes NO base-URL override in
   config; production calls go only to `https://www.jevai.org`; tests inject
   `httptest` URLs through an internal constructor only.
4. Authorization forwarding across redirects → redirects disabled.
5. TLS downgrade → normal verification, no `insecure_skip_verify` knob.
6. Response-DoS via unbounded reads → 64 KiB response cap, bounded JSON.
7. Request over-size → pre-send 32 KiB measurement, fail open without send.
8. Eligibility resurrection / pool / priority / affinity bypass → validator
   enforces `selected ∈ AllowedPrimaryIDs ⊆ E`; affinity short-circuits
   before any network call.
9. Health poisoning (external failure blamed on models, or vice versa) →
   strictly separated: external errors never touch model health; model
   failures are recorded only for executed deployments.
10. Secret/metric/event/log leakage of key, prompt canary, remote error body,
    opaque↔physical map → covered by dedicated canary tests.
11. Silent enablement → external decisions are explicit opt-in; defaults keep
    all external network off and routing unchanged.

## 8. What will change

- NEW `internal/decision/*`: generic contract (`provider.go`, `request.go`,
  `result.go`, `reason.go`), `constraints.go`, `validator.go`,
  `orchestrator.go`, `registry.go`, built-ins `local/` + `policy/`.
- NEW `internal/decision/remote/*`: shared transport primitives
  (`client.go`, `errors.go`, `limits.go`).
- NEW `internal/decision/jev/*`: isolated Jev adapter
  (`provider.go`, `request.go`, `response.go`, `mapper.go`).
- EDIT `internal/config/config.go`: `decision` + `decision_providers` model,
  defaults, validation.
- EDIT `internal/router/router.go`: expose eligible-pin lookup for the
  decision layer (additive; no ordering change).
- NEW `internal/httpapi/decision_wiring.go`: runtime build/swap, feature
  derivation (metadata-only), event/metric/admin integration.
- EDIT `internal/httpapi/server.go`: immutable decision-runtime swap on
  reload; EDIT `openai.go`/`anthropic.go`/`canonical_path.go`: single
  reorder hook after `routeSnapshot` (no-op unless `assisted`).
- EDIT `configs/config.example.json`: opt-in disabled Jev example.
- NEW docs: this note, `PHASE_F_EXTERNAL_DECISION_PROVIDERS.md`,
  `PHASE_F_IMPLEMENTATION_REPORT.md`; NEW `scripts/smoke-jev.sh` (opt-in,
  never CI).

## 9. What will NOT change

- Router eligibility, ordering, affinity recording, health, circuits, quota,
  hedging, cache, auth, translation, SSE, retry/backoff, `max_attempts`,
  credential selection: byte-for-byte behavior when `decision.mode != assisted`.
- No provider chains, no Jev→policy fallback, no retries, no voting, no
  shadow/canary decisions, no scorecards, no learned routing, no supervisor
  plane, no dashboard UI beyond status visibility.
- No `smart`/`hybrid`/`supervisor` modes; no `full_context`/`raw_prompt`
  privacy modes; no second external provider call per request.
