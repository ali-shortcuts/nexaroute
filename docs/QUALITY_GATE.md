# Model Quality Regression Gate

A repeatable, offline-by-default comparison of a candidate
model/deployment against a recorded baseline. It extends the existing
evaluation architecture; it does not replace it or duplicate it.

## What already existed (reused, not rebuilt)

- **Evaluation plane** (`internal/eval`, `internal/httpapi/evaluation_plane.go`):
  deterministic evaluators with deterministic-first precedence, a
  replay executor that performs no I/O, and a bounded run store. A
  missing artifact produces a `missing` case, never a synthetic score.
- **Live evaluation** (`internal/evallive`): one explicit physical
  deployment, an evaluation-isolated adapter clone, no routing, no
  health/affinity/cache state. It is opt-in twice over:
  `evaluation.live_enabled` in config, and a live run posted to the
  admin surface. See `docs/COMPATIBILITY.md` for the same opt-in
  pattern used by the live conformance canary.
- **Scorecards** (`internal/scorecards`): provenance-mandatory values.
  A dimension with no evidence is absent, not scored zero.
- **Compatibility manifest** (`internal/providers/conformance.go`,
  `docs/COMPATIBILITY.md`): per-dialect claims with evidence states.
  The gate's `protocol_compat` suite covers the same protocol area
  at the evaluation level; it does not edit the manifest.

## What this gate adds

- **Suites** (`internal/eval/suite.go`): the v1 suites for tool calls,
  structured JSON and protocol compatibility are reused unchanged.
  Three new v1 suites close the remaining areas, each with 3 cases
  and `MinSamples: 3`:
  - `multi_turn` — final-turn grading only (`multi_turn_final`).
    Earlier turns are never graded and never copied into a result.
  - `streaming` — termination plus a minimum event count
    (`stream_events`, `stream_terminated`).
  - `cancellation` — a recorded cancellation that ends without an
    error (`cancellation_clean`).
  - Tool-call grading is also tightened: recorded arguments that are
    not valid JSON now fail, even when the tool name matches.
  - The runner now surfaces recorded TTFT per case (`ttft_ms`) and
    scores TTFT against an operator target exactly like latency.
- **Comparison** (`internal/qualitygate`): `Summarize` turns recorded
  `eval.Result`s into per-suite aggregates (task success, error
  rate, latency p50/p95, TTFT p50/p95); `Compare` compares them
  against a baseline under configurable thresholds. Defaults: a
  5-point absolute drop in task success or rise in error rate, or a
  20% relative rise in latency, TTFT or cost, is a regression.
  The default required suite set is versioned as `quality-gate/v1`:
  `tool_calling`, `structured_output`, `protocol_compat`,
  `multi_turn`, `streaming`, `cancellation`.
- **CLI** (`nexaroute quality-gate`): `summarize` writes a baseline
  snapshot; `compare` writes a report. It reads files only — no
  network, no model call, no routing change.

## Verdicts: insufficient is not failure

`compare` exit codes and report verdicts are:

| Exit | Overall | Meaning |
| --- | --- | --- |
| 0 | `no_regression` | Comparable, no metric regressed beyond threshold. |
| 1 | `regression` | At least one comparable metric regressed. |
| 3 | `insufficient_evidence` | A required suite is missing, unscoreable, or version-mismatched on either side. No quality score is assigned to that suite. |
| 2 | — | Usage or setup error. |

A regression measurable in a partially comparable suite is still
listed in `regressions` (marked partial evidence), but it does not
promote an incomplete comparison to a regression verdict. Cost is
the one exception to the insufficient rule: cost is optional
evidence, so an unobserved cost makes only the cost metric
insufficient, not the whole report.

## Provenance recorded in every report

Baseline and candidate provenance each record: gateway commit,
model, deployment id, provider id, route-config id and SHA-256 hash
(when the operator supplies them), mode (`replay` or `live`) and
generation timestamp, plus the suite versions compared and the
thresholds used. A report can therefore always be re-derived, and a
baseline recorded on a different suite version is rejected rather
than silently compared.

## Live mode, with strict limits

Live evidence must already exist: it is produced through the admin
live-evaluation path, never by this gate. The gate then enforces,
on top of the plane's own bounds:

- `--acknowledge-live` is required for any live candidate;
- `--max-live-requests` is required and is a hard ceiling on the
  summed upstream calls in the candidate;
- `--max-cost-usd`, when given, is a hard ceiling on the candidate's
  *observed* cost.

Replay candidates must report zero upstream calls. Live evaluation
never runs in public CI (see `.github/workflows/quality-gate.yml`,
which compares only the checked-in offline examples).

## Privacy

Reports, snapshots and summaries contain identifiers, counts,
timings, costs and deltas only. `eval.Result` carries no output
text, multi-turn turns are never copied into a result, and live
prompts are never persisted by the evaluation plane. A regression
test asserts that a sentinel output string and a sentinel
conversation turn present in replay artifacts appear in neither the
eval result nor the gate report.

## No automatic routing or weight change

The report always carries `routing_action: "none"` and
`routing_changed: false`, and the gate package imports neither the
router nor the decision plane. A regression is a report for an
operator to act on — for example by keeping the current deployment,
investigating the failing suite, or re-running with more evidence —
not a signal that changes production routing or model weights by
itself. Note the existing, separate Phase 19 feedback wiring: an
operator may give a policy quality component a positive weight, but
that is a configured policy decision, not a consequence of a gate
report.

## Recording a baseline and comparing (offline)

```bash
# 1. Produce recorded runs with the existing evaluation plane
#    (replay mode; see the admin evaluation surface), and assemble
#    them into a quality-gate-input/v1 file (runs + provenance).
# 2. Summarize it into a baseline snapshot:
nexaroute quality-gate summarize --input candidate.json --out baseline-snapshot.json

# 3. Compare a new candidate against that baseline:
nexaroute quality-gate compare \
  --baseline baseline-snapshot.json \
  --candidate candidate.json \
  --out report.json
echo $?  # 0 no regression, 1 regression, 3 insufficient evidence
```

Custom thresholds use the same JSON shape as
`qualitygate.Thresholds` (`success_drop`, `error_rate_rise`,
`latency_rise_pct`, `ttft_rise_pct`, `cost_rise_pct`,
`required_suites`), with unknown fields rejected:

```bash
nexaroute quality-gate compare --baseline b.json --candidate c.json \
  --thresholds thresholds.json --out report.json
```

Checked-in offline examples live in `docs/quality-gate/examples/`:
`baseline.json`, `candidate-same.json` (exit 0),
`candidate-regressed.json` (exit 1, tool-call success drops),
`candidate-insufficient.json` (exit 3, cancellation suite missing).
`scripts/test-quality-gate.sh` runs the unit tests plus these three
comparisons through the built binary.

## Before / after (this change)

Before, on main `e6c3cdf3`: the evaluation plane could grade one
suite at a time, in replay or live mode, and write a scorecard only
when a run was scoreable — but there was no multi-suite baseline,
no task-success/error-rate/latency/TTFT/cost comparison, no
multi-turn, streaming or cancellation suite, and no reproducible
gate command or CI check for model quality.

After, on this branch: the six-suite gate above runs offline from
recorded evidence, compares a candidate with a baseline under
configurable thresholds, distinguishes insufficient evidence from a
regression, and reports without changing routing. The checked-in
examples demonstrate all three verdicts deterministically.

## Remaining limitations (honest, not hidden)

- **Cost is observed-only.** If neither side supplies an actually
  observed total cost, cost is not compared — it is never estimated
  from token counts or assumed prices. The gate does not compute
  cost from usage; the operator's billing/usage figure is the input.
- **Small samples, no statistics.** Gate suites have 3 cases each
  by design (deterministic, cheap, offline). Deltas are threshold
  comparisons, not confidence intervals; a 3-case suite can move a
  lot on one case. Treat a regression as an investigation trigger,
  and re-run with more evidence (operator suites, or a larger
  recorded run) before acting.
- **Timings are recorded timings.** Latency and TTFT come from the
  artifacts' recorded values. Replay timings describe the recording
  environment, not the candidate's current performance; live timings
  are the only ones that describe a live deployment, and they are
  bounded and opt-in as described above.
- **Live evidence is not produced by the gate.** The CLI compares
  recorded live runs; it does not send prompts. Producing a live
  candidate still requires the admin live-evaluation path with
  `evaluation.live_enabled`, an explicit deployment, and operator
  authorization. No live provider call was made for this change, and
  no live tenant or credential was used.
- **Route config is a reference, not a snapshot.** The report
  records a route-config id and hash when supplied; it does not
  itself snapshot or verify the gateway's routing configuration.
  Supply the hash from the exact configuration the candidate ran
  under, or leave it empty and say so.
- **No admin-surface gate endpoint.** Comparison is a CLI/library
  operation in this change; the admin evaluation surface is
  unchanged. A future slice could expose a read-only gate report
  from stored runs, but it must keep the same no-routing-change and
  no-content rules.
