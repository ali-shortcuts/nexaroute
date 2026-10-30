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
- Network-namespace attempt, 2026-10-28 (recorded in
  `docs/reports/HA_ACCEPTANCE_LOOPBACK_2026-10-28.md`): `unshare -n`
  plus veth creation, veth move into the holder namespace, and
  in-namespace IP configuration all succeed — but every non-loopback
  TCP/ICMP connection is refused with `Operation not permitted`,
  from the host namespace and from inside the new namespace alike
  (a user-namespace variant fails the same way), while loopback TCP
  works. A sandbox with a loopback-only network policy therefore
  cannot carry gateway-to-PostgreSQL traffic across namespaces, and
  the namespace pair is not a multi-host topology there. On a host
  without that restriction, a namespace pair with separate IPs and a
  shared PostgreSQL reachable over the veth network is the
  container-like topology to try — and its isolation must still be
  stated exactly (shared kernel and disk unless proven otherwise).

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
   Regression checks (all against a cold schema, all
   with two independent processes, not two pools in one
   process — a same-process test cannot prove that two
   gateway processes serialize): the control-plane
   cold-migration test, the security-store cold-open
   test, the video job-store cold-open test, and the
   video cost-ledger cold-open test. The migration
   audit behind them: every PostgreSQL schema-creation
   path takes its advisory lock before the first DDL —
   the control-plane migrator holds a session lock for
   the whole apply, and the security, video job, and
   video cost migrations take a transaction lock as
   the first statement of the single migration
   transaction. Advisory locks are per-database, so
   unrelated databases never serialize on each other;
   each component uses a distinct lock key, so
   components do not serialize each other either.
   Redis has no schema step (hashes are created
   implicitly), but its key prefixes are fixed per
   component, so two deployments sharing one Redis
   database would share state: separate deployments
   must use separate Redis URLs / database indexes.
   That is an operator constraint, not a code defect.
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
   Session and audit persistence check: count the
   security session, security audit, and control-plane
   snapshot rows in PostgreSQL before the kill and
   after the restart, and read back one shared record
   through the restarted replica. Row counts must be
   unchanged by the replica's death and restart.
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
   clearing local state, and — the regression checks
   for the 2026-10-27 defect and its follow-up class —
   reading an existing video job returns a retryable
   503, never a 404; creating a job during the outage
   returns 503 (never 400 carrying backend detail);
   cancelling a job during the outage returns 503
   (cancelling a genuinely missing job is still 404);
   and opening a job asset whose file cannot be read
   returns 503 (a genuinely absent file is still 404).
   No response in this scenario may contain SQL, a
   DSN, a host address, or a driver error string.
   Restart the service WITHOUT restarting the
   gateways, record the first accepting-connections
   time, the last sync-error time, and the first
   successful shared-state read on each replica
   (reconnection without a gateway restart is the
   recovery property under test). Count the shared
   rows before and after; committed rows present after
   recovery are the RPO observation (0 lost in the
   2026-10-27 run under a clean stop/restart).
   Also probe health during the outage: `/healthz`,
   `/readyz`, and one established-model chat request
   (bracketed stub hits). In the 2026-10-28 loopback
   run all three stayed 200 during a PostgreSQL outage
   (routing uses the in-memory snapshot with
   `config_failure: last_known_good`), so a 200 chat
   during an outage does NOT imply shared state is
   healthy — the video-store 503s and the breaker-sync
   errors are the outage signal for shared state.
   Scope that claim to established models only; a
   deployment needing a fresh probe during an outage
   was not tested.
7. **Rolling upgrade.** With both replicas healthy on
   the old binary and sharing one database, replace
   one replica with the new binary (stop old, start
   new, same data directory and configuration) while
   the other replica keeps serving. Then replace the
   second replica the same way. After each replacement
   record: the new replica's `/healthz`, one shared
   record read back through it, and the shared row
   counts. No migration error, no duplicated job or
   reservation row, and no dropped committed row is
   allowed. If the old and new binaries disagree on a
   schema version, the upgrade is blocked until the
   migration story for that change is written down —
   never force it by hand-editing the migration table.
8. **Backup and restore.** Run `nexaroute backup` and a
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
9. **Local-only state.** Diff the two config files and
   confirm they differ only in per-instance fields.
   If an admin-edit propagation test is run, it needs
   an admin session; if it is not run, say so. If OIDC
   is disabled in the staging configs, OIDC
   pending-transaction behaviour is NOT_RUN, not proven
   by documentation alone.
10. **Soak (bounded, repeatable).** Run a fixed pattern and
    record its duration and failures exactly; do not
    relabel a short soak as a long soak. Pattern used
    2026-10-28: 45 rounds, one round every 20s (15
    minutes), chat alternating replicas, `/healthz` on
    both every round, plus one video job at rounds 0,
    15, 30; bracket the stub hit counter at start and
    end. Report rounds, chat successes/failures, health
    failures, video jobs completed/failed, and the
    final row counts. A multi-hour soak on a real
    multi-host topology is still open.

## 3. Executable harness

`scripts/ha-two-host-acceptance.py` packages the checks above so a run is
repeatable and its evidence is machine-readable. It never calls production.

- `--self-test` is infrastructure-free and exercises authorization parsing,
  redaction, RPO/RTO aggregation, config diffing, evidence validation, and the
  refusal to label a container run as multi-host.
- `--containers` builds a static gateway, starts two gateway containers plus
  disposable PostgreSQL and Redis on one Docker host, and runs S1–S8. Each
  replica gets a loopback stub because the egress policy intentionally rejects
  plain HTTP to a non-loopback host. This is
  `container-isolated-single-host` evidence: it cannot satisfy the multi-host
  gate, and its report always records `two_independent_hosts: NOT_MEASURED`.
- `--two-host` runs the same drill over SSH with a shared PostgreSQL supplied
  by the owner. It requires `--authorization`, `--host-a`, `--host-b`, the
  `NEXAROUTE_HA_PG_DSN` and `NEXAROUTE_HA_RESTORE_DSN` environment variables,
  and the owner-provided `NEXAROUTE_HA_PG_STOP_CMD` /
  `NEXAROUTE_HA_PG_START_CMD` commands. Missing inputs are reported as exit 3,
  not converted into a partial pass.
- Evidence is written under the requested in-repo directory as
  `evidence.json` (schema `nexaroute-ha-acceptance/1`) and `report.md`. The
  authorization document must name issue #652, the host list, the disposable
  staging scope, an expiry, a cost ceiling, and `production_access: false`.
  The harness rejects wrong-issue, expired, production, and out-of-scope
  authorizations.

The container mode is appropriate for local regression checks. Only
`--two-host` can support the multi-host claim in section 0, and even then the
report must state exactly which host, load-balancer, and failure scenarios
were exercised.

## 4. Write the evidence report

The report must contain, at minimum: exact commit and
versions, topology with the multi-host gate outcome,
every scenario's timestamps and observed outputs,
deviations (including operator errors and interrupted
steps, with their causes), the timing methodology and
its limitations, and a BLOCKED / NOT_RUN list. Sanitise
it: no credentials, tokens, DSNs, or private configuration.
A skipped suite is never a passed suite, and a
same-host result is never a multi-host claim.
