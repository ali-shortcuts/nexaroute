# Phase D Current-State Note — DecisionProvider Contracts + Orchestrator

Date: 2026-09-25
Branch: arena/01a0d825-nexaroute (Phase C baseline 2cf905c)
Spec sections: §7 DecisionProvider contracts, §8 orchestrator, §9 eligible-set validator, §10 decision budget, §11 privacy

## Pipeline Before Phase D (verified)

```
Client (OpenAI / Anthropic / Responses)
  ↓ raw JSON bounded
Protocol decode (core.OpenAIRequest etc)
  ↓
Feature Extractor (internal/feature) → RequestFeatures (privacy-safe)
  ↓
Task Analyzer (internal/taskprofile) → TaskProfile (deterministic, no raw prompt)
  ↓
router.Requirement (Model, Tools, Vision, Streaming, Reasoning, EstimatedInputTokens, MaxOutputTokens, MinContextWindow, SessionKey)
  ↓
candidatesForRequirement(req, protocol):
  - snapshot cfg, resolver, router under RLock
  - resolveVirtualEndpoint(req.Model) → ResolvedRoute (VE ID, public_model, route_profile, pool IDs)
  - if !isVirtual: rt.Candidates(req) → eligible filtered by health, capabilities, provider circuit, context window
  - if isVirtual: protocol restriction exact (empty→allow all, "anthropic"→/v1/messages, "openai"→/v1/chat/completions, "openai_responses"/"responses"→/v1/responses), then reqAll Model="" → rt.Candidates(reqAll) → resolver.AllFilteredCandidates (pool ∩ eligible, dedup, preserve router order)
  - returns cfg, candidates E, resolvedRoute, error if disabled/protocol not allowed
  ↓
task_classified event (privacy-safe)
  ↓
[Decision Plane seam HERE] — eligible set E is authoritative
  ↓
cache lookup (exact-match, opt-in)
  ↓
maxAttempts = min(cfg.Routing.MaxAttempts, len(candidates))
  ↓
Execution loop with failover, hedging, capability repair, session pinning
```

Eligible set E definition: after VE disabled check, protocol check, route profile → candidate pool → AllFilteredCandidates (pool ∩ eligibility) where eligibility = protocol compatibility, capability (tools/vision/streaming/reasoning), health (Healthy only for ready_mesh/cost_aware, not Cooldown otherwise), provider availability, context-window fit, missing/invalid credentials, client policy, security policy.

## Decision Plane Seam Selected

**Location:** immediately after `candidatesForRequirement` returns E and after `task_classified` event, before cache lookup and before execution/failover. Implemented in:
- `internal/httpapi/openai.go`
- `internal/httpapi/anthropic.go`
- `internal/httpapi/canonical_path.go`

Why this seam:
- Router is eligibility owner and must NOT import decision (no cycle, no policy in router)
- Decision must rank ONLY within E, never resurrect unhealthy/incompatible/out-of-pool candidates
- Preserves failover coverage: normalization appends omitted eligible IDs in original order
- OFF mode zero overhead: early return before any decision work, preserves exact order
- Privacy: DecisionRequest uses only RequestFeatures + TaskProfile + candidate IDs, no raw prompt, no headers, no secrets

## Files Touched (Phase D)

New package `internal/decision`:
- `provider.go` — DecisionProvider interface ID/Capabilities/Health/Decide
- `request.go` — DecisionRequest (TaskProfile, Features, []Candidate snapshot, VE/route/pool IDs, Constraints, Budget, RequestID), Candidate struct (ID, ProviderID, Model, Priority, Weight)
- `result.go` — Action SELECT/RANK/ABSTAIN, SelectedID, RankedIDs, Confidence 0-1, ReasonCodes, ProviderID, Abstained, Latency
- `validator.go` — ValidateResult (eligible-set invariant, duplicate, unknown, confidence bounds, action), NormalizeResult (ABSTAIN→original, SELECT→[selected]+rest original, RANK→ranked + omitted appended original order)
- `budget.go` — Budget Timeout, MaxCandidates, DefaultBudget 10ms
- `local.go` — LocalProvider pass-through preserving order, reason EXISTING_ORDER_PRESERVED, respects context cancellation
- `reason.go` — bounded reason codes
- `registry.go` — Registry with local pre-registered, Resolve, Snapshot
- `metrics.go` — Metrics with bounded cardinality, GlobalMetrics
- `orchestrator.go` — Orchestrator with budget/timeout/panic recovery/fail-open/normalization, hot-reload coherent config snapshot, MetricsSnapshot
- `*_test.go` — validator, local, orchestrator, privacy (canary SECRET_DECISION_CANARY_82c1), property (eligible-set invariant), benchmark

Modified:
- `internal/config/config.go` — DecisionConfig struct Mode off|local, Provider local, TimeoutMS default 10ms bounded 1-5000, Config.Decision field, ApplyDefaults trims/lowercases/defaults, Validate rejects invalid, Default() sets decision defaults
- `internal/httpapi/server.go` — adds decisionRegistry, decisionOrchestrator fields, New() instantiates, applyConfigLocked updates orchestrator config on hot-reload
- `internal/httpapi/decision_wiring.go` — NEW: decisionCandidates conversion, reorderScoredByDecision, applyDecisionPlane (OFF fast path, builds DecisionRequest from ti.Features, ti.Profile, resolvedRoute IDs, Budget from config, RequestID, calls orchestrator)
- `internal/httpapi/metrics.go` — adds decision metrics (decisions_total by outcome, latency avg/count) bounded
- `internal/httpapi/admin.go` — snapshot includes decision config, metrics, provider health

Unchanged intentionally:
- `internal/router` — no import of decision, eligibility owner stays pure
- `internal/route` — no decision awareness
- `internal/feature`, `internal/taskprofile` — no decision dependency, privacy-safe

## Invariants Enforced

1. **Eligible-set invariant:** For all decision results, returned IDs ⊆ E. Unknown IDs rejected → fail-open preserves original order. Tested via validator and property test (200 random iterations, 20% invalid injection, 10% panic).

2. **No resurrection:** Unhealthy, incompatible, out-of-pool, disabled VE, protocol-not-allowed candidates never appear in E, thus never in decision output. Decision plane is downstream of `candidatesForRequirement`.

3. **Fail-open:** On timeout (context DeadlineExceeded), error, panic, invalid result (unknown/duplicate/confidence out of bounds/empty eligible), unknown provider → return original order unchanged, mark abstained, record reason codes TIMEOUT, PROVIDER_ERROR, PROVIDER_PANIC, INVALID_RESULT, VALIDATION_FAILED.

4. **ABSTAIN preserves exact order:** LocalProvider returns ABSTAIN with EXISTING_ORDER_PRESERVED, orchestrator returns CloneCandidates(original).

5. **Normalization preserves failover:** If provider returns partial RANK [C,A] from [A,B,C,D] → final [C,A,B,D] (omitted appended original order). SELECT C from [A,B,C,D] → [C,A,B,D]. Ensures max_attempts failover still has coverage.

6. **OFF zero overhead semantic:** Mode off returns original order, no provider call, metrics off_mode_total increments, no latency. Local mode preserves order but exercises contract.

7. **Budget:** Timeout from config (default 10ms) bounded 1-5000ms, per-request Budget can tighten. Orchestrator uses context.WithTimeout, respects parent cancellation.

8. **Privacy:** DecisionRequest contains only TaskProfile + RequestFeatures + candidate IDs + VE/route/pool IDs + Budget + RequestID. No raw prompts, no tool results, no headers, no API keys. Canary SECRET_DECISION_CANARY_82c1 tested not to leak in JSON marshal.

9. **Hot-reload coherence:** applyConfigLocked updates orchestrator config atomically under runtimeMu, ensuring snapshot coherence.

10. **Bounded cardinality:** Reason codes are bounded constants, metrics use outcome label from known set, not raw IDs.

## Risks and Mitigations

- **Decision provider latency:** Default 10ms, bounded 5s max, fail-open on timeout, does not block client response beyond budget.
- **Panic in provider:** Recovered, logged via reason code, fail-open.
- **Invalid result injection:** Validator rejects, fail-open.
- **Router cycle:** Router does not import decision; decision uses its own Candidate snapshot, not router.Scored directly (conversion in httpapi wiring).
- **Config backward compatibility:** Existing configs without decision field load with defaults via ApplyDefaults, Default() sets decision, Validate allows empty as off? Now Default() sets off/local/10ms, ApplyDefaults fills empty, Validate rejects only invalid enums/timeout out of bounds.
- **Metrics cardinality:** Decision metrics use bounded outcome labels (decisions_total, abstains_total, etc.), not per-candidate.
- **OFF mode regression:** OFF must behave like current version (Phase C). Tested via orchestrator off mode test and routing neutrality preserved.

## Why DecisionProvider Not Inside Router Eligibility

- Router is authoritative for health, capabilities, provider circuits, context window, credentials, security. Decision is optional intelligence ranking within E.
- Mixing would create cycle and violate priority CORRECTNESS > COMPATIBILITY > RELIABILITY > OBSERVABILITY > INTELLIGENCE > LEARNING.
- Router neutrality: taskprofile and feature packages already not imported in router; decision also not imported.
- Decision OFF must be zero overhead and zero semantic impact; if inside router, OFF would still require call.

## Next Steps (Phase E onward)

- Phase E: multi-objective policy engine + reason codes (will use TaskProfile + RequestFeatures + DecisionResult, but not change Phase D contracts)
- Phase F: first external adapter (e.g. Jev) isolated in its own package, must implement DecisionProvider, must not leak raw prompts by default (METADATA_ONLY), must be behind adapter boundary
- Metrics/events already extended; dashboard will show decision config/metrics in Phase J

## Verification Checklist for Phase D (before implementation report)

- [ ] Config defaults off/local/10ms, validation
- [ ] DecisionProvider interface with ID/Capabilities/Health/Decide
- [ ] Request/Result structs with privacy-safe fields only
- [ ] Validator enforces eligible-set invariant, duplicate, unknown, confidence bounds
- [ ] Normalization preserves failover (omitted appended original order)
- [ ] Orchestrator budget, timeout, panic recovery, fail-open, OFF zero overhead
- [ ] Local provider preserves order, reason EXISTING_ORDER_PRESERVED
- [ ] Registry with local pre-registered, snapshot for admin
- [ ] Integration in openai.go, anthropic.go, canonical_path.go after candidatesForRequirement
- [ ] Metrics bounded, admin snapshot extension
- [ ] Hot-reload coherent config update
- [ ] Privacy canary test SECRET_DECISION_CANARY_82c1 not leaked
- [ ] Property tests for eligible-set invariant, fail-open
- [ ] Benchmarks for OFF, local, validator, normalize
- [ ] gofmt, go build, go test ./... PASS, race, verify.sh, stress.sh, smoke-local.sh
