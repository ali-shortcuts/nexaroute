# Phase E — Policy Engine — Selection Semantics

Date: 2026-09-25
Branch: arena/01a0d825-nexaroute (final convergence f933313 + fixes)
Status: PASS — real gates executed

This document is the authoritative description of the Phase E policy engine selection band, guardrails, scoring, task overrides, min_delta, tie, SELECT-only contract, PolicyTrace, events, privacy, fail-open, hot reload, and limitations.

---

## 1. Selection Band

The policy engine may rank **only within the eligible candidate set E** produced by `candidatesForRequirement`. E already enforces all hard constraints:

- protocol compatibility
- capability (vision, tools, reasoning)
- context-window incompatibility
- disabled deployment
- open health circuit
- provider cooldown
- invalid credentials
- client policy
- security policy
- virtual endpoint disabled / unknown model (unless fallback allowed)

Policy never reintroduces a candidate outside E, never widens E, never overrides hard filters. Validator rejects unknown IDs.

Within E, policy applies **selection band**:

1. **Pool boundary** — earliest `PoolOrdinal` only.
   - `minOrdinal = min(Candidate.PoolOrdinal)` across E.
   - `bandPool = { c in E | c.PoolOrdinal == minOrdinal }`
   - If `|bandPool| < |E|`, reason `POOL_BOUNDARY_ENFORCED`.
   - This enforces fallback hard boundary: fallback pools never leapfrog primary when primary has eligible candidates.

2. **Affinity** — authoritative before priority.
   - If `PinnedCandidateID` (derived from session affinity via `router.PinnedDeploymentID`) exists **and** is inside `bandPool`, it is selected immediately.
   - Reason `AFFINITY_PRESERVED`.
   - Affinity in later pool (`PoolOrdinal > minOrdinal`) must not leapfrog — it is ignored because it is outside `bandPool`.
   - Expired or non-band affinity (ID not in band) → normal scoring, not forced.

3. **Priority guardrail** — minimum `Priority` tier only.
   - After affinity check, within `bandPool`, find `minPriority = min(c.Priority)`.
   - `band = { c in bandPool | c.Priority == minPriority }`
   - If `|band| < |bandPool|`, reason `PRIORITY_GUARDRAIL_ENFORCED`.
   - Lower-preference (higher numeric priority) never crosses higher-preference tier.

4. **Single candidate fast path**
   - If `|band| == 1`, SELECT it directly with reasons `POLICY_SCORED`, `POLICY_SELECT_FIRST`.

If band empty (defensive), fallback to `bandPool`, then to E.

---

## 2. Pool / Priority / Affinity Semantics

- **Pool boundary** is hard: policy cannot select from fallback when primary has eligible candidates. This preserves operator-intended fallback chain B/A/C order.
- **Priority** is hard: within earliest pool, only highest-preference (lowest numeric) tier is considered. This matches router's priority semantics and prevents lower-priority models from overtaking.
- **Affinity** is authoritative over priority but not over pool boundary:
  - Pinned ID in primary pool with lower priority (e.g., A priority 0, B priority 10, pin B) → B remains primary despite lower priority. Session stickiness wins.
  - Pinned ID in later pool while earlier pool has candidates → ignored, earliest pool wins. This prevents session pin from breaking fallback isolation.

All three are emitted as reason codes for observability.

---

## 3. Context Formula — Request-Relative Headroom

Context scoring uses **request-relative headroom**, not absolute window size alone.

```
required = MinContextWindow (from router.Requirement) else EstimatedInputTokens + MaxOutputTokens else 0
if required <=0 or window <=0 => 0.5 neutral (unknown)
else if window < required => 0.0 defensive (too small, but router should have already filtered; scoring defensively 0)
else headroom = (window - required) / window   ∈ [0,1]
```

Examples:

- 16k vs 128k with 12k requirement:
  - 16k: (16000-12000)/16000 = 0.25
  - 128k: (128000-12000)/128000 = 0.90625 → 128k wins
- Same windows with tiny request 100 tokens:
  - 16k: (16000-100)/16000 = 0.99375
  - 128k: (128000-100)/128000 = 0.9992 → both close to 1, slight edge to larger, but difference minimal (desired)
- Required 0 → neutral 0.5 for all, context does not influence decision
- Unknown window (0 or negative) → neutral 0.5
- Invalid negative → neutral 0.5

This fixes semantic gap where absolute window size alone would always prefer 128k even for tiny requests, causing unnecessary cost.

---

## 4. Reliability — Known vs Unknown

Reliability uses **Successes/Failures** from health manager to distinguish known vs unknown.

```
if Successes+Failures == 0 => ReliabilityKnown false => 0.5 neutral
else
  healthScore: healthy 1.0, unknown 0.5, half_open 0.3, degraded 0.2, cooldown/unavailable 0.0, default 0.5
  failureRate clamped [0,1], NaN/Inf => 0.5
  frScore = 1 - failureRate
  reliability = (healthScore + frScore)/2 clamped [0,1]
```

- No observations → neutral 0.5, not penalized as unhealthy, not rewarded as healthy.
- Measured success (10 successes, 0 failures, healthy) → 1.0
- Mixed (5/5, 0.5 failure rate, healthy) → 0.75
- Degraded with failures → low score
- NaN/Inf failure rate → neutral 0.5 for that component, not crash

This fixes gap where unknown was previously treated as degraded.

---

## 5. Scoring

### Components (all finite [0,1], unknown → 0.5 neutral, never best)

- **router_baseline**: normalized across band `(score-min)/(max-min)`, all equal or single → 0.5, NaN/Inf → 0.5
- **reliability**: as above, healthScore + (1-failureRate) /2
- **latency**: lower better, `1-(lat-min)/delta`, unmeasured (<=0 or NaN/Inf or Successes==0 and latency==0) → 0.5, all equal → 0.5
- **ttft**: same as latency but using TTFT
- **capacity**: `1 - pressure/4` clamped [0,1], pressure 0→1, 2→0.5, 4→0, NaN/Inf →0.5
- **cost**: lower better, PriceKnown false →0.5 (not free), invalid cost NaN/Inf/negative →0.5, all equal →0.5
- **context**: request-relative headroom as above

All components clamped [0,1], NaN/Inf replaced by 0.5, no panic.

### Weighted Sum

```
total = sum(weights)
if total <=0 or NaN/Inf => 0.5 neutral
else weighted = sum(component * weight) / total
```

Weights are `Weights` struct with 7 fields, each finite >=0 <=1_000_000. At least one positive required.

### Full Scoring Pipeline

```
ScoreCandidates(cands, required) -> []ScoredCandidate{Components, OriginalRank}
ApplyWeights(scored, weights) -> WeightedScore
Sort stable: weighted desc, then OriginalRank asc for determinism
```

Deterministic: no random, no map iteration order dependency (sort stable).

---

## 6. Task Overrides

- Key = canonical TaskType lowercased, validated against `taskprofile.AllTaskTypes()` (15 types)
- Non-canonical → error
- Case-insensitive: "CODING" → "coding"
- All-zero override rejected: must have at least one positive weight
- `ResolveWeights(taskType)` returns override if exists, else base, plus bool `taskAware`
- When taskAware, reason `TASK_AWARE_WEIGHTS`

Canonical vocab (must stay in sync with taskprofile):

```
simple_chat, coding, code_edit, debugging, repository_analysis,
architecture_reasoning, deep_reasoning, tool_use, agentic_task,
long_context, vision, structured_output, data_extraction, general, unknown
```

---

## 7. Min Score Delta vs Original Primary

Spec requires **min_score_delta vs original primary**, not vs second-best.

Definitions:

- `originalPrimaryID = candidate with min OriginalRank in band`
- `originalPrimaryScore = weighted score of original primary`
- `topCandidate = highest weighted score in band`

Logic:

```
if topCandidate.ID == originalPrimaryID => ABSTAIN EXISTING_ORDER_PRESERVED (avoid churn)
else if MinScoreDelta >0 and (topCandidate.Score - originalPrimaryScore) < MinScoreDelta => ABSTAIN MIN_DELTA_NOT_MET
else SELECT topCandidate
```

Examples from spec:

- A=0.40, B=0.60, C=0.59, delta 0.10, original primary A:
  - B beats A by 0.20 >=0.10 → SELECT B
- A=0.59, B=0.60, C=0.10, delta 0.05, original primary A:
  - B beats A by 0.01 <0.05 → ABSTAIN

This prevents policy from reordering for marginal gains.

---

## 8. Tie Handling

Tie: `abs(score_i - score_j) <= 1e-9` → preserve original order via `OriginalRank asc`.

If tie and best is original primary → ABSTAIN (existing order preserved).

No random tie-breaking.

---

## 9. SELECT-only Contract

Policy provider:

- CanSelect true, CanRank false
- Never returns RANK action
- Returns only SELECT or ABSTAIN
- SELECT: `SelectedID` must be in eligible set, confidence finite [0,1], reason codes bounded
- ABSTAIN: `SelectedID` empty, `Abstained` true, confidence 0, reasons include `EXISTING_ORDER_PRESERVED` or `MIN_DELTA_NOT_MET` or `EMPTY_ELIGIBLE`
- Validator rejects unknown candidates, duplicates, invalid action

---

## 10. PolicyTrace

`DecisionResult.PolicyTrace` and `DecisionTrace.PolicyTrace` are privacy-safe, bounded:

```go
type PolicyTrace struct {
  PolicyID             string
  TaskType             string
  OriginalPrimaryID    string
  SelectedID           string
  ChangedPrimary       bool
  SelectedScore        float64
  OriginalPrimaryScore float64
  SelectedBreakdown    map[string]float64 // component scores for selected
  OriginalBreakdown    map[string]float64 // component scores for original primary
  Weights              map[string]float64 // effective weights used
}
```

- Only IDs, scores, component breakdowns, weights
- No raw prompts, no secrets, no tool results, no chain-of-thought
- Bounded: IDs 512 chars, PolicyID 128, TaskType 32, maps 7 entries each finite [0,1]
- Wired to `DecisionTrace` for route explanation and to `events.Event` for telemetry

---

## 11. Events

`events.Event` extended with Phase E fields (bounded):

- `DecisionPolicyID` (128)
- `DecisionTaskType` (32)
- `DecisionOriginalPrimary` (512)
- `DecisionSelectedScore` float64
- `DecisionOriginalScore` float64
- `DecisionChangedPrimary` bool
- `DecisionBreakdown` string (4096) — JSON of selected/original breakdowns + weights, always valid JSON (structurally bounded, never byte-sliced)

`emitDecisionEvent` populates from `result.PolicyTrace` else `trace.PolicyTrace`, includes `DecisionBreakdown` JSON via `jsonMarshalBounded` (valid JSON, fallback `{}` if too large).

`MarshalBreakdown` in policy package:

- Bounds to first 10 candidates
- Iteratively reduces count until JSON <=4096
- Never byte-slices JSON (which would produce invalid JSON)
- Always returns valid JSON array `[]` even on error

---

## 12. Privacy

- `DecisionRequest` contains only `TaskProfile`, `RequestFeatures`, candidate snapshot, VE/route/pool IDs, `PinnedCandidateID`, `PolicyID`, token estimates, `RequestID`, budget — no raw prompts, no tool results, no headers, no secrets, no raw session key
- `PinnedCandidateID` derived via `router.PinnedDeploymentID` which uses RLock snapshot, no raw session key exposure
- `PolicyTrace` and breakdowns contain only IDs, scores, weights — no prompts
- Canary test: `SECRET_POLICY_CANARY_4e91` must never appear in decision artifacts (request JSON, result, trace, event, breakdown, metrics, logs)
- Full-path test: request containing canary in prompt → verify canary absent from trace header, events, breakdowns
- Existing `SECRET_DECISION_CANARY_82c1` still enforced for Phase D artifacts

---

## 13. Fail-Open

- Invalid telemetry (NaN, Inf, negative) → neutral 0.5, not crash, not best
- No policy config when provider=policy → ABSTAIN (but config validation requires policy, so this is defensive)
- Empty candidates → ABSTAIN EMPTY_ELIGIBLE
- Single candidate → SELECT directly
- Context window too small already filtered by router; scorer defensively 0.0 but router is authoritative
- Provider panic → orchestrator catches, ABSTAIN PROVIDER_PANIC, fail-open to router order
- Timeout → ABSTAIN TIMEOUT
- Invalid result from provider (unknown ID) → rejected, ABSTAIN VALIDATION_FAILED, router order preserved
- Scoring never overrides hard constraints

---

## 14. Hot Reload

- `Provider.UpdatePolicies(policies, defaultPolicyID)` atomic via RWMutex
- `server.go` hot-reload: on config reload, `Get("policy")` → `UpdatePolicies` with deep-copied policies
- `cloneConfig` deep-copies `DecisionPolicies` and `TaskOverrides` maps to avoid race
- Race test: 50 concurrent requests + 50 config reloads with varying latency weights → no panic, no data race, requests succeed or 429/503 but not crash (verified via `go test -race`)

---

## 15. Limitations (Intentional Phase E)

- Only `select_first` selection mode; no ranking, no weighted random, no multi-winner
- Only local and policy providers; external adapters (Jev, etc.) deferred to Phase F, isolated
- No provider chains/abstention/cooldown beyond existing orchestrator budget and health checks — Phase G
- No scorecards/evaluation engine with provenance — Phase H
- No shadow/canary — Phase I
- No dashboard/dry-run beyond existing decision events/metrics — Phase J
- No supervision contracts — Phase K
- No learned routing — Phase L only if measurable
- Cost: `PriceKnown false` treated as neutral 0.5, not as hard unknown
- Context guardrail reason code defined but not enforced as hard filter beyond router eligibility
- Explain breakdown not yet exposed via admin API, only via events bus and trace header — Phase J will expose
- No persistent policy evaluation, only in-memory scoring
- No per-request policy selection beyond RouteProfile override and global default

---

## 16. Verification

Real gates executed (not mocked):

- `go version` go1.23.0
- `gofmt -l` empty
- `go test -timeout=3m -shuffle=on -count=10 ./...` PASS (17 packages, 10 shuffles each)
- `go vet ./...` PASS
- `go test -race -timeout=3m -shuffle=on -count=3 ./...` PASS
- `node --check internal/httpapi/web/app.js` PASS
- `go test -fuzz FuzzPatchJSONModel -fuzztime=2s` PASS
- `go test -fuzz FuzzParseAnthContent -fuzztime=2s` PASS
- `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build` PASS 7.7M
- `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build` PASS
- `./scripts/stress.sh` PASS (router scale, probe/recovery, event-state, admission, log rotation)
- `./scripts/smoke-local.sh` PASS (UI, hello, models, admin snapshot, count_tokens fallback, provider CRUD, atomic persistence)
- Benchmarks: Score 2/10/100, ApplyWeights, ProviderDecide, ResolveWeights, ContextScoring (see report)

All Phase E semantic gaps fixed:

- Context score request-relative headroom (MinContextWindow)
- Reliability unknown neutral via Successes/Failures
- Min delta vs original primary (not second-best)
- Affinity authoritative before priority
- Explainability wired to DecisionTrace/PolicyTrace + events
- MarshalBreakdown valid JSON (structural bounding)
- Provider=policy explicit config no silent no-op
- Zero-weight task override rejection
- Canonical task vocab sync

---

## 17. Final Verdict

Phase E: PASS — policy engine correct, privacy-safe, bounded, deterministic, fail-open, hot-reload safe, real gates green.
