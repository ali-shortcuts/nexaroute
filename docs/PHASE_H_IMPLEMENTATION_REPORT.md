# Phase H Implementation Report — Empirical Evaluation + Model Scorecards

Date: 2026-09-26
Branch: `arena/01a0dbf9-nexaroute`
Baseline: `090e95e1eabfba1ecf7e4b0ffdcdff4a37d41651` ("Merge Phase G: Decision Provider Chains")
Scope: Phase H only (Empirical Evaluation + Model Scorecards). Phases A–G untouched.
Design reference: `docs/PHASE_H_EVALUATION_AND_SCORECARDS.md`

## 1. Deliverables

### New packages

| File | Lines | Purpose |
|---|---|---|
| `internal/scorecards/scorecards.go` | 950 | Provenance-mandatory scorecard model, 15 dimensions, confidence calibration, strict artifact import, bounded versioned registry |
| `internal/eval/suite.go` | 253 | 8 expectation kinds, 8 versioned built-in suites, catalog + validation, bounds |
| `internal/eval/evaluator.go` | 494 | Deterministic evaluators, disabled judge, resolver with deterministic-first precedence, bounded evaluator registry |
| `internal/eval/runner.go` | 586 | Offline replay executor, bounded runner, decisive-only weighted scoring, scorecard conversion, evaluation-health namespace |
| `internal/eval/store.go` | 154 | Bounded in-memory run store (≤ 512) with retention/aggregates |

### New admin plane (httpapi)

| File | Lines | Purpose |
|---|---|---|
| `internal/httpapi/evaluation_plane.go` | 383 | Plane state, config application, import lifecycle, Retain, Stats, bounded admin views |
| `internal/httpapi/admin_eval.go` | 419 | 4 GET + 1 POST endpoints, payload validation, event emission (no model outputs) |
| `internal/httpapi/evaluation_state.go` | 184 | Optional durable state (version 1), strict whole-document load, atomic 0600 write, oldest-run trimming |

### Modified

- `internal/config/config.go` — `EvaluationConfig` (opt-in, bounded) + defaults/clamps/validation.
- `internal/httpapi/server.go` — plane field/init, snapshot accessor, config-swap integration (`newEvaluationPlane` + `Retain(valid)`), 5 routes.
- `internal/httpapi/admin.go` — `scorecards` + `evaluation` sections in the admin snapshot.
- `internal/httpapi/metrics.go` — bounded Phase H metric families.
- `internal/httpapi/helpers.go` — body/content-type limits reused by the evaluation endpoints.
- `scripts/verify.sh` — adds the four Phase H fuzz targets to the short fuzz gate.
- `scripts/stress.sh` — adds the bounded evaluation-plane stress member.
- `configs/config.example.json`, `docs/CONFIGURATION.md` — documented `evaluation` section.
- `docs/KNOWN_GAPS.md`, `ROADMAP.md` — Phase H boundaries and follow-ups recorded.

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
| `internal/httpapi/evaluation_plane_test.go` | 11 end-to-end admin tests | — | — |
| `internal/config/evaluation_config_test.go` | 4 (+10 rejection subtests) | — | — |

Test fixtures: `internal/scorecards/testdata/fuzz/FuzzImportJSON/714f8e84ca6cf0a5`
is the preserved regression corpus entry from the empty-scorecard bug found by
fuzzing (see §4).

## 2. Requirement coverage

| Phase H requirement | Status | Evidence |
|---|---|---|
| Scorecards with provenance for every value | PASS | `ErrNoProvenance` on empty provenance; `TestValidateValue_ProvenanceIsMandatory`, `TestValidateValue_ProvenanceRules`, `TestPhaseH_RunWritesProvenanceScorecard` asserts every admin row value carries `provenance`, `source`, `sample_count`, `evaluated_at` |
| No fabrication | PASS | `TestNoFabrication_MissingDimensionStaysMissing`, `TestFromEvaluation_NoEvidenceNoScorecard`, `TestRunner_InsufficientSamplesProducesNoScorecard`, `TestPhaseH_ThinEvidenceWritesNoScorecard` (200 OK + `scorecard_written:false`, registry stays empty), `TestPhaseH_RunRejectsFabricationAndMutation`, `FuzzImportJSON` (empty scorecards rejected) |
| Deterministic evaluator > judge precedence | PASS | `TestResolve_DeterministicAlwaysWins`, `TestRunner_JudgeCannotOverrideDeterministicVerdict`, `TestRunner_JudgeUsedOnlyWhenNoDeterministicVerdict`, `FuzzResolve_Verdicts` (random verdict mixes never let a judge override `pass`/`fail`) |
| Imports are all-or-nothing | PASS | `ImportJSON` validates the whole artifact before anything is written; the plane adds a registry-capacity pre-check so an over-bound artifact is rejected before mutation (`TestPhaseH_ImportArtifactIsAllOrNothing`, `TestPhaseH_ImportRespectsRegistryBoundAtomically`) |
| Evaluation isolation (no production health pollution) | PASS | `TestIsolation_ProductionFilesDoNotImportRoutingState` (dependency direction), `TestIsolation_ReplayExecutorMakesNoNetworkCalls`, `TestPhaseH_EvaluationDoesNotTouchRoutingOrUpstreams` (zero upstream calls, identical health snapshot, identical candidate order), `TestPhaseH_EventsAndRunsCarryNoModelOutputs` |
| §16 scorecards + provenance | PASS | `internal/scorecards` package tests + admin surface tests |
| §17/§18 evaluation engine + suites, deterministic-first | PASS | `internal/eval` package tests (suites, evaluators, runner, store, health) |
| Scorecards must not change routing | PASS | structural guard + `reflect.DeepEqual` health/candidate assertions + no production import of the eval packages (verified by grep and by the guard test) |

## 3. Gates actually executed

All commands below were run in this session on this branch, in this sandbox
(Debian 12 x86_64, 2 vCPU, Go 1.23.9). Nothing in this section is projected.

| Gate | Command | Result |
|---|---|---|
| Formatting | `gofmt -l .` | no output (clean) |
| Vet | `go vet ./...` | clean |
| Unit/integration (count=1) | `go test -count=1 -timeout=8m ./...` | 25 packages ok |
| Unit/integration (CI shape) | `./scripts/verify.sh` (`-count=10 -shuffle=on`, race `-count=3`, 6 fuzz targets, 2 cross-builds) | **VERIFY PASS** — 3m21s, and again 2m11s after the final production-code change |
| Race | `go test -race -count=1 -timeout=10m ./...` | 25 packages ok in 22.9s; re-exercised by `verify.sh -race -count=3` after the final change |
| Fuzz | `go test -run='^$' -fuzz=<target> -fuzztime=3s ./internal/{eval,scorecards}/` | `FuzzResolve_Verdicts` 38,073 execs PASS; `FuzzRunner_Artifacts` 61,904 execs PASS; `FuzzImportJSON` 117,174 execs PASS; `FuzzValueValidation` 54,174 execs PASS |
| Benchmarks | `go test -run='^$' -bench=. -benchmem -benchtime=200x ./internal/eval/` | `Resolve 82.9 ns/op` (0 allocs); `Replay 27.5 ns/op`; `Run/Coding 16.4 µs/op` (35 allocs); `Run/AllSuites 163 µs/op`; `HealthFromRuns 28.0 µs/op` |
| Stress | `./scripts/stress.sh` (incl. new evaluation-plane member) | **STRESS PASS** in 3.6s |
| Gated stress | `NEXAROUTE_STRESS=1 go test -count=1 -run='^TestStress' ./internal/eval/` | PASS in 0.14s (32 workers × 250 bounded runs, 60s budget) |
| Smoke | `./scripts/smoke-local.sh` (prebuilt linux-amd64 binary) | **SMOKE PASS** |

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

## 5. Risk #9 (evaluation isolation) — how it is answered

`docs/PHASE_A_CURRENT_STATE_REPORT.md` risk #9 asks that evaluation traffic be
tagged so it cannot create data-plane quota reservations or health signals.
Phase H answers it with a stronger property: **evaluation performs no data-plane
traffic at all.** Artifacts are replayed offline through `ReplayExecutor`
(`UpstreamCalls() == 0`); there is no in-band evaluation call, no quota
reservation, no health signal, and no provider cooldown interaction. If a later
phase introduces in-band evaluation, that phase must reintroduce the tagged-
context mechanism for it.

## 6. Boundaries (explicit)

- Phase H never performs live model calls; `POST /admin/api/evaluation/run`
  requires recorded artifacts and replays them offline (`upstream_calls == 0`).
- No judge implementation is shipped. Deterministic evaluators decide; the
  judge path exists and is proven never to override them.
- Scorecards do not influence routing in any way.
- Operator-defined suites and automatic telemetry ingestion are follow-ups
  (recorded in `ROADMAP.md` / `docs/KNOWN_GAPS.md`), not Phase H.
- Phase I was not started, per instruction.
- The dashboard tab for scorecards/evaluation is Phase J work per
  `docs/PHASE_A_CURRENT_STATE_REPORT.md`; Phase H exposes the admin API,
  snapshot sections and metrics that Phase J will render.
