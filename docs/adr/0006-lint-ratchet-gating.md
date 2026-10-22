# ADR 0006: Static-analysis CI — report-only or ratchet gating

- Status: **Proposed — decision required from the repository owner.**
  The placeholder until the decision is today's behavior (report-only).
- Date: 2026-10-22
- Parent audit: #593; related: the audit workflow's seven report-only
  jobs (landed under the #593 program)

## Context

`audit.yml` runs golangci-lint, staticcheck, gosec, deadcode, coverage,
extended fuzz and repo metrics as **report-only** jobs on manual
dispatch. Nothing blocks a merge when a finding appears; the jobs
produce artifacts a human (or the supervising agent) must read.

The ground has moved since that design was chosen. On current main
(baseline verified 2026-10-22, then re-verified after the triage
changes):

- **golangci-lint (pinned v2.15.0, uncapped): 0 issues.** A full triage
  burned the count from 69 real findings to zero without disabling any
  rule (one scoped, documented test-only exclusion).
- **staticcheck (pinned 2026.2.2): 0 issues.**
- **deadcode (-test): 0 unreachable functions** (the last four were
  removed after caller verification).
- **gosec: 53 findings, all G304**, every one previously triaged as a
  bounded operator-path open; not encoded as a baseline anywhere a
  gate could diff against.
- **Coverage: 84.1% repo-wide** against a documented target of ≥85% —
  below target today, so an immediate coverage gate would fail on main
  itself.

So three of the five signals are at zero and could be gated today with
no baseline mechanism at all (zero is the baseline). The other two
cannot be gated honestly yet: gosec needs an accepted-findings
baseline, and coverage needs the ≥85% target actually reached first.

## Options

### Option A — Ratchet-gate what is at zero; keep the rest report-only until ready

Make golangci-lint, staticcheck and deadcode **blocking** checks (any
finding fails the job; zero-baseline means no exception file to game).
Keep gosec and coverage report-only with their exit criteria written
down: gosec becomes gateable when its 53 triaged G304 findings are
encoded in a checked-in baseline that CI diffs (new findings fail);
coverage becomes gateable at ≥85% once main actually reaches it.

- Pro: locks in the zero states while they are cheap to hold; a new
  lint/static/deadcode finding is caught at the PR that introduces it
  instead of at the next audit. The ratchet only ever tightens.
- Con: contributors occasionally eat a red job for a stylistic finding
  until fixed (mitigated: the fix is usually minutes, and `--fix`
  covers most classes); audit workflow needs a required-check change.

### Option B — Keep everything report-only (status quo)

- Pro: no friction, no workflow changes; the supervising agent and
  audits keep consuming the artifacts.
- Con: the zero states decay silently — exactly how the repository
  accumulated 69 lint findings and 25 dead functions between audits.
  Report-only signals depend on someone remembering to look.

### Option C — Gate everything immediately

- Pro: maximal strictness.
- Con: dishonest today: the coverage gate fails on main (84.1% < 85%)
  and gosec has no baseline, so the gate either goes red permanently
  (training everyone to ignore it) or forces rushed, low-quality
  compliance work. Not recommended.

## Recommendation

Option A. It converts three already-achieved zero states into durable
invariants at near-zero ongoing cost, and names objective exit
criteria for the remaining two instead of gating on hope.

## Exact decision required from the owner

**Adopt ratchet gating for golangci-lint, staticcheck and deadcode
(Option A), or keep the audit workflow fully report-only (Option B)?**
If Option A: also confirm the gosec baseline approach (checked-in
accepted-findings file, diffed in CI) as the path to gating gosec.
