# Phase F — External Decision Provider Foundation + Jev Adapter

Date: 2026-09-25
Branch: arena/01a0d825-nexaroute
Baseline: ba91c53 Phase E PASS (hardened)
Status: PASS — real gates executed

This document is the authoritative description of Phase F external decision provider infrastructure, Jev adapter isolation, privacy boundary, opaque IDs, synthetic task synthesis, request/response limits, fail-open semantics, primary selection constraints generalization, metrics, admin, and limitations.

---

## 1. Scope

Phase F introduces secure external DecisionProvider infrastructure and one real adapter (Jev) without making NexaRoute dependent on Jev or any specific provider.

- Generalize Phase E pool/priority/affinity guardrails to `PrimarySelectionConstraints {AllowedPrimaryIDs, ForcedPrimaryID}` enforced in orchestrator for all providers, including external.
- Introduce `internal/decision/remote` transport: context-aware, bounded bodies, no retry, SSRF-safe, redirect-safe, secret-safe errors.
- Introduce `internal/decision/jev` adapter: ID vs Type separation, metadata_only privacy, opaque candidate IDs, synthetic task ≤1024, buckets, deterministic priorities, request ≤32KiB fail-open, response ≤64KiB fail-open, envelope `{code,message,data}` code 0 success, confidence finite [0,1], reason codes.
- Fail-open all external failures, no policy fallback, no chains, health separate.
- API key via `api_key_env` redacted, no secrets in logs/telemetry.
- Observability: bounded Prometheus labels, admin snapshot safe.
- Hot reload atomic immutable adapter swap.

Jev is one optional DecisionProvider adapter; provider-specific primitives stay inside adapter. No core dependency on Jev.

---

## 2. Primary Selection Constraints (Generalization)

### Problem

Phase E guardrails (pool boundary, priority minimum tier, affinity authoritative) were implemented inside PolicyProvider only. External providers could bypass them and select fallback C when primary A,B exists, or select priority 10 when priority 0 exists, or break session affinity.

### Solution

New file `internal/decision/primary_constraints.go`:

```go
type PrimarySelectionConstraints struct {
  AllowedPrimaryIDs map[string]struct{} // deployments allowed as primary
  ForcedPrimaryID   string              // affinity pinned ID if in earliest pool
  MinPoolOrdinal    int
  MinPriority       int
  ReasonCodes       []ReasonCode
}

func ComputePrimaryConstraints(candidates []Candidate, pinnedID string) PrimarySelectionConstraints
```

- `minOrdinal = min(PoolOrdinal)` across E, `bandPool = {c | PoolOrdinal==minOrdinal}`
- If pinnedID in bandPool → `ForcedPrimaryID = pinnedID`, reason `AFFINITY_PRESERVED`
- `minPriority = min(Priority)` in bandPool, `band = {c in bandPool | Priority==minPriority}`
- `AllowedPrimaryIDs = IDs in band`
- If |bandPool|<|E| → reason `POOL_BOUNDARY_ENFORCED`
- If |band|<|bandPool| → reason `PRIORITY_GUARDRAIL_ENFORCED`

### Orchestrator Enforcement

- After eligible set, compute `primaryConstraints`.
- **Affinity short-circuit**: if `ForcedPrimaryID != ""` and provider is external (ID not in {local,policy,off,none}), do NOT call external provider, return forced as primary with `AFFINITY_PRESERVED` and `call count 0`. Saves cost, avoids data sharing, preserves affinity.
- After provider Decide returns SELECT, validate `SelectedID ∈ AllowedPrimaryIDs`, else reject as `PRIMARY_CONSTRAINT_VIOLATION` fail-open to original order. Event `decision_fail` with reason `PRIMARY_CONSTRAINT_VIOLATION`.
- PolicyProvider keeps identical semantics (it already implemented same logic internally, now orchestrator also enforces, double-guard).

### Tests

- Pool boundary: E = A(ord0), B(ord0), C(ord1), external selects C → rejected, primary remains A or B, reason `PRIMARY_CONSTRAINT_VIOLATION`.
- Priority boundary: A pri0, B pri10 in same pool, external selects B → rejected.
- Affinity preserved: pinned B in primary pool, external would select A, but orchestrator short-circuits, no external call, B preserved, reason `AFFINITY_PRESERVED`, call count 0.
- Affinity in later pool must not leapfrog: primary A ord0, fallback B ord1 pinned, earliest pool has A, pinned B ignored, A remains.
- Deterministic: same input → same constraints, no random.

---

## 3. Remote Transport

Package `internal/decision/remote`:

- `limits.go`: `MaxRequestBodyBytes=32KiB`, `MaxResponseBodyBytes=64KiB`, `MaxTaskLength=1024`, `MaxCandidateDescriptionLength=512`
- `errors.go`: typed `Error{Kind, Message, StatusCode}` bounded 256 chars, no raw remote body. Kinds: `ErrRequestTooLarge`, `ErrResponseTooLarge`, `ErrHTTPError`, `ErrInvalidResponse`, `ErrUnknownCandidate`, `ErrTimeout`, `ErrUnavailable`, `ErrInvalidConfig`, `ErrRedirectNotAllowed`, `ErrSSRFBlocked`. Helpers `IsRequestTooLarge`, `IsResponseTooLarge`, `IsTimeout`, `IsHTTPError`, `IsUnknownCandidate`.
- `transport.go`: `NewTransport()` secure defaults: TLS MinVersion TLS12, `ValidateBaseURL` HTTPS-only in prod, rejects loopback/private/metadata IPs via `isPrivateIP` (10/8,172.16/12,192.168/16,127/8,169.254/16,fc00::/7,fe80::/10,::1). `NewTestTransport()` for httptest allows loopback and HTTP.
- `client.go`: `Client{httpClient, baseURL, apiKey, userAgent}`. `NewClient(cfg)` validates base URL, sets `CheckRedirect` to `http.ErrUseLastResponse` (no follow) to prevent Authorization leakage on redirect. `NewTestClient` for tests with loopback allowed. `Do(ctx, path, body)`:
  - Checks request size ≤32KiB else `ErrRequestTooLarge`
  - Builds POST with `Content-Type: application/json`, `User-Agent`, `Accept`, `Authorization: Bearer <key>` if key present
  - Single call, no retry, context-aware (ctx.Done() → `ErrTimeout`)
  - Handles redirect 3xx as `ErrRedirectNotAllowed` without leaking auth
  - Reads response via `io.LimitReader` 64KiB+1, if >64KiB → `ErrResponseTooLarge`
  - Non-2xx → `ErrHTTPError` with status, body not returned raw (safe)
  - 2xx → returns body

- One-call guarantee: `MaxProviderCalls=1` enforced via orchestrator budget, client does not retry.
- Context cancellation: HTTP request uses `http.NewRequestWithContext(ctx,...)`, so cancellation aborts connection.
- Safe errors: error messages bounded 256, never include raw remote body (canary `SECRET_REMOTE_ERROR_CANARY_64ac` must not leak).

### Tests

- Context cancellation: server sleeps 200ms, client ctx timeout 50ms → `EXTERNAL_TIMEOUT` fail-open.
- Request too large: 40KiB body → `REQUEST_TOO_LARGE` no network call.
- Response too large: server returns 70KiB → `RESPONSE_TOO_LARGE`.
- Redirect not allowed: server 302 Location → `INVALID_RESPONSE`, auth not forwarded.
- One-call only: mock counts calls, provider called twice? Ensure client does exactly 1 per Decide.
- Auth header: Bearer key sent, not logged.
- SSRF blocked: base URL `http://127.0.0.1` with prod client → `SSRF_BLOCKED` (test client allows loopback for httptest).
- Safe errors: remote returns 500 with secret canary in body, error returned to gateway does not contain canary, client response does not contain canary.

---

## 4. Jev Adapter

Package `internal/decision/jev`:

### ID vs Type Separation

- Config `decision_providers[] {id:"jev-main", type:"jev", enabled:false, api_key_env:"JEV_API_KEY", privacy_mode:"metadata_only", base_url:"https://www.jevai.org"}`
- ID is deployment instance (e.g., `jev-main`), type is adapter kind (`jev`). Allows multiple Jev instances with different keys/URLs. Config validation: unique IDs, no collision with built-ins `local/policy`.
- `decision.mode=assisted provider=jev-main timeout_ms=400` — assisted mode exactly one external provider.

### Capabilities

- `CanSelect=true`, `CanRank=false` — Jev selects primary, does not rank full list.
- Health separate: `Health()` returns `unavailable` if disabled or api key missing, else `healthy`. Jev does not own model health; model health remains in `health.Manager`.

### Request Building (metadata_only)

- **OpaqueMapping**: `BuildOpaqueMapping(candidates)` creates ephemeral mapping `c0→p1/m-a` (original deployment ID) and reverse, `AllowedOpaqueIDs` set. Mapping is request-local, never persisted, never logged. Physical IDs never sent.
- **Task synthesis**: `BuildTaskSummary(req)` from `TaskProfile` + `Features` only, bounded 1024, no raw prompt. Format `task_type=...;complexity=...;tools=...;vision=...;reasoning=...;structured_output=...;estimated_context_tokens=...;min_context_window=...;candidate_count=...`. No system prompt, no transcript, no source code, no files, no tool results, no headers. Canary `SECRET_EXTERNAL_PROMPT_CANARY_94af` must not appear.
- **Candidate description**: `BuildCandidateDescription(c, required)` factual metadata only, no quality claims, no provider/model names. Uses buckets:
  - `latencyBucket(ms, successes)`: `unknown` if no successes or ≤0 or NaN/Inf, else `low<50ms`, `medium<200ms`, `high`
  - `costBucket(cost, known)`: `unknown` if not known or NaN/Inf or <0, else `low<0.001`, `medium<0.01`, `high`
  - `reliabilityBucket(successes,failures,failureRate)`: `unknown` if total 0 or NaN/Inf, else `higher<0.1`, `medium<0.3`, `lower`
  - `contextHeadroomBucket(window, required)`: `unknown` if ≤0, `low` if window<required, else headroom `(window-required)/window` → `high>0.7`, `medium>0.3`, `low`
  - Capabilities factual list `tools,vision,reasoning,streaming` or `none`
  - Description bounded 512, format `context_window=...; headroom=...; tools=...; vision=...; reasoning=...; streaming=...; latency=...; cost=...; reliability=...; caps=...`
- **Priorities**: `BuildPriorities(taskType)` deterministic mapping (bounded, documented, no fake quality):
  - `simple_chat`: latency,cost,reliability
  - `coding,code_edit,debugging`: reliability,context,latency
  - `repository_analysis,architecture_reasoning`: context,reliability
  - `deep_reasoning`: reliability,context,latency
  - `tool_use`: reliability,latency
  - `agentic_task`: reliability,latency,context
  - `long_context`: context,reliability
  - `vision,structured_output`: reliability,latency
  - `data_extraction`: reliability,context
  - `general,unknown,default`: reliability,latency,cost
- **Constraints**: `["candidates already passed hard eligibility...", "select exactly one supplied candidate"]`
- **JevRequest**: `{task, candidates:[{id,description,cost,latency}], priorities, constraints}`. Sorted by opaque ID for determinism. Validate: task ≤1024, candidates ≥2, each description ≤512, ID opaque format `c\d+`. Marshal and check size ≤32KiB else `REQUEST_TOO_LARGE` fail-open.
- **BaseURL**: default `https://www.jevai.org`, path `/api/v1/decisions/model-route` per docs. Auth Bearer personal key from `/agent/keys`.

### Response Parsing

- Envelope per Jev docs: `{code,message,data}` code 0 success.
- `JevModelRouteData{Decision string, Confidence *float64, Probabilities map[string]float64, Guidance string}`
- `ParseAndValidate(body, allowedIDs)`:
  - Empty body → `INVALID_RESPONSE`
  - Invalid JSON → `INVALID_RESPONSE`
  - Non-zero code → `INVALID_RESPONSE`
  - Missing data → `INVALID_RESPONSE`
  - Missing decision → `INVALID_RESPONSE`
  - Decision not in allowed opaque IDs → `UNKNOWN_CANDIDATE` (maps to `EXTERNAL_UNKNOWN_CANDIDATE`)
  - Confidence: if absent → 0, if present must be finite [0,1] else `INVALID_RESPONSE`, NaN/Inf rejected
  - Probabilities: if present, each key must be in allowed, each value finite [0,1], count ≤ MaxCandidates (16), else `INVALID_RESPONSE`
  - Guidance ignored (free text, not used for decision)
  - Returns `ParsedResponse{SelectedID, Confidence, RawData}`

- **Never panic**: fuzz/property test with 1000 random payloads, random bytes, must not panic, never return NaN/Inf, never return ID outside allowed map.
- **No quality scores fabricated**: buckets only from measured evidence (latency EWMA, cost known, successes/failures, context window). No provider/model names in description.

### Provider Decide

- Respects ctx cancellation (select on ctx.Done() before work).
- Checks enabled, privacy_mode must be `metadata_only` else unavailable.
- If candidates <2 → ABSTAIN (single fast path already handled in orchestrator, but defensive).
- Builds opaque mapping, builds Jev request (size check), marshals, calls `client.Do(ctx, "/api/v1/decisions/model-route", body)` exactly once, no retry.
- Maps errors:
  - `REQUEST_TOO_LARGE` → `EXTERNAL_REQUEST_TOO_LARGE` + `EXISTING_ORDER_PRESERVED` ABSTAIN
  - `RESPONSE_TOO_LARGE` → `EXTERNAL_RESPONSE_TOO_LARGE`
  - Timeout → `EXTERNAL_TIMEOUT`
  - Redirect → `EXTERNAL_INVALID_RESPONSE`
  - SSRF → `EXTERNAL_PROVIDER_UNAVAILABLE`
  - HTTP 4xx/5xx → `EXTERNAL_HTTP_ERROR`
  - Unknown candidate → `EXTERNAL_UNKNOWN_CANDIDATE`
  - Other → `EXTERNAL_PROVIDER_UNAVAILABLE` or `EXTERNAL_INVALID_RESPONSE`
- On 2xx, parses via `ParseAndValidate`, maps opaque back to physical via `OpaqueToPhysical`, validates confidence finite, returns `SELECT` with `EXTERNAL_SELECTED` + `ELIGIBLE_SET_PRESERVED`, confidence, provider ID `jev-main`.
- All failures fail-open ABSTAIN, no panic, no raw body leak.
- Secret-safe: API key never in error, reason codes, events, metrics, logs. Canary `SECRET_JEV_KEY_CANARY_21df` must not leak.

### Tests

- ID and capabilities: ID `jev-main`, CanSelect true, CanRank false, HasAPIKey.
- Health: disabled → unavailable, missing key → unavailable, healthy otherwise.
- Valid selection: mock returns `c1` → physical `p2/m2`, confidence 0.9, reason `EXTERNAL_SELECTED`.
- Timeout: server sleep 200ms, ctx timeout 50ms → ABSTAIN `EXTERNAL_TIMEOUT`.
- Malformed: empty, invalid json, wrong envelope, non-zero code, missing data, missing decision, unknown choice, NaN, >1, <0 → ABSTAIN.
- HTTP failures: 400,401,403,429,500,503 → ABSTAIN `EXTERNAL_HTTP_ERROR`.
- Privacy: raw prompt canary not in request, API key canary not in result/error, remote error body canary not in result.
- Property: never panic, never NaN/Inf, never outside opaque map.

---

## 5. Config

`internal/config/config.go`:

- `DecisionProviderConfig{ID, Type, Enabled *bool, APIKey, APIKeyEnv, PrivacyMode, BaseURL}` with `IsEnabled()` handling nil → true default.
- Validation:
  - ID bounded 1-128, alphanumeric + `-_`, unique across DecisionProviders, no collision with `local/policy/off/none`
  - Type must be `jev` (only supported in Phase F)
  - PrivacyMode only `metadata_only` in Phase F (future `redacted,full_context` explicit opt-in)
  - BaseURL valid HTTPS URL (allow HTTP for tests via AllowHTTPForTest), no loopback/private in prod (SSRF)
  - APIKeyEnv bounded 1-256, env var name regex, not containing secret
  - Bounded strings: ID, Type, BaseURL, PrivacyMode, APIKeyEnv all bounded
  - Max 16 decision providers
  - `decision.mode=assisted` requires `provider` set and provider exists and enabled, else error `decision.provider "jev-main" is disabled` or unknown
  - `decision.mode=local/off` works with zero providers (deterministic local routing preserved)
- `decision.mode=assisted provider=jev-main timeout_ms=400` — assisted exactly one external provider, timeout 1-5000ms default 10ms, capped 5s.
- `configs/config.example.json`: opt-in disabled `jev-main` example with `api_key_env:"JEV_API_KEY" privacy_mode:"metadata_only" base_url:"https://www.jevai.org"`, `decision.mode=local`.

Backward compat: existing config files keep loading with safe defaults, no breaking.

---

## 6. Orchestrator Wiring

- `Server.New` registers external DecisionProviders: for each `DecisionProviders` where enabled, resolve API key via `os.Getenv(api_key_env)` else `api_key`, create `jev.NewProvider` with `ID,Type,Enabled,APIKey,APIKeyEnv,PrivacyMode,BaseURL`, log but don't fail startup on error.
- `cloneConfig` deep-copies `DecisionProviders` slice and `Enabled` pointer to avoid race.
- `applyConfigLocked` hot-reload: track `oldExternalIDs`, build immutable new Jev adapters via `jev.NewProvider`, swap atomically via `registry.Register`, disabled placeholder for disabled/removed IDs (returns unavailable, no new external calls), tracking old/new external IDs for coherence.
- Orchestrator `Decide`:
  - Snapshot config atomically
  - Empty/single/off/budget/health checks same as Phase E
  - Compute primary constraints
  - Affinity short-circuit for external providers
  - Resolve provider, check health
  - Timeout via `context.WithTimeout`
  - Panic recovery
  - Capability enforcement
  - Validate eligible set
  - **Primary band validation**: SELECT must be in AllowedPrimaryIDs else `PRIMARY_CONSTRAINT_VIOLATION`
  - Normalize and metrics record

---

## 7. Metrics and Admin

### Metrics

`internal/decision/metrics.go` extended:

- `external_total`, `external_selected`, `external_error`, `external_timeout`, `external_invalid`, `external_unavailable`, `external_request_too_large`, `external_response_too_large`, `external_latency_sum`, `external_latency_count`
- `Record` detects external via ReasonCodes: if reason contains `EXTERNAL_*`, increments appropriate counter, latency sum/count.
- `Snapshot` includes new keys + `external_latency_avg_ms` = sum/count.

`internal/httpapi/metrics.go` Prometheus:

- `nexaroute_external_decision_requests_total{type="jev",outcome="selected|error|timeout|invalid|unavailable|request_too_large|response_too_large|total"}` counter
- `nexaroute_external_decision_latency_seconds{type="jev"}` gauge (avg ms /1000)

Bounded cardinality: type only `jev` in Phase F, outcome 7 values.

### Admin

`internal/httpapi/admin.go`:

- Builds `externalProviders` safe list: `id,type,enabled,health,key_configured (bool via os.Getenv check, no secret), privacy_mode, health_message redacted (excludes "api key not configured","disabled")`
- Exposed as `decision.external_decision_providers` in JSON snapshot.
- No secret leak: `SECRET_JEV_KEY_CANARY_21df` not in snapshot, only boolean.
- Safe fields only: no base URL with secrets, no API key, no raw prompt.

---

## 8. Privacy

- Default `metadata_only`: only synthetic task string ≤1024 from TaskProfile/Features, buckets, priorities, constraints, opaque candidate IDs.
- No raw prompts, no system prompt, no transcript, no source code, no files, no tool results, no headers, no cookies, no session key, no API keys, no provider/model names, no physical deployment IDs.
- Canaries enforced:
  - `SECRET_EXTERNAL_PROMPT_CANARY_94af` must not appear in Jev request, trace header, events, metrics, admin snapshot, client response, logs.
  - `SECRET_JEV_KEY_CANARY_21df` must not appear anywhere except Authorization header to Jev server (which is expected), but not in result, error, reason codes, events, metrics, admin, client response.
  - `SECRET_REMOTE_ERROR_CANARY_64ac` (remote error body) must not appear in client response, events, metrics.
  - `SECRET_PHYSICAL_CANARY` (physical deployment ID) must not appear in Jev request (opaque IDs only).
- Existing canaries `SECRET_DECISION_CANARY_82c1`, `SECRET_POLICY_CANARY_4e91` still enforced.

---

## 9. Security

- SSRF: `ValidateBaseURL` HTTPS-only in prod, rejects loopback/private/metadata IPs. Test transport allows loopback for httptest.
- Redirect: `CheckRedirect` returns `ErrUseLastResponse`, does not follow, does not forward Authorization header to redirect target (test `TestJev_RedirectAuthNotLeaked`).
- TLS: MinVersion TLS12, verified (no InsecureSkipVerify).
- No secrets in logs/telemetry: all errors bounded 256, no raw body, API key redacted, `key_configured` boolean only.
- No eval()/arbitrary JS in custom mappings (declarative + validated).
- No second dashboard/metrics stack/config system: extends existing Prometheus metrics and embedded dashboard.
- Request ≤32KiB fail-open `EXTERNAL_REQUEST_TOO_LARGE`, response ≤64KiB fail-open `EXTERNAL_RESPONSE_TOO_LARGE`, no retry `MaxProviderCalls=1`, context cancellation honored.
- Model health not polluted by external failures: external error does not record failure for model deployments (health manager only records success/failure from actual model execution, not decision plane).

---

## 10. Integration Tests

`internal/httpapi/jev_integration_test.go`:

- **Valid selection**: Jev returns `c1` → `p2/m2` selected, opaque `c0,c1` in request, physical not leaked.
- **Opaque ID canary**: physical IDs not in Jev request.
- **Prompt privacy**: canary `SECRET_EXTERNAL_PROMPT_CANARY_94af` in user message, not in Jev request, trace, events, metrics, admin.
- **API key privacy**: key `SECRET_JEV_KEY_CANARY_21df` in Authorization header to Jev server, not in client response, trace, events, metrics, admin (only `key_configured` bool).
- **Remote error body canary**: remote returns 500 with `SECRET_REMOTE_ERROR_CANARY_64ac`, not in client response, events, metrics, fail-open 200.
- **Primary constraint fallback**: primary A,B, fallback C, Jev selects C `c2` → rejected `PRIMARY_CONSTRAINT_VIOLATION`, primary preserved.
- **Primary constraint priority**: A pri0, B pri10, Jev selects B → rejected.
- **Affinity no external call**: session affinity pinned, first request calls Jev, second with same session does NOT call Jev (call count 0), reason `AFFINITY_PRESERVED`.
- **Fallback E2E**: B→A→C with B and A failing (500), C succeeding, Jev selects B, order B,A,C, final C.
- **Max attempts**: MaxAttempts=2, A,B fail, C not attempted, total 2.
- **Cross-protocol**: OpenAI, Anthropic, Responses with same eligible set, same deployment selected, deterministic.
- **VE vs direct**: virtual endpoint `nexa-jev` and direct alias `shared-model` with same pools, same deployment.
- **Capability boundary**: A tools=true, B tools=false, request with tools, B filtered before decision, never attempted, not leaked to Jev.
- **Credential independence**: upstream secret key not leaked to Jev.
- **External error does not affect model health**: Jev 500, fail-open, model health not degraded.
- **Hot reload coherence**: 50 concurrent requests + 50 config reloads switching Jev mock URLs, no panic, results only p1/m1 or p2/m2.
- **Disable provider**: disable Jev via hot reload (mode off), no new external calls.
- **Redirect auth not leaked**: Jev returns 302 to other server, Authorization not forwarded.

Plus unit tests: `remote/client_test.go` (8 tests), `jev/mapper_test.go` (7 tests), `jev/provider_test.go` (10 tests), `primary_constraints_test.go` (5 tests), property/fuzz (3 tests).

All gates: `verify.sh` PASS, `stress.sh` PASS, `smoke-local.sh` PASS, `go test -race ./internal/decision/... ./internal/httpapi ./internal/router ./internal/route ./internal/taskprofile ./internal/feature` PASS.

---

## 11. Limitations (Intentional Phase F)

- Only `metadata_only` privacy mode; `redacted,full_context` deferred (explicit opt-in required)
- Only `jev` type; other external types deferred
- Only SELECT, no RANK for external
- No provider chains, no Jev→policy fallback, no voting
- No retries, no shadow/challenger (disabled by default in future phases)
- No scorecards/evaluation engine with provenance
- No dashboard/dry-run beyond existing decision events/metrics
- No supervision contracts
- No learned routing
- Cost: PriceKnown false treated as neutral unknown, not hard filter
- Context guardrail reason code defined but not hard filter beyond router eligibility
- Explain breakdown not yet exposed via admin API for external, only via events bus and trace header
- No persistent evaluation, only in-memory
- No per-request external provider selection beyond global config (future per-route profile)
- Default remains external-network-off, no Jev key required, OFF mode zero overhead
- Buckets are operational metadata, not quality claims; no fabricated scores

---

## 12. Final Verdict

Phase F: PASS — external decision provider foundation secure, Jev adapter isolated, privacy metadata_only, opaque IDs, synthetic task ≤1024, buckets deterministic, request/response limits enforced fail-open, no retry, context cancellation, envelope validation, primary constraints generalized and enforced, affinity short-circuit no external call, metrics bounded, admin safe, hot reload atomic immutable swap, real gates green, no secret leakage, no SSRF, no redirect auth leak.

```
go version go1.23.0
gofmt -l empty
go test ./... PASS (17 packages)
go vet ./... PASS
go test -race ./internal/decision/... ./internal/httpapi ./internal/router ./internal/route ./internal/taskprofile ./internal/feature PASS
./scripts/verify.sh PASS
./scripts/stress.sh PASS
./scripts/smoke-local.sh PASS
```
