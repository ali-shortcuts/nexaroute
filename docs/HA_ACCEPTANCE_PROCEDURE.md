# HA acceptance procedure (disposable staging)

This procedure reproduces the 2026-10-27 staging run recorded in
`docs/reports/HA_ACCEPTANCE_2026-10-27.md`. It is a procedure,
not a guarantee: every step records its own observations, and
any step that cannot run is reported BLOCKED, never skipped
silently or replaced with a same-host result dressed up as
multi-host evidence.

## 0. Multi-host gate — read first

Before running anything, decide which claim the run can
support and write it at the top of the evidence report:

- Two hosts or containers, a shared PostgreSQL/Redis
  service reachable over the network, and a load balancer
  in front: the run can support a multi-host claim.
- Two processes on one host (even with separate data
  directories and ports): the run supports shared-store
  behaviour across processes only. Physical multi-host
  acceptance stays BLOCKED. Do not close #652 on a
  same-host run.
- No second host, no container runtime, and no network
  namespace (for example `ip netns exec` failing with
  `Operation not permitted`): record the blocker exactly
  and stop at the gate for the multi-host claim. The
  same-host steps below may still run, labelled as such.

Never test against production, never perform destructive
failover, and never destroy persistent data without explicit
operator authorization and a verified recovery plan. All
databases, credentials, and tokens in a staging run are
disposable and test-only; never copy them into a report.

## 1. Prepare a disposable topology

1. Build the gateway from the exact commit under test and
   record the commit, Go version, PostgreSQL version, and
   Redis version.
2. Create disposable PostgreSQL and Redis services. Record
   which backends each gateway actually configures
   (control plane, security store, video job store / cost
   ledger). A service that is running but not configured
   as a backend proves nothing when stopped — record that
   explicitly, as the 2026-10-27 run did for Redis.
3. Create two gateway configurations that differ only in
   per-instance fields: listen address, breaker-sync
   replica id, security-store path, video storage root.
   Enable probing (`probe.enabled`, `probe.on_start`);
   without probing, no deployment becomes usable and
   `/readyz` and chat correctly return 503, which is a
   configuration error in the run, not a gateway defect.
4. Use a loopback stub upstream with a hit counter and
   an `ok`/`fail` switch. Record the counter before and
   after every client request you want to attribute —
   probe traffic also hits the stub, so an unbracketed
   counter proves nothing about client requests.

## 2. Run the scenarios, in order

Record a UTC timestamp at every step boundary.

1. **Concurrent cold start.** Launch both gateways
   simultaneously against an empty database. Both must
   become healthy. If one fails with a migration error,
   that is a defect, not flakiness: record the exact
   error and do not retry it away without a fix.
   Regression check: the cold-migration integration test
   must pass against a cold schema.
2. **Baseline.** Chat completion through each replica
   (200), `/healthz` 200 on both, `/readyz` 200 on both,
   `/api/hello` deployment reports compared field by
   field. Remember `/healthz` is liveness and `/readyz`
   is readiness; a healthy-but-unready replica is a
   correct state, not a failure.
3. **Breaker-sync convergence.** Switch the stub to
   `fail`, trip replica A past its failure threshold,
   wait more than two sync intervals, then send one
   client request through replica B and bracket the stub
   hit counter immediately before and after it. B must
   fail fast (503) with zero new stub hits from that
   request.
4. **Replica kill and restart.** SIGKILL one replica
   while the other serves. Record the kill time, the
   surviving replica's first successful `/healthz`, the
   restart command time, and the restarted replica's
   first successful `/healthz`. That interval is the
   health RTO for this run; load-balancer detection is
   not included unless a load balancer is in the path.
   A restarted replica whose startup probe passes and
   then returns to 503 under breaker sync has not failed
   to restart — it has converged back to the tripped
   state, which is the documented behaviour.
5. **Video job and assets.** Create a job through one
   replica, read it back through the other (identical
   state, lease owner, and job id), count the job rows
   and cost reservation rows in PostgreSQL (exactly one
   each for one job), and download its asset through
   both replicas. A 200 on the creating replica and a
   404 on the other is the documented per-instance asset
   behaviour, not a defect.
6. **Backend outage and recovery.** Stop the shared
   PostgreSQL service (not a proxy, not a mock). Record:
   gateways stay alive, `/healthz` stays 200, breaker
   sync logs loud errors every sync interval without
   clearing local state, and — the regression check for
   the 2026-10-27 defect — reading an existing video job
   returns a retryable 503, never a 404. Restart the
   service, record the first accepting-connections time,
   the last sync-error time, and the first successful
   shared-state read on each replica. Count the shared
   rows before and after; committed rows present after
   recovery are the RPO observation (0 lost in the
   2026-10-27 run under a clean stop/restart).
7. **Backup and restore.** Run `nexaroute backup` and a
   database dump separately. Verify the bundle with
   `restore --verify` and read its status honestly:
   for a PostgreSQL-backed installation the status is
   BLOCKED on external-store ownership by design, and
   the bundle alone is not a restore. Restore the
   database dump into a fresh database, start a gateway
   against it, and read back the job and row counts.
   Recovery requires both artifacts; record the dump
   interval as the shared-state RPO bound and the
   restore-plus-verify-plus-start sequence as the RTO
   procedure. Do not quote a single RTO number unless
   every step's duration was timed in that run.
8. **Local-only state.** Diff the two config files and
   confirm they differ only in per-instance fields.
   If an admin-edit propagation test is run, it needs
   an admin session; if it is not run, say so. If OIDC
   is disabled in the staging configs, OIDC
   pending-transaction behaviour is NOT_RUN, not proven
   by documentation alone.

## 3. Write the evidence report

The report must contain, at minimum: exact commit and
versions, topology with the multi-host gate outcome,
every scenario's timestamps and observed outputs,
deviations (including operator errors and interrupted
steps, with their causes), the timing methodology and
its limitations, and a BLOCKED / NOT_RUN list. Sanitise
it: no credentials, tokens, DSNs, or private configuration.
A skipped suite is never a passed suite, and a
same-host result is never a multi-host claim.
