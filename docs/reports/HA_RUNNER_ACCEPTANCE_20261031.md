# HA runner acceptance, issue 652 — status: NOT_RUN

This document describes a runner workflow, not a result. Nothing in
it is HA acceptance evidence yet. The run itself is NOT_RUN until
an owner merges the workflow PR (separate approval), dispatches
it on GitHub Actions, and sends the run URL and the artifact
back. Only then can this document be completed from the observed
result, and only within the limits stated here.

## Why a runner workflow

This environment has no second host, and a same-host
multi-process run must never be labelled multi-host. The agreed
path is therefore: a workflow in a PR, run by the owner on a
GitHub-hosted runner (which has Docker), result sent back, and
the report completed from that result. The workflow reuses the
shipped harness `scripts/ha-two-host-acceptance.py` in
`--containers` mode; it does not invent a new drill.

## What the workflow does

On a GitHub-hosted runner, `ubuntu-24.04`, `workflow_dispatch`
only, `contents: read`:

1. Harness self-test (`--self-test`). Deterministic,
   infrastructure-free checks of the harness itself. This is
   NOT HA evidence.
2. Container drill (`--containers --evidence-dir
   ha-runner-reports --repetitions 3`). The harness builds the
   gateway binary, starts two independent gateway containers
   (separate configs, data dirs, asset roots) on one Docker
   host against a shared PostgreSQL container, and runs
   scenarios S1–S9:
   - S1 concurrent cold start against an empty shared database
   - S2 externally observable health and chat on both replicas
   - S3 replica SIGKILL, survivor serving, restart (3 repetitions)
   - S4 concurrent video jobs, single claim, exactly-once
     ledger settlement (duplicate lease owners and duplicate
     settlement rows must both be 0)
   - S5 shared PostgreSQL outage and recovery without gateway
     restart (existing job reads 503 during the outage, no
     backend detail leaked, row counts unchanged after
     recovery; RPO measured from before/after counts)
   - S6 application backup plus database dump restored into a
     fresh database
   - S7 local-only config and asset boundaries
   - S8 bounded soak (60 seconds by the harness default, NOT a
     multi-hour soak)
   - S9 breaker-sync convergence (tripped A, B fails fast
     without an upstream call)
3. Evidence validation: the workflow checks that
   `topology.class` is `container-isolated-single-host`, that
   the container verdict is PASS, that
   `two_independent_hosts` is `NOT_MEASURED`, that
   `shared_redis` is `present_but_not_a_configured_backend`,
   that every scenario is PASS, and that `failures` is empty.
4. Redis backend smoke, single process only: the existing
   integration test `TestControlPlaneRedisIntegration` (the
   function is `TestControlPlaneRedisIntegrationRuntimeLifecycle`)
   against a disposable Redis service container, with
   `NEXAROUTE_TEST_REDIS_URL=redis://127.0.0.1:6379/0`. This
   is the same test and the same Redis image
   (`redis:7.4.2-alpine`) as the existing integration-redis
   workflow.
5. Upload, with `if: always()`, of exactly two files:
   `ha-runner-reports/evidence.json` and
   `ha-runner-reports/report.md`, artifact name
   `ha-runner-reports-<sha>`, retention 90 days.

## What the run does NOT prove

- NOT two independent hosts. The verdict for two independent
  hosts stays NOT_MEASURED, with an explicit blocker in the
  evidence. A PASS here must never be cited as multi-host
  acceptance.
- NOT a load balancer. No load balancer is in the topology.
- NOT a Redis outage or Redis failover. A Redis container is
  present in the drill, but no gateway backend in the drill
  points at Redis. The separate Redis step proves only that a
  single gateway process can use a real Redis control-plane
  backend.
- NOT a multi-hour soak. S8 is a bounded 60-second soak.
- NOT production, NOT measured production RPO/RTO. RPO/RTO
  values in the evidence are measured only for the drilled
  faults on this runner topology, and they bind to that run
  and that commit only.
- NOT a duplicate video-job claim fix. The drill measures
  that no duplicate claim or settlement occurred in its own
  runs; it does not prove exactly-once behaviour in general.

After a successful run, issue 652 stays open. The
two-independent-hosts, load-balancer, production, and
multi-hour-soak steps remain BLOCKED until an operator runs
them on a real topology and records them.

## How the owner runs it

1. Merge the workflow PR (separate owner approval; opening
   the PR is not a merge, and this document is not a merge
   request).
2. GitHub Actions → "HA runner acceptance" → Run workflow.
3. Download the `ha-runner-reports-<sha>` artifact.

## What to send back

- The run URL.
- The artifact, or at least `evidence.json` and `report.md`.
- The job-log excerpt with the PASS/FAIL lines, and the
  Redis smoke test output.

The summary alone is not enough to complete anything. On
failure, send the FAIL line and the step log, and do NOT
silently re-run, because a re-run overwrites the first
failure's evidence. If the drill fails, the failure is the
result, not a reason to weaken a check.

## Safety

No real secret is used. The harness generates disposable
admin/video tokens inside the run and writes them into
per-replica config files under its evidence directory. That
directory is therefore never uploaded whole; only the two
redacted files listed above are uploaded. The harness
redacts URIs before logging or serialising evidence, and the
workflow reads no repository secret.
