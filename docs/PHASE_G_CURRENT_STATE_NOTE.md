# Phase G Current-State Note — Decision Provider Chains

Date: 2026-09-26
Branch: arena/01a0db74-nexaroute
Baseline: 16502415aa99a885a50dfbcf1f92f90088dd5ced (arena/01a0d825-nexaroute)
Parent Phase: F PASS (external DecisionProvider infra + Jev adapter)

## Verified Phase F seam (unchanged)

```
candidatesForRequirement(req, protocol):
  - RLock snapshot cfg, resolver, router
  - resolveVirtualEndpoint(req.Model) → ResolvedRoute (VE ID, public_model, route_profile, primaryPool, orderedPoolIDs, allowed sets)
  - rt.Candidates(req) → E (already enforces protocol, capability, context window, disabled, health circuit, provider cooldown, credentials, client policy, security policy)
  ↓
task_classified event
  ↓
[Decision Plane seam] — after E, before cache/execution
  - decisionCandidates() → []decision.Candidate (PoolID, PoolOrdinal, Priority, RouterScore, Health, CapacityPressure, Cost, ContextWindow, Capabilities, OriginalRank)
  - PinnedCandidateID via router.PinnedDeploymentID(req)
  - Constraints = ComputePrimaryConstraints(E, pinnedID) // Allowed, Forced, fallback boundary, priority band, affinity
  - Orchestrator.Decide or ChainExecutor.Execute: produces orderedCandidates (same elements as E, reordered) + DecisionResult + ChainTrace
  ↓
cache lookup (key includes orderedCandidates IDs), maxAttempts loop, execution
```

Router remains eligibility owner, does NOT import decision. DecisionRequest remains privacy-safe: TaskProfile+Features+Candidate snapshot+VE/route/pool IDs+Budget+RequestID, no raw prompt, no secrets, no headers, no PII.

## Phase G additions (what Phase G implements on top of F)

### 1. Chain abstraction
- `config.DecisionChainConfig {ID, Steps: []DecisionChainStep {Provider, TimeoutMS}}` — bounded, max 64 chains, max 8 steps per chain, step timeout 1..5000ms, duplicate provider in chain rejected, unknown provider rejected, collision with provider ID rejected, disabled providers may be referenced.
- `config.DecisionConfig {Mode: hybrid, Chain: "external-policy", TimeoutMS: 800, MaxProviderCalls: 2}` — hybrid requires chain exists, MaxProviderCalls 1..8, TimeoutMS 1..5000, validated.
- `config.DecisionProviderHealth {FailureThreshold, FailureWindowSeconds, CooldownSeconds}` — per-decision-provider circuit, separate from model/provider health.Manager.
- `internal/decision/providerstate.Manager` — in-memory per-decision-provider health with Config, Clock, RecordSuccess, RecordFailure, IsCooldown, CooldownUntil, Snapshot, Reset. Windowed failure counting, cooldown expiry, concurrent safe.
- `internal/decision/ChainExecutor` — request-local immutable chain snapshot, global timeout (min of Budget.Timeout and Decision.TimeoutMS), per-step timeout (min of step TimeoutMS and remaining global), global budget MaxProviderCalls, affinity zero-calls short-circuit, same Constraints for every step, sequential execution, first valid SELECT stops chain, ABSTAIN/ERROR/TIMEOUT/PANIC/INVALID/RANK continues, all exhausted fail-open preserves existing router order.

### 2. Critical invariant: RANK never terminal
- RANK was incorrectly terminating chain in earlier implementation. Fix: `ValidateResult` + `ChainExecutor.Execute` treats RANK as non-terminal: it validates eligible containment, trims unknown IDs, preserves failover order, but always continues to next provider and eventually fail-open. RANK does not move primary, does not become final DecisionResult. Reason codes trimmed to ≤8 bounded distinct, Confidence finite [0,1] enforced.

### 3. Global budget / deadline / affinity / cooldown / hot reload / trace / privacy
- **Global deadline**: computed once before chain loop from Budget.Timeout and cfgTimeout; per-step context derived via `context.WithTimeout` remaining time; if global deadline exceeded, chain stops and returns exhausted; global wins over per-step timeout (perStep = min(stepTimeout, remainingGlobal)).
- **Per-step budget**: MaxProviderCalls consumed only by real calls (attempted providers), skipped due to cooldown/unavailable does NOT consume budget; budget exhausted stops further calls and returns fail-open.
- **Affinity zero-calls**: if Constraints.ForcedPrimaryID present (session pin) and already enforced, chain returns 0 calls, outcome AFFINITY_SKIPPED, preserves order.
- **Cooldown**: `providerstate.IsCooldown(id)` checked before each step; if cooldown, skip without call, trace records COOLDOWN reason, does not consume budget, does not affect physical health.Manager.
- **Hot reload**: ChainExecutor stateless per-request; config swap is atomic immutable (Server rewires Registry, Manager.UpdateConfig, Chain snapshot). Existing in-flight requests keep old snapshot; new requests see new chain/cooldown config. Cooldown state preserved across reload (UpdateConfig preserves failTimes, only thresholds change).
- **Trace bounded**: `ChainTrace {ChainID, StepCount, Outcome, Steps: []ChainStepTrace {ProviderID, ProviderType(jev/policy/local), Outcome(SELECTED/ABSTAIN/ERROR/TIMEOUT/COOLDOWN/UNAVAILABLE/INVALID), ReasonCodes≤8, CallsUsed}}` — bounded per step, types bounded to jev/policy/local for metrics label cardinality.
- **Privacy**: DecisionRequest remains metadata_only; secret canaries (prompt, header, body) never appear in chain events, metrics, admin snapshot, X-Gateway-Decision-Trace header, or upstream payloads. Remote transport already redacts.

### 4. Integration: hybrid E2E
- Hybrid mode selects chain per request, executes via ChainExecutor before cache. Cache key includes orderedCandidates, so decision ordering is cached; second identical request hits cache with zero decision provider calls. Decision result reorders candidates: only selected primary moves to front, remainder preserves original router order (existing failover order preserved, never reordered by RANK).
- Fallback chains preserved: decision selection respects `AllowedIDs` (pool containment + fallback boundary + priority band). SelectedID must be in AllowedIDs else INVALID and continue.
- MaxAttempts routing remains authoritative: decision cannot increase max_attempts. After decision reordering, execution loop still respects cfg.Routing.MaxAttempts; hedging counts against same budget.
- Cross-protocol strict: decision applied per request per protocol; same deployment ID may appear under openai/anthropic/responses but each protocol's AllowedIDs differ — decision for one protocol never leaks to another.
- VE vs direct strict: same underlying deployment via VE and direct route profile must behave identically for same decision selection.
- Health separation: decision provider failures update only providerstate.Manager, never model health.Manager; upstream 500 failures update only model health.

### 5. Existing strict invariants preserved
- Eligibility owner remains router; decision never invents IDs.
- Request/response size bounds 32KiB/64KiB, envelope validation, confidence finite, reason codes bounded, fail-open on all external failures, no retry, context cancellation, SSRF safety, redirect no Auth, metrics bounded, admin safe, hot reload atomic.
- Phase E policy scoring weights and fallback boundary still enforced via PrimarySelectionConstraints for every chain step.
- Cache, hedging, repair, p2c window, session affinity TTL all unchanged.

## Files that matter for Phase G
- `internal/decision/chain.go` — ChainExecutor, Rank handling fix
- `internal/decision/chain_test.go` — 19+ unit/property/fuzz covering SELECT stop, ABSTAIN/ERROR/TIMEOUT/PANIC/INVALID/RANK continue, budget/deadline, affinity, cooldown, fail-open, SameConstraintsEveryStep, SelectedInAllowed, BudgetBound, FuzzChain_ResultHandling
- `internal/decision/providerstate/manager_test.go` — 13 tests for window, cooldown, recovery, config update, concurrency
- `internal/config/chain_config_test.go` — 16 tests for hybrid requires chain, steps bounds, duplicate, unknown, collision, disabled, timeout bounds
- `internal/decision/chain_bench_test.go` — benchmarks for three-step, abstain, cooldown paths
- `internal/httpapi/decision_chain_integration_test.go` — 16 hybrid E2E tests (valid SELECT, error/timeout/invalid/abstain/cooldown continue, fallback B→A→C, max_attempts, cache zero-calls, cross-protocol, VE/direct, health separation, hot reload, trace bounded, privacy/remote-error canaries)
- `internal/httpapi/jev_integration_test.go` — existing Jev transport reused for upstream mocks
- `configs/config.example.json` — hybrid example (mode=hybrid chain=external-policy timeout 800 max_provider_calls 2, decision_chains, decision_provider_health)
- `internal/config/config.go` — chain validation, defaults, max 64 chains / 8 steps

## Known gaps (not Phase G scope)
- Phase H quality scorecards (coding/reasoning) — not fabricated
- No embeddings or LLM judge
- RANK currently never terminal; future may support ranked ordering with additional constraints (not G)
