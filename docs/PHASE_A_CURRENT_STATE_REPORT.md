# Phase A — Current State Report

Repository: `ali-shortcuts/nexaroute`
Baseline commit: `ba91c531e13826a7d6637022fb2a805a4e709501` ("Fix half-open health donut overwriting cooldown")
Audit date: 2026-09-25
Scope: architectural mapping before any Intelligent Routing / Decision / Evaluation / Supervision implementation.
This document is the Phase A deliverable required by the master specification. No feature
implementation is included in this phase.

---

## 0. Verification baseline (executed during this audit)

| Gate | Command | Result |
|---|---|---|
| Formatting | `gofmt -l .` | clean |
| Unit/integration tests | `go test -timeout=3m -shuffle=on -count=10 ./...` | PASS (15 packages) |
| Static analysis | `go vet ./...` | PASS |
| Race detector | `go test -race -timeout=3m -shuffle=on -count=3 ./...` | PASS |
| Web UI JS syntax | `node --check internal/httpapi/web/app.js` | PASS |
| Short fuzz | `FuzzPatchJSONModel`, `FuzzParseAnthContent` (2 s each) | PASS |
| Builds | linux/amd64 + linux/arm64 `CGO_ENABLED=0` | PASS |
| Full gate | `./scripts/verify.sh` | **VERIFY PASS** |

The repository is green at the audit baseline. Toolchain used: go1.23.9 (module declares `go 1.23`,
zero external dependencies — `go.mod` contains only the module line and the Go version).

## 0b. Collaboration state (do not overwrite in-flight work)

8 open pull requests exist at audit time. Several touch files this program will also touch:

| PR | Branch | Relevance to this program |
|---|---|---|
| #31 | `fix/compat-test-upstream-model-name` | compat tests; low conflict risk |
| #26 | `fix/...` (`arena/01a0d36d-nexaroute`) | Responses-native routing + reload/cache correctness — touches `canonical_path.go`, `models.go`, `server.go` |
| #21 | `fix/responses-stateful-semantics` | stateful Responses routing — `canonical_path.go` |
| #20 | `fix/hedge-attempt-budget-r2` | hedge attempt accounting/candidate skipping — `anthropic.go`/`openai.go`/`hedging.go` hot loop |
| #13 | `fix/beta-compatibility-install` | **contains a primitive "unified endpoint" (`internal/httpapi/endpoint.go`, endpoint panel, generated gateway key, shared public model name)** — conceptually adjacent to Virtual Endpoints (Phase B). Must be reviewed/merged or explicitly superseded before Phase B lands a full Virtual Endpoint model. Also touches `clientauth.go`, `models.go`, `config.go`. |
| #12 | `fix/universal-installer-release` | installer/release workflow |
| #10 | `arena/01a0d1d4-nexaroute` | hedged-race latency, UI preset/edit |
| #5 | `arena/01a0cf59-nexaroute` | older v0.3 umbrella |

Issue tracker: no open issues at audit time.

Rule adopted for this program: **every phase rebases on current `main`, never rewrites code that an
open PR is already fixing, and coordinates Phase B endpoint semantics with PR #13** (either building
on its single-endpoint concept as a degenerate Virtual Endpoint, or superseding it deliberately).

---

## 1. Current request data path

```text
Client (Claude Code / OpenAI SDK / any HTTP client)
  │
  ├─ middleware (internal/httpapi/server.go):
  │    request-ID sanitize · global admission ceiling (routing.max_inflight_requests, 503+Retry-After)
  │    · admin/loopback boundary · bounded JSON bodies
  │
  ├─ data-plane ingress:
  │    POST /v1/messages              → anthropicMessages   (anthropic.go)
  │    POST /v1/messages/count_tokens → countTokens         (count_tokens.go)
  │    POST /v1/chat/completions      → openAIChat          (openai.go)
  │    POST /v1/responses             → openAIResponses     (canonical_path.go, canonical IR)
  │    GET  /v1/models                → models              (physical IDs + aliases + auto/claude-auto)
  │
  ├─ optional client auth (clientauth.go): static keys, constant-time SHA-256 digest compare, per-key RPM
  │
  ├─ parse + bounded structural inspection (inspectRequestJSON, routing_helpers.go):
  │    vision content · reasoning keys · prompt token estimate (chars/4, overestimate-biased)
  │    · body session key · depth guard (TooComplex rejects pathological JSON)
  │
  ├─ router.Requirement{model, tools, vision, streaming, reasoning, provider type,
  │    session/selection key, min context window, estimated input, max output, provider load hook}
  │    → s.routeSnapshot(req) → router.Candidates(req)  ← THE single routing choke point
  │
  ├─ optional exact-match response cache lookup (cache_wiring.go, opt-in)
  │
  ├─ attempt loop (≤ routing.max_attempts, hedge counts as 2):
  │    per-attempt revalidation (currentRouteCandidate — closes stale-candidate window)
  │    → capability gate (compat.Store.IneligibleFor via capabilityIneligible)
  │    → payload build: native passthrough | legacy Anth↔OpenAI translate | canonical IR encode
  │    → provider adapter (semaphore · credential P2C · quota reservations · auth)
  │    → optional bounded deterministic repair (≤ max_repair_attempts) + proactive sanitizer
  │    → hedging partner race (first attempt only, one partner, loser = zero health signal)
  │
  ├─ success: recordRouteSuccess → health/scope/provider success + session pin + usage + cache store
  │           (recordRouteSuccess is invoked only after actual execution success — §30 compliant)
  └─ failure: error-class policy (routing_helpers.policyForStatus + compat.ClassifyUpstreamError)
              → quarantine / provider-incident signal / scoped circuit / failover to next candidate
              → pre-stream failover only; after client-visible bytes the request stays bound
```

Translation never chooses a provider. Routing never rewrites protocol semantics. The router
decision itself is local, deterministic, and network-free.

## 2. Routing interfaces

`internal/router/router.go` is the **only** routing core (there is no second router — this must
remain true):

- `router.Requirement` — request requirement profile (model/alias, tools, vision, streaming,
  reasoning, provider-type constraint, session key, selection key, `MinContextWindow`,
  `EstimatedInputTokens`, `MaxOutputTokens`, live `ProviderLoad` hook).
- `router.Scored` — `{Deployment, Health, Score, CapacityPressure, EstimatedCostUSD, PriceKnown}`.
- `Router.Candidates(req) []Scored` — full eligibility + strategy ordering.
- `Router.Eligible(id, req)` — single-candidate revalidation (used per attempt).
- `Router.ObserveSession(req, id)` — affinity pin **after success only** (bounded at 10 000 pins).
- `Router.Reload(cfg)` — hot swap of the deployment index; pins are dropped for removed deployments.
- `Router.Readiness/AllLimit/All/Deployment/SessionCount` — observability surfaces.

Composite score (today): `100 + health band (Healthy +35 … Degraded −20) + weight×10 − priority×3
− EWMA latency×routing.latency_weight − capacity pressure×routing.capacity_weight − EWMA failure
rate×routing.failure_weight`, plus optional bounded request-cost estimate for `cost_aware`.

## 3. Provider abstractions

`internal/providers`:

- `Adapter` interface: `ID, Kind, Stats, CredentialsMatch, RedactBody, Do, DoPath, CountTokens, Probe`.
- `Registry.Prepare/Replace` — build-next-then-swap hot reload that **reuses unchanged adapters**
  (HTTP pools and credential cooldown state survive); `CredentialsMatch` detects env-key rotation and
  rebuilds only that adapter.
- `httpAdapter` (982 LOC) — semaphore-bounded concurrency, credential pool with independent
  per-key cooldown + Retry-After, quota header observation (limit/remaining/reset per resource),
  in-flight request/token reservations overlay, response-size bounds, secret redaction.
- Provider types in config: `openai_compatible`, `anthropic_compatible`, `gemini`, `openai_responses`.
- Dialect registry (`internal/compat/quirks.go`) — `nvidia_nim`, `deepseek`, `openrouter`, `groq`,
  `together`, `openai`, `anthropic`, `gemini`, `generic_openai`, `generic_anthropic`; differences
  live in data, not `if provider == …` branches.

## 4. Deployment identity model

`router.Deployment` = one `provider/model` pair, ID `provider-id/model-id`:

- upstream `model` ID + local `aliases`, `priority`, `weight`,
- `context_window`, `input_cost_per_mtok`, `output_cost_per_mtok`,
- `capabilities{streaming, tools, vision, reasoning}` (config-declared),
- plus live per-deployment state in `health.Manager` and compatibility knowledge in `compat.Store`
  (both keyed by the same deployment ID).

Health, quotas (provider-level), capability contracts and pricing attach to this identity. This is
the unit every future DecisionProvider must return as "selected candidate".

## 5. Candidate eligibility rules (hard constraints today — must remain authoritative)

Applied in `router.eligibleDeployment` + handler revalidation + `compat.Store.IneligibleFor`:

1. requested model/alias/`auto`/`claude-auto` match (or `fallback_on_unknown_model`);
2. provider-type constraint (e.g. reasoning requests prefer `anthropic_compatible`, with one relax);
3. provider incident circuit open → excluded (`health.ProviderAvailable`);
4. advertised `context_window` < estimated prompt + output → excluded (unknown window never filtered);
5. capability flags: request needs tools/vision/streaming/reasoning vs deployment `capabilities`
   and the tri-state compat contract (verified-UNSUPPORTED excludes; UNKNOWN never blocks);
6. health gating by strategy: ready strategies (`ready_mesh`, `ready_queue`, `cost_aware`) require
   verified `healthy` (and scope-ready for `ready_mesh`); legacy strategies exclude `cooldown`;
7. per-attempt revalidation immediately before dispatch (concurrent quarantine / hot reload safe).

Nothing in the codebase can currently revive an excluded candidate — the same invariant the
intelligent layer must preserve.

## 6. Existing strategies

`ready_mesh` (default: verified-ready + session pin + P2C within best priority tier + provider-diverse
failover ordering), `cost_aware` (priority-tier-authoritative bounded price ordering, unknown price
never free), `ready_queue`, `adaptive_round_robin`, `adaptive`, `priority`, `round_robin`,
`least_latency`. Strategy is global (`routing.strategy`), not per-endpoint.

## 7. Health and breaker state

`internal/health/manager.go`:

- Per-deployment `State`: status (`unknown/healthy/degraded/half_open/cooldown`), success/failure
  counters, consecutive failures, EWMA latency, EWMA TTFT (streaming), recency-weighted failure rate,
  cooldown deadline, recovery-failure count, **scoped capability circuit states** (tools/vision/
  streaming/reasoning).
- Lifecycle: first routed failure → quarantine + recovery supervisor (5 probes, 500 ms delay) →
  any success restores `healthy`; 5 failures → 1800 s cooldown → `half_open` → loop. Hard cooldown
  (auth/billing/429) skips recovery immediately. Half-open cannot be overwritten by stale in-flight
  success (fixed at baseline commit).
- `internal/probe/engine.go`: startup `Prime`, event-driven sweeps (healthy leases refreshed by real
  traffic; `probe.ready_lease_seconds`), bounded 64-worker recovery queue, manual "Probe all models".

## 8. Provider-incident state

Separate failure domain in `health.Manager`: `ProviderState` per provider with distinct-deployment
evidence map (default 3 distinct deployments within 20 s), cooldown 30 s, half-open semantics,
stale-observation guard (in-flight results cannot clear a newer cooldown), identity-change
invalidation (base URL/auth change drops stale evidence). Model-specific 404s never open a provider
circuit.

## 9. Credential balancing

`httpAdapter.creds[]credentialState`: resolved primary + named pool (literal / `api_key_env`),
power-of-two reserve of the less-loaded/lower-failure key, reservation held until response body is
consumed (SSE = real load), independent per-key cooldown on auth/quota/rate-limit statuses,
`Retry-After`-aware "all keys cooling" wait. Provider auth is applied after custom headers so user
headers cannot override configured credentials. `RedactBody` scratches both literals and resolved
env values from error snippets.

## 10. Compatibility contracts

`internal/compat` — this is the existing "capability intelligence" plane:

- Tri-state (`SUPPORTED/UNSUPPORTED/UNKNOWN`) matrix per deployment for 19 capabilities, each fact
  carrying `Evidence{source: static|dialect|discovery|probe|runtime, confidence, timestamp}`.
  "UNKNOWN is never UNSUPPORTED; nothing is SUPPORTED without declaration/probe/real traffic."
- `Contract` + invalidation fingerprint (base URL, dialect, model id, credential scope);
  `/admin/api/compat/reset` reopens questions.
- `ClassifyUpstreamError` — 19-class taxonomy separating **capability failures** (no quarantine,
  bounded deterministic repair ≤ `max_repair_attempts`, cached into contract → proactive sanitizer)
  from **health failures** (existing failover/cooldown semantics). Context-overflow 400 is its own
  class (deployment healthy; fail over for window size).
- `Scorecard` — Claude Code readiness facets (BASIC_CHAT / STREAMING / TOOLS / TOOL_RESULTS /
  PARALLEL_TOOLS / REASONING → CLAUDE_CODE_READY | CHAT_READY | NOT_AGENT_READY | NOT_VERIFIED).
  **Note:** this is a compatibility scorecard, *not* the Model Intelligence scorecard of the spec
  (no quality dimensions, no provenance-managed quality scores).
- Level B capability probes + Claude Code agent-loop simulation (`/admin/api/provider-test` modes
  `quick | full | claude_code`).

## 11. Failover behavior

Ordered candidates from `Candidates()`; per-attempt revalidation; pre-stream failover only;
status→policy mapping (`policyForStatus`: 400/422 caller-invalid health-neutral; 409/425 failover
health-neutral; 404 deployment isolation; 401/402/403/429/5xx/timeout → failover + quarantine +
provider evidence where indicated); `Retry-After` honored with cap; jittered retry backoff
(deterministic per request+attempt); attempt budget `routing.max_attempts` (default 4) with hedge
legs counted; hedge skip-map prevents re-attempting the partner; `diversifyProviderFailover`
spreads fallbacks across providers within the priority tier. No mid-stream resume — deliberate.

## 12. Routing-state commit semantics

`recordRouteSuccess(req, deploymentID, providerID, latency)` (routing_helpers.go) is called only on
the actual winning deployment after execution success: `RecordSuccess`, `RecordProviderSuccess`,
`RecordScopeSuccess`, `ObserveSession` (affinity pin). Usage/cost tokens are recorded against the
winning deployment in all four response paths (native/translated × stream/non-stream). Failures are
recorded against the attempted deployment; abandoned hedge legs record nothing. **Spec §30 is
already satisfied and must remain invariant.**

## 13. Configuration model

Single JSON document (`internal/config/config.go`, 870 LOC, stdlib-only):

```text
Config{ listen, admin, logging, routing(RoutingConfig), probe(ProbeConfig),
        cache(CacheConfig), client_auth(ClientAuthConfig), providers[](ProviderConfig
        { models[](ModelConfig), credentials[](CredentialConfig), … }) }
```

- `Default() → Load() → ApplyEnvOverrides → ApplyDefaults → Validate` with hard bounds
  (≤512 providers, ≤4096 models/provider, ≤256 credentials, ≤20 000 deployments, ≤16 MB file, …).
- `SaveAtomic` (0600, same-dir temp + fsync + rename), `RemoveStaleBackup`, secret-preserving edit
  (`preserve_secret` + `CredentialsMatch`).
- Hot-reload path: parse+validate → `Registry.Prepare` preflight → atomic save → short write lock →
  registry/router/health/probe swap.

Missing sections (spec §46): `virtual_endpoints`, `route_profiles`, `candidate_pools`, `decision`,
`evaluation`, `supervision`. All must be **additive** with safe defaults; existing files must load
unchanged (spec Rule 5).

## 14. Admin API architecture

`/admin/api/*` — loopback-only by default (`admin.bind_local_only`), optional `admin.api_key`
(constant-time compare), per-IP token buckets (90 burst / 1.5 tps, auth-failure penalty):

`GET snapshot` (deployments/health/provider-health/events/stats/pressure/sessions/probe-stats/
usage/cache/client_auth/config), `POST probe`, `GET/POST/PUT/DELETE providers[/id]`,
`GET provider-presets`, `POST provider-check`, `POST provider-test`, `GET compat`,
`POST compat/reset`, `POST provider-discover`, `GET/PUT settings` (routing+probe only).

Snapshot is the dashboard's single data source. There is no CRUD for anything model-logical above
aliases.

## 15. Dashboard architecture

Embedded (`go:embed web/*`), single-page vanilla JS (`app.js` ≈ self-contained), dark design system,
polling the admin snapshot. Tabs today: **Overview** (KPI cards, topology ring around a ROUTER core,
health donut, latency chart, cache/spend KPIs) · **Console** (bounded live event feed with filters)
· **Providers** (cards + editor with secret preserve/reveal) · **Models** (deployment table) ·
**Health** · **Compat** (capability matrix, Full test / Agent test / Reset) · **CLI Tools**
(Claude Code / OpenAI / env / docker snippets) · **Settings** (routing + probe forms).

New planes must add tabs/cards in this exact style (spec §36), not a second dashboard.

## 16. Observability

- `events.Bus`: bounded ring + bounded per-kind/error counters (256 keys, `__other__` overflow);
  kinds include `route_attempt/route_ok/route_fail/route_skip/failover/route_timeout/stream_fail/
  client_disconnect/cache_hit/…`.
- Prometheus `/metrics`: request/inflight/overload, probe+recovery gauges, deployment count + health
  histogram, per-deployment EWMA latency/TTFT/failure-rate + success/failure counters, event and
  error counters, provider active/waiting/concurrency/credential-cooldown, full quota family
  (limit/remaining/reserved/effective × requests/tokens + reset timestamps), usage/cost.
- Rotating 0600 operational logs (32 MB ×3 default), sampled access log, console storm limiting.
- Missing metric families (spec §35): decision requests/latency/provider failures/cooldown, task
  classification, intelligent selections, deterministic fallbacks, invalid decisions, abstentions,
  route-profile traffic, shadow executions, evaluation runs, scorecard versions, canary traffic.

## 17. Tests / CI

≈24.6 k LOC Go (source + tests). Per-package unit tests plus: `internal/httpapi` integration,
canonical-path regression, v0.5 feature tests, fuzz (`FuzzPatchJSONModel`, `FuzzParseAnthContent`),
hedging tests; `internal/router` scale (10 k deployments) + stress; `internal/probe` recovery/soak/
stress (5 k recoveries); `internal/events` stress; `internal/logging` stress; `internal/httpapi`
stress/soak (admission overload).

CI (`.github/workflows/ci.yml`): repository-cleanliness (no backup artifacts / legacy identifiers),
`scripts/verify.sh`, `scripts/stress.sh`, `scripts/smoke-local.sh`, installer smoke (config 0600,
no `.bak`), Docker build + runtime smoke. `soak.yml` manual/auto-on-change; `release.yml` for
artifacts. TEST_REPORT.md documents this as the standing verification contract.

## 18. Known architectural gaps (vs the master specification)

| Area | Status at baseline |
|---|---|
| Virtual Endpoints as first-class entities | **missing** (only `auto`/`claude-auto` catch-alls + aliases; PR #13 has a primitive single "unified endpoint") |
| Route Profiles / named candidate pools / fallback chains | **missing** (aliases + global strategy only) |
| Request feature profile / Task Analyzer / Task Profile | **missing** (inline `inspectRequestJSON` flags exist — good seed) |
| Decision Plane (DecisionProvider, orchestrator, chains, budget, confidence, privacy) | **missing** |
| Multi-objective policy engine with separated model/provider/credential scores | **partial** (composite `Scored` + separate credential P2C inside adapter; no model-vs-provider-vs-credential decomposition) |
| Model Intelligence scorecards with provenance | **missing** (compat `Scorecard` is readiness only) |
| Evaluation Plane / suites / deterministic evaluators | **missing** (Level B probes and agent-loop test are compatibility probes, not quality evaluation) |
| Shadow / challenger / canary lifecycle | **missing** |
| Supervision Plane | **missing** (the probe "recovery supervisor" is a health concept only) |
| Route explainability (structured reason codes per request) | **partial** (event messages carry score/health/pressure; no structured reason-code set, no candidate list, no decision trace store) |
| Dry Run | **missing** |
| Decision metrics | **missing** |
| Decision cache | **missing** (response cache exists and proves the invalidation-on-swap pattern) |
| Persistence layering (config vs runtime vs evaluation vs history) | **partial** (config persisted; everything else in-memory; no evaluation/history store abstraction) |

Documentation discrepancies found (docs vs code — code is authoritative):

1. `docs/KNOWN_GAPS.md` "Protocol scope" claims OpenAI Responses and Gemini native are **not**
   implemented; code (v0.6.0) implements `POST /v1/responses` ingress and Gemini
   GenerateContent upstreams via the canonical IR. Section is stale.
2. `docs/CONFIGURATION.md` lists provider types as only `openai_compatible`/`anthropic_compatible`;
   code also accepts `gemini` and `openai_responses`.
3. `SECURITY.md` (labeled v0.4) says there is no built-in client auth on `/v1/*`; v0.5 added opt-in
   `client_auth` with constant-time key compare and per-key RPM.
4. Version labels lag: `ARCHITECTURE.md`, `SECURITY.md`, `CONFIGURATION.md`, `COMPATIBILITY.md`
   headers say v0.4 while the binary is v0.6.0 and the content is v0.5.2–v0.6; `TEST_REPORT.md` and
   `KNOWN_GAPS.md` say v0.5.2. Per spec §1 these will be corrected with the relevant phase.

## 19. Exact extension points (integration seams)

| Seam | Location | How the new planes attach |
|---|---|---|
| Routing choke point | `s.routeSnapshot(req)` used by `anthropic.go`, `openai.go`, `canonical_path.go` | Decision Orchestrator inserts **between** `Candidates()` (hard eligibility) and the attempt loop; it may only reorder/choose within the returned `[]Scored` |
| Eligibility revalidation | `Router.Eligible` + `currentRouteCandidate` | Eligible-set validator for intelligent results reuses exactly this |
| Feature extraction | `inspectRequestJSON` (routing_helpers.go) | Promote to `internal/feature` (Task Analyzer); keep bounded/local |
| Requirement profile | `router.Requirement`, `compat.RequirementProfile` | Extend with endpoint/profile identity + normalized task features (additive fields) |
| Failure policy | `policyForStatus`, `compat.ClassifyUpstreamError` | RETRY/FALLBACK decision tasks consume these classes; never re-implement |
| Success commit | `recordRouteSuccess` | Feedback/telemetry/learning evidence hook fires here (actual success only) |
| Config | `config.Config`, `ApplyDefaults`, `Validate` | Additive sections: `virtual_endpoints`, `route_profiles`, `candidate_pools`, `decision`, `evaluation`, `supervision` |
| Admin API | `server.go Handler()` route table | New `/admin/api/*` handlers with the same auth/bucketing middleware |
| Dashboard | `web/index.html` nav + `app.js` tabs | New tabs: Virtual Endpoints, Route Profiles, Decision, Model Intelligence, Evaluation, Shadow/Canary, Decisions (traces), Dry Run |
| Events / metrics | `events.Bus` kinds, `metrics.go` families | New bounded kinds/families (spec §34–35), label-sanitized |
| Adapter pattern | `providers.Adapter` + cooldown conventions in `health.Manager` | Template for `DecisionProvider` adapters incl. health/cooldown/half-open with generation guard |
| Cache pattern | `cache.Cache` + wholesale invalidation on config swap | Template for the safe parts of a Decision Cache |
| Protocol reach | `internal/protocol/canonical` | Evaluation suites exercise the same IR for cross-protocol checks |

## 20. Files expected to change (cumulative across phases)

```text
internal/config/config.go            additive schema + defaults + validation (+ config_test.go)
internal/router/router.go            candidate-pool/profile-aware Requirement pass-through; reason-code surfacing
internal/httpapi/routing_helpers.go  feature-profile build; route-trace recording; decision hook wiring
internal/httpapi/anthropic.go        call orchestrator between candidates and attempts (OFF = current path)
internal/httpapi/openai.go           same
internal/httpapi/canonical_path.go   same
internal/httpapi/server.go           route table + wiring for new admin endpoints
internal/httpapi/admin.go            snapshot extensions (bounded)
internal/httpapi/models.go           expose virtual endpoint client model names
internal/httpapi/metrics.go          decision/eval/shadow metric families
internal/httpapi/clientauth.go       per-endpoint key policy reuse (coordinate with PR #13)
internal/httpapi/web/index.html      new tabs/cards
internal/httpapi/web/app.js          new views; keep single-file style
configs/config.example.json          new sections with disabled defaults
README.md / ARCHITECTURE.md / docs/* documentation updates; fix stale v0.4 labels + KNOWN_GAPS protocol section
```

## 21. Files expected to be added

```text
internal/feature/          normalized request feature profile + bounded local extraction
internal/taskprofile/      Task Profile contracts + local Task Analyzer (extensible types)
internal/decision/         DecisionProvider interface, DecisionRequest/Result, task types,
                           orchestrator, chains, budget, confidence policy, eligible-set validator,
                           deterministic fallback adapter, privacy/context policy
internal/decision/localrule/    Local Rule Engine (first provider)
internal/decision/localclassifier/ Local Classifier provider
internal/decision/httpjson/     Generic HTTP/JSON custom provider (SSRF-hardened) [Phase F/G]
internal/decision/jev/          Jev adapter ONLY if requested after audit [Phase F — optional]
internal/policy/           multi-objective policy engine (model/provider/credential score separation),
                           reason codes
internal/scorecards/       versioned Model Intelligence scorecards + provenance
internal/eval/             evaluation suites, deterministic evaluators, evaluator registry,
                           evaluation store abstraction (optional file persistence, no DB dependency)
internal/shadow/           bounded shadow/challenger execution + measurement
internal/canary/           lifecycle DISCOVERED→VERIFIED→EVALUATED→SHADOW→CANARY→ACTIVE + promotion policy
internal/supervision/      supervisor contracts, evidence schema, structured actions [Phase K]
internal/routetrace/       structured route explainability records (bounded store)
docs/PHASE_*_REPORT.md     per-phase implementation reports (spec §54)
```

Package names follow existing Go conventions (`internal/<plane>`) and keep zero external module
dependencies unless a later phase proves otherwise.

## 22. Regression risks

1. **Hot-path mutation** — `anthropic.go`/`openai.go`/`canonical_path.go` attempt loops are subtle
   (hedge accounting, attempt budgeting, skip maps). PRs #20/#21/#26 are already fixing edge cases
   here. Decision integration must be a single bounded call site ("rank/choose within `[]Scored`"),
   not a loop rewrite.
2. **Behavior with `decision.enabled=false`** — must be byte-equivalent routing to baseline
   (Rule 5); test explicitly ("Decision OFF follows existing routing semantics").
3. **Config compatibility** — every new field needs `ApplyDefaults` safe values; existing config
   files and the settings PUT payload must keep loading (dashboard settings form round-trip).
4. **Snapshot shape** — dashboard JS reads `snap.*` fields; additions must be additive and bounded.
5. **Affinity semantics** — `affinityBucket` keys on `session|model|providerType|scopes`; virtual
   endpoints + task profiles must define how these keys evolve without pinning storms.
6. **PR #13 overlap** — endpoint/client-key semantics; coordinate before Phase B.
7. **Metrics cardinality** — new families must reuse `sanitizeMetricLabel` and bounded label sets.
8. **Attempt-budget honesty** — shadow/challenger calls must never consume the client request's
   `max_attempts` or health evidence (mirror the hedge "abandonment is not evidence" rule).
9. **Evaluation isolation** — eval traffic must use tagged contexts (like `WithQuotaEstimate`) so it
   cannot create data-plane quota reservations or health signals unless explicitly runtime-health
   tests.

---

# Requirement classification (master spec → repository)

Legend: **I** IMPLEMENTED · **P** PARTIAL · **M** MISSING · **C** CONTRADICTED (docs claim vs code)
· **N/A** NOT YET APPLICABLE (deliberately later phase)

| Spec § | Requirement | Class | Evidence |
|---|---|---|---|
| 0 Rule 1 | NexaRoute owns routing contracts; no second router | I | single `internal/router`; decision layer not yet built (must keep this) |
| 0 Rule 2 | Hard constraints authoritative | I | eligibility rules §5 above |
| 0 Rule 3 | Route without external intelligence | I | today's whole path is deterministic |
| 0 Rule 4 | Intelligence measurable | P | telemetry exists (EWMA, usage, events); no evaluation plane yet |
| 0 Rule 5 | Disabled-feature compatibility | I (to preserve) | current behavior is the reference |
| 2 | WHAT/WHERE/WHICH separation | P | WHERE (provider P2C) + WHICH (credential P2C) exist inside adapter; WHAT (model/task) is alias+priority only |
| 3A | Data Plane | I | §1 |
| 3B | Decision Plane | M | — |
| 3C | Evaluation Plane | M | compat probes ≠ quality evaluation |
| 3D | Supervision Plane | M | "recovery supervisor" is health-only (name collision to document) |
| 4 | Virtual Endpoints first-class | M | `auto`/`claude-auto` + aliases only; PR #13 unified-endpoint primitive in flight |
| 5 | Route Profiles reusable | M | aliases + global strategy |
| 6 | Request feature extraction (bounded, local) | P | `inspectRequestJSON` (vision/reasoning/tools/estimate/session/complexity) — needs normalization + extensibility |
| 7 | Task-aware routing / Task Profile | M | explicit capability flags only; ROADMAP/KNOWN_GAPS explicitly deferred semantic routing |
| 8 | Local classifier first | M | — |
| 9 | Generic DecisionProvider contract | M | `providers.Adapter` is the pattern to mirror |
| 10 | Decision task types | M | — |
| 11 | Multiple decision provider classes | M | — |
| 12 | Jev adapter (optional, isolated) | N/A→M | no Jev code anywhere; per spec, implement only if still requested (Phase F) |
| 13 | Deterministic local engine | I (reuse rule) | existing router is the engine; "consume eligible state, don't duplicate" is satisfiable at `routeSnapshot` |
| 14 | Eligible-candidate invariant | I (to preserve) + M (validator/tests for intelligent results) | `Eligible`/`currentRouteCandidate` exist; no external-decision validator yet |
| 15 | Multi-objective policy engine | P | weights exist; hard-vs-soft separation partially (priority tiers hard in cost_aware); no task/quality dimensions; no score decomposition |
| 16 | Model Intelligence scorecards + provenance | P | `compat.Contract` evidence model is the provenance pattern to extend; quality dimensions absent (operator `priority/weight` only — deliberately, per KNOWN_GAPS) |
| 17 | Evaluation engine + suites | M | Level B probe suite is compatibility-only |
| 18 | Evaluation correctness (deterministic > judge) | N/A→M | no evaluators yet; agent-loop test shows deterministic style |
| 19 | Shadow / challenger | M | hedging is failover-racing, not evaluation shadowing (different concept — do not overload) |
| 20 | Canary promotion lifecycle | M | — |
| 21 | Learned router later | N/A | explicitly deferred by ROADMAP/KNOWN_GAPS; matches spec |
| 22 | Decision modes (OFF/ASSISTED/SMART/HYBRID/SUPERVISOR) | M | single global strategy string instead |
| 23 | Decision provider chains | M | — |
| 24 | Global decision budget | M | request timeout + attempt budget exist for upstream, not for decisions |
| 25 | Confidence policy (per-provider calibration) | M | — |
| 26 | Privacy / context policy | M | (no external decision recipients yet; logs already metadata-only) |
| 27 | Custom decision providers (declarative mapping) | M | provider presets/discovery show declarative style |
| 28 | Custom endpoint security (SSRF) | P | admin-only config surface, header validation, bounded bodies, redaction exist; explicit SSRF allow/deny is a known gap (ROADMAP) — must be built for decision endpoints |
| 29 | Decision-provider health states | M | `health.Manager` supplies the state-machine pattern |
| 30 | Routing-state commit after actual success | I | `recordRouteSuccess` |
| 31 | Fallback correctness (no loops/skips/dupes) | I | attempt budget + skip map + hedge counting + bounded backoff |
| 32 | Decision cache | P (pattern only) | response cache + invalidation-on-swap |
| 33 | Route explainability | P | live console events with score/health/pressure; needs structured reason codes + candidate sets + traces |
| 34 | Observability extensions | P | §16 list minus decision fields |
| 35 | Metrics extensions | P | naming conventions + sanitizers exist; families missing |
| 36 | Dashboard sections | P | shell + patterns exist; listed planes' tabs missing |
| 37 | Dry Run | M | provider-test is provider-scoped, not route-scoped |
| 38 | Supervision architecture boundary | M | — |
| 39 | Supervision model/actions | M | — |
| 40 | Supervision evidence | M | — |
| 41 | Protocol expansion state | I/P per family | Chat ✅, Anthropic Messages ✅, **OpenAI Responses ✅ (v0.6 IR ingress)**, **Gemini native ✅ upstream**; Azure semantics, Bedrock, Vertex, embeddings/rerank still absent (docs C — KNOWN_GAPS stale) |
| 42 | Named pools / fallback chains + cycle validation | M | provider-diverse ordering exists; no named entities |
| 43 | Persistence/state separation | P | config durable (atomic 0600); runtime/eval/history in-memory only; no store abstraction |
| 44 | Performance measurement of decision paths | P (baseline infra) | stress/soak suites exist; decision-path benchmarks absent |
| 45 | Failure policy fail-open default | P | philosophy present (hedging abandonment, cache bypass); decision-level fail-open/fail-closed config missing |
| 46 | Configuration schema extensions | M | §13 |
| 47 | Backward compatibility | I (to preserve) | existing tests are the contract |
| 48 | Security review | P | SECURITY.md + implemented controls §14/§9; decision-specific review pending each phase |
| 49 | Phased implementation | I (process) | this document starts it |
| 50–51 | Tests / invariants | P | strong suite exists for current features; the listed decision/eval/shadow invariants are all M |
| 52 | Documentation | P (and C in places) | rich docs exist; staleness items §18 |
| 53 | Release discipline | I (process) | TEST_REPORT + CI gates |
| 54 | Implementation reports | N/A (per milestone) | this is the Phase A report |
| 55 | Acceptance criteria | P | items 1,2,3,30,31,47,50 partially satisfied at baseline |

---

# Current State → Gap → Required Delta

| # | Current state | Gap | Required delta |
|---|---|---|---|
| 1 | Aliases + `auto`/`claude-auto` select deployments | no stable client identity independent of backend pool | **Virtual Endpoint** entity: ID, display name, client model name, ingress protocol(s), client-auth policy, route profile ref, limits/budget/targets, provider allow/deny, task/privacy/decision-mode/fallback policy overrides, enabled flag. Resolution before `Requirements` build. `/v1/models` lists client model names. |
| 2 | One global strategy + per-model priority/weight | no reusable intent object | **Route Profile** entity (task policy, candidate pool ref, decision mode, objective weights, context policy, fallback chain, evaluation scorecard ref), referenced by endpoints; never contains physical model lists inline |
| 3 | Flat model matching | no named pools / chains | **Candidate Pool** + **Fallback Chain** entities with validation (no cycles, no dupes, disabled ignored, eligibility still authoritative) |
| 4 | Inline request flags | no normalized feature/task profile | `internal/feature` FeatureProfile (protocol, model/alias, est. context, tools count/tool-choice, vision, reasoning, structured-output/JSON-schema signals, code density, session, endpoint/profile IDs, latency/cost hints) + `internal/taskprofile` local bounded Task Analyzer with confidence; extensible task types |
| 5 | Composite score inside router | no separated model/provider/credential scoring, no task/quality dimension | `internal/policy` multi-objective engine consuming router eligible state + scorecards; hard vs soft separation; reason codes out |
| 6 | (none) | no decision abstraction | `internal/decision`: `DecisionProvider{ID, Capabilities, Health, Decide}`, `DecisionRequest/Result` (action, candidate ID, ranking, score, confidence, abstain, provider ID, reason codes, latency, fallback-used, metadata), task-type enum, orchestrator with chain + budget + confidence policy + privacy policy + cycle detection, eligible-set validator (reject-unknown, never reinsert), deterministic fallback adapter wrapping the existing router ordering |
| 7 | compat readiness scorecard | no quality scorecards/provenance | `internal/scorecards` versioned Model Intelligence scorecards: capabilities + quality dimensions + performance + reliability + cost + `evaluation{suite_version, evaluated_at, sample_count}` + **provenance** (operator config / imported / evaluation / production telemetry). Never fabricate. |
| 8 | Level B compat probes | no quality evaluation | `internal/eval` suites (coding, debugging, reasoning, tool-calling, structured output, long context, latency/TTFT/throughput, reliability, protocol compat); deterministic evaluators (compile/tests/JSON/schema/expected tool call) outrank any LLM judge; separate evaluation health namespace |
| 9 | (none) | no shadow/canary | `internal/shadow` bounded challenger execution (sample %, max cost, concurrency, privacy, timeout, evaluation method) that can never touch the client response; `internal/canary` lifecycle + explicit promotion policy + operator approval |
| 10 | Events with free-text messages | no structured route traces | `internal/routetrace` bounded store: endpoint, profile, task, confidence, candidates, selected, ranking, scores, reason codes, decision provider, decision latency/budget, fallbacks, final deployment; dry-run API + dashboard; metrics families of §35 |
| 11 | (none) | no supervision plane | `internal/supervision`: contracts + evidence schema + actions (ACCEPT/CONTINUE/RETRY/REVISE/FALLBACK/ESCALATE/ABORT) + Supervisor API/MCP boundary; explicitly NOT in the proxy path |
| 12 | Config sections §13 | missing planes' config | additive `virtual_endpoints`, `route_profiles`, `candidate_pools`, `decision` (enabled/mode/provider_chain/timeout/total_budget/max_external_calls/context_policy/fail_open/cache/providers), `evaluation`, `supervision` with safe defaults; migration = defaults only |
| 13 | Dashboard tabs §15 | missing plane views | tabs/cards per spec §36 + Test Decision/Dry Run UI |
| 14 | Docs labels/stale sections | trust erosion | fix during each phase's doc update (§18 list) |

# Proposed architecture (target integration — no second router)

```text
Client / Claude Code / Codex / Application
        ↓
Virtual Endpoint (stable client model: e.g. nexa-code)          [internal/config + httpapi resolution]
        ↓
Route Profile (e.g. coding-smart) → Candidate Pool / Fallback Chain
        ↓
Ingress + Request Requirements (existing)
        ↓
Hard Compatibility / Capability / Policy Filters (existing router+compat — AUTHORITATIVE)
        ↓
Eligible Candidate Pool          = router.Candidates(req)  []Scored
        ↓
Local Feature Extraction         [internal/feature]  (bounded, local, deterministic)
Task Analyzer                    [internal/taskprofile] (local first; external only if enabled+proven)
        ↓
Decision Orchestrator            [internal/decision]  (chain, budget, confidence, privacy, cycles)
   ┌────────────────────────────────────────────────────────┐
   │ Local Policy / Classifier / Rule Engine (always usable) │
   │ Optional external providers (Jev, custom HTTP, LLM judge)│
   │ Future learned router (rank eligible only)              │
   └────────────────────────────────────────────────────────┘
        ↓  (validated against the eligible set — invalid ⇒ reject + deterministic fallback)
Policy Engine                    [internal/policy] + Model Scorecards [internal/scorecards]
        ↓
Model Selection → Provider Selection → Credential Selection (existing adapter P2C)
        ↓
Existing NexaRoute Execution / Failover / Hedging / Repair (unchanged)
        ↓
Protocol Adapter → Upstream Model → Response → Telemetry
        ↓
Route Trace + Evaluation / Scorecard / Shadow Evidence

Separate path:  Coding Agent → plan/step/result evidence → Supervisor Plane [internal/supervision]
                → CONTINUE / RETRY / REVISE / FALLBACK / ACCEPT / ESCALATE / ABORT
```

Answer to spec §54 ("How can another DecisionProvider replace the current one without redesigning
NexaRoute?"): because the core only knows `DecisionProvider{ID, Capabilities, Health, Decide}` over
normalized `DecisionRequest`/`DecisionResult`; adapters translate any external primitive (Noul/
Choice/Score-style, HTTP/JSON, OpenAI/Anthropic structured output, local models) at their own
boundary; orchestrator, validator, policy engine, router and execution never change. Replacing Jev
(or any provider) = editing one adapter directory + config.

# Tests required (per master spec §50–51, mapped to phases)

- **B**: virtual-endpoint resolution (incl. stable client model identity, `/v1/models`), route-profile
  matching, named pools, fallback-chain cycle detection, config migration (old files load), admin
  CRUD validation, settings/dashboard round-trip.
- **C**: feature extraction bounds (fuzz pathological JSON), task classifier cases + confidence,
  telemetry fields; Decision-OFF equivalence.
- **D**: DecisionProvider contract tests, eligible-set invariant (property test: any result outside
  the set is rejected and never reinserted), unknown/excluded/disabled/unhealthy/incompatible
  candidate rejection, external timeout/malformed output/auth failure/rate limit, abstention,
  low confidence, decision budget (sum of provider timeouts ≤ deadline), context cancellation,
  deterministic fallback, fail-open vs explicit fail-closed.
- **E**: task suitability scoring, hard-vs-soft separation (property: quality score never overrides
  incompatibility), reason-code emission, profile weight application.
- **F** (if Jev/external lands): error taxonomy mapping, confidence normalization, privacy mode
  enforcement (METADATA_ONLY default — assert no raw prompt egress), secret redaction, SSRF guards
  (metadata IP, scheme allowlist, redirect policy, header validation, body bounds).
- **G**: chain ordering, per-chain policies (on_error/timeout/low_confidence/abstain/invalid/
  rate_limit/unhealthy), cycle detection, budget propagation to fallbacks, cooldown + half-open with
  stale-generation guard.
- **H**: scorecard provenance (every quality value carries source), no-fabrication test, deterministic
  evaluator > judge precedence, evaluation isolation (no production health pollution).
- **I**: shadow never replaces client response (invariant), sample/cost/concurrency bounds, canary
  lifecycle transitions + promotion policy enforcement (no auto-promote on one benchmark win).
- **J**: dry-run non-execution (no upstream call unless explicit), route-trace completeness,
  metric cardinality bounds.
- **K**: supervisor step budget, evidence-schema validation, chain termination.
- **Always**: existing suite green; race; stress additions for decision-path overhead (OFF / local /
  external / failing / slow / cached / shadow on-off as spec §44).

# Risk assessment (summary)

See §22. Top risks: hot-path attempt-loop changes (mitigate: one call-site seam + equivalence test),
PR #13 endpoint overlap (mitigate: coordinate before Phase B), affinity-key evolution, snapshot/JS
compatibility, metric cardinality, and evaluation/shadow cost leakage (mitigate: tagged contexts +
budgets + disabled-by-default).

# Implementation order (spec §49, adjusted for audit findings)

1. **Phase B — Virtual Endpoint foundation** (coordinate with PR #13 first): entities + config +
   resolution + pools/chains + admin API + tests. No AI.
2. **Phase C — Request/Task intelligence**: `internal/feature` + `internal/taskprofile` + telemetry.
3. **Phase D — Decision contracts + orchestrator + budget + validator + deterministic fallback**
   (wrap existing `Candidates()`; Decision OFF ≡ baseline).
4. **Phase E — Policy engine + reason codes**.
5. **Phase F — First external adapter** (Jev only if still requested; otherwise the generic
   HTTP/JSON or Local Classifier as the first optional provider). Isolated; health/timeout/
   normalization/confidence/error-class tests.
6. **Phase G — Provider chains + confidence/abstention/cooldown recovery**.
7. **Phase H — Scorecards + evaluation** (provenance; deterministic-first).
8. **Phase I — Shadow/Canary** (bounded, opt-in).
9. **Phase J — Dashboard/observability integration + Dry Run** (can start cards alongside E–I as
   each plane lands; finalize traces/metrics here).
10. **Phase K — Supervision foundation** (contracts/evidence/API only).
11. **Phase L — Learned routing research** (only on measured telemetry; keep deterministic/hybrid
    default if benefit unproven).

After every phase: `gofmt` · `go vet ./...` · `go test ./...` · full `scripts/verify.sh` ·
race · relevant stress · report results · fix regressions before proceeding.

# Intentionally not done in Phase A

No feature code, no config changes, no renames, no dependency additions. This document is the only
artifact, plus toolchain bootstrap notes for reproducing the baseline (`go1.23.x`; the repo has zero
external Go dependencies).
