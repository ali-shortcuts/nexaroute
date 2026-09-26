# Phase H Current-State Note — Empirical Evaluation + Model Scorecards

Date: 2026-09-26
Branch: arena/01a0dbf9-nexaroute
Baseline: 090e95e1eabfba1ecf7e4b0ffdcdff4a37d41651 (arena/01a0d825-nexaroute, "Merge Phase G: Decision Provider Chains")
Parent Phase: G PASS (decision provider chains, hybrid execution, chain trace)

## Verified Phase G seam (unchanged)

```
candidatesForRequirement(req, protocol):
  - RLock snapshot cfg, resolver, router
  - rt.Candidates(req) → E (eligibility owner; protocol, capability, context window, disabled,
    health circuit, provider cooldown, credentials, client policy, security policy)
  ↓
task_classified event
  ↓
[Decision Plane seam] — cache check first; on MISS decisionCandidates() → ChainExecutor.Execute
  ↓
maxAttempts loop → execution → health/usage accounting
```

Phase H touches none of this. No data-plane package imports `internal/eval` or
`internal/scorecards`, and no routing decision reads a scorecard. The structural
guard test `TestIsolation_ProductionFilesDoNotImportRoutingState`
(`internal/eval/isolation_test.go`) parses production files and fails if the
dependency direction is ever inverted:

- `internal/router`, `internal/route`, `internal/decision`, `internal/health`,
  `internal/probe`, `internal/providers`, `internal/config` may not import
  `internal/eval` or `internal/scorecards`;
- `internal/eval` and `internal/scorecards` may not import the routing/health/
  provider packages.

## What Phase H adds

Phase H is an **admin-plane observation surface**, not a routing feature. It
answers one question honestly: *what evidence do we actually have about this
deployment's behavior?* — and refuses to answer anything else.

1. **Model Intelligence scorecards with mandatory provenance.**
   `internal/scorecards` models a scorecard as a set of dimension values where
   every value must carry a provenance (`operator_config`, `imported`,
   `evaluation`, `production_telemetry`). A value without provenance is rejected
   with `ErrNoProvenance`; there is no "unknown ⇒ 0" path anywhere. Quality
   dimensions without samples are omitted, not fabricated.

2. **Deterministic evaluation engine + versioned suites.**
   `internal/eval` ships eight built-in suites (coding, debugging, reasoning,
   tool_calling, structured_output, instruction_following, long_context,
   protocol_compat) whose cases are declarative expectations
   (`exact_match`, `json_valid`, `json_schema`, `expected_tool_call`,
   `regex_match`, `unit_tests`, `numeric_tolerance`, `stream_terminated`).
   Deterministic evaluators always outrank a judge; the judge evaluator exists
   but is disabled and Phase H ships no judge implementation.

3. **Two execution modes: offline replay (default) and explicitly-targeted
   live evaluation.**
   `POST /admin/api/evaluation/run` replays *recorded artifacts* through a
   `ReplayExecutor` whose `UpstreamCalls()` is always zero — the safe default,
   no network. With `evaluation.live_enabled` and `mode: "live"`, a
   `LiveEvaluationExecutor` sends the declared case inputs to the one
   explicitly selected deployment through its existing provider adapter (no
   second client stack) and judges the real responses. Live evaluation performs
   no router selection, never consults a DecisionProvider (local/policy/Jev/
   hybrid chain), and leaves production model/provider health, session
   affinity, the response cache and routing state untouched — proven by the
   strict `TestPhaseH_LiveEvaluation*` suite, including success, HTTP 500,
   timeout, cache, affinity, canary-privacy and credential-privacy cases.

4. **Admin surface, metrics and events.**
   Five admin endpoints (scorecard list/detail, suite catalog, run history,
   run submission), a dedicated evaluation-health namespace, bounded Prometheus
   families, and privacy-safe `eval_run` events that carry counts/verdicts/scores
   but never model outputs.

5. **Optional bounded durability.**
   One atomic 0600 state file (`evaluation.state_path`) mirrors runs and
   scorecards; a malformed or oversized state file is rejected as a whole.

## Explicit non-goals (Phase H)

- No routing influence: scorecards do not reorder candidates, change weights,
  affect health/circuits, or gate failover — whether produced by replay or by
  live evaluation.
- No judge model calls and no LLM-as-judge scoring in the request path or the
  admin path.
- Live evaluation stays narrow on purpose: one explicitly selected deployment,
  bounded non-streaming completions, no failover/hedging/retries to other
  deployments, no route-profile/pool semantics, default off.
- No invented values: insufficient evidence produces no value and no scorecard.

## Acceptance criteria (from `docs/PHASE_A_CURRENT_STATE_REPORT.md`, Phase H)

| Requirement | Where it is enforced | Test |
|---|---|---|
| Every quality value carries provenance | `scorecards.ValidateValue`, `Value.Provenance.Valid()` | `TestValidateValue_ProvenanceIsMandatory`, `TestNoFabrication_MissingDimensionStaysMissing`, `TestPhaseH_RunWritesProvenanceScorecard` |
| No fabrication (no evidence ⇒ no score) | `Value.Validate`, `FromEvaluation` skip rule, `Result.Scorecard()` `ok=false` | `TestFromEvaluation_NoEvidenceNoScorecard`, `TestRunner_InsufficientSamplesProducesNoScorecard`, `TestPhaseH_ThinEvidenceWritesNoScorecard` |
| Deterministic evaluator > judge precedence | `eval.Resolve` | `TestResolve_DeterministicAlwaysWins`, `TestRunner_JudgeCannotOverrideDeterministicVerdict`, `FuzzResolve_Verdicts` |
| Evaluation isolation (no production health pollution) | package dependency direction, `ReplayExecutor` (replay), `LiveEvaluationExecutor` bypass (live), admin-only plane | `TestIsolation_*`, `TestPhaseH_EvaluationDoesNotTouchRoutingOrUpstreams`, `TestPhaseH_LiveEvaluationLeavesProductionHealthUnchanged*` |
| Live physical-deployment evaluation with explicit deployment_id, no router selection | `LiveEvaluationExecutor` + `POST mode=live` | `TestPhaseH_LiveEvaluationCallsSelectedDeploymentExactlyOnce`, `TestPhaseH_LiveEvaluationScorecardAndRoutingUnchanged` |
| Live isolation: decision providers, health, cache, affinity, routing | executor never enters the request path | `TestPhaseH_LiveEvaluationBypassesDecisionProviders`, `TestPhaseH_LiveEvaluationDoesNotPopulateProductionCache`, `TestPhaseH_LiveEvaluationDoesNotTouchSessionAffinity` |
| Live privacy: dataset canary + provider credential | records carry verdicts/categories only; adapter-side redaction | `TestPhaseH_LiveEvaluationCanaryPrivacy`, `TestPhaseH_LiveEvaluationProviderKeyNeverLeaks` |
| §16 scorecards + provenance | `internal/scorecards` | package tests + HTTP admin tests |
| §17/§18 evaluation engine + suites, deterministic-first | `internal/eval` | package tests + suite catalog tests |
