# Issue #664 — CI smoke vs long-form soak procedure

This document is the separation contract between the two halves of
soak evidence. Short CI tests never prove long-duration stability, and
a long soak is never part of a normal PR. Both halves are recorded
with the same status vocabulary, and neither half authorizes live
acceptance.

## The two halves

| | Bounded CI smoke | Long-form soak |
|---|---|---|
| Script | `scripts/ci-soak-smoke.sh` | `scripts/soak.sh` |
| CI wiring | `.github/workflows/ci.yml`, job `ci-soak-smoke` (runs on pull requests and pushes to main, in parallel with `verify`) | `.github/workflows/soak.yml` (workflow_dispatch for long runs; push to main on soak-file changes runs the short 0s shape only) |
| Duration | Fixed-iteration soak shape; sustained durations above 5m are rejected by the smoke script | Operator-chosen sustained duration, script cap 6h (see the timeout rule below for the workflow-feasible maximum) |
| Load | Stress selectors identical to `scripts/stress.sh`, loopback outage/cancellation tests, short race soak | Repeated stress rounds (`NEXAROUTE_SOAK_ROUNDS`, default 3, max 20) plus the same soak tests, a race soak stage with a pinned short sustained duration, and a budget-leak slope stage |
| Report | Sanitized Markdown + JSON under `.artifacts/ci-soak-smoke/<label>/`, uploaded as a CI artifact (90 days) | Sanitized Markdown + JSON under `.artifacts/soak/<label>/`, uploaded as a workflow artifact (90 days) |
| Status today | PASS locally; see `docs/reports/ISSUE664_CI_SMOKE_2026-10-29.md` | NOT RUN (multi-hour). The bounded 0s shape was exercised in `docs/reports/issue664-stress-soak-evidence.md`; that is not a long soak. |

## Running the bounded CI smoke

```sh
./scripts/ci-soak-smoke.sh
```

Knobs (all validated and recorded in the report):
`NEXAROUTE_CI_SMOKE_OUT`, `NEXAROUTE_CI_SMOKE_LABEL`,
`NEXAROUTE_CI_SMOKE_STAGE_TIMEOUT` (default 900s, range 60–3600),
`NEXAROUTE_CI_SMOKE_SOAK_DURATION` (default 0s, maximum 5m),
`NEXAROUTE_CI_SMOKE_RACE` (1 by default; 0 records the race stage as
SKIPPED instead of running it).

Exit codes: 0 PASS, 1 a stage failed or timed out, 2 invalid
configuration, 3 environment BLOCKED (for example no Go toolchain),
130 cancelled. A report is written in all five cases. A failed stage
does not stop the run: the remaining stages still run so the report is
complete, and stages that never ran are listed as SKIPPED (or BLOCKED
when configuration blocked the run).

## Running the long-form soak (opt-in only)

1. Choose a suitable runner. A long soak must not run on a normal PR
   runner slot: dispatch the Soak workflow by hand from the Actions
   tab, on a runner that can stay up for the whole run. GitHub-hosted
   jobs are capped at 360 minutes.
2. Pick the inputs:
   - `rounds`: repeated stress rounds before the soak (default 3).
   - `duration`: sustained mixed-workload duration, for example `30m`
     or `2h` (`0s` disables the sustained phase).
   - `timeout_minutes`: the job timeout. It must exceed the sustained
     duration by at least 30 minutes to cover the other stages. The
     workflow now fails fast before starting if it does not.
3. Because of the 360-minute job cap and the 30-minute overhead rule,
   the largest sustained duration dispatchable through this workflow
   is 330 minutes (5h30m). `scripts/soak.sh` itself accepts up to 6h,
   but only for runs outside this workflow (for example a manual run
   on a dedicated host); a 6h dispatch through the workflow is
   rejected by the guard step with a clear message, not killed
   mid-soak.
4. Read the report, not just the job result. `scripts/soak.sh` records
   every planned stage: stages that ran carry PASS/FAIL with exit code,
   duration, and peak RSS; stages the run never reached are listed as
   SKIPPED. A run that reports SKIPPED stages is an incomplete run,
   even if the stages that did run passed.
5. Attach the report (Markdown + JSON, plus per-stage logs) to the
   issue it evidences. Do not summarize a long soak as PASS from the
   job badge alone.

## What neither half does

- No paid provider call. Every upstream in both halves is a loopback
  `httptest` server or a fixture. Live provider canaries (#646) have
  their own harness and authorization gate and are never triggered
  from here.
- No production-cluster mutation. The Kubernetes, Vault, OIDC, and
  multi-host items (#647–#652) are separate, operator-authorized
  acceptance tracks with their own runbooks. Nothing in these scripts
  or workflows contacts a production cluster, a real Vault, a real
  OIDC tenant, or a real ComfyUI instance.
- No credentials. The scripts never read, print, or record secrets;
  reports carry only the commit, Go version, host/container identity,
  durations, load parameters, resource samples, and failure summaries.
- No RPO/RTO. Recovery objectives are not measured by either half and
  must not be quoted from these reports. The multi-host HA + RPO/RTO
  drill (#652) is a separate exercise.
- No real-service outage drill. The PostgreSQL/Redis outage drills run
  in the CI integration jobs against disposable service containers;
  they are referenced here, not duplicated.

## Reading a status

- PASS: every stage that ran passed, and no planned stage was skipped
  for a reason that matters to the claim being made.
- FAIL: at least one stage failed or timed out; the report's failure
  summaries name the failing tests. Fix the failure, do not re-label it.
- SKIPPED: a planned stage did not run (operator knob, cancellation,
  or an earlier abort). A SKIPPED stage is not evidence for anything.
- BLOCKED: the run could not start or a stage could not run for an
  environmental reason (missing toolchain, invalid configuration).
  BLOCKED is not PASS and not FAIL; it names what is missing.
- NOT RUN: the check was never executed at all. The multi-hour soak
  and the live items (#646–#652) are NOT RUN until an operator
  actually runs them in a representative environment with evidence.
  Fixtures, mocks, and green CI are not a substitute for them.
