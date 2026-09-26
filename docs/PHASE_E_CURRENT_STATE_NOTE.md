# Phase E Current-State Note — Policy Engine Baseline

Date: 2026-09-25
Branch: arena/01a0d825-nexaroute
Baseline: 7f8c0a97f7580645598267988b14a55f3b0c5c7c (Phase D PASS final convergence)

## Verified Phase D seam

```
candidatesForRequirement(req, protocol):
  - snapshot cfg, resolver, router under RLock
  - resolveVirtualEndpoint(req.Model) → ResolvedRoute (VE ID, public_model, route_profile, primaryPool, orderedPoolIDs, allowed sets)
  - if !virtual: rt.Candidates(req) → E
  - if virtual: protocol exact check, reqAll Model="" → rt.Candidates(reqAll) → resolver.AllFilteredCandidates (primary then fallback pools, dedup first occurrence, preserve router order inside each pool)
  - returns cfg, E, resolvedRoute, error
  ↓
task_classified event
  ↓
[Decision Plane seam] — after E, before cache/execution
  - orchestrator.Decide handles empty (EMPTY_ELIGIBLE no call), single (SINGLE_CANDIDATE no call), OFF (OFF_MODE), budget MaxProviderCalls, health unavailable → PROVIDER_UNHEALTHY no call, capabilities enforcement, timeout, panic recovery, strict validation, normalization preserving failover
  - wiring applyDecisionPlane in openai.go, anthropic.go, canonical_path.go
  ↓
cache, maxAttempts, execution loop
```

Router remains eligibility owner, does NOT import decision. DecisionRequest privacy-safe: TaskProfile+Features+Candidate snapshot+VE/route/pool IDs+Budget+RequestID, no raw prompt, no secrets.

## Available candidate signals (trustworthy, already-known operational facts)

From `router.Scored`:
- Deployment: ID (providerID/modelID), ProviderID, ProviderName, ProviderType, Model (upstream), Aliases, Priority (operator-configured), Weight, ContextWindow (0 if unknown), InputCostPerMTok, OutputCostPerMTok, Capabilities {Streaming, Tools, Vision, Reasoning}
- Health: health.State {Status (Healthy, Unknown, HalfOpen, Degraded, Cooldown), EWMALatencyMS, EWMATTFTMS, EWMAFailureRate, Successes, Failures, ConsecutiveFailures, LastFailure, Scopes}
- Score: router baseline Score float64 (already includes health, priority, weight, latency weight, failure weight, capacity weight)
- CapacityPressure: float64 derived from ProviderLoad (active, waiting, limit, quota pressure)
- EstimatedCostUSD: float64 derived from EstimatedInputTokens+MaxOutputTokens * per-MTok cost, PriceKnown bool

From `router.Requirement` and `router.ProviderLoad`:
- ProviderLoad map: Active, Waiting, Limit, QuotaExhausted, QuotaPressure — already summarized as CapacityPressure, but raw available if needed
- MinContextWindow, EstimatedInputTokens, MaxOutputTokens — for context headroom calculation

From `route.Resolver` and `ResolvedRoute`:
- PrimaryPoolID, OrderedPoolIDs, AllowedDeployments, FallbackAllowed, AllAllowed, PrimaryMode
- Pool expansion: poolID → set of deployment IDs (explicit or all)
- Deduplication semantics: first pool where deployment appears wins

From session affinity:
- Router has `pinned(req, cfg)` returning pinned deployment ID if session affinity enabled and pin exists and not expired. Pin key = hash(session_id)+model+providerType+scopes. Pin stores Deployment string and Expires. SessionKey itself is hashed, not exposed raw.
- Need safe read-only method to get pinned deployment for Requirement without exposing raw SessionKey.

From usage:
- `usage.Tracker` records prompt/completion tokens per deployment, but not directly in Scored; could be added if trustworthy, but currently not needed for Phase E.

## Unavailable signals (MUST NOT be invented)

- No empirical model-quality scorecards (coding quality, reasoning quality) — Phase H. Must not fabricate.
- No embeddings, no LLM judge, no evaluation results.
- No real-time cost beyond EstimatedCostUSD from config pricing + token estimates.
- No latency beyond EWMA values from health manager.
- No per-request provider load beyond CapacityPressure snapshot.
- No credential identity, API keys, headers, raw prompts, tool schemas, user identity.
- No session key raw value — only pinned deployment ID.
- No hidden reasoning, chain-of-thought.

## Policy insertion point

Same seam as Phase D: after `candidatesForRequirement` returns E, after `task_classified` event, before cache lookup and execution. This ensures:

- Eligibility already enforced (health, capabilities, context window, provider circuit, quota hard rejection, VE disabled, protocol)
- Pool containment already enforced (AllFilteredCandidates)
- TaskProfile available (TaskType, Complexity, Confidence, ReasonCodes, EstimatedContextTokens, Requires* flags)
- RequestFeatures available (protocol, streaming, vision count, tool count, etc.)
- Session pin can be read via router method without exposing raw key

Phase E PolicyProvider will be registered as `provider ID = policy` in existing `decision.Registry`, used when `decision.provider=policy` and `decision.mode=local`.

## Pool/fallback semantics (must preserve)

- `Resolver.AllFilteredCandidates(candidates, resolved)` concatenates:
  1. Filter primary pool: `FilterCandidates(allCandidates, primaryAllowed, primaryMode)`
  2. For each fallback pool in chain order: filter and append if not already seen (dedup first occurrence)
  - Preserves router ordering inside each pool.
  - Duplicate deployment in two pools attempted once.
- `OrderedPoolIDs` = [primary, fallback1, fallback2...]
- PoolOrdinal: 0 for primary, 1 for fallback1, 2 for fallback2, etc. If deployment appears in multiple pools, assign first ordinal where it appears.
- **Fallback hard boundary**: Policy MUST NOT select fallback tier over earlier non-empty pool. Winner must come from minimum PoolOrdinal among eligible.
- Implementation: need to add PoolID and PoolOrdinal to Decision Candidate snapshot, derived from ResolvedRoute and expanded sets.

## Affinity semantics (must preserve)

- Session affinity pins physical deployment ID, not virtual public model.
- Pin key = hash(session_id)+model+providerType+scopes, stored in Router.sessions map with expiry.
- `pinned(req, cfg)` returns deployment ID if affinity enabled and pin exists and not expired.
- `ObserveSession` records success only after actual execution success, not on failure.
- For VE, eligibility uses `reqEligible.Model=""` so pin lookup works with physical deployment.
- Phase E must NOT override pin by default. If eligible pin exists, policy should ABSTAIN or SELECT pinned candidate, reason AFFINITY_PRESERVED, without scoring work.
- DecisionRequest may carry `PinnedCandidateID` but NEVER raw SessionKey.
- Need safe Router method returning pinned deployment for Requirement without exposing session key.

## Scoring risks

- **Unknown data as best**: Unknown latency (0), unknown cost (0), unknown failure rate (0) must NOT be treated as best. Unknown → neutral (0.5).
- **Division by zero**: Normalize RouterScore relative to active selection band, handle all equal case as neutral.
- **NaN/Inf**: All components must defend against NaN/Inf, negative telemetry, invalid cost/latency/context, clamp to [0,1] finite.
- **Priority override**: Operator priority is hard guardrail, not just score component. Within active pool, find best/minimum priority value, only candidates in that tier compete. Lower-preference priority must never cross boundary.
- **Cost**: PriceKnown=false must NOT be treated as zero-cost, unknown → neutral.
- **Context**: Unknown ContextWindow → neutral, not zero. Headroom = (ContextWindow - Required)/ContextWindow clamped [0,1].
- **Weight normalization**: effective weights deterministic, weighted_sum / total_positive_weight, reject all-zero.
- **Tie breaking**: ties preserve existing router order, no random, no map iteration, stable sort.
- **Min delta**: optional min_score_delta [0,1], if best does not beat existing first by at least delta → ABSTAIN to avoid churn.

## Exact fields that can be trusted

- `router.Scored.Deployment.ID`, `ProviderID`, `Model`, `Priority`, `Weight`, `ContextWindow`, `InputCostPerMTok`, `OutputCostPerMTok`, `Capabilities`
- `router.Scored.Health.Status`, `EWMALatencyMS`, `EWMATTFTMS`, `EWMAFailureRate`, `Successes`, `Failures`
- `router.Scored.Score` (router baseline)
- `router.Scored.CapacityPressure`
- `router.Scored.EstimatedCostUSD`, `PriceKnown`
- `route.ResolvedRoute.PrimaryPoolID`, `OrderedPoolIDs`, `AllowedDeployments`, `FallbackAllowed`, `AllAllowed`, `PrimaryMode`
- `router.Requirement.EstimatedInputTokens`, `MaxOutputTokens`, `MinContextWindow`
- `taskprofile.TaskProfile.Type`, `Complexity`, `Confidence`, `ReasonCodes`, `EstimatedContextTokens`, `Requires*`
- `feature.RequestFeatures` (protocol, streaming, vision count, tool count, etc.)
- Pinned deployment ID via safe router method (to be added)

## Fields that MUST NOT be invented

- quality_score, intelligence_score, coding_quality, reasoning_quality unless operator-configured with provenance CONFIG
- embeddings, LLM judge scores
- fabricated cost when PriceKnown=false
- fabricated latency 0ms when unmeasured
- fabricated reliability perfect when no history
- credentials, API keys, headers, raw prompts, tool schemas, session key raw, user identity, chain-of-thought
- deployment IDs not in E
- pool IDs not in OrderedPoolIDs

## Expected files to change for Phase E

New:
- internal/decision/policy/provider.go — PolicyProvider implementing DecisionProvider, ID=policy, CanSelect=true CanRank=false, healthy
- internal/decision/policy/scorer.go — score components finite [0,1], unknown neutral
- internal/decision/policy/policy.go — DecisionPolicy config, weights, task overrides, validation
- internal/decision/policy/explain.go — PolicyScoreBreakdown
- internal/decision/policy/normalization.go — weight normalization, task weight resolution
- internal/config: DecisionPolicies, DecisionConfig.Policy, RouteProfile.DecisionPolicy
- internal/router: add method PinnedDeploymentID or similar safe read-only, and PoolOrdinal handling in decision_wiring
- internal/httpapi/decision_wiring.go: enrich candidate snapshot with PoolID, PoolOrdinal, RouterScore, Health, CapacityPressure, Cost, ContextWindow, Capabilities, OriginalRank, PinnedCandidateID
- tests for config, scoring, selection band, pool/priority/affinity guardrails, task-aware, privacy, property, integration, benchmarks

Modified:
- internal/decision/request.go: extend Candidate snapshot
- internal/decision/orchestrator.go: already handles empty/single/budget/health/capabilities, but need to ensure policy provider uses same
- internal/httpapi/server.go: ensure policy provider registered, config reload updates policies
- internal/config/config.go: DecisionPolicies, validation
- docs

Unchanged:
- internal/router eligibility logic (health, capabilities, context, quota hard rejection)
- No second router, no external AI provider
