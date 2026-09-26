# Phase H Implementation Report — Empirical Evaluation + Model Scorecards

Date: 2026-09-26
Branch: `arena/01a0dbf9-nexaroute`
Phase H baseline: `7bc67662118d56120da841c1e552e8a0c5e60396` ("Phase H offline-replay convergence")
Scope: Phase H final convergence — **live physical-deployment evaluation**. Phases A–G untouched; Phase I not started.
Design reference: `docs/PHASE_H_EVALUATION_AND_SCORECARDS.md`
Current-state note: `docs/PHASE_H_CURRENT_STATE_NOTE.md`

---

## 0. Verdict

**PHASE H: PASS**

Offline replay and live physical-deployment evaluation both ship, are both
explicit and isolated, and Phase H still has zero production routing influence.

---

## 1. What this convergence added

Phase H previously evaluated **recorded artifacts offline only**. The gap closed
here is live evaluation of a specifically selected physical deployment, without
turning Phase H into a routing feature.

| Area | Before | After |
|---|---|---|
| Execution modes | replay only | `mode=replay` (offline) **and** `mode=live` (physical deployment) |
| Upstream I/O | never | replay: never; live: one real request per prompted case |
| DecisionProviders | not involved | still not involved — asserted at zero for live too |
| Config gate | `evaluation.enabled` | `evaluation.enabled` **and** `evaluation.live_enabled` (both default `false`) |

Replay is unchanged. `eval.ReplayExecutor` keeps its exact previous behaviour,
semantics and bounds; it is simply now one of two explicit modes.

---

## 2. Live executor architecture

### 2.1 Components

| File | Lines | Role |
|---|---|---|
| `internal/evallive/executor.go` | 320 | `LiveEvaluationExecutor` — the live `eval.Executor` |
| `internal/providers/evaluation.go` | 382 | `EvaluationTwin` (isolated adapter clone), `LiveCapable`, `LiveComplete`, native request/response shaping |
| `internal/httpapi/admin_eval.go` | +265 | `mode` handling, live validation, live run path, `live` response block |
| `internal/config/config.go` | +7 | `EvaluationConfig.LiveEnabled` |
| `internal/eval/runner.go` | +17 | `upstreamCalls(exec)` so a run record reports the real live upstream count |
| `internal/providers/http_adapter.go` | +4 | `evaluationOnly` field on the adapter |

### 2.2 Request flow

```
POST /admin/api/evaluation/run  {mode:"live", deployment_id, suite_id, prompts[]}
   │
   ├─ plane enabled? .................... no  → 409
   ├─ evaluation.live_enabled? .......... no  → 409
   ├─ suite exists? prompts valid? ...... no  → 400
   ├─ deployment exists in registry? .... no  → 404
   │
   ├─ s.reg.Get(providerID)                    ← production adapter (read-only)
   ├─ providers.EvaluationTwin(adapter)        ← isolated clone, shares transport
   ├─ evallive.NewLiveEvaluationExecutor(...)  ← refuses a production adapter
   │
   └─ eval.Runner.RunWithExecutor(...)         ← UNCHANGED runner
        └─ per case: LiveEvaluationExecutor.Execute
              └─ providers.LiveComplete(ctx, twin, model, prompt, maxTokens)
                    └─ twin.DoPath(POST, native path, native body)
                          ⇒ same path resolution / auth / headers / transport
                            as the data plane
        └─ deterministic evaluators → verdicts → score → scorecard
```

### 2.3 Reuse, not a second client architecture

There is exactly one provider adapter implementation in the repository
(`providers.httpAdapter`), and live evaluation uses it:

- the request body and path come from the adapter's own per-provider
  conventions (`chat_path` / `messages_path` / `responses_path` /
  Gemini `:generateContent`), the same ones `Probe` and the data plane use;
- auth mode, forwarded headers, `anthropic-version`, the concurrency semaphore,
  proxy and TLS settings and the HTTP transport are the adapter's;
- the response is decoded against the provider's native shape
  (OpenAI / Anthropic / Responses / Gemini).

No new OpenAI or Anthropic client type, no new transport, no parallel
protocol stack was introduced.

### 2.4 Isolation by construction

`providers.EvaluationTwin` returns a clone that **shares the production
`http.Transport`** (connection pool, proxy, TLS) but owns **private copies** of
every counter the data plane observes:

| State | Production adapter | Evaluation twin |
|---|---|---|
| transport / connection pool | shared | shared (that is the reuse) |
| credentials (key material) | shared strings | **private cooldown / success state** |
| quota accounting from response headers | production | **private** |
| `active` / `waiting` gauges | production | **private semaphore + counters** |
| rate-limit sequence counters | production | **private** |

`LiveComplete` and `NewLiveEvaluationExecutor` both **refuse** an adapter that is
not evaluation-isolated. Passing the production adapter is a hard error, so
"live evaluation accidentally used production state" cannot compile-and-pass;
it fails.

`internal/evallive` imports neither `internal/router`, nor `internal/health`,
nor `internal/decision`, nor `internal/route`, nor `internal/probe`. It holds a
four-field `Deployment` projection (`ID`, `ProviderID`, `Model`,
`ProviderType`) and nothing else. `internal/eval` and `internal/scorecards`
still import none of the routing packages, and the pre-existing structural
guard test is unchanged and still passes.

---

## 3. Isolation guarantees

Verified by tests, and every guarantee below is **mutation-checked**: the test
was observed to fail when the guarantee was deliberately broken, and to pass
again after the mutation was reverted (§7).

| Production state | Live evaluation effect | Test |
|---|---|---|
| Model / deployment health (`health.Manager`) | untouched | `TestPhaseH_Live_HealthIsolationSuccessErrorAndTimeout` |
| Circuits / cooldowns / quarantine | untouched | same |
| Provider health incidents | untouched | same |
| DecisionProvider health (`providerstate`) | untouched (no provider called) | `TestPhaseH_Live_ProviderCredentialAndQuotaIsolation` |
| Provider credential cooldown & success marks | untouched | same |
| Provider quota accounting (`remaining`/`limit`/`reserved`) | untouched | same |
| Provider concurrency gauges (`active`/`waiting`) | untouched | same |
| Session affinity pins | none created, moved or read | `TestPhaseH_Live_CreatesNoSessionAffinityState` |
| Production response cache | never read, never written | `TestPhaseH_Live_NeverWritesProductionResponseCache` |
| Production usage accounting | untouched | included in the health fingerprint |
| Candidate ordering / routing metrics | untouched | `TestPhaseH_LiveScorecardsHaveZeroRoutingInfluence` |

Both directions are covered: a **successful** live evaluation cannot improve
production health, and a live evaluation against an **HTTP 500** or a
**timeout** cannot degrade it. The health test snapshots a fully-populated
fingerprint (deployment health, provider health, provider stats, session count,
cache stats, usage) and requires it to be **byte-identical** before and after
each of the three runs.

---

## 4. Real upstream test

`TestPhaseH_Live_ExactlyOneUpstreamCallAndZeroDecisionProviderCalls`
(`internal/httpapi/eval_live_test.go`) runs against a real `httptest` upstream.

**Setup that makes the assertions falsifiable.** The gateway is configured in
`hybrid` decision mode with a three-step chain (`jev-main` → `policy` → `local`).
A production request is sent first and the test **fails if the three
DecisionProvider counters are all zero**, because then the "zero calls"
assertion would be vacuous.

**Asserted results**

| Assertion | Result |
|---|---|
| Selected physical deployment upstream calls | **exactly 1** (single prompted case) |
| Non-selected deployment upstream calls | **0** (no fan-out, no fallback) |
| LocalProvider calls | **0** |
| PolicyProvider calls | **0** |
| Jev provider calls | **0** |
| Hybrid chain (`DecisionOrchestrator.MetricsSnapshot()`) | **0** movement on every metric |
| New `decision_*` events | **0** |

`TestPhaseH_Live_ResponseIsGradedAndCreatesScorecard` drives three live cases
through a real upstream, asserts `upstream_calls == 3` (one per case), asserts
the response is graded (`score == 1`), and asserts a scorecard is written with
the `reasoning` quality dimension and measured `evaluation` provenance.

---

## 5. Health / cache / affinity isolation (detail)

### 5.1 Health isolation

`healthFingerprint` serialises deployment health, provider health, provider
stats, session count, cache stats and usage into one string, sorted so the value
is stable. The test performs:

1. baseline (with two real `RecordSuccess` entries so the snapshot is not empty);
2. a successful live run → fingerprint must be identical;
3. a live run against an HTTP 500 upstream → identical (and the run is graded
   `score == 0`);
4. a live run against a timing-out upstream (`case_timeout_ms: 60`) → identical
   (and graded `score == 0`).

### 5.2 Cache isolation

The live prompt is **byte-identical to the production request body**, so any
evaluation write into the production response cache would surface as a `HIT`.
After the live run, `cache.Stats()` shows `entries == 0` and `stores == 0`. The
equivalent production request then **MISSes**, and a second identical request
**HIT**s — proving both that the cache is enabled and that the MISS was real.

### 5.3 Affinity isolation

A production request with `X-Session-Id` first creates a real affinity pin (the
test fails if `SessionCount() == 0`). After a live run, the full fingerprint
including `SessionCount()` is unchanged and `PinnedDeploymentID` for that
session key is unchanged.

### 5.4 Provider credential and quota isolation

An upstream that returns `429` with `Retry-After` and full `x-ratelimit-*`
headers — exactly the response that moves production credential and quota state
on the data plane — is driven by a live run. The production adapter's
`credentials_cooling`, `remaining_tokens`, `token_limit`,
`remaining_requests`, `request_limit`, `active_requests`, `waiting_requests`,
`reserved_requests` and `reserved_tokens` are all unchanged.

---

## 6. Privacy canaries

`TestPhaseH_Live_PrivacyCanaries`.

### 6.1 Dataset canary — `SECRET_EVAL_DATASET_CANARY_7b91`

Placed inside every live prompt. The test first **proves it reached the
explicitly selected physical deployment** (the upstream received the requests),
then asserts it appears in **none** of:

- `/metrics`
- `/admin/api/snapshot` (normal admin snapshot)
- events snapshot
- the evaluation run response
- `/admin/api/evaluation/runs`
- `/admin/api/scorecards`
- `/admin/api/health`
- gateway log output

### 6.2 Provider credential canary — `SECRET_EVAL_PROVIDER_KEY_3f42`

Configured as the provider's API key. The upstream deliberately returns
HTTP 500 and **echoes the `Authorization` header in the error body**, so the
canary is present in the failure payload. The test proves it was actually sent
(`auth` header observed at the upstream), then asserts it appears in **none** of
the same surfaces above, plus:

- `decision.DecisionTrace`
- routing events (every event in the bus is marshalled and checked)
- model-health state (`hm.Snapshot()` and `hm.ProviderSnapshot()`)
- scorecards (raw prompt content is not required, so it is not stored)
- errors surfaced by the admin API

Mechanism: `LiveComplete` never copies an upstream message into an
`eval.Outcome`; failure details are reduced to a bounded `ErrorType`
(`http_500`, `upstream_timeout`, `upstream_transport`, `empty_completion`).
Transport errors pass through `httpAdapter.RedactBody` before they can reach a
run record, event, metric or log line.

---

## 7. Routing neutrality

`TestPhaseH_LiveScorecardsHaveZeroRoutingInfluence`:

1. Send a production request; record `X-Gateway-Deployment`.
2. Populate **extreme** scorecards into the live registry: `p1/m1` quality `1.0`
   with 4096 samples, `p2/m2` quality `0.0` with 4096 samples. The test asserts
   the registry really holds both.
3. Send the **same** production request again; assert
   `X-Gateway-Deployment` is identical.

Additionally, a capturing PolicyProvider records the `DecisionRequest` it
receives; the marshalled **candidate payload** is asserted to contain no
`quality`, `scorecard`, `eval_score`, `dim_` or `confidence_score` field.
Phase H does **not** add scorecard quality to PolicyProvider and does **not**
send scorecards to Jev.

Not implemented, as required: no scorecard-aware production routing, no shadow
routing, no canary routing, no active quality weighting, no learned routing.

### 7.1 Mutation checks (the isolation tests are not tautologies)

Each mutation below was applied, the listed tests were observed to **fail**, and
then the mutation was reverted and the tests observed to **pass** again.

| Mutation | Test that caught it |
|---|---|
| `EvaluationTwin` returns the production adapter; `LiveComplete`/`LiveCapable` stop enforcing `evaluationOnly` | `TestPhaseH_Live_ProviderCredentialAndQuotaIsolation` |
| Live path calls `hm.RecordSuccess(...)` | `TestPhaseH_Live_HealthIsolationSuccessErrorAndTimeout` |
| Live path calls `rt.ObserveSession(...)` | `TestPhaseH_Live_CreatesNoSessionAffinityState` |
| Live path calls `cacheStoreResponse(...)` | `TestPhaseH_Live_NeverWritesProductionResponseCache` |
| Live path calls `decisionOrchestrator.Decide(...)` | `TestPhaseH_Live_ExactlyOneUpstreamCallAndZeroDecisionProviderCalls` (metric `decisions_total` moved 1 → 2) |

---

## 8. Gates actually executed

Everything below was run in this session, on this branch, in this sandbox
(Debian 12 x86_64, 2 vCPU, Go 1.23.9). Nothing here is projected. Go 1.23.9 was
bootstrapped from source in the sandbox because `go.dev` and the module proxy
are unreachable from it; the module has no external dependencies, so all
commands ran with `GOPROXY=off`.

| Gate | Command | Result |
|---|---|---|
| Formatting | `gofmt -l .` | clean |
| Vet | `go vet ./...` | clean |
| Full gate | `./scripts/verify.sh` | **VERIFY PASS** |
| Stress | `./scripts/stress.sh` | **STRESS PASS** |
| Smoke | `./scripts/smoke-local.sh` | **SMOKE PASS** |
| Race (required set) | `go test -race ./internal/eval/... ./internal/scorecards/... ./internal/httpapi ./internal/decision/... ./internal/router ./internal/route` | all packages ok |
| Race (live package) | `go test -race ./internal/evallive/...` | ok |
| Full suite | `go test ./...` | 26 packages ok |

### 8.1 `verify.sh` detail

`verify.sh` now runs `gofmt`, shell syntax, `go test -count=10 -shuffle=on`,
`go vet`, `go test -race -count=3 -shuffle=on`, **eight** short fuzz targets
(the six pre-existing ones plus the two new live-evaluation targets), and
linux amd64 + arm64 cross builds. Result: **VERIFY PASS**.

### 8.2 Fuzz results (30s per target, `GOMAXPROCS=2`)

| Target | Execs | Result |
|---|---|---|
| `FuzzResolve_Verdicts` (`internal/eval`) | 453,945 | PASS |
| `FuzzRunner_Artifacts` (`internal/eval`) | 918,421 | PASS |
| `FuzzImportJSON` (`internal/scorecards`) | 618,490 | PASS |
| `FuzzValueValidation` (`internal/scorecards`) | 662,021 | PASS |
| `FuzzLiveExecutor_UpstreamResponse` (`internal/evallive`) | 32,537 | PASS |
| `FuzzLiveExecutor_Prompts` (`internal/evallive`) | 6,661 | PASS |

No crashers were written to any `testdata/fuzz` corpus.

### 8.3 Benchmarks (`-benchtime=100x`)

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `BenchmarkResolve_Verdicts` (`internal/eval`) | 15.58 | 0 | 0 |
| `BenchmarkReplayExecutor` (`internal/eval`) | 19.58 | 0 | 0 |
| `BenchmarkRun_CodingSuite` (`internal/eval`) | 9,097 | 7,285 | 35 |
| `BenchmarkRun_AllSuites` (`internal/eval`) | 108,327 | 101,196 | 553 |
| `BenchmarkHealthFromRuns` (`internal/eval`) | 14,108 | 5,217 | 32 |
| `BenchmarkLiveExecutor_Execute` (`internal/evallive`) | 83,006 | 13,524 | 182 |
| `BenchmarkLiveExecutor_RunSuite` (`internal/evallive`, 3 live upstream calls) | 359,843 | 44,422 | 569 |
| `BenchmarkLiveExecutor_PromptValidation` (`internal/evallive`) | 4,893 | 2,542 | 3 |

The live numbers are dominated by real loopback HTTP through the shared
transport; they are recorded so a regression in the live path is visible.

---

## 9. Test inventory added in this convergence

| File | Tests |
|---|---|
| `internal/evallive/executor_test.go` | 9 (construction guards, one-call-per-case, missing prompt, HTTP failure graded, timeout graded, empty completion, runner integration producing a scorecard, prompt privacy at the executor boundary) |
| `internal/evallive/fuzz_test.go` | 2 fuzz targets |
| `internal/evallive/bench_test.go` | 3 benchmarks |
| `internal/httpapi/eval_live_test.go` | 9 (real upstream + zero DecisionProvider calls, graded scorecard, health isolation for success/500/timeout, provider credential & quota isolation, cache isolation, affinity isolation, routing neutrality, privacy canaries, mode validation, opt-in gate) |

Pre-existing Phase H tests were not weakened: replay still requires artifacts,
still performs zero upstream I/O, and the disabled-plane behaviour is unchanged.

---

## 10. Documentation updates

- `docs/PHASE_H_EVALUATION_AND_SCORECARDS.md` — new §2.4 "Execution modes"
  (replay / live), §2.4.2 live architecture, an expanded §3 isolation contract
  with a per-state table, updated HTTP surface and config sections, and a
  rewritten non-goals list. The claim that Phase H performs no live model calls
  was **removed** and replaced with an accurate statement of the two modes.
- `docs/PHASE_H_CURRENT_STATE_NOTE.md` — "Offline replay, never live probing"
  replaced by "Two explicit execution modes"; non-goals corrected; acceptance
  table extended with the live isolation, zero-DecisionProvider and canary rows.
- `docs/CONFIGURATION.md` — `evaluation.live_enabled` documented plus a new
  "Evaluation modes" subsection.
- `configs/config.example.json` — `evaluation.live_enabled: false`.
- `internal/httpapi/admin.go` — the admin snapshot note now reads "Phase H
  supports offline replay and opt-in live physical-deployment evaluation;
  scorecards never change routing in Phase H".
- `scripts/verify.sh` — the two live-evaluation fuzz targets added to the gate.

---

## 11. Boundaries (unchanged and enforced)

- Replay mode still performs **zero** upstream I/O.
- Live mode is opt-in twice (`evaluation.enabled` + `evaluation.live_enabled`),
  per-request explicit (`"mode":"live"`), targets one explicit `deployment_id`,
  and runs only when an admin posts it. No evaluation traffic on startup, none
  on config reload, none automatically.
- Live evaluation bypasses every DecisionProvider and cannot change production
  health, affinity, cache, quota or routing state.
- Scorecards still have **zero** production routing influence.
- Phase I was not started: no scorecard-aware routing, no shadow routing, no
  canary routing, no active quality weighting, no learned routing.

---

## 12. Final verdict

**PHASE H: PASS**
