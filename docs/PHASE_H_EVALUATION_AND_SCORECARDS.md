# Phase H — Empirical Evaluation + Model Scorecards (design)

Phase H adds an observation plane that records what a deployment is actually
known to do, and refuses to state anything it cannot back with evidence.

Nothing in this document changes routing. Scorecards are admin-plane data:
no candidate ordering, health circuit, weight, priority, quota or failover
decision reads them.

## 1. Scorecards: every value carries provenance

A scorecard is a versioned claim about one deployment (`provider_id/model_id`),
made of dimension values:

```json
{
  "deployment_id": "chat2api/deepseek",
  "provider_id": "chat2api",
  "model": "deepseek-chat",
  "version": 1,
  "generated_at": "2026-09-26T12:00:00Z",
  "values": {
    "coding": {
      "score": 0.78, "raw": 0.78, "unit": "ratio",
      "provenance": "evaluation",
      "sample_count": 12, "confidence": 0.375,
      "evaluated_at": "2026-09-26T12:00:00Z",
      "suite_version": "1", "source": "coding"
    }
  },
  "evaluation": {"suite_id": "coding", "suite_version": "1", "run_id": "…", "sample_count": 12, "evaluated_at": "…"}
}
```

### 1.1 Provenance vocabulary (closed set)

| Provenance | Meaning | Required fields |
|---|---|---|
| `evaluation` | produced by a deterministic Phase H suite run | `suite_version`, `sample_count ≥ 1`, `evaluated_at` |
| `production_telemetry` | derived from observed production traffic evidence | `sample_count ≥ 1`, observed timestamp |
| `imported` | loaded from an external artifact (benchmark export, vendor report) | `source`, `sample_count ≥ 1` |
| `operator_config` | an operator declaration, not a measurement | `source` key, `sample_count == 0` |

An empty provenance is rejected (`ErrNoProvenance`). A score outside `[0,1]`, a
non-finite raw value, a confidence outside `[0,1]`, or a sample count beyond
`MaxSampleCount` are rejected too. An import that contains one invalid
scorecard rejects the whole artifact: partially trusted evidence is worse than
none.

### 1.2 Dimensions (15, closed set)

Quality (8): `coding`, `debugging`, `reasoning`, `tool_calling`,
`structured_output`, `instruction_following`, `long_context`, `protocol_compat`.
Operational (7): `latency_ms`, `ttft_ms`, `throughput`, `availability`,
`failure_rate`, `input_cost`, `output_cost`.

Quality dimensions are the ones suites can produce. Operational dimensions are
produced only when the corresponding target/evidence exists (a latency value
without a latency target is omitted, not scored against an invented budget).

### 1.3 Confidence is a documented calibration, not statistics

`ConfidenceFromSamples(n) = min(1, n / 32)` — monotonic, saturating, and named
as a calibration in code so nobody mistakes it for a confidence interval. A
value with 0 samples cannot exist at all.

### 1.4 Registry semantics

`scorecards.Registry` keeps the latest scorecard per deployment plus bounded
version history: ≤ 4096 deployments and ≤ 16 versions per deployment.
`Upsert` assigns the next version and clones on read, so callers can never
mutate live state. `Retain(valid)` drops evidence for deployments that no
longer exist; it is called on every config swap. `Load` (state file) and
`ImportJSON` (artifact) are validated entry points.

Failures are visible, not silent: rejections are counted
(`nexaroute_scorecard_rejected_total`) and an import failure is surfaced as
`import_error` on the admin surface.

## 2. Evaluation engine: deterministic-first

### 2.1 Suites and expectations

Eight built-in suites (`eval.Catalog()`, versioned `1`) map to quality
dimensions and declare cases with weights and expectations. Bounds:
≤ 128 cases per suite, ≤ 4096 bytes per expectation payload.
`ValidateSuite` rejects duplicate case IDs, unknown dimensions, invalid
weights, and invalid expectations.

### 2.2 Evaluators and precedence

Deterministic evaluators implement the eight expectation kinds. Verdict
vocabulary is closed and JSON-stable: `pass`, `fail`, `error`, `skip`,
`missing`, `unjudged`.

`Resolve(verdicts, judgeEnabled)` returns the decided verdict **and a
resolution category**:

| Category | Meaning |
|---|---|
| `deterministic` | a deterministic evaluator decided the case |
| `judge` | only a judge produced a verdict (judge must be enabled) |
| `none` | no evaluator produced a usable verdict (e.g. missing artifact) |

A judge verdict is recorded but can never override a deterministic verdict. With
no judge injected, the judge evaluator returns `unjudged`; the HTTP path never
enables it.

### 2.3 Runner, scoring, and honesty rules

- Input artifacts are bounded (`MaxOutcomesPerRun`, per-field byte caps) and
  validated; an artifact for an unknown case is ignored so a run cannot invent
  coverage.
- Only **decisive** verdicts are scored. Weighted score = Σ(weight × pass) /
  Σ(weight of decisive cases). `error`/`skip`/`missing`/`unjudged` never count as
  passes and never become scores.
- A run is `scoreable` only when decisive samples ≥ the suite's `min_samples`
  (≥ `MinDecisiveSamples`). Otherwise `Reason = "insufficient_samples"` and
  `Result.Scorecard()` returns `ok=false` — no scorecard is written.
- Optional per-case timeout and context cancellation are honored; cancellation
  marks cases and never blocks.
- Repeats of the same artifacts produce byte-identical results (determinism
  test).
- `HealthFromRuns` produces an **evaluation-health namespace** that is separate
  from routing health and is never read by the router.

### 2.4 Execution modes (one endpoint, two explicit modes)

`POST /admin/api/evaluation/run` takes an explicit `mode`. There is no implicit
mode and no fallback between them.

| `mode` | Executor | Upstream I/O | Input | Notes |
|---|---|---|---|---|
| `replay` (default, and when `mode` is omitted) | `eval.ReplayExecutor` | **none** | `artifacts[]` (required) | Grades recorded evidence. `UpstreamCalls() == 0`. |
| `live` | `evallive.LiveEvaluationExecutor` | **one real request per prompted case** | `deployment_id` (required) + `prompts[]` (required) | Measures one explicitly selected physical deployment. |

A replay run that carries `prompts` is rejected; a live run that carries
`artifacts` is rejected. The two modes are never mixed.

#### 2.4.1 Offline replay (isolation mechanism)

`ReplayExecutor` is constructed from recorded artifacts. It performs no I/O and
reports `UpstreamCalls() == 0`. Replay cannot prompt a model, probe an upstream,
spend quota, or warm or poison production health.

#### 2.4.2 Live physical-deployment evaluation

Live evaluation answers a question replay cannot: *what does this deployment do
right now?* It is measurement only, and it is deliberately the most constrained
code path in Phase H.

- **One explicit target.** The run carries `deployment_id`. The deployment must
  already exist in the routing registry; it is read once, as identity
  (`id`, `provider_id`, `model`, `provider_type`) and nothing else. No candidate
  set is built, no eligibility check runs, no fallback exists.
- **No DecisionProviders.** Live evaluation does not call `LocalProvider`,
  `PolicyProvider`, any Jev provider, or any hybrid `DecisionProvider` chain. It
  does not call the decision orchestrator at all.
- **One call per case.** A prompted case produces exactly one upstream request.
  There is no retry, no second candidate, no hedge. The run record reports the
  real count in `upstream_calls`.
- **Same adapter, isolated instance.** The request is built and dispatched by
  the provider adapter that already serves the data plane — same path
  resolution, same auth application, same headers, same HTTP transport. There is
  no second OpenAI/Anthropic/Gemini client architecture. What *is* duplicated is
  state: the call goes through an **evaluation twin**
  (`providers.EvaluationTwin`) that shares the transport but owns private copies
  of credential cooldowns, quota accounting and concurrency gauges.
  `providers.LiveComplete` refuses to run against a production adapter, so
  "isolation bypassed" is a hard error rather than a review convention.
- **Failure is evidence.** A transport error, an HTTP error (recorded as
  `http_<status>`), a timeout and an empty completion are all recorded and
  graded like any other artifact. Nothing is retried and nothing is invented. A
  case with no prompt produces a `missing` result, never a synthetic score.

`internal/evallive` imports neither the router, nor the health manager, nor the
decision plane, so it structurally cannot reach routing state. `internal/eval`
still imports none of the routing packages either — the existing dependency
guard is unchanged and still passes.

## 3. Isolation contract

Two independent mechanisms enforce that evaluation cannot influence routing:

1. **Dependency direction** (structural, test-enforced). Routing/health/provider
   packages do not import `internal/eval` or `internal/scorecards`; the eval
   packages do not import routing packages. `TestIsolation_*` parses production
   files with `go/parser` and fails on any violation.
2. **Runtime shape**. The evaluation plane is owned by the admin surface only.
   The only production data it reads is deployment identity (to refuse
   evaluating a deployment that does not exist). It writes to the scorecard
   registry, run store and its own state file — never to health, router,
   provider, usage or cache state.

Live evaluation adds a third mechanism, because it *does* perform I/O:

3. **Evaluated traffic is structurally non-production.** A live run goes through
   an evaluation twin of the provider adapter, never the production instance.
   That single fact is what keeps the following true in both directions —
   evaluation success cannot improve production health and evaluation failure
   cannot degrade it:

   | Production state | Live evaluation effect |
   |---|---|
   | Model / deployment health (`health.Manager`) | untouched (never read, never recorded) |
   | Circuits, cooldowns, quarantine | untouched |
   | DecisionProvider health (`providerstate`) | untouched (no provider is called) |
   | Provider credential cooldown / success marks | untouched (twin has private credential state) |
   | Provider quota accounting from response headers | untouched (twin has private counters) |
   | Provider concurrency gauges (`active`/`waiting`) | untouched (twin has private semaphore) |
   | Session affinity pins | untouched (no pin created, moved or read) |
   | Production response cache | never read, never written |
   | Candidate ordering / routing metrics | untouched |
   | Production usage accounting | untouched |

   Every row is asserted by a test that is mutation-checked: breaking isolation
   makes the test fail (`docs/PHASE_H_IMPLEMENTATION_REPORT.md` §7).

## 4. HTTP surface (admin only)

| Endpoint | Behaviour |
|---|---|
| `GET /admin/api/scorecards?limit=&deployment=` | Bounded rows (`limit ≤ 500`, default 50) with per-value provenance, quality coverage, provenance histogram; unknown deployment returns an empty list **plus a note**, never a score. |
| `GET /admin/api/scorecards/{deployment_id}` | Single scorecard with version history; `404` when no evidence exists. |
| `GET /admin/api/evaluation/suites` | Suite catalog, evaluator IDs, bounds, `judge_available: false`, `modes`, `live_enabled` and the live bounds. |
| `GET /admin/api/evaluation/runs?limit=&run=` | Bounded run history (`limit ≤ 100`) plus the evaluation-health namespace. |
| `POST /admin/api/evaluation/run` | `mode=replay` (default) grades recorded artifacts; `mode=live` measures one physical deployment. Both store the run and write a scorecard only when evidence is sufficient. |

`POST` payload:

- shared: `suite_id`, `deployment_id` (required), `mode` (`replay` | `live`,
  default `replay`), optional `provider_id`, `model`, `case_timeout_ms`,
  `latency_target_ms`, `ttft_target_ms`;
- `mode=replay`: `artifacts[]` (required, ≤ `evaluation.max_artifacts`);
- `mode=live`: `prompts[]` (required, ≤ 512 entries, each `{case_id, prompt}`
  with `prompt ≤ 64 KiB`), optional `max_output_tokens` (≤ 4096).

Unknown JSON fields are rejected; the body is bounded at 4 MiB (413 over);
`deployment_id` must exist and `provider_id`/`model` must match it; an unknown
mode returns 400; a disabled plane returns 409; live mode with
`evaluation.live_enabled = false` returns 409; a live prompt whose `case_id` is
not in the named suite returns 400; too many artifacts or prompts returns 400.
`mode=live` additionally reports a `live` block with the targeted deployment and
the real `upstream_calls` count.

The admin snapshot carries `scorecards` and `evaluation` sections, and the
metrics endpoint exposes bounded families (counts by provenance and verdict,
runs by outcome, scorecards written, import failures, state write failures).

Events: kind `eval_run` with the suite label (≤ 32 chars), samples, score,
status and verdict counts. **Model outputs never enter events.**

## 5. Configuration

```json
{
  "evaluation": {
    "enabled": false,
    "live_enabled": false,
    "max_runs": 64,
    "max_scorecards": 1024,
    "import_path": "",
    "state_path": "",
    "max_artifacts": 128,
    "latency_target_ms": 0,
    "ttft_target_ms": 0
  }
}
```

| Field | Default | Bounds | Meaning |
|---|---|---|---|
| `enabled` | `false` | — | Opt-in. While false the plane accepts no runs and no imports; scorecards can never be written. |
| `live_enabled` | `false` | — | Second, independent opt-in for **live** evaluation. While false, `mode=live` is refused with 409 and no prompt ever leaves the gateway. Setting it does not create traffic by itself: live calls happen only when an admin POSTs a run with `mode=live`. |
| `max_runs` | `64` | 1–512 | Bounded retained runs (memory and state file). |
| `max_scorecards` | `1024` | 1–4096 | Scorecard registry bound. |
| `import_path` | `""` | ≤ 4096 bytes | Read-only scorecard artifact (JSON). Re-read on config reload; failures are reported, never partially applied. |
| `state_path` | `""` | ≤ 4096 bytes | Optional durable state file, written atomically with mode 0600. |
| `max_artifacts` | `128` | 1–512 | Per-run artifact bound. |
| `latency_target_ms` / `ttft_target_ms` | `0` | 0–600000 | Optional scoring targets used as evidence only when provided. |

Hot reload: a config swap prepares the next plane reusing the live registry and
store, so recorded evidence survives; evidence for deployments removed from the
config is dropped (`Retain`); a state file is re-read only when `state_path`
actually changes, so unrelated reloads cannot roll memory back to stale disk
state.

## 6. Durability

`evaluation.state_path` holds one versioned JSON document (`version: 1`) with
the bounded runs and scorecards. Writes are atomic (temp file + fsync + rename,
mode 0600, directory 0700) and best-effort: a write failure increments
`state_writes_failed` and is surfaced, but never fails a run. Loads are strict:
oversized files (> 8 MiB), unknown fields, unsupported versions and any invalid
run/scorecard reject the **whole** document before anything is mutated. When the
document would exceed the size bound, the oldest runs are trimmed; scorecards
are never dropped to make room.

## 7. Non-goals

- No routing influence of any kind in Phase H.
- No judge implementation, no LLM-as-judge calls.
- No replay-mode network access: `mode=replay` performs zero upstream I/O.
- No *automatic* live traffic: `mode=live` runs only when an operator
  explicitly posts a run, requires `evaluation.enabled` **and**
  `evaluation.live_enabled`, and targets one explicitly named deployment.
- No scorecard-aware routing, shadow routing, canary routing, active quality
  weighting or learned routing — that is Phase I and was not started.
- No operator-defined suites yet (catalog is built-in, versioned).
- No telemetry ingestion pipeline yet (`FromTelemetry` exists as a validated
  constructor, but nothing feeds it automatically).
