# Phase D — DecisionProvider Contracts + Orchestrator + Eligible-Set Validator — Implementation Report (Final Safety Convergence)

Date: 2026-09-25 (final convergence)
Branch: arena/01a0d825-nexaroute
Baseline: 2cf905cab6323f29892beb1d4d96a59956472cec (Phase C PASS) → fb28d52 (initial Phase D) → final convergence
Spec sections: §7 DecisionProvider, §8 Orchestrator, §9 Validator, §10 Budget, §11 Privacy, §42 Config, §46 Observability, and safety convergence requirements

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
router.Requirement
  ↓
candidatesForRequirement(req, protocol):
  - snapshot cfg, resolver, router under RLock
  - resolve VE (disabled→404, protocol exact)
  - if virtual: reqAll Model="" → rt.Candidates(reqAll) → AllFilteredCandidates (pool ∩ eligible, dedup, preserve router order)
  - else: rt.Candidates(req)
  - returns E = eligible set (authoritative)
  ↓
task_classified event
  ↓
[Decision Plane] — Phase D seam
  - Empty E: return empty, no provider call, reason EMPTY_ELIGIBLE
  - Single E: return [A], no provider call, reason SINGLE_CANDIDATE
  - OFF: clone E, zero overhead, reason OFF_MODE
  - Budget: Timeout (default 10ms, bounded 1-5000ms) + MaxProviderCalls (default 1)
  - Provider health check: if unavailable → fail-open PROVIDER_UNHEALTHY, no call
  - Context with timeout, panic recovery, error bounding
  - Capabilities enforcement: RANK requires CanRank, SELECT requires CanSelect
  - ValidateResult strict: Action must be known non-empty, confidence finite not NaN/Inf [0,1], reason codes canonical bounded, ranked_ids bounded <= len(E) and <=4096, SELECT requires selected_id ∈ E and ranked empty, RANK requires ranked non-empty ⊆ E no duplicates selected empty, ABSTAIN requires both empty
  - NormalizeResult: ABSTAIN→E original, SELECT C from [A,B,C,D]→[C,A,B,D], RANK [C,A]→[C,A,B,D] (omitted appended original order)
  - Metrics per-orchestrator, events bounded, DecisionTrace
  - Reorder original []router.Scored by ordered []decision.Candidate
  ↓
cache lookup
  ↓
maxAttempts hard bound
  ↓
Execution with failover/hedging/repair/session affinity
```

No second router, no second metrics stack, no second config system. Router remains eligibility owner, does not import decision.

---

## B. Exact Final Structs

### Config (internal/config/config.go)

```go
type DecisionConfig struct {
    Mode      string `json:"mode,omitempty"`       // off | local
    Provider  string `json:"provider,omitempty"`   // local
    TimeoutMS int    `json:"timeout_ms,omitempty"` // 1-5000 default 10
}
type Config struct {
    ...
    Decision DecisionConfig `json:"decision,omitempty"`
}
```

Defaults: `Default()` returns Decision{Mode:"off", Provider:"local", TimeoutMS:10}, `ApplyDefaults()` trims/lowercases/defaults, `Validate()` rejects invalid.

### DecisionProvider (internal/decision/provider.go)

```go
type DecisionProvider interface {
    ID() string
    Capabilities() Capabilities
    Health() ProviderHealth
    Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error)
}
// Contract: Decide MUST obey ctx.Done(), use context-aware I/O, return promptly, no unbounded goroutines.
// Future HTTP adapters must bind requests to supplied context.
// Health: if unavailable, orchestrator will not call Decide, fail-open PROVIDER_UNHEALTHY.
// Capabilities: RANK requires CanRank, SELECT requires CanSelect.
type Capabilities struct { CanRank, CanSelect bool }
type ProviderHealth struct { Status string; Message string; CheckedAt time.Time }
const HealthHealthy, HealthDegraded, HealthUnavailable
```

### DecisionRequest (internal/decision/request.go)

```go
type Candidate struct { ID, ProviderID, Model string; Priority int; Weight float64 }
type Budget struct { Timeout time.Duration; MaxProviderCalls int } // default 1
type DecisionRequest struct {
    TaskProfile       TaskProfile
    Features          Features
    Candidates        []Candidate
    VirtualEndpointID string
    RouteProfileID    string
    CandidatePoolID   string
    Constraints       Constraints
    Budget            Budget
    RequestID         string
}
func CloneCandidates(in []Candidate) []Candidate
```

Privacy: only TaskProfile+Features+IDs, no raw prompt, no secrets.

### DecisionResult — Strict Contract (internal/decision/result.go)

```go
type Action string
const ActionSelect, ActionRank, ActionAbstain Action = "SELECT","RANK","ABSTAIN"

type ReasonCode string // bounded enum
const (
    ReasonExistingOrderPreserved, ReasonLocalPassThrough, ReasonAbstained,
    ReasonTimeout, ReasonProviderError, ReasonProviderPanic, ReasonInvalidResult,
    ReasonBudgetExceeded, ReasonOffMode, ReasonEligibleSetPreserved,
    ReasonNormalizationApplied, ReasonValidationFailed, ReasonSingleCandidate,
    ReasonEmptyEligible, ReasonProviderUnhealthy ReasonCode = ...
)
const MaxReasonCodes=8, MaxReasonCodeLen=64, MaxSelectedIDLen=512, MaxProviderIDLen=128, MaxRankedIDs=4096

type DecisionResult struct {
    Action      Action       // must be SELECT,RANK,ABSTAIN — empty/unknown INVALID
    SelectedID  string       // SELECT required ∈ E bounded 512, RANK/ABSTAIN must be empty
    RankedIDs   []string     // RANK required non-empty ⊆ E no duplicates bounded <=len(E) <=4096, SELECT/ABSTAIN empty
    Confidence  float64      // finite not NaN/Inf [0,1]
    ReasonCodes []ReasonCode // bounded count <=8 len <=64 canonical only
    ProviderID  string       // bounded 128
    Abstained   bool
    Latency     time.Duration
    Error       string // internal bounded 256 sanitized not for client
}
func (r DecisionResult) ValidAction() bool // known non-empty
func (r DecisionResult) IsStrictAbstain() bool // ABSTAIN with empty payload
```

### Validator (internal/decision/validator.go)

```go
func ValidateResult(eligible []Candidate, result DecisionResult) error
// strict: action known non-empty, confidence finite not NaN/Inf [0,1], reason codes canonical bounded count/len, ranked bounded, SELECT/RANK/ABSTAIN payload rules, unknown/duplicate rejected

func NormalizeResult(eligible []Candidate, result DecisionResult) (ordered []Candidate, applied bool, reason ReasonCode)
// ABSTAIN→clone original, SINGLE_CANDIDATE/EMPTY_ELIGIBLE handling, SELECT→[selected]+rest original, RANK→ranked+omitted appended original order
```

### Budget (internal/decision/budget.go)

```go
type Budget struct { Timeout time.Duration; MaxProviderCalls int }
func DefaultBudget() Budget { Timeout:10ms, MaxProviderCalls:1 }
func (b Budget) IsZero() bool
```

MaxCandidates removed (dead field). MaxProviderCalls default 1, enforced, exhausted → BUDGET_EXCEEDED, no call.

### Orchestrator (internal/decision/orchestrator.go)

```go
type DecisionTrace struct {
    Mode, ProviderID string; CandidateCount int; Action Action; SelectedID string; ReasonCodes []ReasonCode; Duration time.Duration; FallbackUsed bool
}
type Orchestrator struct { registry *Registry; metrics *Metrics; mu sync.RWMutex; cfg config.DecisionConfig }
func NewOrchestrator(reg *Registry, cfg config.DecisionConfig, metrics *Metrics) *Orchestrator // per-instance metrics, not global
func (o *Orchestrator) UpdateConfig(cfg config.DecisionConfig) // atomic under mu
func (o *Orchestrator) Config() config.DecisionConfig // coherent snapshot under RLock
func (o *Orchestrator) Decide(ctx context.Context, req DecisionRequest) (ordered []Candidate, result DecisionResult, trace DecisionTrace)
// Empty→empty no call EMPTY_ELIGIBLE
// Single→[A] no call SINGLE_CANDIDATE
// OFF→clone OFF_MODE off_mode_total
// Budget MaxProviderCalls enforced → BUDGET_EXCEEDED no call
// Resolve provider → unknown → PROVIDER_ERROR fail-open
// Health check → unavailable → PROVIDER_UNHEALTHY no call
// Timeout via context.WithTimeout, provider must honor ctx.Done()
// Panic recovery bounded 256 → PROVIDER_PANIC
// Capabilities enforcement → mismatch → INVALID_RESULT, VALIDATION_FAILED
// Validate strict → invalid → INVALID_RESULT, VALIDATION_FAILED
// Normalize preserves failover
// Error strings bounded 256
```

Hot-reload race fixed: cfg protected by RWMutex, Decide snapshots under RLock, UpdateConfig under Lock, Request A coherent snapshot, Request B new snapshot, no half-old/half-new. Race test `TestOrchestrator_HotReloadRace` overlaps Decide and UpdateConfig 100 iterations, passes with `-race`.

### Registry (internal/decision/registry.go)

Pre-registers local, RWMutex protected, Resolve empty→local, Snapshot for admin.

### Metrics (internal/decision/metrics.go)

Per-orchestrator Metrics, not global shared. GlobalMetrics fallback only for tests. Each Server owns its Metrics via `&decision.Metrics{}` in `server.go` New(). Test `TestOrchestrator_MetricsIsolation` proves isolation.

Metrics use fixed enums only for labels, not arbitrary provider text, deployment ID, request ID, VE ID, session ID. Snapshot fixed keys: decisions_total, abstains_total, failures_total, timeouts_total, invalid_total, select_total, rank_total, latency_avg_ms, latency_count, off_mode_total.

Exposed via `/metrics` as `nexaroute_decision_total{outcome="..."}`.

### Events (internal/events/bus.go)

Added decision fields bounded: DecisionProvider 128, DecisionAction 32, DecisionSelected 512, DecisionReasonCodes 512, DecisionCandidateCount, DecisionConfidence.

Kinds: decision_ok, decision_abstain, decision_fail, decision_timeout, decision_rejected.

Safe fields: provider ID, action, candidate count, selected deployment ID, confidence, bounded reason codes, latency, VE ID, route profile, pool. Never raw prompt, tool schema, API keys, headers, arbitrary text, chain-of-thought.

Emitted in `decision_wiring.go` via `emitDecisionEvent`.

### Wiring (internal/httpapi/decision_wiring.go)

- `decisionCandidates` converts Scored to Candidate
- `reorderScoredByDecision` preserves metadata, fallback if len mismatch
- `emitDecisionEvent` builds bounded reason string, determines kind from result, adds event with safe fields
- `applyDecisionPlane`: OFF fast path via snapshot, len<=1 fast path (but orchestrator also handles empty/single safely), converts, Budget Timeout + MaxProviderCalls=1, builds DecisionRequest from Features+Profile+VE IDs+RequestID, calls orchestrator.Decide (3 returns), emits event, reorders Scored.

---

## C. File Changes (vs Phase C)

New:
- internal/decision/provider.go, request.go, result.go, validator.go, budget.go, local.go, reason.go, registry.go, metrics.go, orchestrator.go
- internal/decision/validator_test.go, local_test.go, orchestrator_test.go, privacy_test.go, property_test.go, benchmark_test.go
- internal/httpapi/decision_wiring.go, decision_integration_test.go
- docs/PHASE_D_CURRENT_STATE_NOTE.md, PHASE_D_DECISION_ARCHITECTURE.md, PHASE_D_IMPLEMENTATION_REPORT.md

Modified:
- internal/config/config.go — DecisionConfig, Default() sets, ApplyDefaults, Validate
- internal/httpapi/server.go — decisionRegistry, decisionOrchestrator per-instance Metrics, New() uses same registry for orchestrator, applyConfigLocked updates orchestrator config
- internal/httpapi/openai.go, anthropic.go, canonical_path.go — decision seam after candidatesForRequirement
- internal/httpapi/metrics.go — decision metrics bounded
- internal/httpapi/admin.go — snapshot includes decision config/metrics/provider health
- internal/events/bus.go — decision fields bounded, Add truncates

Unchanged intentionally:
- internal/router, internal/route — no decision import, eligibility authoritative
- internal/feature, internal/taskprofile — no decision dependency

---

## D. Verification

### Compile & Format
- gofmt -l clean
- go build ./... PASS
- go vet ./... PASS

### Unit Tests

```
go test ./... -count=1
ok cmd/gateway
ok internal/cache
ok internal/compat
ok internal/config
ok internal/core
ok internal/decision
ok internal/events
ok internal/feature
ok internal/health
ok internal/httpapi
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

Decision tests (detailed):
- Validator: ValidRank, UnknownCandidate, Duplicate, InvalidConfidence (too high, too low, NaN, +Inf, -Inf), SelectUnknown, StrictAction (8 cases), BoundedRanked, ReasonCodeBounded (unknown, too many) PASS
- Normalize: RankPartial [C,A] from [A,B,C,D]→[C,A,B,D] NORMALIZATION_APPLIED, Select, AbstainPreservesOrder, EmptyEligible→EMPTY_ELIGIBLE PASS
- Local: PreservesOrder ABSTAIN EXISTING_ORDER_PRESERVED, ContextCancellation PASS
- Orchestrator: OffMode preserves order OFF_MODE, LocalPreservesOrder, ValidRank [C,A,B,D], InvalidResultFailOpen unknown Z, TimeoutFailOpen 50ms delay/5ms budget TIMEOUT, ErrorFailOpen, PanicRecovery PROVIDER_PANIC, EligibleSetInvariant EVIL injection, SelectNormalization [C,A,B,D], EmptyEligible no call EMPTY_ELIGIBLE, SingleCandidate no call SINGLE_CANDIDATE, CapabilitiesEnforced (no-rank, no-select), ProviderHealthUnavailable no call PROVIDER_UNHEALTHY, BudgetMaxProviderCalls 0→BUDGET_EXCEEDED no call, NaNConfidenceRejected, InfConfidenceRejected (+Inf/-Inf), StrictContract 8 cases, BoundedRankedIDs, ReasonCodeValidation unknown, HotReloadRace 100 concurrent Decide+UpdateConfig, MetricsIsolation (2 orchestrators isolated), ContextAware timeout early PASS
- Privacy: NoCanaryLeak, FieldsBounded, CompletePath (Request, Result, Trace, Event, Metrics, Admin) PASS
- Property: EligibleSetPreserved 200 random iterations 20% invalid 10% panic → no injection no loss no panic PASS

### Integration Tests (httpapi)

- TestDecision_CrossProtocolLocalPreservesOrder: same pool, decision.mode=local, OpenAI/Anthropic/Responses identical ordering (p1/m1) PASS
- TestDecision_RankingProviderSharedSeam: test provider [B,A] used across all three protocols, first deployment B PASS
- TestDecision_PoolContainment: VE pool A,B, C exists, evil provider returns C,A → rejected, final A,B, C never executed (hitsC=0) PASS
- TestDecision_MaxAttempts: candidates [A,B,C] auto model, max_attempts=2, A fail B fail C not attempted PASS
- TestDecision_FallbackIntegration: VE→primary/fallback pool→A fail B succeed→success B, upstream model physical not virtual PASS
- TestDecision_SessionAffinity: ranking [B,A] first request B, second same session pins B, affinity physical not provider ID/virtual model PASS
- TestDecision_CredentialSelectionUnchanged: DecisionRequest no credential, credential P2C authoritative PASS
- TestDecision_PrivacyCanaryCompletePath: canary absent from events, metrics, admin snapshot PASS
- TestDecision_EventsEmitted: decision_ok/abstain/fail/timeout/rejected emitted with safe fields PASS
- TestDecision_DecisionTrace: bounded, no canary PASS
- TestDecision_MetricsBounded: metrics fixed enums, no canary, decision_total present PASS
- TestDecision_RouterDoesNotImportDecision PASS

### Privacy Canary — Complete Path

Canary `SECRET_DECISION_CANARY_82c1` verified absent from:
- DecisionRequest serialized form (privacy_test.go)
- DecisionResult
- DecisionTrace
- decision events (TestDecision_PrivacyCanaryCompletePath checks bus snapshot JSON)
- metrics (snapshot keys + /metrics endpoint)
- admin snapshot (TestDecision_PrivacyCanaryCompletePath checks /admin/api/snapshot)

### Benchmarks — Exact Output (final convergence)

```
goos: linux
goarch: amd64
pkg: github.com/ali-shortcuts/nexaroute/internal/decision
cpu: Intel(R) Xeon(R) Processor @ 2.60GHz
BenchmarkOrchestrator_OffMode-2    4689223    277.0 ns/op    544 B/op    2 allocs/op
BenchmarkOrchestrator_Local-2       750606    1469 ns/op    992 B/op    8 allocs/op
BenchmarkValidator_10-2            2422431    478.2 ns/op    291 B/op    1 allocs/op
BenchmarkValidator_100-2            237072    4435 ns/op   2840 B/op    2 allocs/op
BenchmarkNormalize_10-2            1239799    999.3 ns/op   2127 B/op    2 allocs/op
BenchmarkNormalize_100-2            118330    9085 ns/op  19464 B/op    3 allocs/op
BenchmarkValidator-2               2498163    475.7 ns/op    291 B/op    1 allocs/op
BenchmarkNormalize-2               1000000    1062 ns/op   2127 B/op    2 allocs/op
```

OFF ~277 ns/op, LOCAL ~1469 ns/op, validator 10 ~478 ns/op, validator 100 ~4435 ns/op, normalizer 10 ~999 ns/op, normalizer 100 ~9085 ns/op.

### Targeted Race Gate

```
go test -race ./internal/decision ./internal/httpapi ./internal/router ./internal/route ./internal/taskprofile ./internal/feature -count=1
ok internal/decision 1.096s
ok internal/httpapi 4.611s
ok internal/router 1.349s
ok internal/route 1.011s
ok internal/taskprofile 1.013s
ok internal/feature 1.236s
```

PASS, including hot-reload race test overlapping Decide and UpdateConfig.

### Full Mandatory Gates

- `./scripts/verify.sh` PASS (go version, shell syntax, formatting, unit/integration count=10 shuffle, vet, race count=3 shuffle, js syntax, fuzz 2s, linux amd64/arm64 builds)
- `./scripts/stress.sh` PASS (router scale, probe/recovery, event-state, admission, log rotation)
- `./scripts/smoke-local.sh` PASS (UI 200, hello 200, model list, admin snapshot includes decision config/metrics, count_tokens fallback, provider CRUD, atomic persistence)

---

## E. Acceptance Checklist (Final Safety Convergence)

- [x] NaN confidence rejected (math.IsNaN check, test NaN)
- [x] +Inf/-Inf confidence rejected (math.IsInf check, test Inf)
- [x] empty/unknown Action rejected (ValidAction requires known non-empty, tests empty/FOO)
- [x] contradictory SELECT/RANK/ABSTAIN payload rejected (strict contract: SELECT with ranked, RANK with selected/empty ranked, ABSTAIN with selected/ranked)
- [x] ranked result bounded (len > len(eligible) rejected, hard limit 4096, test too-many)
- [x] reason codes typed/bounded/validated (ReasonCode type, allowed set, MaxReasonCodes 8, MaxReasonCodeLen 64, validation, test unknown/too many)
- [x] orchestrator config hot reload race fixed (RWMutex around cfg, snapshot under RLock, UpdateConfig under Lock, coherent snapshot)
- [x] concurrent hot-reload race test passes (TestOrchestrator_HotReloadRace 100 iterations, go test -race PASS)
- [x] Budget includes MaxProviderCalls (Budget struct with MaxProviderCalls, DefaultBudget 1)
- [x] provider-call budget enforced (if <=0 and explicitly set → BUDGET_EXCEEDED no call, test budget 0)
- [x] dead MaxCandidates removed (Budget no longer has MaxCandidates, removed from code)
- [x] empty eligible set skips provider (orchestrator handles empty, no call, EMPTY_ELIGIBLE, test empty)
- [x] single candidate skips provider (orchestrator handles single, no call, SINGLE_CANDIDATE, test single)
- [x] capabilities enforced (RANK requires CanRank, SELECT requires CanSelect, tests no-rank/no-select)
- [x] provider health checked independently (Health() check before Decide, unavailable → PROVIDER_UNHEALTHY no call, test unhealthy)
- [x] context cancellation contract documented/tested (provider.go comment: MUST obey ctx.Done(), test context-aware timeout)
- [x] error strings bounded/private (Error truncated 256, panic truncated 256, not arbitrary upstream body, no secrets)
- [x] decision events implemented (decision_ok, abstain, fail, timeout, rejected, safe fields, bounded, privacy-safe, test events emitted)
- [x] DecisionTrace exists (struct with Mode, ProviderID, CandidateCount, Action, SelectedID, ReasonCodes, Duration, FallbackUsed, bounded, test trace)
- [x] metrics remain bounded (fixed outcome labels, not deployment/request/VE/session, test metrics bounded, no canary)
- [x] per-server metrics isolation verified (each Server owns Metrics via &Metrics{}, test isolation proves 2 orchestrators isolated)
- [x] OpenAI decision integration tested (cross-protocol test)
- [x] Anthropic decision integration tested (cross-protocol test)
- [x] Responses decision integration tested (cross-protocol test via bus events)
- [x] fallback integration tested (VE→primary/fallback pool→A fail B succeed)
- [x] pool containment integration tested (evil provider returns C not in pool → rejected, C never executed)
- [x] max_attempts integration tested (max_attempts=2, C not attempted)
- [x] session affinity integration tested (ranking changes order, affinity pins physical B)
- [x] credential selection unchanged (DecisionRequest no credential, P2C authoritative)
- [x] complete privacy canary path passes (Request, Result, Trace, events, metrics, admin snapshot)
- [x] exact benchmarks recorded (OFF 277.0 ns/op 544 B/op 2 allocs, LOCAL 1469 ns/op 992 B/op 8 allocs, validator 10 478.2 ns/op 291 B/op 1 alloc, validator 100 4435 ns/op 2840 B/op 2 alloc, normalizer 10 999.3 ns/op 2127 B/op 2 alloc, normalizer 100 9085 ns/op 19464 B/op 3 alloc)
- [x] targeted race command PASS (decision, httpapi, router, route, taskprofile, feature)
- [x] verify.sh PASS
- [x] stress.sh PASS
- [x] smoke-local.sh PASS
- [x] architecture doc exists (PHASE_D_DECISION_ARCHITECTURE.md)
- [x] implementation report matches code (this report)

---

## F. Remaining Limitations (intentional Phase D)

- Only local provider implemented; external adapters deferred to Phase F, isolated
- No policy engine, scorecards, evaluation, shadow/canary, learned routing, supervision — all deferred per spec
- Constraints empty placeholder for Phase E
- DecisionTrace minimal, no cost/latency history yet — Phase E will add if needed, privacy-safe
- Error field internal bounded, not persisted as chain-of-thought
- No task-aware scoring yet — LOCAL pass-through only

---

## G. Final Verdict

PHASE D: PASS

- Safety convergence complete: NaN/Inf rejected, strict result contract, bounded result size, reason codes typed/bounded/validated, hot-reload race fixed with RWMutex and race test, Budget includes MaxProviderCalls enforced, dead MaxCandidates removed, empty/single candidate skip provider, capabilities enforced, provider health checked, context contract documented/tested, error strings bounded, decision events implemented, DecisionTrace exists, metrics bounded and per-server isolated, cross-protocol integration tested, failover/pool containment/max_attempts/session affinity/credential/privacy complete path all PASS, exact benchmarks recorded, targeted race PASS, verify/stress/smoke PASS.
