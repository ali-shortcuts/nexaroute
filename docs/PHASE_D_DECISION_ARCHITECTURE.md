# Phase D — Decision Architecture

Date: 2026-09-25 (final safety convergence)
Branch: arena/01a0d825-nexaroute
Commit: fb28d52e49bd284865adede09caa681ff919877e → final convergence

## Overview

Phase D introduces a provider-agnostic Decision Plane that ranks **only within the eligible candidate set E**. It does NOT own eligibility. Router remains authoritative for health, capabilities, provider circuits, context-window fit, credentials, security policy, client policy, virtual endpoint enabled/protocol checks.

```
Client
 ↓ raw JSON bounded
Protocol decode
 ↓
Feature Extractor → RequestFeatures (privacy-safe)
 ↓
Task Analyzer → TaskProfile
 ↓
router.Requirement
 ↓
candidatesForRequirement(req, protocol):
  - resolve VE (disabled→404, protocol exact)
  - rt.Candidates(req) or reqAll Model="" → AllFilteredCandidates (pool ∩ eligible)
  - returns E = eligible set
 ↓
task_classified event
 ↓
[Decision Plane seam] — after E, before cache/execution
  - OFF: clone E, zero overhead
  - Empty: return empty, no provider call, reason EMPTY_ELIGIBLE
  - Single: return [A], no provider call, reason SINGLE_CANDIDATE
  - Budget: Timeout (default 10ms, bounded 1-5000ms) + MaxProviderCalls (default 1, Phase D)
  - Provider health check: if unavailable → fail-open PROVIDER_UNHEALTHY, no call
  - Context with timeout, panic recovery, fail-open
  - Capabilities enforcement: RANK requires CanRank, SELECT requires CanSelect
  - ValidateResult strict: Action must be known non-empty, confidence finite not NaN/Inf in [0,1], reason codes bounded canonical, ranked_ids bounded <= len(eligible) and hard limit 4096, selected_id bounded 512, provider_id bounded 128, SELECT requires selected_id ∈ E and ranked_ids empty, RANK requires ranked_ids non-empty ⊆ E no duplicates selected_id empty, ABSTAIN requires both empty
  - NormalizeResult: ABSTAIN→E original, SELECT C from [A,B,C,D]→[C,A,B,D], RANK [C,A] from [A,B,C,D]→[C,A,B,D] (omitted appended original order, preserves failover)
  - Metrics per-orchestrator, events bounded
 ↓
cache lookup
 ↓
maxAttempts hard bound
 ↓
Execution with failover/hedging/repair/session affinity
```

## DecisionProvider Contract

```go
type DecisionProvider interface {
    ID() string // bounded 128
    Capabilities() Capabilities // CanRank, CanSelect
    Health() ProviderHealth // healthy, degraded, unavailable, checked_at
    Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error)
}
```

**Context cancellation requirement**: Every Decide MUST obey ctx.Done(). Implementations MUST use context-aware I/O and return promptly after cancellation. No unbounded goroutines. Future HTTP adapters must bind requests to supplied context. Orchestrator uses context.WithTimeout from Budget.

**Health**: Before invoking provider, orchestrator checks Health(). If unavailable, does not call Decide, fail-open with PROVIDER_UNHEALTHY. Degraded allowed for local in Phase D.

**Capabilities**: Before accepting result, orchestrator enforces Capabilities. RANK requires CanRank true, SELECT requires CanSelect true, otherwise invalid → fail-open. ABSTAIN always permitted.

## DecisionRequest

Privacy-safe, no raw prompts, no tool results, no headers, no secrets.

```go
type Candidate struct {
    ID         string  // deployment ID, bounded 512
    ProviderID string
    Model      string
    Priority   int
    Weight     float64
}
type Budget struct {
    Timeout          time.Duration // max time for Decide
    MaxProviderCalls int           // max provider invocations, Phase D default 1
}
type DecisionRequest struct {
    TaskProfile       TaskProfile // from taskprofile
    Features          Features    // from feature.RequestFeatures
    Candidates        []Candidate // authoritative eligible set E
    VirtualEndpointID string
    RouteProfileID    string
    CandidatePoolID   string
    Constraints       Constraints // placeholder Phase E
    Budget            Budget
    RequestID         string // correlation, no PII
}
```

Candidate snapshot only routing-relevant IDs, not secrets.

## DecisionResult — Strict Contract

```go
type Action string
const (
    ActionSelect  Action = "SELECT"
    ActionRank    Action = "RANK"
    ActionAbstain Action = "ABSTAIN"
)
type ReasonCode string // bounded enum
const (
    ReasonExistingOrderPreserved ReasonCode = "EXISTING_ORDER_PRESERVED"
    ReasonLocalPassThrough       ReasonCode = "LOCAL_PASS_THROUGH"
    ReasonAbstained              ReasonCode = "ABSTAINED"
    ReasonTimeout                ReasonCode = "TIMEOUT"
    ReasonProviderError          ReasonCode = "PROVIDER_ERROR"
    ReasonProviderPanic          ReasonCode = "PROVIDER_PANIC"
    ReasonInvalidResult          ReasonCode = "INVALID_RESULT"
    ReasonBudgetExceeded         ReasonCode = "BUDGET_EXCEEDED"
    ReasonOffMode                ReasonCode = "OFF_MODE"
    ReasonEligibleSetPreserved   ReasonCode = "ELIGIBLE_SET_PRESERVED"
    ReasonNormalizationApplied   ReasonCode = "NORMALIZATION_APPLIED"
    ReasonValidationFailed       ReasonCode = "VALIDATION_FAILED"
    ReasonSingleCandidate        ReasonCode = "SINGLE_CANDIDATE"
    ReasonEmptyEligible          ReasonCode = "EMPTY_ELIGIBLE"
    ReasonProviderUnhealthy      ReasonCode = "PROVIDER_UNHEALTHY"
)

type DecisionResult struct {
    Action      Action       // must be SELECT, RANK, ABSTAIN — empty/unknown INVALID
    SelectedID  string       // SELECT: required ∈ E, bounded 512, RANK/ABSTAIN must be empty
    RankedIDs   []string     // RANK: required non-empty ⊆ E no duplicates bounded <= len(E) and <=4096, SELECT/ABSTAIN must be empty
    Confidence  float64      // finite, not NaN/Inf, [0,1]
    ReasonCodes []ReasonCode // bounded count <=8, each len <=64, only canonical values
    ProviderID  string       // filled by orchestrator, bounded 128
    Abstained   bool
    Latency     time.Duration
    Error       string // internal, bounded 256, not for client exposure, sanitized
}
```

**Strict rules**:
- SELECT: selected_id required ∈ E, ranked_ids empty
- RANK: ranked_ids required non-empty ⊆ E no duplicates len <= len(E), selected_id empty
- ABSTAIN: selected_id empty, ranked_ids empty
- Empty/unknown Action → INVALID
- Contradictory payloads (e.g. SELECT with ranked) → INVALID
- NaN/Inf confidence → INVALID
- Reason codes must be canonical, bounded count/len
- Ranked size > eligible → INVALID
- Error string bounded 256, not exposed as arbitrary upstream body

## Validator

```go
func ValidateResult(eligible []Candidate, result DecisionResult) error
func NormalizeResult(eligible []Candidate, result DecisionResult) (ordered []Candidate, applied bool, reason ReasonCode)
```

- Rejects unknown candidates, duplicates, NaN/Inf, out-of-bounds confidence, invalid action, contradictory payloads, too many ranked, invalid reason codes, too many reason codes, too long IDs.
- Normalization preserves failover coverage: omitted eligible IDs appended original order.

## Budget

```go
type Budget struct {
    Timeout          time.Duration
    MaxProviderCalls int // Phase D default 1
}
func DefaultBudget() Budget { Timeout:10ms, MaxProviderCalls:1 }
```

- Timeout from config (default 10ms, bounded 1-5000ms) + per-request Budget can tighten (min).
- MaxProviderCalls enforced: if <=0 and budget explicitly set, fail-open BUDGET_EXCEEDED, no provider call.
- Phase D only one provider invoked; chains deferred.

## Orchestrator — Concurrency & Fail-Open

```go
type Orchestrator struct {
    registry *Registry
    metrics  *Metrics
    mu       sync.RWMutex // protects cfg
    cfg      config.DecisionConfig
}
func NewOrchestrator(reg *Registry, cfg config.DecisionConfig, metrics *Metrics) *Orchestrator
func (o *Orchestrator) UpdateConfig(cfg config.DecisionConfig) // atomic snapshot under mu
func (o *Orchestrator) Config() config.DecisionConfig // coherent snapshot under RLock
func (o *Orchestrator) Decide(ctx context.Context, req DecisionRequest) (ordered []Candidate, result DecisionResult, trace DecisionTrace)
```

- **Hot-reload race fixed**: cfg protected by RWMutex, Decide snapshots cfg under RLock, UpdateConfig writes under Lock, Request A gets one coherent snapshot, Request B gets new snapshot, no half-old/half-new.
- **Empty eligible**: returns empty, no provider call, reason EMPTY_ELIGIBLE, metrics recorded, fallbackUsed true.
- **Single candidate**: returns [A], no provider call, reason SINGLE_CANDIDATE.
- **OFF**: clone original, ABSTAIN OFF_MODE, off_mode_total metric, zero provider call.
- **Provider health**: check before call, unavailable → fail-open PROVIDER_UNHEALTHY, no call.
- **Capabilities**: enforce CanRank/CanSelect, mismatch → fail-open INVALID_RESULT, VALIDATION_FAILED.
- **Budget**: MaxProviderCalls enforced, exhausted → BUDGET_EXCEEDED, no call.
- **Timeout**: context.WithTimeout, provider must honor ctx.Done(), timeout → TIMEOUT reason, original order.
- **Panic recovery**: recover, bounded error string, PROVIDER_PANIC reason, original order.
- **Error bounding**: Error field truncated 256, panic string truncated 256, no arbitrary upstream body, no secrets.
- **Validation**: strict, fail-open INVALID_RESULT, VALIDATION_FAILED.
- **Normalization**: preserves failover.

**Race test**: `TestOrchestrator_HotReloadRace` overlaps Decide and UpdateConfig 100 iterations, passes with `-race`.

## DecisionTrace

```go
type DecisionTrace struct {
    Mode           string
    ProviderID     string
    CandidateCount int
    Action         Action
    SelectedID     string
    ReasonCodes    []ReasonCode
    Duration       time.Duration
    FallbackUsed   bool
}
```

Bounded, no raw prompts, no chain-of-thought, supports later route explanation.

## Events

Bounded event-bus integration, privacy-safe.

Kinds:
- decision_ok
- decision_abstain
- decision_fail
- decision_timeout
- decision_rejected

Safe fields: provider ID, action, candidate count, selected deployment ID, confidence, bounded reason codes (comma-joined, bounded 512), latency, virtual endpoint ID, route profile ID, pool.

Never emit: raw prompt, tool schema, API keys, authorization, headers, arbitrary provider text, chain-of-thought, canary.

Implemented in `internal/httpapi/decision_wiring.go` via `emitDecisionEvent`, using `events.Bus.Add` with bounded strings.

## Metrics

Per-orchestrator Metrics instance, not global shared (except GlobalMetrics fallback for tests). Each Server owns its Metrics to avoid cross-server contamination. Test `TestOrchestrator_MetricsIsolation` proves two orchestrators do not share counters.

Metrics use fixed enums only for labels, not arbitrary provider-returned text, deployment ID, request ID, VE ID, session ID.

```go
type Metrics struct {
    decisionsTotal, abstainsTotal, failuresTotal, timeoutsTotal, invalidTotal, selectTotal, rankTotal, latencySum, latencyCount, offModeTotal
}
func (m *Metrics) Record(result DecisionResult, err error, timedOut bool, offMode bool)
func (m *Metrics) Snapshot() map[string]int64 // fixed keys: decisions_total, abstains_total, failures_total, timeouts_total, invalid_total, select_total, rank_total, latency_avg_ms, latency_count, off_mode_total
```

Exposed via `/metrics` as `nexaroute_decision_total{outcome="..."}` with outcome from fixed set, not per-request.

## Integration Seam

In `internal/httpapi/openai.go`, `anthropic.go`, `canonical_path.go`:

```go
cfg, candidates, resolvedRoute, resolveErr := s.candidatesForRequirement(req, protocol)
s.emitTaskClassified(...)
if len(candidates)==0 { 503 }
candidates = s.applyDecisionPlane(r.Context(), candidates, ti, resolvedRoute, requestID)
```

`applyDecisionPlane`:
- Fast path OFF check via snapshot cfg.Decision.Mode
- Fast path len<=1 returns early (but orchestrator also handles empty/single safely for direct use)
- Converts to []decision.Candidate
- Budget Timeout from cfg + MaxProviderCalls=1
- Builds DecisionRequest from ti.Features, ti.Profile, VE/route/pool IDs, RequestID
- Calls orchestrator.Decide (returns ordered, result, trace)
- Emits decision event
- Reorders original []router.Scored via `reorderScoredByDecision`

Privacy: only Features+Profile+IDs passed.

## End-to-End Routing Regressions Verified

- Cross-protocol: OpenAI, Anthropic, Responses with same pool and decision.mode=local preserve identical ordering (priority strategy). Test `TestDecision_CrossProtocolLocalPreservesOrder` PASS.
- Ranking provider shared seam: test provider returns [B,A] used across all three protocols, first deployment B. Test `TestDecision_RankingProviderSharedSeam` PASS.
- Pool containment: VE pool A,B, deployment C healthy, evil provider returns C,A → validator rejects, final order A,B, C never executed (hitsC=0). Test `TestDecision_PoolContainment` PASS.
- Max attempts: candidates [A,B,C], max_attempts=2, A fails, B fails, C must NOT be attempted (hitsC=0). Test `TestDecision_MaxAttempts` PASS.
- Fallback: VE → primary/fallback pool → A fails → B succeeds → success records B, upstream model physical not virtual. Test `TestDecision_FallbackIntegration` PASS.
- Session affinity: ranking changes order, successful physical deployment remains affinity value, not provider ID or virtual model. Test `TestDecision_SessionAffinity` PASS.
- Credential selection: DecisionRequest has no credential field, credential P2C remains authoritative. Test `TestDecision_CredentialSelectionUnchanged` PASS.
- Privacy canary complete path: SECRET_DECISION_CANARY_82c1 absent from DecisionRequest, DecisionResult, DecisionTrace, events, metrics, admin snapshot. Test `TestDecision_PrivacyCanaryCompletePath` + `TestPrivacy_CompletePath` PASS.

## Privacy Evidence

- Canary `SECRET_DECISION_CANARY_82c1` placed in raw request content → DecisionRequest JSON does not contain it (privacy_test.go)
- DecisionResult, DecisionTrace, events, metrics, admin snapshot also verified absent (privacy complete path)
- DecisionRequest fields: no api_key, authorization, prompt, headers
- Candidate snapshot: ID, ProviderID, Model, Priority, Weight only
- Error string bounded 256, not arbitrary upstream body

## Benchmarks (final convergence, exact)

From `go test -bench . -benchmem ./internal/decision`:

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

OFF path ~277 ns/op, local ~1469 ns/op, validator 10 ~478 ns/op, validator 100 ~4435 ns/op, normalizer 10 ~999 ns/op, normalizer 100 ~9085 ns/op.

## Verification

- `gofmt -l` clean
- `go test ./... -count=1` PASS (19 packages)
- `go test -race ./internal/decision ./internal/httpapi ./internal/router ./internal/route ./internal/taskprofile ./internal/feature` PASS
- `go test -race -count=1 ./...` via verify.sh PASS
- `./scripts/verify.sh` PASS
- `./scripts/stress.sh` PASS
- `./scripts/smoke-local.sh` PASS (admin snapshot includes decision config/metrics)

## Remaining Limitations

- Only local provider; external adapters deferred to Phase F
- No policy engine, scorecards, evaluation, shadow/canary, learned routing, supervision — all deferred
- Constraints empty placeholder
- DecisionTrace minimal, no cost/latency history yet
- Error field internal bounded, not persisted as chain-of-thought
