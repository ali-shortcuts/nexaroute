# Phase G — Decision Provider Chains — Specification & Semantics

Date: 2026-09-26
Branch: arena/01a0db74-nexaroute
Status: Final acceptance

## 1. Goal
Introduce ordered DecisionProvider chains for hybrid mode, where multiple providers are consulted sequentially until one yields a valid SELECT. Preserve all Phase A-F invariants, keep router as eligibility owner, keep fail-open.

## 2. Configuration

### DecisionConfig
```json
{
  "decision": {
    "mode": "hybrid",
    "chain": "external-policy",
    "timeout_ms": 800,
    "max_provider_calls": 2
  },
  "decision_chains": [
    {"id": "external-policy", "steps": [{"provider": "jev-main"}, {"provider": "policy"}]},
    {"id": "policy-only", "steps": [{"provider": "policy"}]}
  ],
  "decision_provider_health": {
    "failure_threshold": 2,
    "failure_window_seconds": 30,
    "cooldown_seconds": 60
  },
  "decision_providers": [
    {"id": "jev-main", "type": "jev", "enabled": true, "api_key_env": "JEV_API_KEY", "privacy_mode": "metadata_only", "base_url": "https://www.jevai.org"}
  ]
}
```

Validation:
- `mode=hybrid` requires `chain` references existing `decision_chains[].id`
- `mode!=hybrid` ignores chain
- Max 64 chains, max 8 steps per chain
- Step `provider` must reference existing provider ID or built-in `policy`/`local`; duplicate provider in same chain rejected; collision with provider ID rejected
- Step `timeout_ms` 1..5000 if set, else inherits global timeout
- `timeout_ms` (global) 1..5000, default 10ms if 0
- `max_provider_calls` 1..8, default = len(chain) if hybrid else 1
- Disabled providers may be referenced (validated at runtime via cooldown/unavailable)
- `privacy_mode` only `metadata_only` for jev
- `decision_provider_health` thresholds 1..100, windows 1..3600, cooldown 1..86400, defaults 3/30/60

## 3. Runtime objects

### providerstate.Manager
Per-decision-provider circuit separate from model health.
- Config {FailureThreshold, FailureWindow, Cooldown}
- State {Status healthy/cooldown, ConsecutiveFailures, FailuresInWindow, LastFailure, CooldownUntil, failTimes ring}
- Methods: New(Config, Clock), RecordSuccess(id), RecordFailure(id), IsCooldown(id) bool, CooldownUntil(id), Snapshot(), SnapshotOne(id), UpdateConfig(Config) preserves existing failTimes, Reset(id), Config()
- Concurrency safe (RWMutex), window pruning on each RecordFailure, atomic for hot reload.

### ChainExecutor
Stateless per-request executor, request-local chain snapshot.
```go
type ChainExecutor struct{ registry *Registry; state *providerstate.Manager; metrics *Metrics }
type ChainConfig struct { ID string; Steps []ChainStepConfig }
type ChainStepConfig struct { Provider string; TimeoutMS int }
func NewChainExecutor(reg, state, metrics) *ChainExecutor
func (e *ChainExecutor) Execute(ctx, chain, req, constraints, budget, cfgTimeout) (ordered []Candidate, result DecisionResult, trace ChainTrace)
```
- `constraints PrimarySelectionConstraints` computed once via `ComputePrimaryConstraints(candidates, pinnedID)` and reused for every step (SameConstraintsEveryStep invariant).
- `budget Budget {Timeout, MaxProviderCalls}` computed once; global deadline = min(budget.Timeout, cfgTimeout) if both set.

## 4. Execution semantics (sequential, fail-open)

For each step in chain order:
1. Check affinity: if `constraints.ForcedPrimaryID != ""` and already satisfied, return 0 calls, outcome AFFINITY_SKIPPED.
2. Check global deadline: if `time.Until(globalDeadline) <=0`, stop and return exhausted.
3. Check cooldown: if `state.IsCooldown(providerID)` then trace step COOLDOWN, do NOT consume budget, continue.
4. Check unavailable (provider not in registry or unhealthy per Capabilities/Health): trace UNAVAILABLE, do NOT consume budget? Actually unavailable is skipped without call, but budget not consumed (budget counts only attempted calls).
5. Check budget: if `callsUsed >= MaxProviderCalls` then stop.
6. Derive per-step timeout: `stepTimeout = step.TimeoutMS` if set else global timeout; `remaining = time.Until(globalDeadline)`; `perStep = min(stepTimeout, remaining)`; `ctxStep, cancel := context.WithTimeout(ctx, perStep)`.
7. Call `provider.Decide(ctxStep, req)` with panic recovery.
8. Handle result:
   - `error` → record failure, trace ERROR, continue.
   - `context.DeadlineExceeded` or `ctxStep.Err()` → record failure, trace TIMEOUT, continue if global remains.
   - `panic` → recover, record failure, trace PANIC, continue.
   - `ActionAbstain && Abstained` with ReasonAbstained → trace ABSTAIN, not failure, continue (healthy abstain).
   - `ActionSelect` with valid `SelectedID` in `constraints.AllowedIDs`, `Confidence` finite [0,1], `ReasonCodes` distinct ≤8, `SelectedID` known → ValidateResult passes → NormalizeResult (preserve failover order, move only selected to front) → record success, return SELECTED, callsUsed++, trace SELECTED.
   - `ActionSelect` with invalid (unknown ID, violates AllowedIDs, confidence NaN/Inf/outside [0,1], reason codes >8 or duplicate/invalid) → trace INVALID, record failure? Invalid is provider error but not cooldown? Currently counts as failure for providerstate (since provider returned unusable), continue.
   - `ActionRank` → treat as INVALID for chain purpose: validate eligible containment, trim unknown, but never terminal, always continue. Trace INVALID_RANK or INVALID, does not move primary, does not become final result. Reason: RANK must NOT become terminal chain result (must continue, preserve failover order).
   - Any other → continue.
9. If all steps exhausted without SELECT → return `ordered = original router order` (fail-open), `result.Action = Abstain`, `trace.Outcome = Exhausted`.

Metrics: per step latency histogram, `nexaroute_decision_chain_requests_total{chain, outcome}` and `nexaroute_decision_provider_requests_total{providerType=jev/policy/local, outcome}` bounded labels.

Trace bounded: providerType mapped to jev/policy/local (not raw ID), ReasonCodes truncated to 8, Steps ≤8.

## 5. Global budget / deadline details
- Global deadline computed once: if Budget.Timeout and cfgTimeout both >0, use min; else use whichever >0; else no deadline.
- Per-step timeout respects remaining global: global wins over step timeout. If step asks 500ms but global remaining 20ms, perStep=20ms.
- Budget MaxProviderCalls is global call budget: only increments for actual provider calls (Decide invoked). Cooldown/unavailable skips do not increment. Applied identically for every step; never increased by decision.
- Affinity short-circuit yields 0 calls regardless of budget (affinity preservation cheaper than decision).
- SameConstraintsEveryStep verified by property test: constraints snapshot is identical for each provider.

## 6. Hot reload
- `Server` holds `decisionRegistry`, `decisionState`, `decisionExecutor`, `chainConfig` as atomic pointers or under RLock. `ReloadConfig` parses new file, validates, builds new Registry (re-registering jev adapters with new API keys/base URLs), calls `decisionState.UpdateConfig(newHealthCfg)` preserving existing per-provider states (failTimes, cooldownUntil), swaps chain snapshot. In-flight requests keep old pointer; new requests see new. No data race, no partial swap.

## 7. Observability & privacy
- Events: `decision_chain_executed {chain, outcome, callsUsed, selectedProvider, reasonCodes}` with bounded fields, never raw prompt. `decision_provider_failed {providerType, reason}`.
- Trace header: `X-Gateway-Decision-Trace: chain=external-policy;outcome=SELECTED;calls=1;selected=jev-main;providers=jev,policy` — no secrets, no candidate counts beyond bounded, no raw IDs beyond deployment IDs already in routing.
- Metrics bounded: chain IDs ≤64 distinct, provider types 3 distinct, outcomes 5 distinct → cardinality <2000.
- Admin snapshot: `GET /admin/api/snapshot` includes `decisionChains`, `decisionProviderHealth`, `key_configured` booleans, not secrets. Previous canaries (SECRET_EXTERNAL_PROMPT_CANARY_94af etc.) never appear in any output.

## 8. Routing integration invariants
- Cache lookup/serve occurs BEFORE decision-plane execution (Phase G optimization): `Cache.Get` checked immediately after `candidatesForRequirement` and before `applyDecisionPlane`. On HIT, `X-Cache: HIT` and zero `DecisionProvider` calls; cached response includes prior decision ordering. Only MISS proceeds to `ChainExecutor`. Note: `openai_responses` currently has no cache path (documented, not covered).
- MaxAttempts routing authoritative: decision cannot increase max_attempts; after decision, execution loopDiversify respects cfg.Routing.MaxAttempts and hedged attempts count against same budget.
- Fallback boundary: decision selection must be in AllowedIDs (which already excludes fallback-violating IDs). Policy/priority guardrails still apply per step.
- Cross-protocol and VE/direct strictness preserved: each request recomputes AllowedIDs per protocol/VE; decision for one never leaks.
- Health separation: decision RecordFailure updates only providerstate; model health updated only on upstream HTTP 500/timeout.

## 9. Error handling & fail-open
All provider errors (HTTP error, timeout, malformed envelope, unknown candidate, invalid confidence, request too large, response too large, provider unavailable, primary constraint violation, panic) are fail-open: chain continues, metrics record, no candidate set change. If all fail, router proceeds with existing order.

## 10. Testing
- Unit: `internal/decision/chain_test.go` (FirstValidSelectStopsChain, AbstainContinues, ErrorContinues, TimeoutContinues, UnavailableSkipped, CooldownSkipped, InvalidContinues, PanicContinues, RankRejectedContinues, RankCannotReorderFailoverList_Regression, AllExhaustedFailOpen, BudgetStopsLaterCalls, SkippedDoesNotConsumeBudget, GlobalDeadlineStopsChain, PerStepTimeout, AffinityZeroCalls, SameConstraintsEveryStep, SelectedAlwaysInAllowed, OnlySelectedPrimaryMoves, SameProviderNeverTwice, Property_SelectedInAllowed, Property_BudgetBound, FuzzChain_ResultHandling) + benchmarks
- Providerstate: `internal/decision/providerstate/manager_test.go`
- Config: `internal/config/chain_config_test.go`
- E2E: `internal/httpapi/decision_chain_integration_test.go` 16 tests

## 11. Non-goals
- No quality scorecards, no embeddings, no LLM judge
- No RANK ordering for failover (future)
- No retry inside transport, no chain retry
