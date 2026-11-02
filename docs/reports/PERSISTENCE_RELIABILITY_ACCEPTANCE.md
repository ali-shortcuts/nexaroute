# Persistence reliability acceptance — 2026-11-02

**Overall: BLOCKED.** These are targeted local engineering checks, not final release acceptance or independent review. No external infrastructure was accessed. Tested source corresponds to local commit `19456135f22bc8f2c57f05b3ca152766357249a7`; tests were run on its identical working-tree contents immediately before that commit. Remote PR #824 remains at `05a3e966965dae5147f5f0efd22089e82034ff78` and its required verify check failed; none of these results changes that status.

## Environment and execution

Disposable local PostgreSQL 16.15 on a Unix socket, Redis 7.4.7 on a Unix socket, Go 1.27.3, Linux, race detector enabled. Tests used `/tmp/nr-deps/local.mod` with local dependency replacements, `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off`; this is **not** locked-dependency CI. PostgreSQL DSN and Redis URL pointed only at the disposable services. No power-loss, remote TLS, failover, multi-host or cloud-provider test was performed.

- NexaFlow: 20 race repetitions of `TestReconciliationIdempotencyConcurrentDuplicate`, `TestConcurrentReplayWaitsForInitialization`, `TestPostgresConcurrentFirstApproval`, and `TestPostgresConcurrentConflictingFirstApprovals`: PASS (13.063s). This does not substitute for the complete exact-head CI suite.
- Real PostgreSQL `TestAppendIdempotencyPostgres`, `TestPostgresFinalInitializationLostReplyReplay`, `TestPostgresNetworkDropsCommitAcknowledgement`, and subprocess `TestFileStoreProcessKillRecovery` before/after rename: PASS under race detector. Process termination cannot establish power-loss durability.
- Video queue PostgreSQL concurrent cold open, duplicate create/job IDs, claims and expiry, restart recovery, terminal guards and replay, idempotency updates; local FileStore fault matrix: PASS under race detector.
- Cost ledger PostgreSQL concurrent reserve, duplicate/conflicting settlement, estimate-to-actual and release, restart, invalid amounts and store errors: PASS under race detector. Fake-ledger tests are separate from the real database cases.
- Control-plane PostgreSQL and Redis store conformance, two-instance/restart or corruption tests, concurrent initial writes, and file atomic-write/corruption tests: PASS under race detector. Shared-state PostgreSQL/Redis and security-store PostgreSQL/Redis integration tests: PASS under race detector. Verbose output confirmed these named tests ran rather than being skipped.

## Material unresolved risks

- NexaFlow operation IDs are scoped to run/node. Empty IDs remain non-idempotent; reusing an ID with a different payload is not checked, and mixed-version writers have not been validated. Initialization spans multiple writes; bounded waiting fails closed after two seconds. Selected local acknowledgement-loss tests do not prove all remote network/failover behavior.
- Video FileStore is single-instance/process only. Reopening after a process kill is not a power-cut test. Redis/video leases and outage/reconciliation require broader realistic, multi-process and infrastructure failure acceptance.
- Legacy cost `ReleaseForJob` has a void/log-only failure path; the orchestrator does not propagate it, and aggregate reads can degrade to zero on store errors. Full reconciliation and failure propagation remain unresolved.
- No exact-head required GitHub CI, exact-final coverage, independent review, genuine provider/HA acceptance or two-hour final-RC soak has been obtained. No release authorization has been given.

## Coverage gate follow-up

`scripts/test-coverage-gate.sh` passed on the tested tree, including 94.95% and 94.99% FAIL, 95.00% and 100% PASS, and a threshold infinitesimally above 95% FAIL. The latter failed before the arbitrary-precision integer comparison fix. A profile containing a large out-of-function block also failed after a rounded-total consistency guard was added; before the guard it incorrectly passed. A genuine local `internal/authz` profile also passed with a zero threshold. The guard detects discrepancies beyond the Go tool's one-decimal rounding interval, but cannot prove profile authenticity or detect every smaller out-of-function discrepancy. Generated coverage profiles must be trusted; this is not a fresh whole-repository coverage measurement.

## 2026-11-02 follow-up: PostgreSQL prune race

Tested code commit `ce7eb49eb9c36c2f7cb6e2c4c493f79c7a3605cc`. A deterministic real-PostgreSQL regression held a node row lock while another run became terminal. Before the fix, prune returned 2 instead of the one initially eligible run, creating a risk of orphaning its node; after the fix, 10 race-detector repetitions passed. Prune now selects and locks the initial eligible run IDs in deterministic order and uses that same set for all child and parent deletions. The full `internal/nexaflow` race suite with disposable PostgreSQL passed (25.730s) and package vet passed, using the same temporary modfile caveat as above. CI is configured locally to require the new test without skipping; this configuration has not been pushed or run remotely.

This fix is not a blanket orphan-prevention guarantee: approval creation currently does not require or lock a parent run, and old/mixed-version writers or out-of-band SQL may not share the locking protocol. Approval behavior is an existing cross-store contract and needs a separately reviewed compatibility decision. Prune commit acknowledgement loss still makes the returned deletion count uncertain; reconcile stored state rather than assuming a retry recovers the original count.

The canonical `./scripts/verify.sh` was attempted at `ce7eb49` with `GOPROXY=off`, because the ordinary module proxy is inaccessible here. Initial shell, release-gate, documentation and harness checks passed, but multiple packages reported setup failures for unavailable locked modules; the command timed out at 120 seconds. It is **not a completed or passing canonical verification**.

## Required next steps

Run pristine locked-dependency full CI and the required persistence/network fault tests on the actual remote candidate, verify no skips, obtain independent review, and address the unresolved risks with appropriate nonproduction infrastructure. The GitHub App needs authorized Contents and Workflows write access to fast-forward #824 with workflow changes; Actions write is needed if manual dispatch is required. Do not bypass access controls. Release remains BLOCKED.

## Recovered workspace and follow-up — 2026-11-02 13:xx UTC

The environment was re-created with HEAD at main `436c4e98618eed0c41e59059eae7eca5f05dd190`; the previously reported local commit objects `ce7eb49`, `39e8ba5026ade8c922ded66483e890195689ffae`, and `17d7329ee7bcd7fd56419e3efd55d84254a3d7ee` were not in this Git object database or reflog. Their corresponding file changes were present in the working tree, not verifiable as those original commit objects. Preserved tracked patch and untracked archive under `/home/user/nexaroute-work-preservation/2026-11-02-latest/`, committed the snapshot as `2aa28d29ca3ceff30d438a3aa89a3f2565984451`, and reconciled the actual #824 head `05a3e966965dae5147f5f0efd22089e82034ff78` as an ancestor in merge `178c29d3f5ddbd20fecd470ba86dacf007cbad3f`. This is a reconstruction, not restoration of the lost object IDs. No push or PR merge occurred.

Code commit `e36d17d` rejects null run, node and approval entries on file open, leaves the corrupt file untouched, and releases the lock; red regression rejected all three prior behaviors and passed after the fix. Commit `265468b19762657a89a50e04ab8ddad59281846c` reports directory close errors after rename as uncertain and fences the instance until reopened. The injected directory-close test failed before the fix and passed with all prior file fault stages after it. This conservative response is not a power-loss guarantee. Commit `10dc6ba` restores the existing unused-decoder scanner regression in addition to the intermediate-decoder test.

On exact local code SHA `265468b19762657a89a50e04ab8ddad59281846c`, Go 1.27.3 and `-race` with temporary codeload dependency replacements (not locked-dependency CI), disposable PostgreSQL 16.15 and Redis 7.4.7:

- Full NexaFlow package: PASS, 26.759s; verbose log confirmed prune race, actual COMMIT-ack loss, duplicate append, StartRun regression, null records, directory failure matrix and process-kill recovery ran. Some other environment-dependent tests may skip; this is not full repository coverage.
- Targeted PostgreSQL/Redis control-plane, shared-state and security-store integration tests: PASS, actual named cases confirmed in verbose output.
- Video PostgreSQL claims, expired lease, duplicate, terminal and restart cases, video FileStore fault matrix, and PostgreSQL cost-ledger targeted cases: PASS; fake-ledger tests are separate from real DB tests.
- NexaFlow vet: PASS under the same temporary replacements. No final locked-module canonical CI, exact-SHA coverage, independent review, power-loss or remote HA evidence is claimed.

On prior local code SHA `10dc6ba6fef575511021c15745483888598e58d1`, 20 combined race repetitions of StartRun reconciliation, initialization wait, PostgreSQL approval races and concurrent prune transition passed (17.131s); the standalone standard-library scanner regression, JavaScript activity window/refresh/workflow tests, JS syntax and exact-threshold gate fixtures passed. These results do not transfer to later SHAs. The first attempt at Redis integrations failed setup due to a missing temporary replacement module file; after creating that temporary metadata, the specified real-service tests passed.
