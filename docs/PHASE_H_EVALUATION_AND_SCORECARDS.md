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

### 2.4 Offline replay (isolation mechanism)

`ReplayExecutor` is constructed from recorded artifacts. It performs no I/O and
reports `UpstreamCalls() == 0`. The HTTP run endpoint requires artifacts and
returns `400` otherwise: Phase H never prompts a model, never probes an
upstream, never spends quota, and cannot warm or poison production health.

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

## 4. HTTP surface (admin only)

| Endpoint | Behaviour |
|---|---|
| `GET /admin/api/scorecards?limit=&deployment=` | Bounded rows (`limit ≤ 500`, default 50) with per-value provenance, quality coverage, provenance histogram; unknown deployment returns an empty list **plus a note**, never a score. |
| `GET /admin/api/scorecards/{deployment_id}` | Single scorecard with version history; `404` when no evidence exists. |
| `GET /admin/api/evaluation/suites` | Suite catalog, evaluator IDs, bounds, `judge_available: false`. |
| `GET /admin/api/evaluation/runs?limit=&run=` | Bounded run history (`limit ≤ 100`) plus the evaluation-health namespace. |
| `POST /admin/api/evaluation/run` | Replays recorded artifacts, stores the run, writes a scorecard only when evidence is sufficient. |

`POST` payload: `suite_id`, `deployment_id`, `artifacts[]` (required), optional
`provider_id`, `model`, `case_timeout_ms`, `latency_target_ms`, `ttft_target_ms`.
Unknown JSON fields are rejected; the body is bounded at 4 MiB (413 over);
`deployment_id` must exist and `provider_id`/`model` must match it; a disabled
plane returns 409; too many artifacts (config bound) returns 400.

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
- No live model calls, no network access, no prompt submission.
- No operator-defined suites yet (catalog is built-in, versioned).
- No telemetry ingestion pipeline yet (`FromTelemetry` exists as a validated
  constructor, but nothing feeds it automatically).
