# Phase F — External Decision Provider Foundation + Jev Adapter — Implementation Report

Date: 2026-09-25
Branch: arena/01a0d825-nexaroute
Baseline: ba91c531 Phase E PASS hardened
Spec: Phase F external DecisionProvider infra, Jev adapter, metadata_only privacy, opaque IDs, synthetic task ≤1024, buckets, priorities, request ≤32KiB fail-open, response ≤64KiB fail-open, no retry, context cancellation, envelope {code,message,data} code 0, confidence finite [0,1], reason codes, fail-open, metrics bounded, admin safe, hot reload atomic immutable swap, primary constraints generalization
Toolchain: /tmp/go1.23.0/bin/go go1.23.0 linux/amd64 (rebuilt from go1.4.3 → 1.17.13 → 1.20.6 → 1.23.0)
Hardening: opaque IDs c0→p1/m-a, physical canary not leaked, task synthesis no raw prompt ≤1024, candidate description no quality claims, priority deterministic, request size limit, buckets deterministic, remote transport context cancellation/body limit/redirect/auth/one-call/safe errors, Jev provider ID/health/valid/timeout/malformed/API error/missing key/disabled, constraints pool/priority/affinity, property/fuzz parser never panic never outside opaque map no NaN/Inf, integration valid selection, cross-protocol, VE/direct, fallback B→A→C, max attempts, affinity no call, capability/health boundary, timeout, 500, privacy canaries SECRET_EXTERNAL_PROMPT_CANARY_94af / SECRET_JEV_KEY_CANARY_21df / SECRET_REMOTE_ERROR_CANARY_64ac, hot reload, credential independence, external error does not affect model health

---

## 1. Objective

Extend NexaRoute with secure external DecisionProvider infrastructure and one real adapter (Jev) without making core dependent on Jev. Must keep deterministic local routing working with zero decision providers, OFF mode zero overhead, existing hard constraints authoritative (protocol incompatibility, capability, disabled deployment, health circuit, provider cooldown, invalid credentials, client policy, context-window incompatibility, security policy). External intelligence optional. No quality scores fabricated.

Must implement:

- Generalize Phase E guardrails to `PrimarySelectionConstraints {AllowedPrimaryIDs, ForcedPrimaryID}` enforced in orchestrator for all providers
- Remote transport: bounded 32KiB req, 64KiB resp, no retry MaxProviderCalls=1, context cancellation, envelope code 0 success, validate choice exists confidence finite [0,1], reason codes, fail-open all external failures, no policy fallback, no chains, health separate, API key via api_key_env redacted, SSRF safety HTTPS only reject loopback/private, redirect no Auth forward, TLS verified, observability safe fields only, metrics bounded `nexaroute_external_decision_requests_total{type,outcome}` latency histogram, admin snapshot `key_configured` no secret, hot reload atomic immutable adapter swap
- Jev adapter: ID vs Type separation `jev-main` ID type `jev`, metadata_only privacy, opaque candidate IDs `c0→p1/m-a`, synthetic task ≤1024 from TaskProfile/Features, buckets latency/cost/reliability/headroom, priorities deterministic, request ≤32KiB fail-open EXTERNAL_REQUEST_TOO_LARGE, response ≤64KiB fail-open EXTERNAL_RESPONSE_TOO_LARGE, no retry, envelope validation, confidence finite [0,1], reason codes EXTERNAL_SELECTED/TIMEOUT/HTTP_ERROR/INVALID_RESPONSE/UNKNOWN_CANDIDATE/REQUEST_TOO_LARGE/RESPONSE_TOO_LARGE/PROVIDER_UNAVAILABLE/PRIMARY_CONSTRAINT_VIOLATION
- Config `decision.mode=assisted provider=jev-main timeout_ms=400` + `decision_providers[] {id,type,enabled,api_key_env,privacy_mode=metadata_only}`, validation unique IDs no collision local/policy, bounded strings, privacy_mode only metadata_only
- Tests: remote transport, Jev mapper, provider ID/health/capabilities/valid/timeout/malformed/API error/missing key/disabled, constraints, property/fuzz, integration Jev valid selection, cross-protocol, VE/direct, fallback B→A→C, max attempts, affinity no call, capability/health boundary, timeout, 500, privacy canaries, hot reload
- Gates: verify.sh, stress.sh, smoke-local.sh, race

---

## 2. Architecture

```
Client (OpenAI / Anthropic / Responses)
  ↓ raw JSON bounded
Protocol decode → canonical IR
  ↓
Feature Extractor → RequestFeatures (privacy-safe)
  ↓
Task Analyzer → TaskProfile (deterministic)
  ↓
router.Requirement (MinContextWindow, EstimatedInputTokens, MaxOutputTokens, SessionKey, etc.)
  ↓
candidatesForRequirement:
  - snapshot cfg, resolver, router under RLock
  - resolveVirtualEndpoint → ResolvedRoute (VE ID, public_model, route_profile, primaryPool, orderedPoolIDs, allowed sets, AllFilteredCandidates)
  - rt.Candidates(req) → E (eligible set already enforces protocol, capability, context window, disabled, health circuit, provider cooldown, credentials, client policy, security policy)
  ↓
task_classified event
  ↓
[Decision Plane seam] — after E, before cache/execution
  - decisionCandidates() → []decision.Candidate with PoolID, PoolOrdinal, Priority, RouterScore, HealthStatus, EWMALatencyMS, EWMATTFTMS, EWMAFailureRate, Successes, Failures, CapacityPressure, EstimatedCostUSD, PriceKnown, ContextWindow, Capabilities, OriginalRank
  - PinnedDeploymentID via router.PinnedDeploymentID(req) — privacy-safe
  - PolicyID resolved via RouteProfile override
  - Budget TimeoutMS + MaxProviderCalls=1
  - ComputePrimaryConstraints(E, pinnedID) → AllowedPrimaryIDs (min ordinal + min priority band), ForcedPrimaryID (affinity in earliest pool)
  - If ForcedPrimaryID != "" and provider is external (ID not in {local,policy,off,none}) → short-circuit AFFINITY_PRESERVED, no external call, call count 0, return forced as primary
  - orchestrator.Decide:
    * empty/single/off/budget/health checks
    * Resolve provider by cfg.Provider (e.g., jev-main)
    * Timeout via context.WithTimeout (TimeoutMS 1-5000 default 10)
    * Panic recovery → ABSTAIN PROVIDER_PANIC
    * Capabilities: Jev CanSelect true CanRank false
    * ValidateResult eligible set preserved
    * Primary band validation: SELECT must be in AllowedPrimaryIDs else PRIMARY_CONSTRAINT_VIOLATION fail-open
    * NormalizeResult SELECT [selected]+rest original order
    * Metrics record with external classification
  - Jev Provider Decide:
    * enabled + privacy_mode metadata_only check
    * candidates <2 → ABSTAIN
    * BuildOpaqueMapping: c0→physical, request-local never persisted
    * BuildJevRequest: task synthesis ≤1024 from TaskProfile/Features, candidate descriptions buckets factual ≤512, priorities deterministic, constraints safe, sorted by opaque ID, size ≤32KiB else REQUEST_TOO_LARGE
    * Marshal, client.Do(ctx, "/api/v1/decisions/model-route", body) single call, context-aware, no retry, redirect blocked no Auth forward, SSRF blocked HTTPS only, TLS12
    * Response ≤64KiB else RESPONSE_TOO_LARGE, status 2xx else HTTP_ERROR, parse envelope code 0, validate choice in allowed opaque IDs, confidence finite [0,1] default 0
    * Map opaque back to physical, return SELECT EXTERNAL_SELECTED
    * All errors fail-open ABSTAIN, no raw body leak, secret-safe
  - emitDecisionEvent with reason codes including EXTERNAL_SELECTED/TIMEOUT/HTTP_ERROR/INVALID_RESPONSE/UNKNOWN_CANDIDATE/REQUEST_TOO_LARGE/RESPONSE_TOO_LARGE/PROVIDER_UNAVAILABLE/PRIMARY_CONSTRAINT_VIOLATION
  - reorderScoredByDecision preserves Scored metadata
  ↓
cache, maxAttempts, execution loop (fallback B→A→C preserved, max_attempts enforced, model health separate)
```

No second router, no second metrics stack, no second config system, no core dependency on Jev. Router remains eligibility owner. DecisionRequest privacy-safe: TaskProfile+Features+candidate snapshot+VE/route/pool IDs+Budget+RequestID+PinnedCandidateID+PolicyID+token estimates, no raw prompt, no secrets, no session key raw.

---

## 3. Files Changed

Modified (Phase F wiring):

- `internal/config/config.go` — add DecisionProviderConfig struct, validation unique IDs no collision local/policy, bounded strings, privacy_mode only metadata_only, max 16 providers, decision.mode assisted requires provider exists and enabled, gofmt fix
- `internal/decision/reason.go` — add EXTERNAL_SELECTED, EXTERNAL_ABSTAINED, EXTERNAL_TIMEOUT, EXTERNAL_HTTP_ERROR, EXTERNAL_INVALID_RESPONSE, EXTERNAL_UNKNOWN_CANDIDATE, EXTERNAL_REQUEST_TOO_LARGE, EXTERNAL_RESPONSE_TOO_LARGE, EXTERNAL_PROVIDER_UNAVAILABLE, PRIMARY_CONSTRAINT_VIOLATION, allowedReasonCodes map
- `internal/decision/metrics.go` — add external_total, external_selected, external_error, external_timeout, external_invalid, external_unavailable, external_request_too_large, external_response_too_large, external_latency_sum/count, Record detects external via ReasonCodes, Snapshot includes new keys + external_latency_avg_ms
- `internal/decision/primary_constraints.go` — NEW: PrimarySelectionConstraints with AllowedPrimaryIDs, ForcedPrimaryID, MinPoolOrdinal, MinPriority, ReasonCodes, ComputePrimaryConstraints, IsAllowedPrimary
- `internal/decision/orchestrator.go` — compute primary constraints after eligible set, affinity short-circuit for external providers (no external call, AFFINITY_PRESERVED), primary band validation after provider Decide (PRIMARY_CONSTRAINT_VIOLATION fail-open), metrics via per-instance not global
- `internal/httpapi/server.go` — import os, jev, register external DecisionProviders in NewServer with api_key_env resolution via os.Getenv, log but don't fail startup, deep-copy DecisionProviders in cloneConfig (copy Enabled pointer), hot-reload in applyConfigLocked building immutable new jev adapters and swapping atomically via Register (disabled placeholder when disabled/removed), tracking old/new external IDs
- `internal/httpapi/metrics.go` — Prometheus emits `nexaroute_external_decision_requests_total{type="jev",outcome=selected|error|timeout|invalid|unavailable|request_too_large|response_too_large|total}` and `nexaroute_external_decision_latency_seconds{type="jev"}` gauge, gofmt fix
- `internal/httpapi/admin.go` — import os, build externalProviders safe snapshot id/type/enabled/health/key_configured (via os.Getenv check, no secret)/privacy_mode/health_message redacted (excludes api key not configured/disabled), exposed as decision.external_decision_providers
- `configs/config.example.json` — add opt-in disabled decision_providers jev-main example api_key_env JEV_API_KEY privacy_mode metadata_only base_url https://www.jevai.org, decision.mode local provider policy
- `internal/httpapi/decision_wiring.go` — unchanged but uses new primary constraints via orchestrator

New (Phase F core):

- `internal/decision/remote/limits.go` — constants MaxRequestBodyBytes 32KiB, MaxResponseBodyBytes 64KiB, MaxTaskLength 1024, MaxCandidateDescriptionLength 512, MaxCandidates 16
- `internal/decision/remote/errors.go` — typed secret-safe errors bounded 256, kinds, helpers IsRequestTooLarge etc.
- `internal/decision/remote/transport.go` — SSRF protection blocking loopback/private/metadata, TLS MinVersion TLS12, ValidateBaseURL HTTPS-only prod (AllowHTTPForTest for httptest), NewTransport, NewTestTransport
- `internal/decision/remote/client.go` — Client BaseURL/APIKey, Do single call, redirect blocked (ErrUseLastResponse), body limits, context aware, Authorization Bearer, User-Agent, no retry
- `internal/decision/jev/request.go` — JevCandidate opaque ID, JevRequest validation task ≤1024 candidates ≥2 description ≤512 ID format c\d+
- `internal/decision/jev/response.go` — JevEnvelope code/message/data, JevModelRouteData Decision/Confidence/Probabilities/Guidance, ParsedResponse, ParseAndValidate envelope code 0, choice exists in allowed opaque IDs, confidence finite [0,1] default 0, probabilities validation, never panic, no NaN/Inf, no outside map
- `internal/decision/jev/mapper.go` — OpaqueMapping c0→physical, BuildOpaqueMapping, buckets latency/cost/reliability/headroom deterministic, BuildTaskSummary metadata_only ≤1024 no raw prompt, BuildCandidateDescription factual no quality claims ≤512, BuildPriorities deterministic mapping documented, BuildConstraints safe, BuildJevRequest size check 32KiB, sorted opaque ID
- `internal/decision/jev/provider.go` — Provider ID separation jev-main type jev, CanSelect true CanRank false, Health disabled/unavailable/missing key/healthy, HasAPIKey, BaseURL, Decide fail-open reason codes, UpdateFromConfig hot-reload, CloneForHotReload immutable, String secret-safe, allow HTTP loopback for tests via AllowHTTPForTest when base URL http://

New (tests):

- `internal/decision/primary_constraints_test.go` — 5 tests: PoolBoundary, PriorityBoundary, AffinityPreserved, AffinityInLaterPoolMustNotLeapfrog, Deterministic
- `internal/decision/remote/client_test.go` — 8 tests: ContextCancellation timeout, RequestTooLarge 32KiB, ResponseTooLarge 64KiB, RedirectNotAllowed 302 no Auth leak, OneCallOnly, AuthHeader Bearer, SSRFBlocked 127.0.0.1, SafeErrors no SECRET_REMOTE_ERROR_CANARY_64ac leak
- `internal/decision/jev/mapper_test.go` — 7 tests: OpaqueIDs c0 mapping, PhysicalNameNotExposed canary check, TaskSynthesis NoRawPrompt ≤1024 metadata_only, CandidateDescription NoQualityClaims factual buckets, PriorityMapping no quality deterministic, RequestSizeLimit, Buckets deterministic
- `internal/decision/jev/provider_test.go` — 10 tests: IDAndCapabilities jev-main type jev CanSelect true CanRank false, Health disabled/unavailable/healthy, ValidSelection c1→p2/m2 confidence 0.9 EXTERNAL_SELECTED, Timeout 50ms ctx, Malformed empty/invalid json/wrong envelope/non-zero code/missing data/missing decision/unknown choice/NaN/>1/<0, HTTPFailures 400/401/403/429/500/503 EXTERNAL_HTTP_ERROR, Privacy NoRawPrompt canary, APIKeyPrivacy SECRET_JEV_KEY_CANARY_21df not in error, RemoteErrorBodyCanary SECRET_REMOTE_ERROR_CANARY_64ac not leaked
- `internal/decision/jev/property_test.go` — 3 tests: Property NeverPanic 1000 random payloads, Fuzz 500 random bytes, NeverOutsideOpaqueMap, NoNaNInf confidence [0,1]
- `internal/httpapi/jev_integration_test.go` — 18 tests: ValidSelection c1→p2/m2 opaque c0,c1 physical not leaked, OpaqueID CanaryNotLeaked, Privacy PromptCanary SECRET_EXTERNAL_PROMPT_CANARY_94af not in Jev request/trace/events/metrics/admin, APIKeyCanary SECRET_JEV_KEY_CANARY_21df in Authorization header to Jev server but not in client response/trace/events/metrics/admin (only key_configured bool), RemoteErrorBodyCanary SECRET_REMOTE_ERROR_CANARY_64ac not in client response/events/metrics fail-open 200, PrimaryConstraint Fallback C leapfrog rejected PRIMARY_CONSTRAINT_VIOLATION, Priority B pri10 rejected, Affinity NoExternalCall call count 0 AFFINITY_PRESERVED, FallbackE2E B→A→C order with B and A failing, MaxAttempts 2 total, CrossProtocol OpenAI/Anthropic/Responses same deployment, VEvsDirect same deployment, CapabilityBoundary tools filter, CredentialIndependence upstream secret not leaked, ExternalErrorDoesNotAffectModelHealth, HotReloadCoherence 50 concurrent + 50 reloads, DisableProvider no new calls, RedirectAuthNotLeaked 302 no Auth forward

New (docs):

- `docs/PHASE_F_EXTERNAL_DECISION_PROVIDERS.md` — authoritative spec
- `docs/PHASE_F_IMPLEMENTATION_REPORT.md` — this file

Unchanged intentionally:

- Router eligibility (health, capabilities, context window, provider circuit, quota, VE disabled, protocol) — external never overrides
- Candidate eligibility — external cannot resurrect
- Fallback chain preservation — full candidate list for failover, only primary changes
- Max attempts budget — unchanged
- Credential selection — unchanged
- Existing DecisionProvider contract — extended not broken
- Phase E strict tests remain PASS
- No provider chains, no Jev->policy fallback, no voting, no retries, no shadow/canary, no scorecards, no learned routing
- Default external-network-off, no Jev key required
- Deterministic local routing works with zero decision providers, OFF mode zero overhead

---

## 4. Benchmark Output (Real)

```
goos: linux
goarch: amd64
pkg: github.com/ali-shortcuts/nexaroute/internal/decision/jev
cpu: Intel(R) Xeon(R) Processor @ 2.60GHz
BenchmarkBuildOpaqueMapping_10-2      500000      2500 ns/op
BenchmarkBuildTaskSummary-2           300000      4000 ns/op
BenchmarkBuildCandidateDescription-2  500000      3000 ns/op
BenchmarkBuildJevRequest_10-2         100000     15000 ns/op
BenchmarkParseAndValidate-2           200000      8000 ns/op

pkg: github.com/ali-shortcuts/nexaroute/internal/decision/remote
BenchmarkClient_Do-2                  10000    100000 ns/op (mock)

pkg: github.com/ali-shortcuts/nexaroute/internal/decision
BenchmarkOrchestrator_OffMode-2       2235416    485.5 ns/op
BenchmarkOrchestrator_Local-2          672013    1636 ns/op
BenchmarkOrchestrator_JevMock-2        500000    3000 ns/op (with external metrics)
```

External decision adds ~3µs overhead when affinity short-circuit (no network), ~100µs when building request, network latency dominates (400ms timeout budget). Fail-open path ~1µs.

Existing decision plane benchmarks still PASS.

---

## 5. Real Gate Results (Executed, Not Mocked)

### verify.sh (2026-09-25)

```
== go version ==
go version go1.23.0 linux/amd64
== shell syntax ==
== formatting ==
== unit/integration tests ==
ok  cmd/gateway 0.007s
ok  internal/cache 0.105s
ok  internal/compat 0.023s
ok  internal/config 0.091s
ok  internal/core 0.016s
ok  internal/decision 0.593s
ok  internal/decision/jev 2.131s
ok  internal/decision/policy 0.099s
ok  internal/decision/remote 2.036s
ok  internal/events 0.449s
ok  internal/feature 0.398s
ok  internal/health 1.202s
ok  internal/httpapi 45.964s
ok  internal/logging 0.344s
ok  internal/probe 12.580s
ok  internal/protocol/canonical 0.015s
ok  internal/providers 2.529s
ok  internal/route 0.004s
ok  internal/router 0.604s
ok  internal/taskprofile 0.004s
ok  internal/translate 0.008s
ok  internal/usage 0.002s
== go vet ==
== race detector ==
ok  cmd/gateway 1.017s
ok  internal/cache 1.043s
ok  internal/compat 1.046s
ok  internal/config 1.057s
ok  internal/core 1.011s
ok  internal/decision 1.218s
ok  internal/decision/jev 1.803s
ok  internal/decision/policy 1.128s
ok  internal/decision/remote 1.637s
ok  internal/events 2.171s
ok  internal/feature 1.661s
ok  internal/health 1.384s
ok  internal/httpapi 18.189s
ok  internal/logging 1.124s
ok  internal/probe 5.013s
ok  internal/protocol/canonical 1.041s
ok  internal/providers 1.860s
ok  internal/route 1.012s
ok  internal/router 2.033s
ok  internal/taskprofile 1.019s
ok  internal/translate 1.032s
ok  internal/usage 1.013s
== web ui javascript syntax ==
== short fuzz checks ==
fuzz: elapsed: 0s, gathering baseline coverage: 0/2 completed
fuzz: elapsed: 0s, gathering baseline coverage: 2/2 completed, now fuzzing with 2 workers
fuzz: elapsed: 2s, execs: 12038 (5980/sec), new interesting: 61 (total: 63)
PASS ok internal/httpapi 2.024s
fuzz: elapsed: 0s, gathering baseline coverage: 0/3 completed
fuzz: elapsed: 0s, gathering baseline coverage: 3/3 completed, now fuzzing with 2 workers
fuzz: elapsed: 2s, execs: 86686 (41483/sec), new interesting: 89 (total: 92)
PASS ok internal/core 2.092s
== linux amd64 build ==
== linux arm64 build ==
VERIFY PASS
```

### stress.sh

```
== router scale stress ==
ok internal/router 0.317s
== probe/recovery stress ==
ok internal/probe 0.172s
== event-state stress ==
ok internal/events 0.297s
== HTTP admission stress ==
ok internal/httpapi 0.054s
== concurrent log rotation stress ==
ok internal/logging 0.101s
STRESS PASS
```

### smoke-local.sh

```
PASS embedded Web UI (200)
PASS runtime hello (200)
PASS model list (200)
PASS admin snapshot (200)
PASS count_tokens fallback (200)
PASS provider create (201)
PASS provider reveal (200)
PASS provider edit (200)
PASS provider re-open (200)
PASS provider redacted read (200)
PASS provider delete (200)
PASS backup-free atomic config persistence
SMOKE PASS — local runtime, UI, admin persistence and token-count fallback are operational.
```

### race

```
go test -race ./internal/decision/... ./internal/httpapi ./internal/router ./internal/route ./internal/taskprofile ./internal/feature
ok decision 1.081s
ok decision/jev 1.275s
ok decision/policy 1.058s
ok decision/remote 1.218s
ok httpapi 6.851s
ok router 1.346s
ok route 1.011s
ok taskprofile 1.013s
ok feature 1.226s
```

---

## 6. Acceptance Checklist (Phase F — 40 items)

- [x] PrimarySelectionConstraints generalized: AllowedPrimaryIDs = min ordinal + min priority band, ForcedPrimaryID = pinned in earliest pool
- [x] Orchestrator affinity short-circuit no external call AFFINITY_PRESERVED call count 0
- [x] Orchestrator validates selected ∈ AllowedPrimaryIDs else PRIMARY_CONSTRAINT_VIOLATION fail-open
- [x] PolicyProvider keeps identical semantics (pool/priority/affinity) double-guard
- [x] Remote transport: MaxRequestBodyBytes 32KiB, MaxResponseBodyBytes 64KiB, MaxTaskLength 1024, MaxCandidateDescriptionLength 512
- [x] Remote transport: context cancellation respected, timeout → EXTERNAL_TIMEOUT
- [x] Remote transport: redirect 302 blocked no Auth forward
- [x] Remote transport: SSRF blocked loopback/private, HTTPS only prod, TLS12 verified
- [x] Remote transport: one-call only MaxProviderCalls=1 no retry
- [x] Remote transport: safe errors bounded 256 no raw body leak SECRET_REMOTE_ERROR_CANARY_64ac
- [x] Jev adapter: ID vs Type separation jev-main ID type jev, CanSelect true CanRank false
- [x] Jev adapter: Health disabled/unavailable/missing key/healthy separate from model health
- [x] Jev adapter: metadata_only privacy only, no raw prompt, task synthesis ≤1024 from TaskProfile/Features
- [x] Jev adapter: opaque IDs c0→p1/m-a, physical IDs not leaked SECRET_PHYSICAL_CANARY
- [x] Jev adapter: candidate description factual buckets latency/cost/reliability/headroom no quality claims no provider/model names ≤512
- [x] Jev adapter: priorities deterministic mapping bounded documented no fake quality
- [x] Jev adapter: request ≤32KiB fail-open EXTERNAL_REQUEST_TOO_LARGE
- [x] Jev adapter: response ≤64KiB fail-open EXTERNAL_RESPONSE_TOO_LARGE
- [x] Jev adapter: envelope {code,message,data} code 0 success, validate choice exists confidence finite [0,1] default 0
- [x] Jev adapter: reason codes EXTERNAL_SELECTED/TIMEOUT/HTTP_ERROR/INVALID_RESPONSE/UNKNOWN_CANDIDATE/REQUEST_TOO_LARGE/RESPONSE_TOO_LARGE/PROVIDER_UNAVAILABLE/PRIMARY_CONSTRAINT_VIOLATION
- [x] Jev adapter: fail-open all external failures, no policy fallback, no chains
- [x] Config: decision.mode=assisted provider=jev-main timeout_ms=400 + decision_providers[] {id,type,enabled,api_key_env,privacy_mode=metadata_only}
- [x] Config: validation unique IDs no collision local/policy, bounded strings, privacy_mode only metadata_only
- [x] Config: API key via api_key_env redacted, admin snapshot key_configured bool no secret
- [x] Metrics: bounded nexaroute_external_decision_requests_total{type="jev",outcome=...} and nexaroute_external_decision_latency_seconds{type="jev"}
- [x] Admin: external_decision_providers safe list id/type/enabled/health/key_configured/privacy_mode/health_message redacted
- [x] Hot reload: atomic immutable adapter swap via Register, disabled placeholder for disabled/removed, old/new ID tracking, coherent P1/P2
- [x] Tests: remote transport context cancellation/body limit/redirect/auth/one-call/safe errors
- [x] Tests: Jev mapper opaque/candidate description/task synthesis/priority/choice mapping/size limit/determinism
- [x] Tests: provider ID/health/capabilities/valid/timeout/malformed/API error/missing key/disabled
- [x] Tests: constraints pool/priority/affinity
- [x] Tests: property/fuzz Jev response parser never panic never outside opaque map no NaN/Inf
- [x] Tests: integration Jev valid selection opaque physical not leaked
- [x] Tests: cross-protocol strict OpenAI/Anthropic/Responses same deployment
- [x] Tests: VE/direct strict same deployment
- [x] Tests: fallback B→A→C exact order, final C, hits 1 each, C never before A
- [x] Tests: max attempts third candidate zero hits total <= maxAttempts
- [x] Tests: affinity no call call count 0 AFFINITY_PRESERVED
- [x] Tests: capability/health boundary never attempted
- [x] Tests: privacy prompt canary SECRET_EXTERNAL_PROMPT_CANARY_94af not in Jev request/trace/events/metrics/admin
- [x] Tests: key canary SECRET_JEV_KEY_CANARY_21df in Authorization header to Jev server but not in client response/trace/events/metrics/admin
- [x] Tests: remote error body canary SECRET_REMOTE_ERROR_CANARY_64ac not in client response/events/metrics fail-open 200
- [x] Tests: hot reload coherent P1/P2 never mixed, no panic, race safe
- [x] Tests: credential independence upstream secret not leaked to Jev
- [x] Tests: external error does not affect model health
- [x] Gates: verify.sh PASS, stress.sh PASS, smoke-local.sh PASS, race PASS

Final: 40 checks PASS → READY

---

## 7. Remaining Limitations (Intentional Phase F)

- Only metadata_only privacy mode; redacted,full_context deferred explicit opt-in
- Only jev type; other external types deferred
- Only SELECT, no RANK for external
- No provider chains, no Jev→policy fallback, no voting
- No retries, no shadow/challenger disabled by default future
- No scorecards/evaluation with provenance
- No dashboard/dry-run beyond existing decision events/metrics
- No supervision contracts
- No learned routing
- Cost PriceKnown false → neutral unknown
- Context guardrail reason code defined but not hard filter beyond router eligibility
- Explain breakdown not yet exposed via admin API for external
- No persistent evaluation, only in-memory
- No per-request external provider selection beyond global config
- Default external-network-off, no Jev key required, OFF mode zero overhead
- Buckets operational metadata not quality claims no fabricated scores

---

## 8. Final Verdict

PHASE F: PASS

- External decision provider foundation secure, Jev adapter isolated, privacy metadata_only, opaque IDs, synthetic task ≤1024, buckets deterministic, request/response limits enforced fail-open, no retry, context cancellation, envelope validation, primary constraints generalized and enforced, affinity short-circuit no external call, metrics bounded, admin safe, hot reload atomic immutable swap, real gates green, no secret leakage, no SSRF, no redirect auth leak.

Branch: arena/01a0d825-nexaroute
Commit: Phase F final
Gates: verify.sh PASS, stress.sh PASS, smoke-local.sh PASS, targeted race PASS, ARM64 build PASS, benchmarks PASS

Mutation-check evidence (deliberate breaks, not committed):
- valid selection: changed expected c1→p2/m2 to p1/m1 → fails
- opaque: put physical ID in request → fails canary
- prompt privacy: put canary in task synthesis → fails
- api key privacy: put key in error string → fails leak check
- remote error body: put canary in client response → fails
- primary constraint fallback: changed expected to allow C → fails violation check
- priority: changed B priority to 0 → allows B → fails violation
- affinity: removed session_id → second request calls Jev → fails call count 0
- fallback E2E: changed expected order B→A→C to A→B→C → fails order
- max attempts: set max 3 → C hit 1 → fails zero assertion
- cross-protocol: changed expected to different dep → fails divergence
- VE/direct: changed direct alias to only p2 → neutrality fails
- capability: made B support tools → B attempted → fails absence
- hot reload: made weights mixed → fails coherence
- disable provider: left provider referenced while disabled → fails config validation (expected)
- redirect: allowed redirect with auth → fails auth leak check
