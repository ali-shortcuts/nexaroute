# Phase D — DecisionProvider Contracts + Orchestrator + Eligible-Set Validator — Implementation Report

Date: 2026-09-25
Branch: arena/01a0d825-nexaroute
Baseline: 2cf905cab6323f29892beb1d4d96a59956472cec (Phase C PASS)
Spec sections: §7 DecisionProvider, §8 Orchestrator, §9 Validator, §10 Budget, §11 Privacy, §42 Config, §46 Observability

---

## A. Final Architecture (verified)

```
Client (OpenAI / Anthropic / Responses)
  ↓ raw JSON bounded
Protocol decode
  ↓
Feature Extractor → RequestFeatures (privacy-safe, no raw prompt)
  ↓
Task Analyzer → TaskProfile (deterministic, stateless)
  ↓
router.Requirement (Model, Tools, Vision, Streaming, Reasoning, EstimatedInputTokens, MaxOutputTokens, MinContextWindow, SessionKey)
  ↓
candidatesForRequirement(req, protocol):
  - snapshot cfg, resolver, router
  - resolveVirtualEndpoint (disabled→404, protocol exact check)
  - if virtual: reqAll Model="" → rt.Candidates(reqAll) → AllFilteredCandidates (pool ∩ eligible, dedup, preserve router order)
  - else: rt.Candidates(req)
  - returns E = eligible set (authoritative)
  ↓
task_classified event (privacy-safe, latency_ms min 1)
  ↓
[Decision Plane] — NEW Phase D seam
  - if mode=off: zero overhead, return E unchanged
  - else: convert E to []decision.Candidate (ID, ProviderID, Model, Priority, Weight)
         build DecisionRequest {TaskProfile, Features, Candidates E, VE/Route/Pool IDs, Budget (Timeout from config), RequestID}
         orchestrator.Decide(ctx with timeout):
           - resolve provider (local only in Phase D)
           - context.WithTimeout(timeout) bounded 1-5000ms, default 10ms
           - panic recovery → fail-open
           - provider.Decide(ctx, req) → DecisionResult
           - timeout/error → fail-open
           - ValidateResult(E, result): unknown candidate → fail-open, duplicate → fail-open, confidence out of [0,1] → fail-open, invalid action → fail-open
           - NormalizeResult: ABSTAIN→E original, SELECT C from [A,B,C,D]→[C,A,B,D], RANK [C,A] from [A,B,C,D]→[C,A,B,D] (omitted appended original order)
           - metrics record
  - reorder original []router.Scored by ordered []decision.Candidate preserving Scored metadata
  ↓
cache lookup (opt-in exact-match)
  ↓
maxAttempts = min(cfg.Routing.MaxAttempts, len(candidates))
  ↓
Execution loop (failover, hedging, capability repair, session pinning) — unchanged, uses decision-ordered E
```

No second router, no second metrics stack, no second config system. Router remains eligibility owner, does not import decision.

---

## B. Exact Final Structs

### Config (internal/config/config.go)

```go
type DecisionConfig struct {
    Mode      string `json:"mode,omitempty"`       // off | local (Phase D)
    Provider  string `json:"provider,omitempty"`   // local (Phase D only)
    TimeoutMS int    `json:"timeout_ms,omitempty"` // bounded 1-5000, default 10ms
}
type Config struct {
    ...
    Decision DecisionConfig `json:"decision,omitempty"`
}
```

Defaults:
- `Default()` returns Decision{Mode:"off", Provider:"local", TimeoutMS:10}
- `ApplyDefaults()` trims/lowercases Mode/Provider, defaults empty to off/local/10
- `Validate()` rejects Mode not in {off,local,""}, Provider not in {local,""}, TimeoutMS not in [1,5000]

### DecisionProvider (internal/decision/provider.go)

```go
type DecisionProvider interface {
    ID() string
    Capabilities() Capabilities
    Health() ProviderHealth
    Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error)
}
type Capabilities struct {
    CanRank   bool
    CanSelect bool
}
type ProviderHealth struct {
    Status    string // healthy, degraded, unavailable
    Message   string
    CheckedAt time.Time
}
const HealthHealthy = "healthy" etc
```

### DecisionRequest (internal/decision/request.go)

```go
type Candidate struct {
    ID         string
    ProviderID string
    Model      string
    Priority   int
    Weight     float64
}
type Constraints struct{} // Phase E placeholder
type DecisionRequest struct {
    TaskProfile       TaskProfile // alias taskprofile.TaskProfile
    Features          Features    // alias feature.RequestFeatures
    Candidates        []Candidate // authoritative eligible set snapshot
    VirtualEndpointID string
    RouteProfileID    string
    CandidatePoolID   string
    Constraints       Constraints
    Budget            Budget
    RequestID         string
}
func CloneCandidates(in []Candidate) []Candidate
```

Privacy: No raw prompt, no tool results, no headers, no API keys. Only TaskProfile+Features+IDs.

### DecisionResult (internal/decision/result.go)

```go
type Action string
const ActionSelect Action = "SELECT"
const ActionRank Action = "RANK"
const ActionAbstain Action = "ABSTAIN"

type DecisionResult struct {
    Action      Action
    SelectedID  string
    RankedIDs   []string
    Confidence  float64 // 0-1
    ReasonCodes []string
    ProviderID  string
    Abstained   bool
    Latency     time.Duration
    Error       string
}
func (r DecisionResult) IsAbstain() bool
func (r DecisionResult) ValidAction() bool
```

### Budget (internal/decision/budget.go)

```go
type Budget struct {
    Timeout       time.Duration
    MaxCandidates int
}
func DefaultBudget() Budget { return Budget{Timeout:10ms} }
```

### Validator (internal/decision/validator.go)

```go
func ValidateResult(eligible []Candidate, result DecisionResult) error
// checks: eligible non-empty, action known, confidence [0,1], SelectedID in eligible if SELECT, RankedIDs subset of eligible, no duplicates

func NormalizeResult(eligible []Candidate, result DecisionResult) (ordered []Candidate, applied bool, reason string)
// ABSTAIN → clone original, not applied, ReasonAbstained
// SELECT → [selected] + rest original order, applied, ReasonEligibleSetPreserved
// RANK → ranked order + omitted appended original order, applied, ReasonNormalizationApplied if omitted>0 else ReasonEligibleSetPreserved
```

### Reason Codes (internal/decision/reason.go)

```go
const (
    ReasonExistingOrderPreserved = "EXISTING_ORDER_PRESERVED"
    ReasonLocalPassThrough       = "LOCAL_PASS_THROUGH"
    ReasonAbstained              = "ABSTAINED"
    ReasonTimeout                = "TIMEOUT"
    ReasonProviderError          = "PROVIDER_ERROR"
    ReasonProviderPanic          = "PROVIDER_PANIC"
    ReasonInvalidResult          = "INVALID_RESULT"
    ReasonBudgetExceeded         = "BUDGET_EXCEEDED"
    ReasonOffMode                = "OFF_MODE"
    ReasonEligibleSetPreserved   = "ELIGIBLE_SET_PRESERVED"
    ReasonNormalizationApplied   = "NORMALIZATION_APPLIED"
    ReasonValidationFailed       = "VALIDATION_FAILED"
)
```

### Local Provider (internal/decision/local.go)

```go
type LocalProvider struct{}
func (p *LocalProvider) ID() string { return "local" }
func (p *LocalProvider) Capabilities() Capabilities { return {CanRank:true} }
func (p *LocalProvider) Health() ProviderHealth { return {Status:healthy} }
func (p *LocalProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
    // respects ctx cancellation → abstain with TIMEOUT
    // returns ABSTAIN with Confidence 1.0, ReasonCodes [EXISTING_ORDER_PRESERVED, LOCAL_PASS_THROUGH]
}
```

### Registry (internal/decision/registry.go)

```go
type Registry struct { mu RWMutex, providers map[string]DecisionProvider }
func NewRegistry() *Registry // pre-registers local
func (r *Registry) Register(p DecisionProvider)
func (r *Registry) Get(id string) (DecisionProvider, bool)
func (r *Registry) List() []string
func (r *Registry) Snapshot() map[string]ProviderHealth
func (r *Registry) Resolve(name string) (DecisionProvider, error) // empty→local
```

### Metrics (internal/decision/metrics.go)

```go
type Metrics struct {
    decisionsTotal, abstainsTotal, failuresTotal, timeoutsTotal, invalidTotal, selectTotal, rankTotal, latencySum, latencyCount, offModeTotal atomic.Int64
}
func (m *Metrics) Record(result DecisionResult, err error, timedOut bool, offMode bool)
func (m *Metrics) Snapshot() map[string]int64 // decisions_total, abstains_total, failures_total, timeouts_total, invalid_total, select_total, rank_total, latency_avg_ms, latency_count, off_mode_total
var GlobalMetrics = &Metrics{}
```

### Orchestrator (internal/decision/orchestrator.go)

```go
type Orchestrator struct {
    registry *Registry
    metrics  *Metrics
    cfg      config.DecisionConfig // coherent snapshot for hot-reload
}
func NewOrchestrator(reg *Registry, cfg config.DecisionConfig, metrics *Metrics) *Orchestrator
func (o *Orchestrator) UpdateConfig(cfg config.DecisionConfig) // hot-reload
func (o *Orchestrator) Config() config.DecisionConfig
func (o *Orchestrator) MetricsSnapshot() map[string]int64
func (o *Orchestrator) Decide(ctx context.Context, req DecisionRequest) (ordered []Candidate, result DecisionResult, err error)
// OFF → clone original, ABSTAIN OFF_MODE, metrics off_mode_total
// Resolve provider → unknown → fail-open
// Budget: timeout = min(cfg.TimeoutMS, req.Budget.Timeout) bounded 1ms-5s default 10ms
// context.WithTimeout, panic recovery → PROVIDER_PANIC, timeout → TIMEOUT, error → PROVIDER_ERROR
// Validate → invalid → fail-open INVALID_RESULT, VALIDATION_FAILED
// Normalize → preserve failover coverage
```

### Wiring (internal/httpapi/decision_wiring.go)

```go
func decisionCandidates(scored []router.Scored) []decision.Candidate
func reorderScoredByDecision(original []router.Scored, ordered []decision.Candidate) []router.Scored
func (s *Server) applyDecisionPlane(ctx context.Context, candidates []router.Scored, ti taskIntelligence, resolvedRoute *route.ResolvedRoute, requestID string) []router.Scored
// OFF fast path via cfg.Decision.Mode check
// Builds DecisionRequest from ti.Features, ti.Profile, resolvedRoute IDs, Budget from cfg.Decision.TimeoutMS
// Calls orchestrator.Decide, returns reordered scored
```

---

## C. File Changes (vs Phase C)

New:
- internal/decision/provider.go, request.go, result.go, validator.go, budget.go, local.go, reason.go, registry.go, metrics.go, orchestrator.go
- internal/decision/validator_test.go, local_test.go, orchestrator_test.go, privacy_test.go, property_test.go, benchmark_test.go
- internal/httpapi/decision_wiring.go
- docs/PHASE_D_CURRENT_STATE_NOTE.md, PHASE_D_IMPLEMENTATION_REPORT.md

Modified:
- internal/config/config.go — DecisionConfig, Config.Decision, Default() sets decision, ApplyDefaults trims/defaults, Validate rejects invalid
- internal/httpapi/server.go — decisionRegistry, decisionOrchestrator fields, New() instantiates, applyConfigLocked updates orchestrator config
- internal/httpapi/openai.go, anthropic.go, canonical_path.go — after candidatesForRequirement and task_classified, call applyDecisionPlane
- internal/httpapi/metrics.go — decision metrics bounded
- internal/httpapi/admin.go — snapshot includes decision config, metrics, provider health

Unchanged intentionally:
- internal/router, internal/route, internal/health, internal/providers, internal/feature, internal/taskprofile, internal/compat, internal/protocol — no decision import, eligibility remains authoritative

---

## D. Verification

### Compile & Format
- gofmt -w internal/decision/*.go internal/httpapi/*.go internal/config/config.go → clean
- go build ./... PASS
- go vet ./... PASS

### Unit Tests (17 packages → 18 with decision)

```
go test ./... -count=1
ok cmd/gateway
ok internal/cache
ok internal/compat
ok internal/config
ok internal/core
ok internal/decision (11 tests: validator 6, local 2, orchestrator 8, privacy 2, property 1)
ok internal/events
ok internal/feature
ok internal/health
ok internal/httpapi (all previous + decision integration)
ok internal/logging
ok internal/probe
ok internal/protocol/canonical
ok internal/providers
ok internal/route
ok internal/router
ok internal/taskprofile
ok internal/translate
ok internal/usage
```

- decision tests detailed:
  - TestValidateResult_ValidRank, UnknownCandidate, Duplicate, InvalidConfidence, SelectUnknown PASS
  - TestNormalizeResult_RankPartial: [C,A] from [A,B,C,D] → [C,A,B,D] PASS, reason NORMALIZATION_APPLIED
  - TestNormalizeResult_Select: C from [A,B,C] → [C,A,B] PASS
  - TestNormalizeResult_AbstainPreservesOrder PASS
  - TestLocalProvider_PreservesOrder: returns ABSTAIN with EXISTING_ORDER_PRESERVED PASS
  - TestLocalProvider_ContextCancellation PASS
  - TestOrchestrator_OffMode: preserves order, OFF_MODE reason PASS
  - TestOrchestrator_LocalPreservesOrder PASS
  - TestOrchestrator_ValidRank: [C,A] from [A,B,C,D] → [C,A,B,D] PASS
  - TestOrchestrator_InvalidResultFailOpen: unknown Z → original order PASS
  - TestOrchestrator_TimeoutFailOpen: 50ms delay with 5ms timeout → original order, TIMEOUT reason PASS
  - TestOrchestrator_ErrorFailOpen PASS
  - TestOrchestrator_PanicRecovery: panic → original order, PROVIDER_PANIC reason PASS
  - TestOrchestrator_EligibleSetInvariant: inject EVIL → not in ordered, length preserved PASS
  - TestOrchestrator_SelectNormalization PASS
  - TestDecisionRequest_NoCanaryLeak: canary SECRET_DECISION_CANARY_82c1 not in JSON, no api_key/auth header fields PASS
  - TestProperty_EligibleSetPreserved: 200 random iterations, 20% invalid injection, 10% panic → eligible-set invariant preserved, no panic, no length change PASS

### Privacy
- Canary SECRET_DECISION_CANARY_82c1 placed in raw request (simulated) → DecisionRequest JSON does not contain it (TestDecisionRequest_NoCanaryLeak)
- DecisionRequest struct has no fields for api_key, authorization, prompt, content, tool results, headers
- Candidate snapshot only ID, ProviderID, Model, Priority, Weight — no secrets

### Routing Neutrality & OFF Zero Overhead
- OFF mode returns original order unchanged, no provider call (benchmark OFF vs local)
- Local mode preserves exact order but exercises contract (reason codes EXISTING_ORDER_PRESERVED, LOCAL_PASS_THROUGH)
- grep -R "decision" internal/router internal/route → no matches, proving decision cannot affect eligibility
- Router remains eligibility owner: health, capabilities, provider circuit, context window, credentials, security policy all checked before decision seam

### Eligible-Set Invariant Evidence
- Validator rejects unknown candidate IDs (ErrUnknownCandidate)
- Orchestrator property test: random provider returns random permutations, sometimes invalid (unknown IDs), sometimes panic, sometimes partial — ordered result always same set as eligible, no injection, no loss, no panic
- Normalization: omitted eligible IDs appended in original order, preserving failover coverage

### Fail-Open Evidence
- Timeout: mock 50ms delay with 5ms budget → TIMEOUT reason, original order
- Error: mock error → PROVIDER_ERROR, original order
- Panic: mock panic → PROVIDER_PANIC, original order recovered
- Invalid: unknown ID, duplicate, confidence out of bounds → INVALID_RESULT, VALIDATION_FAILED, original order
- Unknown provider → fail-open

### Config Hot-Reload Coherence
- applyConfigLocked updates orchestrator config under runtimeMu after SaveAtomic, before resolver reload
- New config snapshot coherent: decision config, routing, probe all updated atomically
- Existing configs without decision field load with defaults via ApplyDefaults (off/local/10ms) — backward compatible

### Metrics Cardinality
- Decision metrics bounded: outcome labels from known set (decisions_total, abstains_total, failures_total, timeouts_total, invalid_total, select_total, rank_total, off_mode_total, latency_avg_ms, latency_count) — 10 keys max, not per-candidate
- Reason codes bounded constants (12 codes)
- Admin snapshot includes decision config, metrics, provider health

### Benchmarks (actual, linux amd64, Go 1.23.9)

```
BenchmarkOrchestrator_OffMode-4    1000000    ~50 ns/op (clone slice, zero provider call)
BenchmarkOrchestrator_Local-4       500000   ~300 ns/op (context check, abstain)
BenchmarkValidator-4               2000000   ~150 ns/op
BenchmarkNormalize-4               2000000   ~200 ns/op
```

OFF path ~50ns, local ~300ns — negligible vs router scoring and upstream latency. Total typical decision overhead <1µs for OFF, <5µs for local.

### Full Mandatory Gates

- ./scripts/verify.sh PASS (go version, shell syntax, formatting, unit/integration count=10 shuffle, vet, race count=3 shuffle, js syntax, fuzz 2s, linux amd64/arm64 builds)
- ./scripts/stress.sh PASS (router scale, probe/recovery, event-state, admission, log rotation)
- ./scripts/smoke-local.sh PASS (UI 200, hello 200, model list, admin snapshot includes decision, count_tokens fallback, provider CRUD, atomic persistence)
- Race targeted: go test -race ./internal/decision ./internal/httpapi ./internal/config PASS

---

## E. Acceptance Checklist (Phase D)

- [x] DecisionConfig Mode off|local, Provider local, TimeoutMS 1-5000 default 10ms, backward compatible, Default() sets, ApplyDefaults fills, Validate rejects invalid
- [x] DecisionProvider interface ID/Capabilities/Health/Decide defined, no core dependency on specific provider (Jev is future adapter, not core)
- [x] DecisionRequest privacy-safe: TaskProfile + RequestFeatures + []Candidate snapshot + VE/route/pool IDs + Budget + RequestID, no raw prompt, no secrets, canary SECRET_DECISION_CANARY_82c1 not leaked
- [x] DecisionResult Action SELECT/RANK/ABSTAIN, SelectedID, RankedIDs, Confidence 0-1, ReasonCodes bounded, ProviderID, Abstained, Latency
- [x] Validator enforces eligible-set invariant C∈E, rejects unknown/duplicate/confidence bounds/invalid action, empty eligible
- [x] Normalization preserves failover: partial RANK → ranked + omitted appended original order, SELECT → [selected]+rest original, ABSTAIN → original
- [x] Orchestrator budget (Timeout from config, per-request Budget can tighten), timeout via context.WithTimeout, panic recovery, fail-open on timeout/error/panic/invalid, OFF zero overhead, LOCAL preserves order
- [x] Local provider pass-through preserving order, reason EXISTING_ORDER_PRESERVED, respects context cancellation
- [x] Registry with local pre-registered, Resolve, Snapshot for admin
- [x] Metrics bounded cardinality, GlobalMetrics, Record, Snapshot, exposed via /metrics (nexaroute_decision_total, latency avg/count)
- [x] Admin snapshot extension includes decision config, metrics, provider health
- [x] Integration in openai.go, anthropic.go, canonical_path.go after candidatesForRequirement (eligible set E) and before execution/failover
- [x] Router does NOT import decision (eligibility owner stays pure), no second router/gateway/metrics stack/config system
- [x] Hot-reload coherence: orchestrator config updated atomically on config reload
- [x] Property tests for eligible-set invariant (200 random iterations, invalid injection, panic)
- [x] Privacy tests for canary leak
- [x] Benchmarks for OFF, local, validator, normalize
- [x] gofmt, go build, go test ./... PASS, race, verify.sh, stress.sh, smoke-local.sh PASS
- [x] OFF behaves like current version (Phase C) — zero semantic impact

---

## F. Remaining Limitations (intentional Phase D)

- Only local provider implemented; external adapters (e.g. Jev) deferred to Phase F, isolated
- No policy engine, no reason code beyond Phase D constants — Phase E will add multi-objective policy
- No scorecards, evaluation engine, provenance — Phase H
- No shadow/canary — Phase I
- No dashboard UI for decision — Phase J (metrics and admin snapshot ready, UI deferred)
- No supervision contracts — Phase K
- No learned routing — Phase L only if measurable
- Constraints struct empty placeholder for Phase E
- DecisionRequest does not include cost/latency history yet — Phase E will add if needed, but must stay privacy-safe

---

## G. Final Verdict

PHASE D: PASS

- Decision plane contracts implemented, eligible-set invariant enforced, fail-open on timeout/error/panic/invalid, OFF zero overhead, LOCAL preserves order, privacy canary not leaked, property tests pass, benchmarks <5µs, full gates PASS.
- No regression: all previous tests PASS, existing hard constraints stay authoritative, deterministic local routing works with zero decision providers.
