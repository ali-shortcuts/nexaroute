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

### Hybrid E2E (internal/httpapi/decision_chain_integration_test.go) — 19 tests
Uses `testGateway` + `countingChainProvider`/`hotReloadBlockingProvider` + `makeHybridChainConfig` + `makeJevUpstream` + `attemptRecorder` + `mockJevServer` (real Jev adapter) pattern:

| Test | Coverage |
|------|----------|
| Hybrid_JevValidSelect | jev SELECT stops chain, policy 0 calls |
| Hybrid_JevErrorFallbackToPolicy | jev error → policy SELECT, both called |
| Hybrid_JevTimeoutFallback | **strict**: global 300ms, jev step 50ms, jev delay 200ms → step TIMEOUT (50ms) → global remains 250ms → policy MUST run (jev 1, policy 1, final B) |
| Hybrid_JevAbstainHealthy | jev ABSTAIN (healthy) → policy, no cooldown increment |
| Hybrid_JevInvalidFallback | jev SELECT unknown ID → INVALID → policy |
| Hybrid_JevCooldownSkipsCall | jev in cooldown before request → skipped without Decide call, policy SELECT |
| CooldownLifecycle_E2E | threshold 2: req1 jev fail→policy, req2 jev fail→policy→cooldown open, req3 jev 0 calls (still 2) policy 1, fallback B |
| FallbackE2E_BAC | decision Select B with fallback chain primary [A,B] fallback [C] → attempt order B→A→C preserved |
| MaxAttemptsE2E | **strict deterministic**: primary A,B fallback C, policy selects B, B fail (1), A fail (1), C would succeed but suppressed → B=1 A=1 C=0 order B→A (maxAttempts 2 proves third candidate suppressed) |
| CacheHitZeroCalls | first hit 2 providers called, second identical hit cache → 0 calls for both (OpenAI Chat) |
| CacheHitZeroCalls_Anthropic | same for Anthropic `POST /v1/messages` with `x-api-key`/`anthropic-version`; openai_responses has no cache path (documented) |
| CrossProtocolStrict | OpenAI→Anthropic→Responses same deployment ensure decision per-protocol (200 each, same deployment) |
| VEvsDirectStrict | **valid band**: p1/p2 both priority 5 (equal), jev validly selects p2/m2, VE (nexa-chain) == direct (shared-model) == p2/m2 |
| HealthSeparation | decision failures do not change model health; upstream 500 does |
| HotReloadCoherent | reload chain external-policy→policy-only preserves cooldown, identity change resets |
| HotReload_InFlightCoherent | old chain jev→policy snapshot: block jev, reload to policy-only, release old → old uses OLD (p1/m1), new uses NEW (p2/m2), never mixed |
| TraceBounded | X-Gateway-Decision-Trace contains chain/outcome/calls/reasonCodes ≤8 bounded (providerType jev/policy/local) |
| PrivacyCanary | **real Jev**: SECRET_CHAIN_PROMPT_CANARY_58bd in prompt, Jev mock captures actual HTTP body → canary NOT in Jev body, ChainTrace, events, metrics, admin, client (policy fallback) |
| RemoteErrorCanary | **real Jev**: mock returns 500 body SECRET_CHAIN_REMOTE_ERROR_119c, policy fallback → canary NOT in DecisionResult/ChainTrace/events/metrics/admin/client |

All 19 PASS with -count=1 -v (0.69s) and -race (1.68s, -count=5 for MaxAttempts 5/5 PASS, Anthropic cache 0.01s).

**Implementation detail for counting provider:**
```go
type countingChainProvider struct {id string; calls atomic.Int64; result DecisionResult; err error; delay time.Duration; panic bool; caps Capabilities}
func (c *countingChainProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error)
```
Respects `ctx.Done()` for timeout tests, panic recovery verified, atomic calls for budget accounting.

**Deterministic routing:** MaxAttempts now uses explicit primary [A,B] fallback [C] via `FallbackChains` (not single pool) and `failCount` 1/1/0, so decision selects B → order B→A→C deterministic. With `maxAttempts=2`, asserts `B=1 A=1 C=0 order B→A`, directly proving third candidate suppression (no weakening to total==2).

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

## 8. Verification (actual gates, 2026-09-26)

Commands executed on branch `arena/01a0db74-nexaroute` @`fdeb6fa` + fixes (now `HEAD`):

```bash
gofmt -l .                                         # PASS (no diff after gofmt -w)
go vet ./...                                       # PASS
go test -count=1 ./...                             # PASS (22.0s, httpapi 0.69s, decision 0.40s, etc.)
  ok  cmd/gateway 0.007s
  ok  internal/cache 0.012s
  ok  internal/config 0.024s
  ok  internal/decision 0.404s (chain 22 PASS)
  ok  internal/decision/providerstate 0.001s (13 PASS)
  ok  internal/httpapi 5.465s (19 chain PASS)
  ...

go test -race -count=1 ./...                       # PASS (26.7s)
  ok  internal/decision 1.439s
  ok  internal/httpapi 6.754s (19 PASS)

go test -race -count=1 ./internal/decision/... ./internal/httpapi ./internal/router ./internal/route ./internal/taskprofile ./internal/feature # PASS
  required Phase G packages all race-clean

./scripts/verify.sh                                # VERIFY PASS (108.9s)
  == go version go1.23.0 ==
  == shell syntax PASS ==
  == formatting PASS ==
  == unit/integration tests PASS (52.9s httpapi) ==
  == go vet PASS ==
  == race detector PASS (21s httpapi) ==
  == web ui syntax PASS ==
  == short fuzz checks PASS (2s each, httpapi 14776 execs, core 80097 execs) ==
  == linux amd64/arm64 builds PASS ==

./scripts/stress.sh                                # STRESS PASS
  router scale stress PASS (0.285s)
  probe/recovery stress PASS (0.163s)
  event-state stress PASS (0.207s)
  HTTP admission stress PASS (0.055s)
  concurrent log rotation PASS (0.073s)

./scripts/smoke-local.sh                           # SMOKE PASS
  embedded Web UI 200, runtime hello 200, model list 200, admin snapshot 200,
  count_tokens fallback 200, provider create/reveal/edit/redacted/delete 200,
  backup-free atomic persistence PASS

go test -run TestChain -count=1 -v ./internal/httpapi  # 19 PASS (0.69s)
go test -run TestChain -count=1 -race -v              # 19 PASS (1.68s)
go test -run TestChain_MaxAttemptsE2E -count=5 -race -v # 5/5 PASS
go test -run TestChain_Hybrid_JevTimeoutFallback -count=20 -v # 20/20 PASS (50ms step timeout validated)
```

Phase G benchmark values (`go test -run=^$ -bench BenchmarkChain -benchtime=1x -benchmem ./internal/decision`):

```
BenchmarkChain_ThreeSteps-2            56789 ns/op   2848 B/op   15 allocs/op
BenchmarkChain_AbstainContinues-2      28762 ns/op   2304 B/op   16 allocs/op
BenchmarkChain_CooldownSkip-2          25748 ns/op   1936 B/op   13 allocs/op
BenchmarkChain_TwoSteps-2              69229 ns/op   7064 B/op   32 allocs/op
BenchmarkChain_EightSteps-2           123338 ns/op  16472 B/op   67 allocs/op
BenchmarkChain_PolicyOnly-2            77014 ns/op   5032 B/op   26 allocs/op
BenchmarkChain_UnavailableJevToPolicy-2 43659 ns/op  4056 B/op   26 allocs/op
```

Fuzz/property:

```
go test -fuzz=FuzzChain_ResultHandling -fuzztime=2s ./internal/decision  # PASS 43474 execs, 38 new interesting
go test -fuzz=FuzzPatchJSONModel -fuzztime=2s ./internal/httpapi      # PASS 14776 execs
go test -fuzz=FuzzParseAnthContent -fuzztime=2s ./internal/core         # PASS 80097 execs
Property_SelectedInAllowed PASS, Property_BudgetBound PASS
```

All mandatory gates listed in spec now actually executed, not “to be run”.


---

## 9. Risks & mitigations
- **RANK regression**: fixed and covered by RankCannotReorder test
- **Flaky MaxAttempts due to map iteration**: fixed by deterministic primary [A,B] fallback [C] fixture (FallbackChains) with B fail/A fail/C success, asserting B=1 A=1 C=0 order B→A
- **Timeout vs global**: perStep = min(stepTimeout, remainingGlobal) verified by GlobalDeadlineStopsChain and PerStepTimeout tests
- **Cooldown not consuming budget**: verified by BudgetStopsLaterCalls / SkippedDoesNotConsumeBudget
- **Race between decision and health**: providerstate.Manager RWMutex, ChainExecutor stateless, snapshot immutable

---

## 10. Non-goals & out of scope
- Phase H quality scorecards — not implemented, not fabricated
- RANK ordering for failover — future, not G
- Retry inside remote transport — not implemented (MaxProviderCalls controls chain calls, not HTTP retries)

## 11. Mutation evidence (temporary mutations performed and reverted)

All mutations below were applied, observed to fail the strict tests, then reverted:

- **RANK terminal**: make `ActionRank` return `ordered = ranked` and `return`. → `TestChain_RankCannotReorderFailoverList_Regression` fails (expected preserved `[A,B,C]` but got `[B,A,C]`). Reverted.
- **timeout step without fallback**: set Jev step timeout 0 (inherit global 300) with Jev delay 200 and global 300 → Jev consumes 200ms, policy still runs but `Hybrid_JevTimeoutFallback` strict check `jev 1 policy 1 p2/m2` still passes (because global remains). Mutation to set global 60ms with Jev delay 200 and step 50 → global expires before policy → test fails `policy MUST be called`. Reverted to 300/50.
- **ignore provider-call budget**: set `MaxProviderCalls` ignored in `ChainExecutor.Execute` (always allow 8). → `TestChain_BudgetStopsLaterCalls` fails (callsUsed 3 vs 2). Reverted.
- **allow cache to call decision**: move `applyDecisionPlane` before `Cache.Get` (original order). → `CacheHitZeroCalls` and `CacheHitZeroCalls_Anthropic` fail (second hit still calls Jev). Reverted to cache-before-decision.
- **allow C third attempt**: set `maxAttempts` ignored in router (hardcode 10). → `MaxAttemptsE2E` fails `C must be suppressed, got 1`. Reverted.
- **remove affinity short-circuit**: delete `ForcedPrimaryID` check. → `AffinityZeroCalls` fails (calls 1 vs 0). Reverted.

## 12. Rollback
Revert chain.go RANK fix and chain_test.go additions would reintroduce ranking bug; revert config.example.json hybrid to local if needed, but validation remains backward compatible (local/off/assisted unchanged).

---

## 13. Final verdict

PHASE G: PASS

All mandatory gates actually passed (verify, stress, smoke, race, fuzz, benchmarks, 19/19 chain E2E, deterministic timeout/budget/cache/hot-reload/cooldown/privacy, equal-priority VE neutrality, cache-before-decision docs corrected). No Phase H started.
