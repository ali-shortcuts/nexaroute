# Phase H Implementation Report — Empirical Evaluation + Model Scorecards

Date: 2026-09-26
Branch: `arena/01a0dc29-nexaroute`
Baseline: `090e95e1eabfba1ecf7e4b0ffdcdff4a37d41651` ("Merge Phase G: Decision Provider Chains")
Scope: Phase H only (Empirical Evaluation + Model Scorecards). Phases A–G untouched. Phase I not started.
Design reference: `docs/PHASE_H_EVALUATION_AND_SCORECARDS.md`

This revision adds the **live physical-deployment evaluation executor** on top
of the already-reviewed Phase H core (deliverables §1, gates §3): evaluation
can now make real upstream model requests against one explicitly selected
deployment — while the safe default remains offline replay with
`UpstreamCalls() == 0`.

## 1. Deliverables

### New packages

| File | Lines | Purpose |
|---|---|---|
| `internal/scorecards/scorecards.go` | 950 | Provenance-mandatory scorecard model, 15 dimensions, confidence calibration, strict artifact import, bounded versioned registry |
| `internal/eval/suite.go` | 253 | 8 expectation kinds, 8 versioned built-in suites, catalog + validation, bounds |
| `internal/eval/evaluator.go` | 494 | Deterministic evaluators, disabled judge, resolver with deterministic-first precedence, bounded evaluator registry |
| `internal/eval/runner.go` | 670 | Offline replay executor, bounded runner, decisive-only weighted scoring, scorecard conversion, evaluation-health namespace, **execution-mode accounting (`replay` default vs explicit `live`)** |
| `internal/eval/store.go` | 154 | Bounded in-memory run store (≤ 512) with retention/aggregates |
| **Live addition** `internal/providers/completion.go` | 416 | Bounded non-streaming completion added **on the existing `httpAdapter`** — reuses `DoPath` (same endpoint, credentials/headers, TLS, transport, error redaction); no second client stack |

### New admin plane (httpapi)

| File | Lines | Purpose |
|---|---|---|
| `internal/httpapi/evaluation_plane.go` | 405 | Plane state, config application, import lifecycle, Retain, Stats, bounded admin views, live/upstream counters |
| `internal/httpapi/admin_eval.go` | 465 | 4 GET + 1 POST endpoints, payload validation, **`mode` dispatch (`replay` default / `live` opt-in)**, event emission (no model outputs, no prompts) |
| `internal/httpapi/evaluation_state.go` | 184 | Optional durable state (version 1), strict whole-document load, atomic 0600 write, oldest-run trimming |
| **Live addition** `internal/httpapi/live_eval.go` | 204 | `LiveEvaluationExecutor`: binding strict live-payload validation to the one selected deployment's existing provider adapter |

### Modified

- `internal/config/config.go` — `EvaluationConfig` (opt-in, bounded) + defaults/clamps/validation; **`live_enabled` (default `false`)**.
- `internal/httpapi/server.go` — plane field/init, snapshot accessor, config-swap integration (`newEvaluationPlane` + `Retain(valid)`), 5 routes. **No data-plane changes.**
- `internal/httpapi/admin.go` — `scorecards` + `evaluation` sections in the admin snapshot (incl. `live_enabled` + live counters).
- `internal/httpapi/metrics.go` — bounded Phase H metric families (`nexaroute_evaluation_enabled`, `nexaroute_evaluation_live_enabled`, `nexaroute_evaluation_runs_total`, `nexaroute_evaluation_live_runs_total`, `nexaroute_evaluation_upstream_calls_total`, `nexaroute_evaluation_scorecards_written_total`, `nexaroute_evaluation_stored_runs`, `nexaroute_evaluation_cases_total`, `nexaroute_scorecards_total`, `nexaroute_scorecard_values{provenance}`). No scores-per-deployment, no prompts, no outputs.
- `internal/httpapi/helpers.go` — body/content-type limits reused by the evaluation endpoints.
- `scripts/verify.sh` — adds the pre-existing Phase H fuzz targets **plus `internal/providers:FuzzCompletionDecode`** to the short fuzz gate (7 targets total).
- `scripts/stress.sh` — adds the bounded evaluation-plane stress member.
- `configs/config.example.json`, `docs/CONFIGURATION.md` — documented `evaluation` section incl. `live_enabled` and both modes.
- `docs/PHASE_H_EVALUATION_AND_SCORECARDS.md` — §2.4 rewritten for both execution modes + isolation table; §4 payload/metrics; §5 config; §7 non-goals.
- `docs/PHASE_H_CURRENT_STATE_NOTE.md` — live mode, strict-test evidence, updated non-goals/acceptance rows.
- `docs/KNOWN_GAPS.md`, `ROADMAP.md` — live evaluation boundaries and follow-ups recorded.

### Tests

| File | Top-level tests | Fuzz | Bench |
|---|---|---|---|
| `internal/scorecards/scorecards_test.go` | 21 | — | — |
| `internal/scorecards/fuzz_test.go` | — | 2 | — |
| `internal/eval/eval_test.go` | 28 (incl. always-on concurrency) | — | — |
| `internal/eval/isolation_test.go` | 2 (structural guard + replay isolation) | — | — |
| `internal/eval/fuzz_test.go` | — | 2 | — |
| `internal/eval/bench_test.go` | — | — | 5 |
| `internal/eval/stress_test.go` | 2 (1 gated by `NEXAROUTE_STRESS=1`) | — | — |
| `internal/eval/live_mode_test.go` | 8 (mode accounting: replay default, honest upstream counts, recorded mode can never claim contact it did not make) | — | — |
| `internal/httpapi/evaluation_plane_test.go` | 11 end-to-end admin tests | — | — |
| **Live addition** `internal/httpapi/evaluation_live_test.go` | **13 strict tests** (see §2a) | — | 2 |
| **Live addition** `internal/providers/completion_test.go` | 7 decode/validation tests | 1 (`FuzzCompletionDecode`) | — |
| `internal/config/evaluation_config_test.go` | 4 (+10 rejection subtests, incl. `live_enabled`) | — | — |

Test fixtures: `internal/scorecards/testdata/fuzz/FuzzImportJSON/714f8e84ca6cf0a5`
is the preserved regression corpus entry from the empty-scorecard bug found by
fuzzing (see §4).

## 2. Requirement coverage

| Phase H requirement | Status | Evidence |
|---|---|---|
| Scorecards with provenance for every value | PASS | `ErrNoProvenance` on empty provenance; `TestValidateValue_ProvenanceIsMandatory`, `TestValidateValue_ProvenanceRules`, `TestPhaseH_RunWritesProvenanceScorecard` asserts every admin row value carries `provenance`, `source`, `sample_count`, `evaluated_at` |
| No fabrication | PASS | `TestNoFabrication_MissingDimensionStaysMissing`, `TestFromEvaluation_NoEvidenceNoScorecard`, `TestRunner_InsufficientSamplesProducesNoScorecard`, `TestPhaseH_ThinEvidenceWritesNoScorecard` (200 OK + `scorecard_written:false`, registry stays empty), `TestPhaseH_RunRejectsFabricationAndMutation`, `FuzzImportJSON` (empty scorecards rejected) |
| Deterministic evaluator > judge precedence | PASS | `TestResolve_DeterministicAlwaysWins`, `TestRunner_JudgeCannotOverrideDeterministicVerdict`, `TestRunner_JudgeUsedOnlyWhenNoDeterministicVerdict`, `FuzzResolve_Verdicts` |
| Imports are all-or-nothing | PASS | `ImportJSON` validates the whole artifact before anything is written; the plane adds a registry-capacity pre-check so an over-bound artifact is rejected before mutation (`TestPhaseH_ImportArtifactIsAllOrNothing`, `TestPhaseH_ImportRespectsRegistryBoundAtomically`) |
| Evaluation isolation (no production health pollution) | PASS | `TestIsolation_ProductionFilesDoNotImportRoutingState` (dependency direction), `TestIsolation_ReplayExecutorMakesNoNetworkCalls`, `TestPhaseH_EvaluationDoesNotTouchRoutingOrUpstreams` (zero upstream calls, identical health snapshot, identical candidate order), `TestPhaseH_EventsAndRunsCarryNoModelOutputs`, plus the live rows in §2a |
| §16 scorecards + provenance | PASS | `internal/scorecards` package tests + admin surface tests |
| §17/§18 evaluation engine + suites, deterministic-first | PASS | `internal/eval` package tests (suites, evaluators, runner, store, health) |
| Scorecards must not change routing | PASS | structural guard + `reflect.DeepEqual` health/candidate assertions + no production import of the eval packages (verified by grep and by the guard test) |

## 2a. Live evaluation — requirement coverage (new in this revision)

| Requirement | Status | Evidence |
|---|---|---|
| `ReplayExecutor` kept as optional offline mode | PASS | `mode` omitted ⇒ replay; replay rejects `inputs`; `TestPhaseH_LiveEvaluationRequiresExplicitOptIn`; existing replay tests untouched |
| `LiveEvaluationExecutor` targets exactly one explicit physical deployment | PASS | `deployment_id` resolved directly from config + router view (no selection); `TestPhaseH_LiveEvaluationCallsSelectedDeploymentExactlyOnce`: selected upstream receives exactly `len(inputs)` posts, other deployments receive 0 |
| Reuses existing provider/adapter/transport stack (no second client stack) | PASS | executor calls only the selected deployment's own `providers.Adapter.DoPath`-based completion; `TestPhaseH_LiveExecutorUsesOnlyTheExistingAdapterStack` (executor struct holds no `net/http` field; nil adapter rejected) |
| Bypasses Local/Policy/Jev/hybrid DecisionProviders | PASS | `TestPhaseH_LiveEvaluationBypassesDecisionProviders`: counting providers registered for local/policy/Jev + armed hybrid chain — call counters remain **0**; decision metrics + provider state `reflect.DeepEqual` unchanged; no route/decision events |
| Makes real upstream model requests | PASS | fake physical upstreams (`httptest`) assert `Authorization: Bearer SECRET_EVAL_PROVIDER_KEY_3f42`, model, and the prompt arriving on the wire; outcomes scored from real responses |
| Does not affect production model health (success / HTTP 500 / timeout) | PASS | `TestPhaseH_LiveEvaluationLeavesProductionHealthUnchanged{OnSuccess,OnHTTP500,OnTimeout}` — health snapshot byte-identical before/after; 500s become honest `fail` verdicts; timeouts become `case_timeout` error verdicts |
| Does not affect session affinity | PASS | `TestPhaseH_LiveEvaluationDoesNotTouchSessionAffinity` — pre-pinned session stays pinned after live runs |
| Does not populate production cache | PASS | `TestPhaseH_LiveEvaluationDoesNotPopulateProductionCache` — cache `Stats` `reflect.DeepEqual` before/after |
| Does not alter routing state; routing identical before/after scorecard | PASS | `TestPhaseH_LiveEvaluationScorecardAndRoutingUnchanged` — scorecard written with provenance `evaluation`; routing candidates + model health byte-identical pre/post |
| Evaluation request explicitly selects `deployment_id`; router selection forbidden | PASS | the live path resolves the deployment by id from config/router view before building the executor (no scoring or candidate selection is invoked), and only that deployment's adapter is ever called — proven by the exactly-once test plus routing/decision DeepEqual tests above |
| `mode=live` + `mode=replay`; safe default replay; `evaluation.enabled=false` unchanged | PASS | `TestPhaseH_LiveEvaluationRequiresExplicitOptIn` (409 gated on `live_enabled=false`; disabled plane 409; unknown mode 400) + existing disabled-plane tests |
| Canary privacy | PASS | `TestPhaseH_LiveEvaluationCanaryPrivacy`: canary `SECRET_EVAL_DATASET_CANARY_7b91` reaches **only** the selected upstream; absent from the run response, `/metrics`, admin snapshot, events, health JSON, run history, scorecard detail, decision metrics, and the persisted state file |
| Provider-credential privacy | PASS | `TestPhaseH_LiveEvaluationProviderKeyNeverLeaks`: key never appears in evaluation records, scorecards, events, metrics, admin responses or error messages — even when the upstream 500 body echoes it (adapter `RedactBody` path) |
| Concurrency safety | PASS | `TestPhaseH_LiveEvaluationConcurrentRunsRaceFree` (8×6 concurrent live runs), exercised under `-race` |

## 3. Gates actually executed

All commands below were run in this session on this branch, in this sandbox
(Debian 12 x86_64, 2 vCPU, Go 1.23.9, cgo-enabled toolchain for `-race`).
Nothing in this section is projected.

| Gate | Command | Result |
|---|---|---|
| Formatting | `gofmt -l .` | no output (clean) |
| Vet | `go vet ./...` | clean |
| Unit/integration (count=1) | `go test -count=1 -timeout=8m ./...` | 25 packages ok |
| Full verify gate | `./scripts/verify.sh` (`-count=10 -shuffle=on`, race `-count=3`, **7 fuzz targets** incl. `FuzzCompletionDecode`, web-UI JS check, 2 cross-builds) | **VERIFY PASS** in 2m26s — unit/integration 25 ok ×10, race 25 ok ×3, all fuzz targets PASS, linux/amd64 + linux/arm64 build PASS |
| Race focus (required command) | `go test -race -count=1 -timeout=8m ./internal/eval/... ./internal/scorecards/... ./internal/httpapi ./internal/decision/... ./internal/router ./internal/route` | 10 packages ok (~12s) |
| Fuzz (3s per target, final head) | `FuzzResolve_Verdicts` 79,005 execs; `FuzzRunner_Artifacts` 50,555; `FuzzCompletionDecode` 37,867; `FuzzImportJSON` 115,392; `FuzzValueValidation` 60,888; `FuzzPatchJSONModel` 22,615; `FuzzParseAnthContent` 122,608 | all PASS, no crashes |
| Benchmarks | `go test -run='^$' -bench=. -benchmem ./internal/...` (whole repository, default benchtime) | all packages ok. Phase H: `Resolve_Verdicts 7.0 ns/op` (0 allocs), `ReplayExecutor 17.0 ns/op` (0 allocs), `Run_CodingSuite 5.7 µs/op` (35 allocs), `Run_AllSuites 87.5 µs/op` (553 allocs), `HealthFromRuns 11.1 µs/op` (32 allocs), providers `Complete 69.0 µs/op` (161 allocs), **`LiveEvaluationExecutor 83.2 µs/op` (180 allocs — one loopback upstream call through the production adapter)**, **`LiveEvaluationRun 268.0 µs/op` (562 allocs — full reasoning suite → Scoreable result)** |
| Stress | `./scripts/stress.sh` (incl. evaluation-plane member) | **STRESS PASS** |
| Smoke | `./scripts/smoke-local.sh` (prebuilt linux-amd64 binary) | **SMOKE PASS** (11 assertions) |

## 4. Findings and fixes during implementation

1. **Fuzzing found a real fabrication hole.** `FuzzImportJSON` produced a
   scorecard with an empty `values` map that passed validation. Fix: `Validate`
   now rejects a scorecard with no values ("no evidence, no scorecard"), and the
   regression input is kept in `internal/scorecards/testdata/fuzz/`.
2. **`Resolve` returned human-readable reasons as its category.** Metrics and
   admin labels need bounded values, so `Resolve` now returns the stable
   categories `deterministic` / `judge` / `none`; the human reason stays on
   `VerdictResult.Reason`.
3. **Hot reload could roll evidence back to a stale state file.** The plane now
   remembers `loadedStatePath` and re-reads the state file only when
   `evaluation.state_path` actually changes
   (`TestPhaseH_ReloadKeepsEvidenceForLiveDeploymentsOnly` overwrites the state
   file with an empty one, reloads, and proves memory is not rolled back).
4. **A flaky isolation assertion.** The first version of
   `TestPhaseH_EvaluationDoesNotTouchRoutingOrUpstreams` compared
   `[]health.State` with `reflect.DeepEqual`; the health snapshot has
   non-deterministic map-iteration order, so the test failed under
   `verify.sh -count=10` despite the product being correct. Fixed by comparing
   maps keyed by deployment ID — found only because the gate was actually run.
5. **Import atomicity under a full registry.** `ImportJSON` already rejected a
   malformed artifact wholesale, but a valid artifact could still be applied
   halfway if the live registry filled up mid-loop. `loadImport` now performs a
   capacity pre-check and refuses the artifact before writing anything
   (`TestPhaseH_ImportRespectsRegistryBoundAtomically`), so an import is atomic
   in every case.
6. **Removed an inert knob.** The initial `evaluation.judge_enabled` config field
   could not enable anything (Phase H ships no judge), so it was deleted rather
   than documented as a lie.
7. **(Live) Mode honesty.** A run record can never overstate its contact with
   the world: `Runner.RunWithExecutor` validates `mode ∈ {replay, live}`
   (default replay), records the exact `UpstreamCalls()` reported by the
   executor (replay always reports 0 — it cannot make calls), and the admin
   endpoint pairs mode and executor 1:1 (`replay`→`ReplayExecutor`,
   `live`→`LiveEvaluationExecutor`, only after the `live_enabled` opt-in
   gate). The convenience `Runner.Run` pins replay explicitly. Run records
   therefore carry both `mode` and the true `upstream_calls` count.
8. **(Live) The admin token bucket is a real control.** Benchmarking the live
   run through the HTTP admin surface hit the per-IP admin bucket (capacity 90,
   refill 1.5/s) — by design. Live benchmarks therefore measure the executor
   and runner directly (the true hot path), and the bucket remains the
   operator-facing frequency bound for launching upstream evaluation work.
9. **(Live) Upstream failures must be honest verdicts, not health signals.**
   An upstream HTTP 500 inside a live run maps to a `fail` verdict on that
   case; a transport/timeout error maps to an error verdict with category
   `case_timeout`. Neither writes to `health.Manager`, provider credential
   cooldown or the router — asserted by the unchanged-snapshot tests.

## 5. Risk #9 (evaluation isolation) — how it is answered

`docs/PHASE_A_CURRENT_STATE_REPORT.md` risk #9 asks that evaluation traffic be
tagged so it cannot create data-plane quota reservations or health signals.
The replay plane answered it by performing no data-plane traffic at all; live
evaluation now performs bounded data-plane traffic, so the tagged-context
concern is answered structurally and by test:

- **No quota reservation**: the adapter only reserves quota when the request
  context carries the data-plane `WithQuotaEstimate` tag. The live executor
  deliberately does not set it — evaluation legs reserve nothing and appear
  in no reservation counters.
- **No health signals**: health/failure/cooldown recording lives in the
  request-path handlers and the probe engine. The live executor never runs
  through either; upstream 500s and timeouts change no production health or
  provider-incident state (strict tests on success, 500 and timeout).
- **No cache / affinity / routing writes**: those are written by request-path
  handlers after a routed response. The executor never enters the request
  path; cache `Stats`, pinned sessions and candidate ordering are asserted
  unchanged.
- **No decision-plane entry**: Local/Policy/Jev/hybrid DecisionProviders are
  never consulted (counting-provider proof), and decision metrics/traces are
  unchanged.
- **No second stack**: completions travel through the selected deployment's
  own configured adapter (same endpoint/credentials/TLS/redaction), selected
  by explicit `deployment_id` only — never by router selection.

## 6. Boundaries (explicit)

- Phase H ships **two** execution modes on `POST /admin/api/evaluation/run`:
  `replay` (safe default; recorded artifacts; `upstream_calls == 0`; no
  network) and `live` (opt-in; requires both `evaluation.enabled` and
  `evaluation.live_enabled`; real bounded non-streaming calls to the one
  explicitly selected deployment; `upstream_calls == len(inputs)`,
  reported honestly).
- Live mode never performs router selection, never consults a
  DecisionProvider, never streams, never hedges, never fails over to another
  deployment, and never reserves quota.
- No judge implementation is shipped. Deterministic evaluators decide; the
  judge path exists and is proven never to override them.
- Scorecards do not influence routing in any way — this holds equally for
  scorecards produced by live runs.
- Operator-defined suites and automatic telemetry ingestion are follow-ups
  (recorded in `ROADMAP.md` / `docs/KNOWN_GAPS.md`), not Phase H.
- Phase I was not started, per instruction.
- The dashboard tab for scorecards/evaluation is Phase J work per
  `docs/PHASE_A_CURRENT_STATE_REPORT.md`; Phase H exposes the admin API,
  snapshot sections and metrics that Phase J will render.

## Final verdict

**PHASE H: PASS** — the live evaluation executor and its isolation guarantees
are implemented **and proven by strict tests** (§2a): exactly-one upstream
call per case to the explicitly selected deployment and nowhere else, zero
DecisionProvider calls, production model health unchanged after success,
upstream HTTP 500 and timeout, production cache and session affinity
untouched, routing and decision state byte-identical before/after scorecard
generation, and canary/credential privacy enforced end-to-end. The safe
default remains offline replay with `UpstreamCalls() == 0`, the plane is
off by default, and every existing gate (`verify.sh`, `stress.sh`,
`smoke-local.sh`, race, fuzz, benchmarks) is green on the final head.
