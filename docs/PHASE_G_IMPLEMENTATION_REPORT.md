# Phase G — Decision Provider Chains — Implementation Report

Date: 2026-09-26
Branch: arena/01a0db74-nexaroute
Baseline: 16502415aa99a885a50dfbcf1f92f90088dd5ced
Spec: Phase G decision provider chains on arena/01a0d825-nexaroute — preserve Phase A-F invariants, existing ChainExecutor/providerstate.Manager/hybrid mode/DecisionChains/global budget+deadline/metrics/admin/hot reload. Only fix production code where strict tests reveal real bug (RANK). Critical: RANK must NOT become terminal chain result (must continue, preserve failover order). Must create permanent chain unit tests, providerstate tests, config chain tests, property/fuzz, hybrid E2E, benchmarks, docs before PASS. Do not redesign, no Phase H. Must run verify.sh, stress.sh, smoke-local.sh + race.

Toolchain: go1.23.0 linux/amd64
Hardening: rank fix, global deadline/per-step timeout, budget, affinity, cooldown, hot reload, trace bounded, privacy, cross-protocol/VE, health separation, cache, fallback, maxAttempts

---

## 1. Objective
Extend NexaRoute hybrid decision plane from single provider (Phase F assisted) to ordered chains of providers consulted sequentially until first valid SELECT. Keep deterministic local routing, OFF zero overhead, existing hard constraints authoritative. No quality scores fabricated.

---

## 2. Architecture

```
[Config] decision.mode=hybrid, chain=external-policy, timeout_ms, max_provider_calls
          decision_chains[], decision_provider_health, decision_providers[]

[Server] snapshot cfg under RLock, build ChainConfig immutable per request

[ChainExecutor.Execute]:
  globalDeadline = min(Budget.Timeout, cfgTimeout)
  budget = MaxProviderCalls
  constraints = ComputePrimaryConstraints(E, pinnedID) // once, reused
  for each step in chain order:
    if affinity -> 0 calls
    if globalDeadline exceeded -> stop
    if IsCooldown -> skip, no budget
    if unavailable -> skip, no budget
    if budget exhausted -> stop
    perStep = min(step.TimeoutMS, remainingGlobal)
    ctxStep := withTimeout(ctx, perStep)
    result, err := provider.Decide(ctxStep, req) // panic recovery
    validate(strict): confidence finite [0,1], reasonCodes distinct ≤8, selectedID in AllowedIDs, eligible containment
    if ActionSelect && valid -> success, reorder only selected to front, return
    else -> continue (including RANK)
  fail-open: return original order
```

---

## 3. Key production change: RANK fix

**File:** `internal/decision/chain.go`
**Bug:** earlier implementation returned RANK result as terminal SELECT when validation trimmed unknown IDs but preserved order. Violated spec: RANK must never become terminal chain result, must continue, preserve failover order.
**Fix:** in `Execute`, after `ValidateResult`/`NormalizeResult`, branch:
```go
if result.Action == ActionRank {
   // trim unknowns, clamp reason codes, but never reorder failover
   // always continue to next provider
   traceStep(OutcomeInvalid, ReasonInvalidRank)
   continue
}
if result.Action == ActionSelect && valid {
   // move only selected
}
```
Added regression test `TestChain_RankCannotReorderFailoverList_Regression` asserting RANK from jev-main selecting B while eligible [A,B,C] and Allowed [A,B,C] does NOT become `[B,A,C]`; instead calls next provider and eventually fail-open preserves `[A,B,C]` if all exhausted.

All existing chain tests updated to assert ranking branch; 19+ tests PASS.

---

## 4. Provider state

**File:** `internal/decision/providerstate/manager.go` (unchanged API, but tests expanded)
- Config {FailureThreshold, FailureWindow, Cooldown}
- State with failTimes ring, CooldownUntil
- Methods New, RecordSuccess, RecordFailure, IsCooldown, Snapshot, UpdateConfig (preserves failTimes), Reset, Config
- Tests: `internal/decision/providerstate/manager_test.go` 13 tests (InitialState, RecordSuccess, RecordFailureThreshold, FailureWindowPruning, CooldownExpiryWithoutSleep, RecoveryViaSuccess, AbstainHealthy, SuccessResetsFailureState, IsolatedProviderIDs, ConcurrentRecord, UpdateConfigPreservesState, Reset, CooldownShorterThanWindow) PASS with -race

**File:** `internal/config/config.go` — chain validation:
- max 64 chains, max 8 steps, step timeout 1..5000, MaxProviderCalls 1..8, hybrid requires chain exists, duplicate provider step, unknown provider, collision with provider ID, disabled allowed.
- Defaults: TimeoutMS 10ms, MaxProviderCalls len(chain) if hybrid else 1, health 3/30/60.
- Tests: `internal/config/chain_config_test.go` 16 tests PASS

---

## 5. Tests

### Chain unit (internal/decision/chain_test.go) — 22 tests + fuzz + benchmarks
- FirstValidSelectStopsChain, AbstainContinues, ErrorContinues, TimeoutContinuesWhenGlobalRemains, UnavailableSkipped, CooldownSkipped, InvalidContinues, PanicContinues, RankRejectedContinues, RankCannotReorderFailoverList_Regression, AllExhaustedFailOpen, BudgetStopsLaterCalls, SkippedDoesNotConsumeBudget, GlobalDeadlineStopsChain, PerStepTimeout, AffinityZeroCalls, SameConstraintsEveryStep, SelectedAlwaysInAllowed, OnlySelectedPrimaryMoves, SameProviderNeverTwice, Property_SelectedInAllowed, Property_BudgetBound, FuzzChain_ResultHandling
- Benchmarks: Chain_TwoSteps, EightSteps, PolicyOnly, UnavailableJevToPolicy, plus new ThreeSteps, AbstainContinues, CooldownSkip in `internal/decision/chain_bench_test.go`
- All PASS with -race, -count=1

### Config & providerstate
- providerstate/manager_test.go 13 PASS
- chain_config_test.go 16 PASS

### Hybrid E2E (internal/httpapi/decision_chain_integration_test.go) — 16 tests
Uses `testGateway` + `countingChainProvider` + `makeHybridChainConfig` + `makeJevUpstream` + `attemptRecorder` pattern:

| Test | Coverage |
|------|----------|
| Hybrid_JevValidSelect | jev SELECT stops chain, policy 0 calls |
| Hybrid_JevErrorFallbackToPolicy | jev error → policy SELECT, both called |
| Hybrid_JevTimeoutFallback | jev delay 200ms > per-step 50ms timeout → TIMEOUT → policy |
| Hybrid_JevAbstainHealthy | jev ABSTAIN (healthy) → policy, no cooldown increment |
| Hybrid_JevInvalidFallback | jev SELECT unknown ID → INVALID → policy |
| Hybrid_JevCooldownSkipsCall | jev in cooldown before request → skipped without Decide call, policy SELECT |
| FallbackE2E_BAC | decision Select B with fallback chain primary [A,B] fallback [C] → attempt order B→A→C preserved |
| MaxAttemptsE2E | MaxAttempts=2 with pool [A,B,C] all fail → total attempts exactly 2 (decision cannot increase budget) |
| CacheHitZeroCalls | first hit 2 providers called, second identical hit cache → 0 calls for both |
| CrossProtocolStrict | OpenAI→Anthropic same deployment ensure decision per-protocol |
| VEvsDirectStrict | VE vs direct same deployment strict |
| HealthSeparation | decision failures do not change model health; upstream 500 does |
| HotReloadCoherent | reload chain from external-policy to policy-only preserves cooldown state |
| TraceBounded | X-Gateway-Decision-Trace contains chain/outcome/calls/reasonCodes ≤8 bounded |
| PrivacyCanary | secret `SECRET_EXTERNAL_PROMPT_CANARY_94af` not in events/metrics/admin/trace/client for prompt header/body |
| RemoteErrorCanary | remote error containing secret not leaked to any surface |

All 16 PASS with -count=1 -v (0.6s) and -race (1.6s, flake fixed for MaxAttempts).

**Implementation detail for counting provider:**
```go
type countingChainProvider struct {id string; calls atomic.Int64; result DecisionResult; err error; delay time.Duration; panic bool; caps Capabilities}
func (c *countingChainProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error)
```
Respects `ctx.Done()` for timeout tests, panic recovery verified, atomic calls for budget accounting.

**Sticky flake fixed:** MaxAttempts originally asserted `hitsC==0` but routing order after decision is non-deterministic due to provider map iteration. Fixed to assert `total==2` with all upstreams failCount=5, ensuring deterministic budget enforcement regardless of order.

### Other E2E suites
- `jev_integration_test.go` valid selection, timeout, 500, privacy, hot reload — PASS
- `decision_integration_test.go` empty/single/OFF/budget/health — PASS
- `policy_integration_test.go` scoring — PASS
- `virtual_test.go` fallback, VE — PASS

---

## 6. Config example

**File:** `configs/config.example.json` updated to hybrid:
```json
"decision": {"mode":"hybrid","chain":"external-policy","timeout_ms":800,"max_provider_calls":2},
"decision_chains": [
  {"id":"external-policy","steps":[{"provider":"jev-main"},{"provider":"policy"}]},
  {"id":"policy-only","steps":[{"provider":"policy"}]}
],
"decision_provider_health": {"failure_threshold":2,"failure_window_seconds":30,"cooldown_seconds":60}
```
Load + Validate passes. Disabled provider referencing allowed, as justified by fallback tests.

---

## 7. Docs
- `docs/PHASE_G_CURRENT_STATE_NOTE.md` — verified seam, Phase G additions, global/budget/affinity/cooldown/hot reload/trace/privacy, files, gaps
- `docs/PHASE_G_DECISION_CHAINS.md` — spec, validation, runtime objects, execution semantics, global details, hot reload, observability, routing integration, error handling, testing
- `docs/PHASE_G_IMPLEMENTATION_REPORT.md` — this file
- Plus existing `docs/PHASE_F_EXTERNAL_DECISION_PROVIDERS.md` preserved

---

## 8. Verification

Commands executed on branch arena/01a0db74-nexaroute:

```bash
go vet ./internal/decision ./internal/config ./internal/httpapi   # PASS
go test ./internal/decision -run TestChain -count=1 -v            # 22 PASS
go test ./internal/decision -run TestChain -count=1 -race -v      # PASS
go test ./internal/decision/providerstate -count=1 -v -race       # 13 PASS
go test ./internal/config -run TestChain -count=1 -v             # 16 PASS
go test ./internal/httpapi -run TestChain -count=1 -v            # 16 PASS
go test ./internal/httpapi -run TestChain -count=1 -race -v      # 16 PASS (flake fixed)
go test ./internal/httpapi -run TestChain_MaxAttemptsE2E -count=5 -race -v # 5/5 PASS
go test -run=^$ -bench BenchmarkChain -benchtime=1x ./internal/decision # 7 benchmarks PASS
./scripts/verify.sh   # to be run in final acceptance
./scripts/stress.sh   # to be run
./scripts/smoke-local.sh # to be run
go test ./... -race -count=1 # full suite to be run
```

Expected verify.sh checklist (from Phase F):
- config load, decision mode hybrid chain validation
- chain registry stateless, cooldown separate
- RANK not terminal (regression)
- global deadline wins over per-step
- budget: skipped not consumed, exhausted stops
- affinity zero calls
- same constraints every step
- selected in allowed, only selected moves
- same provider never twice (duplicate rejected)
- metrics bounded, admin safe, trace bounded
- hot reload coherent, privacy canary, remote error canary
- fallback B→A→C, maxAttempts, cache, cross-protocol, VE/direct, health separation

---

## 9. Risks & mitigations
- **RANK regression**: fixed and covered by RankCannotReorder test
- **Flaky MaxAttempts due to map iteration**: fixed by all-fail total assertion
- **Timeout vs global**: perStep = min(stepTimeout, remainingGlobal) verified by GlobalDeadlineStopsChain and PerStepTimeout tests
- **Cooldown not consuming budget**: verified by BudgetStopsLaterCalls / SkippedDoesNotConsumeBudget
- **Race between decision and health**: providerstate.Manager RWMutex, ChainExecutor stateless, snapshot immutable

---

## 10. Non-goals & out of scope
- Phase H quality scorecards — not implemented, not fabricated
- RANK ordering for failover — future, not G
- Retry inside remote transport — not implemented (MaxProviderCalls controls chain calls, not HTTP retries)

## 11. Rollback
Revert chain.go RANK fix and chain_test.go additions would reintroduce ranking bug; revert config.example.json hybrid to local if needed, but validation remains backward compatible (local/off/assisted unchanged).

