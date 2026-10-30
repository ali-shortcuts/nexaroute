# Issue #664 — soak evidence validator + explicit outage stages (2026-10-29)

Status: **bounded local soak PASS on the new code; long-form multi-hour
soak on the current head NOT RUN; issue #664 stays OPEN.** This report
records exactly what was executed and what was not. It does not claim
multi-hour stability, production SLOs, or any RPO/RTO value — those
require representative runs that have not happened.

## What already existed (not duplicated)

- `scripts/soak.sh` + `.github/workflows/soak.yml` (workflow_dispatch
  long soak, push-to-main short 0s shape) with JSON + Markdown evidence.
- `scripts/ci-soak-smoke.sh` + the `ci-soak-smoke` job in
  `.github/workflows/ci.yml` (bounded CI smoke; see
  `docs/reports/ISSUE664_CI_SMOKE_2026-10-29.md`).
- `docs/reports/ISSUE664_SOAK_PROCEDURE.md` (the smoke/soak separation
  contract) and `docs/reports/issue664-stress-soak-evidence.md`
  (video stress/soak tests).

## Gap found first-hand

The long-soak summary, unlike the smoke summary, had no per-stage
failure text, no summarized resource-sample peaks, and no `not_run`
block — and no step ever checked that a summary was complete before
anyone quoted it. A job badge alone could therefore pass for evidence
even with a skipped stage, a wrong commit, or a sustained duration
shorter than requested. The long soak also had no explicit
outage-provider / outage-gameday stage: its fault coverage rested
only on the flapping-upstream behavior inside the sustained stage,
while the smoke script ran those selectors explicitly.

## What this change adds (no competing framework)

1. `scripts/check-soak-evidence.py` (stdlib only, no credentials read
   or recorded): validates a soak or smoke `summary.json` and exits 0
   only when the schema, full 40-hex commit (and `--expected-commit`
   when given), timestamps vs `total_duration_seconds`, Go version
   and host identity, workload parameters, stage completeness and
   exit-code consistency, resource samples for a passed hot-reload
   stage, per-stage `failure_summary` for failed stages, and the
   `not_run` block all check out. For a requested sustained duration
   it requires the sustained stage itself to have run at least that
   long. SKIPPED stages fail validation unless named in
   `--allow-skipped` (the smoke race stage with `RACE=0` is the only
   intended use; the long soak allows none). Schema gaps in older
   summaries are reported as warnings or errors, not silently
   ignored.
2. `scripts/soak.sh`: two explicit loopback stages (`outage-provider`
   with `^TestFailureInjection`, `outage-gameday` with
   `^TestResilienceGameDay$` — the same selectors the smoke script
   runs), a `failure_summary` per stage in JSON (empty for PASS) and
   a Failure summaries section in Markdown, a `resource_sample_summary`
   (peak goroutines/FDs/heap over the hot-reload JSONL samples) in
   both JSON and Markdown, and an explicit `not_run` block (live
   items #646–#652 BLOCKED, production failover / SLOs / live
   game-day NOT_RUN).
3. `.github/workflows/soak.yml`: a `Validate soak evidence
   completeness` step (`if: always()`, after the soak step) that finds
   the newest `summary.json` and runs the checker against
   `github.sha`. A missing summary, wrong commit, skipped stage,
   short sustained duration, missing samples, or missing `not_run`
   block now fails the run instead of leaving a badge to be quoted.
4. `docs/reports/ISSUE664_SOAK_PROCEDURE.md`: the validator contract
   (including how to validate a downloaded artifact, whose recorded
   sample path is absolute on the producing runner) and the updated
   load/report rows.

## Evidence (local, bounded 0s shape — not a long soak)

- Code base: main `808ebde260cc0deae44414d8895b5f1e4118b9df`
  (still the latest main at push time; branch
  `supervisor/issue664-soak-evidence-20261029`).
- Command (with the repository's known `/tmp` workaround,
  `TMPDIR=$HOME/workspace/.tmp GOTMPDIR=$HOME/workspace/.tmp`):

```sh
NEXAROUTE_SOAK_ROUNDS=1 NEXAROUTE_SOAK_DURATION=0s \
  NEXAROUTE_SOAK_LABEL=local-20261029-664validator \
  NEXAROUTE_SOAK_OUT=.artifacts/soak ./scripts/soak.sh
```

- Result: **SOAK PASS** (exit 0), started 2026-10-29T15:31:36Z,
  finished 2026-10-29T15:47:28Z, total 952s, sustained duration 0s
  (fixed-iteration shape), stress rounds 1.
- Environment: Go 1.27.0 linux/amd64, Linux htch-runtime
  (Ubuntu 24.04.5 LTS host line in the summary), 2 CPUs
  (AMD EPYC 9D25), GOMAXPROCS auto.
- Resource samples (hot-reload stage): samples=125,
  peak_goroutines=119, peak_open_fds=62,
  peak_heap_alloc_bytes=15166896. Per-stage peak RSS is `n/a` on
  this host because no GNU time binary is installed
  (`/usr/bin/time` absent); the JSONL samples above are the
  resource evidence, not RSS.
- Tree note, stated honestly: at run start the worktree carried 3
  porcelain entries (the validator, `soak.sh`, `soak.yml`). The
  procedure-doc edit landed during the run and does not affect any
  stage's behavior; the PR head (including that doc) is covered by
  CI on the PR. The summary's own `worktree_entries: 3` is the
  run-time count, not a claim of a clean tree.

### Stages

| Stage | Status | Duration |
|---|---|---|
| stress-round-1 | PASS | 278s |
| http-hot-reload-soak | PASS | 67s |
| recovery-soak | PASS | 12s |
| video-recovery-soak | PASS | 24s |
| outage-provider | PASS | 30s |
| outage-gameday | PASS | 19s |
| race-soak | PASS | 491s |
| budget-leak-slope | PASS | 30s |

Durations are wall times on a shared 2-vCPU VM with sibling sessions
active (stress-round-1 includes the first compile), not latency
assertions: no stage asserts a wall-clock threshold.

### Recovery outcomes (from this run only)

- Flapping upstream: recovered after 758.912017ms
  (`TestSoakFlappingUpstreamFailureSemantics`).
- Video ledger sustained: 5 rounds, 500 jobs, spent 1772,
  500 entries, errors 0.
- Video queue restart rounds: 5 rounds, 100 jobs, errors 0,
  file bytes per round 10497 → 52321 (linear, accounted).
- Budget leak slopes: all +0.000000, within limits (goroutines
  ±0.500000, budget_rows/open ±0.050000, budget_reserved ±0.000000,
  cache entries ±0.500000).
- Failure summaries: none — every stage passed, so the new section
  correctly reports none. Its behavior on failure is covered by
  the validator fixtures below, not by a staged failure here.

### Validator results

- New local summary: `python3 scripts/check-soak-evidence.py
  --summary .artifacts/soak/local-20261029-664validator/summary.json
  --kind soak --expected-commit 808ebde...` → **VALID**, exit 0
  (8 stages, total 952s, result PASS).
- Synthetic fixtures (under `~/workspace/.tmp/validator-fixtures`,
  not committed): good summary → exit 0; wrong commit, sustained
  stage shorter than requested (100s vs 7200s), SKIPPED race stage
  with result PASS, and a failed stage with an empty
  failure_summary → each exit 1 with the specific error named.
- Prior 2h run's summary (run 40863869045, commit `c2ca13c`,
  downloaded artifact, old schema): core checks pass (commit,
  total 7407s ≥ requested 7200s, all 8 stages PASS, no skipped
  stage), but the checker reports it INVALID on exactly one
  current-schema field — the missing `not_run` block — plus
  warnings for the missing `failure_summary` keys and
  `resource_sample_summary`. That is a schema gap in the old
  summary, not a failure of that run, and it is why the new
  fields exist. The old run's own evidence comment on #664/#653
  stands as written; this checker does not retroactively rewrite it.
- Secret-shape scan of the new summary files (Markdown + JSON):
  0 matches for `ghp_…`, `AKIA…`, or PEM private-key markers.
  The harness uses loopback fixtures only, as expected.

## NOT RUN / BLOCKED (explicit, not claimed)

- Long-form multi-hour soak on the current head (`808ebde`):
  NOT RUN. This local run used `NEXAROUTE_SOAK_DURATION=0s`.
  The 2h soak on the previous head `bb5865f` (run 40891002045,
  workflow_dispatch, rounds 3, duration 2h, timeout 150m) was
  still **in_progress** at the last check in this work
  (2026-10-29T15:50:44Z); its outcome, artifact, and stage table
  belong in a separate comment on #664/#653 once it completes,
  and must not be merged into this report's numbers.
- Long-form soak on any head is not part of normal PR CI by design;
  it is opt-in via the Soak workflow on a suitable runner.
- Live provider canaries (#646), live OIDC (#647), Vault KV/Transit
  (#648/#649), live Kubernetes (#650), real ComfyUI (#651),
  multi-host HA + RPO/RTO drill (#652): BLOCKED/NOT RUN. They need
  explicit operator authorization and their own runbooks. No
  RPO/RTO value is reported here or anywhere in this change.
- Real PostgreSQL/Redis outage drills: NOT RUN here (they need
  disposable service containers). They run in the CI integration
  jobs against ephemeral containers and are referenced, not
  duplicated, by `soak.sh`.
- PostgreSQL-backed video ledger/queue stores: NOT RUN here (their
  suites skip without a DSN; no PG server was provisioned for this
  run).
- Full `scripts/ci-soak-smoke.sh` locally and full
  `scripts/verify.sh` locally: NOT run on this branch. This change
  touches scripts, one workflow file, and docs only (no Go code),
  and its wider gates are the PR's CI jobs (including
  `ci-soak-smoke` and `verify`). A scripts/docs-only change does
  not justify re-running the full local verification suite, and
  claiming otherwise would be fabrication.
- Process (binary) kill/restart: NOT a soak stage. Store-reopen
  restart rounds and probe recovery cycles are in the soak stages
  above; gateway SIGTERM/SIGKILL restart tests live in the
  `cmd/gateway` and `internal/controlplane` suites (verify scope)
  and are referenced here, not duplicated into `soak.sh`. No
  live binary kill/restart drill was run in this work.

## Reproduce

```sh
./scripts/soak.sh            # bounded 0s shape locally (rounds/duration via env)
python3 scripts/check-soak-evidence.py --summary <path>/summary.json --kind soak --expected-commit <SHA>
./scripts/ci-soak-smoke.sh   # bounded CI smoke (separate half; see procedure doc)
```

The script writes its sanitized Markdown + JSON report under
`.artifacts/soak/<label>/` on every run, including failed runs
(summary via EXIT trap, stages never reached listed as SKIPPED).
