# NexaRoute v0.25.0 — review and readiness status

**Decision: BLOCKED.** Snapshot 2026-11-02. This is an engineering handoff, not an independent human review, release approval, comprehensive audit, or authorization to publish.

## Identity and working-tree caution

- Remote main: `436c4e98618eed0c41e59059eae7eca5f05dd190`.
- PR #823 head: `58bacddbc59e8cf9a01bc59620128c93ea2d7e1a` (open).
- PR #824 head: `276625829a58389ee46a7dfe9ba6eb8482e512f2` (open).
- #824 synthetic PR merge: `1f31c3155f9043bd97762d582a635c6d4093e4c1`.
- The restored local branch points to main, not the remote PR head; its working tree initially had 50 modified/untracked paths. Fetching the two PR commit objects without moving any branch showed tracked working-tree differences and all untracked paths match the #824 candidate. This report is an additional untracked file. No reset, checkout, commit or push was performed. Do not derive candidate provenance from `git rev-parse HEAD` in this workspace.

The [updated #824 title and description](https://github.com/ali-shortcuts/nexaroute/pull/824) give the complete 50-path inventory, actual scope, limitations and links; the previous description is retained as historical text. The remote head was not changed. Both PRs had zero formal reviews and zero requested reviewers at this check. The owner must nominate a qualified non-author, and formal reviews must be requested for both PRs. No reviewer decision has been made.

## #823 preservation and separation

The existing [author-side preservation map](https://github.com/ali-shortcuts/nexaroute/blob/276625829a58389ee46a7dfe9ba6eb8482e512f2/docs/reports/PR823_PRESERVATION_MAP.md) maps four #823 commits to #824. Read-only spot checks of the candidate confirm the chart model uses a half-open window (`at < start` or `at >= end` is excluded), separates ok/failed/active/stale/cancelled/other, and has chart and refresh regression tests. Autopilot decoding uses `MaxBytesReader(..., 4096)` and has a body-limit test. The AST-based scanner and red/green tests cover selected direct `json.NewDecoder` and `io.ReadAll` reads, not general aliases or interprocedural flow. The gate fix and historical-evidence mapping are recorded in the map. No missing contribution was established in these spot checks; they do not prove every material contribution preserved or substitute for independent review. #823 is not an ancestor of #824. Leave it open until independent comparison, approved integration into main, and an owner disposition.

NexaFlow conditional publication is a substantial, separable concurrency/persistence change deserving focused independent review. It compares the validated draft hash inside memory/file locks or the PostgreSQL transaction and fails closed for external stores lacking the capability; it does not supply an editor revision precondition. A focused PR could improve reviewability, but splitting this frozen candidate now would change its scope or head and invalidate existing checks. Decide after reviewer/owner assessment; no split was made.

## GitHub workflow evidence

All five runs below report PR head `276625829a58389ee46a7dfe9ba6eb8482e512f2`, and are PR merge-check evidence, not final-main release evidence.

| Workflow/run | Jobs and result | Artifacts |
|---|---|---|
| [CI 41376300375](https://github.com/ali-shortcuts/nexaroute/actions/runs/41376300375) | verify `125057156454`, PostgreSQL `125057156196`, Redis `125057156370`, container `125057156589`, fuzz smoke `125057156403`, soak smoke `125057156357`: success | browser `13111872296`, smoke `13111332592`, Docker `13111681355`, `13111217671` |
| [Coverage 41376300569](https://github.com/ali-shortcuts/nexaroute/actions/runs/41376300569) | fresh Go coverage job `125057156830`, threshold gate: success | `13111511582` |
| [Security 41376300286](https://github.com/ali-shortcuts/nexaroute/actions/runs/41376300286) | govulncheck `125057155609`; CodeQL (Go) `125057155918`: success | none |
| [Real-system 41376300298](https://github.com/ali-shortcuts/nexaroute/actions/runs/41376300298) | two local gateway processes with PostgreSQL `125057155507`; Secret Service store/load `125057155837`: success | none |
| [Redis 41376300356](https://github.com/ali-shortcuts/nexaroute/actions/runs/41376300356) | Redis control-plane integration `125057156096`: success | none |

No job in those five runs was reported failed or skipped. The separate GitHub Advanced Security `CodeQL` check `125057767588` is neutral/skipped and warns it cannot determine alerts introduced by the PR because a main configuration is absent. A passing CodeQL job is not a passing alert comparison. No new workflows were run this turn.

The user reports independent inspection of coverage artifact `13111511582`: 42,224 covered / 44,262 total statements = 95.3955989336%, threshold 95%; browser artifact `13111872296` reports four passing suites and 26 screenshots. The user also reports smoke artifact `13111332592` shows PASS, 110 seconds total and `soak_duration_seconds=0`. Metadata and artifact names identify the synthetic merge above. This agent could not download artifacts or logs because storage redirects returned EOF, so has not personally checked their contents. [Evidence handoff](https://github.com/ali-shortcuts/nexaroute/pull/824#issuecomment-6567956375).

**Two-hour sustained soak: NOT RUN for a final candidate.** The final post-review candidate is not yet established, local `go` is unavailable, the restored working tree is dirty with local HEAD at main, and manual workflow dispatch previously returned 403. A 110-second smoke with zero sustained duration cannot satisfy this requirement. After reviews and candidate stabilization, an authorized operator must run the documented `soak.yml` with `rounds=3`, `duration=2h`, `timeout_minutes=180` on the frozen final ref, verify run head SHA and actual sample/summary contents and >=7200 sustained seconds, and record exact Go version, commands, run/artifact IDs, race, stress, startup/shutdown, outage/recovery, persistence, retry and idempotency coverage. Any gap requires additional testing; do not assume the soak covers it without inspecting its stages. See `docs/RELEASE_BLOCKERS.md`.

## Local checks in this turn

These ran against the restored working-tree contents, not a clean checkout with candidate HEAD, and do not replace candidate CI:

- `node scripts/test-activity-refresh.js`: PASS.
- `node scripts/test-workflow-config.js`: PASS.
- `python3 scripts/test-v025-pagination.py`: 3 tests PASS.
- `python3 scripts/test-execution-protection-model.py`: 4 tests, OK, **1 skipped**.
- `python3 scripts/test-release-control-assessment.py`: 4 tests, OK, **1 skipped**.
- Go test/vet/race, `scripts/verify.sh`, long soak and local browser: NOT RUN this turn. No local Go executable was available. No external-provider operations were attempted.

Offline policy tests are hypothetical and establish no live control enforcement. Full dashboard, backend, concurrency, persistence, security and operational audits remain incomplete.

## Security and publication controls

The neutral CodeQL check names `.github/workflows/independent-environment.yml:security`. The file has identical blob `e74eb71278764c311c220cfcb0108c7b9905429a` on main and #824, is manual-only, and uploads CodeQL without an explicit category; the PR `security.yml` uses `/language:go`. A main-analysis list request returned 403; `security_events=read` is required. Stored analysis ID, key, category, ref, commit SHA, timestamp and deletable status remain unknown. The warning's displayed workflow/job string is not proven to equal the stored category. No analysis was deleted or dismissed. Owner should grant authorized read access or inspect the paginated Code Scanning analyses, compare main and PR records, and evaluate removing only a redundant manual upload or coordinating scan identity without assuming different workflow keys will match. Historical retirement requires explicit approval and alert-history impact review. Do not add the entire manual workflow to PRs merely to hide the warning; resolution requires a later actual GitHub alert comparison.

Current API observations: one active branch ruleset `main-required-checks` ID `26200123`; environment list returned empty; Actions workflow permissions read returned 403. The execution-protection policy, inherited/bypass settings, secrets, cloud/registry trust and effective enforcement remain inaccessible or unverified. Visible absence is not proof of protection. Current `release.yml` is tag-triggered, grants the publish job `contents: write` and `id-token: write`, has no declared protected environment, signs and calls `gh release create`. Historical tagged workflow definitions and alternate workflow paths, direct APIs, human/App tokens, cloud and registry credentials are distinct risks. `workflow_call`-only is not a proven quarantine. Owner must independently inspect live workflow execution protections, tag rulesets, bypasses, protected environments, actual OIDC claims and trust, all publication credentials and alternate paths, and safely perform negative tests with isolated non-production credentials. See `docs/reports/PUBLICATION_CONTROL_INVENTORY.md`, `ACTIONS_EXECUTION_PROTECTIONS.md`, and `REUSABLE_PUBLICATION_SECURITY_REVIEW.md`; these are not proof of enforcement.

## External acceptance: all BLOCKED

| Issue | Missing prerequisite / evidence |
|---|---|
| #646 | Explicitly authorized provider, endpoint, model and spending/request/token/time ceilings; approved secret reference; real streaming, tools, usage, failure and failover reports. No paid calls authorized by this report. |
| #647 | Operator-owned organizational IdP, callback, role mapping, mapped/unmapped accounts and approved access; PKCE, rotation, denial, logout and audit evidence. Local IdP does not satisfy it. |
| #648 | Authorized staging/non-dev Vault KV, least-privilege secret reference and outage/rotation coordination; sanitized real round-trip and recovery evidence. |
| #649 | Authorized real Vault Transit instance, dedicated key and least-privilege policy; real fail-closed, rewrap, rotation and rollback evidence. |
| #650 | Authorized disposable Kubernetes cluster, storage and secret references; real install, previous-version upgrade, rollback, probes, security and persistence evidence. |
| #651 | Authorized ComfyUI host, compatible real model/workflow and resource budget; real playable output, SHA-256, gateway retrieval and negative-case evidence. |
| #652 | Two independent hosts, shared staging PostgreSQL/Redis, load endpoint, backup and permission for failure drills; shared-state, recovery and measured RPO/RTO evidence. |
| #653 | Final-candidate fresh >=95% coverage, required stress and independently inspected >=2-hour sustained soak including samples and provenance. |
| #654 | All preceding issues completed and independently reviewed, followed by umbrella readiness review; close last only when authorized. |

All issues #646–#654 were open at recheck. Report contents require independent review; no issue may be closed based solely on run metadata or fixtures. v0.25.0 is completion-only: no exclusions. No merge, issue closure, policy change, tag or publication has been performed.

## Subsequent execution update (same date)

This report's earlier workspace and tool-availability observations describe the start of the prior review snapshot. In this execution, the restored branch was still locally at main with all candidate changes in the working tree. After verifying all 50 candidate paths (49 byte-identical, with only the scanner intentionally edited), preserving the pre-existing report and patch, verifying an empty index and fast-forward ancestry, the local branch was advanced to the existing remote #824 head without resetting or overwriting files. `git read-tree` reconciled the index. The old report itself was preserved.

### Confirmed scanner defect and fix

The request-body test scanner used source position alone to decide whether an earlier unconditional bound remained valid. It missed an unbounded read on a later loop iteration when `r.Body` was reassigned *after* the read in the loop body. The `for` and `range` red/green fixtures failed before the fix. `internal/httpapi/request_body_bounds_test.go` now conservatively invalidates the inherited bound when an enclosing loop can contain a later body write, including nested loops. Inline bound wrappers remain accepted. This can produce conservative false positives; it is still a narrow syntactic guard, not alias, control-flow, goroutine or interprocedural taint analysis. No production request handler was changed.

- Installed Go 1.27.3 in temporary storage from the npm-distributed `@ttsc/linux-x64` package; no Go binary was committed.
- Isolated scanner fixture before fix: **FAIL** on both loop-reassignment regressions, as expected.
- Isolated standard-library-only scanner test after fix: `go test -race -count=1 -run '^TestBodyReadFindingsRedGreen$' -v`: **PASS**, including for/range, nested-loop, no-write and inline-bound cases.
- Compiled that isolated scanner test binary and ran `TestAdminRequestBodyReadsAreBounded` from `internal/httpapi`: **PASS**, 14 direct reads checked. This parses package sources; it is not a full package build.
- `GOTOOLCHAIN=local GOPROXY=off go test ./internal/httpapi -run 'Test(AdminRequestBodyReadsAreBounded|BodyReadFindingsRedGreen|ActivityChartLogic)' -count=1`: **BLOCKED/FAIL at setup**, required modules are not cached and module lookup was disabled. `go vet ./internal/httpapi` was similarly **BLOCKED**. No package or full-project Go pass is claimed.
- `bash .github/scripts/test-final-release-gate.sh`: **PASS**, including its offline completion fixture invocation. A subsequent separate `python3 .github/scripts/test-v025-completion.py` did not finish within the combined 120-second command timeout; its independent outcome is unknown and no second pass is claimed.
- `node scripts/test-activity-refresh.js`, `node scripts/test-workflow-config.js`, `node --check internal/httpapi/web/app.js`, `bash scripts/test-release-gate-status.sh`, `git diff --check`: **PASS**. Python pagination, policy-model and offline-assessment results remain as recorded in the prior section, not rerun to completion in the timed-out combined command.
- Current security analyses API still returns **403**, and #646–#654 remain open. #823 checks show their listed workflow jobs passing and a separate CodeQL check skipped; results apply only to that PR's own tested checkout.

### Release policy decision pending

PR #822 on main provides a generic version-scoped exclusion mechanism; #824 proposes a stricter v0.25.0 completion-only gate while retaining the generic mechanism for other releases. Previous records describe an owner completion-only decision, but the current mission explicitly asks that the policy question not be silently settled. No policy or manifest change was made in this execution. The owner must confirm how these requirements apply before release preparation; until then both the historical nonempty unsigned manifest and open acceptance issues remain blockers. Do not use exclusions as an implicit resolution.

The subsequent scanner fix requires a new commit and fresh checks, so the old PR merge artifacts above are historical and do not attest to that new commit. Comprehensive repository audits, independent review, external acceptance and a genuine final-candidate sustained two-hour soak remain incomplete.

## Correction and engineering update — 2026-11-02 (PR head dafffaf)

The preceding sections are historical snapshots. PR #824 subsequently reached head `dafffafcb76df4dde937359e1ab7aa8fd0e2985e`, tested as synthetic merge `be8a0314ea425bc7007200eae06d0dccc577f12b`. GitHub reports the following PR runs completed successfully (attempt 1, `pull_request`, head SHA above):

- [CI 41385538642](https://github.com/ali-shortcuts/nexaroute/actions/runs/41385538642): verify, bounded stress, PostgreSQL and Redis integration, container, fuzz and soak smoke jobs passed. Full verify step passed; its checked-in script runs `go test -timeout=3m -count=1 ./...`, `go vet ./...`, and `go test -race -timeout=3m -count=1 ./...`. The user independently confirms all three passed. Browser acceptance reports all four suites passed: browser-e2e, browser-e2e-502, live-visual-agent and workflow-config-browser. The container job reports successful amd64/arm64 image build, amd64 runtime smoke and Helm lint/render. Browser artifact `13114564843`; CI soak smoke artifact `13114933732`; Docker build artifacts `13115422163`, `13115203298`.
- [Coverage 41385538653](https://github.com/ali-shortcuts/nexaroute/actions/runs/41385538653): fresh coverage gate passed; artifact `13115141833`. The user reports 42,226 covered / 44,262 total statements (95.4001174823%, approximately 95.4%). These are the supplied independently verified counts, not a new calculation from an artifact this agent downloaded.
- [Security 41385538892](https://github.com/ali-shortcuts/nexaroute/actions/runs/41385538892): CodeQL (Go) and govulncheck jobs passed. Separate CodeQL comparison check `125085994993` remained **neutral**, reporting that the main `independent-environment.yml:security` configuration was not found, so it could not determine PR-introduced alerts.
- [Real-system 41385538637](https://github.com/ali-shortcuts/nexaroute/actions/runs/41385538637): runner-local two gateway processes with PostgreSQL and Secret Service store/load passed.
- [Redis 41385538684](https://github.com/ali-shortcuts/nexaroute/actions/runs/41385538684): Redis control-plane integration passed.

These are CI tests on the PR synthetic merge, not final-main, independent-host, production, or real external-tenant acceptance. The bounded CI soak remains smoke, not the required 7,200-second sustained soak. The separate Code Scanning analyses endpoint again returned HTTP 403; `security_events:read` is needed to inspect stored analysis keys, categories and provenance. This agent has not downloaded these artifacts or logs. The full project audits, independent reviews and all external requirements remain incomplete. No decision on the conflicting #822/#824 exclusion policy is made here.

### FileStore closed-writer data loss found and fixed

Audit of NexaFlow file persistence found that `Close` released the exclusive file lock, but a caller retaining the closed instance could still invoke a mutating operation. A second instance could acquire the lock and persist new data, after which the stale closed instance could overwrite that data from its old in-memory snapshot. The new regression `internal/nexaflow/file_store_close_test.go` demonstrates reopen, write, stale write, and disk recovery. `internal/nexaflow/file_store.go` now checks the lock state under `s.mu` in the common mutation path and returns an error wrapping `os.ErrClosed`, before copying or writing. Read methods on a closed instance are unchanged and may still return stale in-memory data; callers must not use a closed store. The guard protects all mutations routed through `mutate`; it does not create cross-host shared-file support.

A temporary standard-library harness used the actual `OpenFileStore`, `Close`, `persist`, `mutate`, `SaveDraft`, `GetDraft` and Unix flock code, with a simplified state model. With Go 1.27.3, `go test -race -count=1 -run '^TestFileStoreClosedWriterCannotOverwriteReopenedStore$' -v` **failed before the fix** (closed write succeeded, new data lost) and **passed after it**. This is not a full NexaFlow package or integration test; those need fresh CI on the new head.

Further persistence risk identified for follow-up: `persist` renames before opening and syncing the directory. If a later directory open or sync fails, the operation returns an error while the disk may already contain the new state and the in-memory state remains old. This failure mode is not reproduced here; fault-injection, documented crash semantics and a reviewed recovery design are still needed. Do not interpret the closed-writer fix as resolving it.

Additional checks on this follow-up: `GOTOOLCHAIN=local GOPROXY=off go test -race ./internal/nexaflow -run '^TestFileStoreClosedWriterCannotOverwriteReopenedStore$' -count=1` was **BLOCKED at package setup** because pgx was not cached. `node scripts/test-activity-refresh.js`, `node scripts/test-workflow-config.js`, `python3 scripts/test-v025-pagination.py` (3 tests) and `git diff --check` **passed**. API recheck showed one visible active branch ruleset, no listed environments, and a 403 reading default Actions workflow permissions; these observations do not prove live enforcement or absence of bypasses. Both PRs remain open with zero formal reviews/requests, and all issues #646–#654 remain open.

## Persistence audit update — 2026-11-02

### Previously submitted candidate `79b96e67b59cb49d98b45817e57fb6cc12552d74`

Waited for all existing checks. GitHub reports five exact-head PR runs completed successfully: CI [41389484596](https://github.com/ali-shortcuts/nexaroute/actions/runs/41389484596), coverage [41389484553](https://github.com/ali-shortcuts/nexaroute/actions/runs/41389484553), security [41389484714](https://github.com/ali-shortcuts/nexaroute/actions/runs/41389484714), real-system [41389484556](https://github.com/ali-shortcuts/nexaroute/actions/runs/41389484556), Redis [41389484654](https://github.com/ali-shortcuts/nexaroute/actions/runs/41389484654). PR merge checkout was `19c6f4eef6f595771230792e20e7e18781d9ba09`. CI verify, stress, browser, PostgreSQL/Redis, container and fuzz job metadata passed. Coverage gate passed; coverage artifact `13115959673`, browser `13116932671`, smoke `13116143857`. Exact coverage percentage and artifact contents have not been independently inspected here. The separate CodeQL comparison `125097989831` remains neutral due to the missing main configuration. CI log downloads and coverage logs were attempted after completion but the storage redirect returned EOF; therefore individual package/race test log lines and the actual CI output cannot be independently confirmed from full logs. CI metadata reports the full-verification step passed, and its source invokes package tests, vet and race, but this is not a claim that the logs were inspected. As an independent local check, an archive of the exact 79b96e6 source passed `go test -race ... ./internal/nexaflow -run '^TestFileStoreClosedWriterCannotOverwriteReopenedStore$'` with temporary replacement dependencies (see below). Evidence belongs only to that PR merge, not the new changes.

### Changes under review

1. **Post-rename uncertainty and lifecycle:** `internal/nexaflow/file_store.go`, `filelock_unix.go`, `state.go`, and `docs/nexaflow-task8-guide.md`. Previously a successful rename followed by a directory open/sync failure returned an ordinary error and retained stale memory, permitting a later write to overwrite the renamed state. `persist` now distinguishes the rename boundary; on post-rename error it returns an error wrapping `ErrFileStoreCommitUncertain` and the original filesystem error, and the instance rejects all reads and writes until closed and reopened. Callers must inspect recovered state and reconcile before retrying; reopening only observes the file available then and does not establish crash durability after failed directory sync. A short temp write is rejected. Closed stores reject reads and mutations. Unix lock release now reports unlock and close errors; on release error the instance remains closed and an operator may need to resolve a retained lock before reopening. Linux/Darwin locking and lack of interprocess exclusion on other platforms are documented.
2. **Corrupt empty or null file:** `OpenFileStore` previously treated an existing zero-byte file or JSON `null` as an empty store, potentially masking data loss. It now returns a corruption error and releases its lock; nonexistent files still start empty. Existing zero-byte stores require operator recovery rather than automatic replacement.
3. **PostgreSQL serialization:** `internal/nexaflow/postgres_store.go` silently replaced any JSON marshal failure with `{}`, potentially persisting lost workflow/run/node state. It now propagates encoding errors instead of writing a fabricated object. Draft, run definition/input and node execution tests verify invalid values are rejected before database access. Revision publishing also checks its encoding error inside the transaction. The existing PG corruption test now marshals its fixture explicitly. The test uses a nil database to verify invalid inputs do not reach it; it is not a live PostgreSQL integration test.

`file_store_fault_test.go` injects temp write, short write, temp fsync, temp close, rename, directory open and directory fsync failures and inspects memory and disk, reopening and subsequent mutation. `file_store_guard_test.go` checks every public store operation on uncertain/closed instances and injected lock-release error. Close-versus-write and lock reacquisition tests exercise race and lifecycle behavior. `postgres_json_test.go` adds the serialization regressions. Before the PG fix, the invalid-draft regression reached the nil database and panicked instead of reporting a JSON error; after the fix all five subcases passed.

**Executed locally:** Go 1.27.3 `go test -race -timeout=5m -count=1 ./internal/nexaflow` and `go vet ./internal/nexaflow` passed on the changed working tree. Because Go module proxy access was unavailable, these commands used a temporary `-modfile=/tmp/nexaflow-deps/local.mod -mod=mod` with six Go module replacements downloaded from GitHub codeload and `GOPROXY=off GOSUMDB=off`; no repository module files were changed. This is real package execution but not a pristine locked-module CI run. The targeted persistence fault tests, closed-store tests, store contract/restart tests, and invalid-JSON tests passed under `-race`. Live PostgreSQL tests requiring a DSN were not run. `git diff --check` passed. The next commit and PR CI results must be recorded separately; previous green runs do not transfer.

This is not a complete persistence audit. PostgreSQL real outage/commit ambiguity, multi-host operation, other storage paths and external recovery remain unverified. Filesystem guarantees on network/unsupported filesystems and process/power-loss crash behavior were not demonstrated by injected errors. File-store reopen and standard lock tests are not independent-host acceptance. Review all changes independently and keep release BLOCKED.

Additional red/green check: a standalone regression run against an archive of `79b96e6` failed because an existing zero-byte file was accepted; the new empty/null regressions pass in the changed package's race run. Real PostgreSQL behavior under injected commit failures remains NOT RUN, and the encoding tests do not replace live database acceptance.

### Continued persistence audit — working changes after 8dd7411

Release remains **BLOCKED**. These results are not independent review, final
acceptance, or a two-hour soak. The evidence below applies to the working tree
in which this entry was authored, not to the earlier `8dd7411` CI result; use
Git history and the PR checks to identify the subsequent committed candidate.

* Provisioned an isolated PostgreSQL 16.15 instance on a local Unix socket,
  using a disposable trust-authenticated test cluster. With the temporary
  codeload-based Go module replacements described earlier, the full NexaFlow
  package race suite passed with `NEXAROUTE_TEST_PG_DSN` pointing to this real
  database. The migration rollback test now uses the configured disposable
  administrator DSN rather than hardcoding a root peer-auth socket and TCP
  port. It still requires a role authorized to create/drop temporary roles and
  databases. Updated the obsolete invalid-JSON fallback expectation to check
  rejection and preservation against real PostgreSQL.
* Added a driver-boundary fault that performs a real PostgreSQL COMMIT, then
  injects an error in place of its acknowledgment. Tested durable readback of
  the committed run and node, error cause/sentinel propagation, and fail-closed
  engine replay/Execute after a partially initialized run. This is **not** an
  actual network partition, server crash, failover, or a full transaction fault
  matrix. `ErrPostgresCommitUncertain` now marks transaction COMMIT errors;
  it conservatively includes definite failures and does not cover every
  autocommit statement. Blind `AppendNodeExecution` retry still creates a new
  attempt: there is no stable append operation token. The engine now detects
  missing initial nodes on replay/Execute but cannot atomically create or
  automatically repair them. Direct store clients, startup recovery, other
  write paths, multi-process races, and actual lost packets remain open.
* Fixed approval replay returning the caller's requester instead of the stored
  requester, discarding an ignored query error, and returning a timestamp with
  precision different from PostgreSQL's persisted timestamp. Concurrent
  first-decision races and ambiguous autocommit paths remain to be audited.
* Fixed video memory/file stores failing to update or enforce the idempotency
  index when a job's key changes; file persistence failure now rolls back the
  index too. Reopen reconstructs the index from jobs rather than trusting stale
  mappings; `null` documents fail closed. Regression tests exercised key
  changes, conflicts, reuse, deletion, restart, stale-index recovery and JSON
  encoding rollback; a matching real-PostgreSQL integration test passed.
  Full video queue integration race suite also passed against this disposable
  PostgreSQL. Video FileStore still ignores directory open/sync errors and has
  no interprocess locking; this work does **not** certify its crash durability.
* Added actual subprocess SIGKILL tests before rename and after rename/before
  directory sync, checking reopen, preserved baseline, visible rename state and
  released lock, plus Unix file/directory permissions. These are process-kill
  tests only: they do not prove power-loss persistence or network filesystem
  behavior. Continue corruption, concurrency, unsupported filesystem, and
  fault-injection audits across stores and the broader project.
* GitHub Code Scanning analyses endpoint still returns HTTP 403. Authorized
  `security_events` analysis access is needed to investigate the neutral
  comparison without deleting or dismissing analyses. Owner decision remains
  necessary for the #822/#824 release-policy conflict. #646–#654 remain open;
  their organizational infrastructure, authorized spend, independent review,
  and final-candidate soak requirements remain unmet as documented above.

#### Local network acknowledgment-loss follow-up

A subsequent real-network test supersedes the statement above that lost packets
remain entirely untested: a local TCP PostgreSQL protocol proxy observed the
server's `CommandComplete(COMMIT)`, dropped that message and closed the
connection before the client received it. Against disposable PostgreSQL 16.15,
`CreateRun`, `AppendNodeExecution`, `TryClaim`, `SaveNodeExecution`,
`UpdateRunState`, `RecordApproval`, `Publish`, and `PublishIfUnchanged` each
returned the uncertainty sentinel; reconnection observed the committed state.
Both publication variants replayed without a second revision for the tested
inputs. This is a real local connection loss, in addition to the earlier
post-commit driver injection. It does not establish remote-network, TLS,
server crash/failover, rollback, serialization-conflict, or exhaustive
operation/retry safety. In particular direct autocommit `SaveDraft` and
`ReleaseLease`, and `Prune`, still need dedicated fault cases; blind append
retry is still unsafe. The test intentionally runs only against an explicitly
configured local plaintext disposable PostgreSQL instance and otherwise skips.

### Persistence continuation — implementation and evidence

This section supersedes earlier *current-state* statements about video file
sync and token-bearing append replay, while retaining them as historical
records. Release remains **BLOCKED**. Commits below are on PR #824; no merge,
tag, publication, deployment, paid-provider call, or independent approval is
claimed.

1. **Video queue file persistence:** `8cfc7da3b41dac34c08f47f1c62de40b3f3802c1`.
   Root causes: unchecked short writes, ignored parent-directory errors, and
   memory rollback after a successful rename could permit stale overwrite.
   `internal/video/queue/file_store.go` now detects short writes, reports
   post-rename uncertainty, invalidates the instance until Close/reopen, and
   guards all reads and writes. `file_store_fault_test.go` deterministically
   covers mkdir, temp creation, chmod, write, short write, file sync/close,
   rename, directory open/sync/close for Create/Update/Delete/TryClaim,
   including index and reopened-state checks. Local race output included
   `ok github.com/ali-shortcuts/nexaroute/internal/video/queue 2.850s` with
   real PostgreSQL integration enabled. No cross-process file locking is added;
   process kill or reopening does not establish power-loss durability.
2. **Append and workflow initialization:**
   `81b38fca1758ff42b31cf5928584fb4a547a5c09` and
   `0855ffe280f1d8e99cdeea3b6faecc37bde502c9`.
   Root cause: a lost commit response could turn a repeated append into a new
   attempt, and run creation exposed partial initialization. Node executions
   now optionally carry `AppendOperationID`; memory, file and PostgreSQL
   return the stored attempt for the same run/node/token, and PostgreSQL
   serializes appends by locking the run. The engine uses deterministic
   initial/retry tokens. Empty tokens retain the legacy unsafe behavior;
   clients must supply unique logical-operation tokens and old writers must be
   quiesced during upgrade. Caller-provided token reuse for different payloads
   is not independently verified as a conflict, and this is not exactly-once
   external side-effect execution. `append_idempotency_test.go`,
   `postgres_commit_integration_test.go` and `postgres_network_fault_test.go`
   cover concurrent duplicate tokens, legitimate subsequent attempts,
   reconnection, lost final initialization acknowledgment, missing attempt zero,
   and fail-closed execution. Replays wait up to two seconds for a concurrent
   initial creator; no automatic repair is claimed. Local output:
   `ok github.com/ali-shortcuts/nexaroute/internal/nexaflow 24.491s` (race,
   real disposable PostgreSQL); 20 repetitions of the prior concurrent
   duplicate regression and 10 repetitions with the new synchronization test
   also passed.
3. **Fault test and CI wiring:** `81b38fca1758ff42b31cf5928584fb4a547a5c09`
   expands actual local TCP fault injection to autocommit SaveDraft and
   ReleaseLease (drop ReadyForQuery), Prune (isolated disposable schema),
   before-COMMIT connection loss and rollback, and a genuine PostgreSQL 40001
   serialization conflict on a dedicated probe transaction. It inspects state
   after reconnect; the probe does not imply workflow transactions use
   serializable isolation. Explicit CI command uses the existing local
   plaintext PostgreSQL 16 service, retains the video integration step, checks
   for actual PASS rather than skip, and writes per-test status to the job
   summary. Locally `TestPostgresNetworkDropsCommitAcknowledgement` passed
   against PostgreSQL 16.15 with `-race`; GitHub CI for this new wiring has
   not yet been established. More isolation, failover, autocommit replay,
   and cross-operation fault combinations remain open.
4. **Control-plane and cost:**
   `8ebfbee58ff252e8a2313e332abffac107a71a42`. Root causes in
   `internal/controlplane/filestore.go`: short writes and directory errors
   were ignored, and decoding into a preinitialized document silently treated
   `null` or missing required fields as an empty store. Atomic file/backup/
   restore writes now report uncertain post-rename errors; invalid documents
   fail closed. `filestore_durability_test.go` tests short write, rename,
   directory failures and invalid documents. Local output:
   `ok github.com/ali-shortcuts/nexaroute/internal/controlplane 3.333s` (race).
   Control-plane integration tests could not compile locally with the offline
   temporary modfile because `github.com/redis/go-redis/v9@v9.17.0` was not
   cached. In `internal/video/cost/pg_ledger.go`, failed release was silently
   discarded: `ReleaseForJobChecked` now reports it and the legacy void method
   logs it. The orchestrator still uses the void interface, so errors are not
   propagated to that caller; reporting aggregates can still degrade to zero
   during outage. The fake-driver regression is not real-fault acceptance.
   Local real-PostgreSQL integration/race output:
   `ok github.com/ali-shortcuts/nexaroute/internal/video/cost 2.320s`.
5. **CI evidence on previous `05a3e966965dae5147f5f0efd22089e82034ff78`:**
   CI run `41394087286` **failed** at Full verification in the mandatory race
   pass, specifically `TestReconciliationIdempotencyConcurrentDuplicate` saw
   incomplete initialization during concurrent CreateRun. GitHub check-run
   `125111865107` annotations supplied the failure despite full log download
   EOF. The bounded wait fix is `0855ffe` above; that old failure is not a
   pass and cannot validate later SHAs. Coverage `41394087209`, security
   `41394087226`, real-system `41394087386`, and Redis `41394087429` reported
   success for that old SHA; browser and later verify-job stages were skipped
   after the failure. Separate CodeQL comparison remained skipped/neutral.

All local tests above used `/tmp/nexaflow-deps/local.mod` with codeload module
replacements and Go 1.27.3; they are not pristine locked full-repository CI.
Security store, shared state, Redis/PostgreSQL control-plane, cost, backup,
restore and migration audits remain incomplete, as do network/filesystem
fault matrices, external acceptance, independent review, CodeQL comparison
access, owner release-policy decision, and the exact-final-candidate two-hour
soak. Do not transfer old-head checks to a new head.

### Addendum state reconciliation (2026-11-02)

Release remains **BLOCKED**. This workspace's branch is
`arena/6220b7d9-nexaroute`, but on reinspection its local HEAD was
`436c4e98618eed0c41e59059eae7eca5f05dd190` with 74 changed/untracked
paths; previously reported local commit `409f60756a743f7eba11eae40122f0fafd966bc5`
is not present as a local Git object in this workspace. Its file-level work
is present. GitHub confirms PR #824 head is still
`05a3e966965dae5147f5f0efd22089e82034ff78`; the previous push rejection for
workflow permissions remains unresolved. The working files and binary patch
were preserved in `/home/user/nexaroute-work-preservation/2026-11-02-addendum/`.
Fetched the remote branch without moving the local branch pointer. After
staging all preserved work, `git diff --cached --stat origin/arena/6220b7d9-nexaroute`
identified 17 files of local follow-up relative to remote; these are not a
pushed commit and have no exact-head GitHub CI evidence. Do not treat the old
local SHA as current history or transfer CI results. Reconciliation and
workflow-write authorization are still required before pushing.

Live GitHub ruleset `26200123` (`main-required-checks`) is active; its required
contexts are verify, ci-soak-smoke, fuzz-smoke, integration-postgres,
integration-redis, container, Go vulnerability scan, CodeQL (Go), and Fresh
Go coverage. It also contains deletion and non-fast-forward rules and has
`strict_required_status_checks_policy=false`. No review rule was returned;
independent human review remains a separate engineering requirement.

PR #823 remains open at `58bacddbc59e8cf9a01bc59620128c93ea2d7e1a`, and the
user-identified chart-window, lifecycle classification, and body-reader
scanner findings remain held until fixed and tested **on that PR's actual
branch**. This session is restricted to its own branch, so it cannot edit or
rebase #823's branch. Do not mistake the proposed patch or #824's working
files for a verified fix to #823. An authorized operator/session on #823's
branch must implement, independently review, browser-test, and verify its new
head. Do not merge either PR or silently select the conflicting release policy.

PR #824's prior CI run `41394087286` failed verify on
`TestReconciliationIdempotencyConcurrentDuplicate`; see annotation of job
`125111865107`. A bounded-wait fix exists in the preserved local files but is
not pushed and has not passed GitHub CI. Browser steps after the failed verify
were skipped. The full exact-main/final-candidate tests, 95% coverage proof,
2-hour sustained soak, external acceptance, review and CodeQL comparison
remain outstanding.

The 79 requested closed-unmerged PRs were individually queried via GitHub
GraphQL and all 79 are actually `CLOSED` with `merged=false`. See
`HISTORICAL_PR_DISPOSITION_2026-11-02.md`: all 79 remain
`BLOCKED_PENDING_DEPENDENCY` pending behavior-level comparison, regression
proof and decisions; no recovery, equivalence or successor is claimed merely
from metadata. The requested cross-branch/rebase/merge actions cannot be
performed from this session and are not authorized by the existing no-merge
constraints.

#### Canonical status snapshot and local dashboard follow-up

GitHub GraphQL repository totals observed 2026-11-02: 208 closed and 9 open
issues; 526 merged, 2 open, and 79 closed-unmerged PRs. These are GitHub state
counts, **not** counts of independently verified completed work. The nine
open issues are #646–#654 and remain blocked/partial as described above.
Verified-completed and partially-completed issue totals have not been audited;
mark them **unknown**, not 208 or zero. No PR was merged or issue closed in
this continuation. Of the 79 historical PRs, 79 are presently blocked pending
technical evidence; recovered 0, evidenced superseded 0, and evidenced
rejected 0 in this inventory.

| Acceptance area | Current evidence/status |
|---|---|
| Backend | Partial; exact final-main verification and cross-project audit incomplete. |
| Frontend | Partial; no exact-head real-Gateway browser acceptance this continuation. |
| Dashboard | Partial; panel/control, permission and localization audit incomplete. |
| Security | Partial; independent review and CodeQL comparison analysis access missing. |
| External acceptance | Blocked: authorized infrastructure and spend limits absent. |
| Coverage | Previous synthetic-merge artifact ~95.3956%; fresh exact final-main covered/total statements unknown. |
| Soak | 110-second smoke is not a two-hour sustained soak; final candidate not frozen. |
| Release readiness | **BLOCKED**. |

A local #824 working-tree follow-up (not applied to #823) fixes a chart-window
rounding defect: e.g. a 45-second requested window with 30-second buckets
previously included 60 seconds. `scripts/test-activity-window.js` failed
before the change with `lookback exceeds requested window 45000`, then passed
(`activity window bounds OK`); `node scripts/test-activity-refresh.js`
passed. A temporary standalone Go 1.27.3 harness using the actual
`activity_chart_test.go` and embedded production JS passed `TestActivityChartLogic`
and `TestActivityChartWiring` with `-race`. This is not browser or real-Gateway
acceptance and is not evidence of a fix on #823.

The #824 working-tree scanner regression now explicitly exercises an
intermediate decoder variable with both unbounded and `io.LimitReader`-bounded
input. A temporary copy of the real handler with its 4 KiB bound removed
failed as expected at `autopilot.go:406:12: unbounded r.Body read`; restoring
the copy passed and reported 14 direct body reads. The standalone scanner
race tests passed. The scanner remains limited to direct syntactic body flows,
not a proof covering aliases, alternative APIs, or interprocedural flows.
No actual #823 head or GitHub CI was changed by these local tests.

#### Local Git history reconciliation and authorized push blocker

The preserved file snapshot was committed locally as
`edf5d11b9b90774aa75c1d0993c7e25b07842760`. It was then reconciled with
remote PR #824 head `05a3e966965dae5147f5f0efd22089e82034ff78` in local merge
commit `d9a03c98dcc08ee2a4ba8179c6f05e3df4f2ac80`. Before the merge,
`git diff --cached origin/arena/6220b7d9-nexaroute` showed the precise
22-file follow-up on top of the remote; conflict resolutions preserved the
already-accounted-for local snapshot, and the resulting merge tree was
verified byte-for-byte identical to that snapshot. The remote head is an
ancestor of the local merge. This is local branch-history reconciliation, not
a PR merge, a rebase of #823, or independent review.

A fresh `git push origin arena/6220b7d9-nexaroute` was rejected: GitHub App
lacks `workflows` permission to update `.github/workflows/ci.yml`. The user
must reconnect/authorize the GitHub integration with workflow-write access;
there was no bypass, force-push or remote branch change. PR #824 remains at
`05a3e966965dae5147f5f0efd22089e82034ff78` until an authorized push succeeds.

At local tree `d9a03c98dcc08ee2a4ba8179c6f05e3df4f2ac80`, the standalone body
scanner race suite passed, the chart-window and activity-refresh Node tests
passed, and the control-plane package race suite passed (`3.049s`). A local
attempt to run NexaFlow and video queue race suites with the pristine module
file failed during setup because `github.com/jackc/pgx/v5@v5.11.0` was not
cached and `GOPROXY=off`; these are not passing full-package results for this
SHA. No real PostgreSQL/Redis integration, browser acceptance, full Go
coverage or GitHub CI has been established for this local candidate.

### Continuation: workspace verification and independent local work

At the start of this continuation, `git status --short --branch` showed a
clean `arena/6220b7d9-nexaroute`; `git cat-file -t` confirmed commit
`17d7329ee7bcd7fd56419e3efd55d84254a3d7ee`, parent
`d9a03c98dcc08ee2a4ba8179c6f05e3df4f2ac80`. Its only direct change is this
status report; its aggregate diff from main
`436c4e98618eed0c41e59059eae7eca5f05dd190` contains 76 paths, 6,802 insertions
and 363 deletions. Git confirmed remote PR #824 head
`05a3e966965dae5147f5f0efd22089e82034ff78` is an ancestor of the local
commit. The unpushed older object `409f607` is absent; its file-level work
was preserved in the verified local history and the backup described above.
No reset, force push, PR merge, or history rewrite was performed.

**New local implementation commits (not pushed, not GitHub-CI-verified):**

* `b41ffe6108f4341826f502a0279b2e0bf70a6c45`: in
  `internal/nexaflow/postgres_store.go`, concurrent first approvals formerly
  selected a nonexistent row and raced to insert; on real PostgreSQL the new
  `TestPostgresConcurrentFirstApproval` reproduced SQLSTATE 23505 on four of
  twelve rounds before the fix. Insert-on-conflict plus locked read now
  returns the same persisted decision for identical requests and
  `ErrApprovalConflict` for different decisions. Added real-database tests for
  both races and included them in the explicit PostgreSQL CI step, with
  fail-if-skipped checks. Three repetitions passed; a combined 20-repetition
  `-race` run of both approval tests and the unchanged
  `TestReconciliationIdempotencyConcurrentDuplicate` plus its bounded-wait
  regression returned `ok github.com/ali-shortcuts/nexaroute/internal/nexaflow
  12.069s`. The full NexaFlow package with the real database and `-race`
  returned `ok .../internal/nexaflow 25.717s`; package vet passed. These are
  local results, not exact-head CI, independent review, or exhaustive fault
  verification. `.github/workflows/ci.yml` requires workflow-write access to
  push; no permission bypass was attempted.
* `a93654b6cd4b88604b4985ab0fcb74ae18aa8bab`: the coverage gate used the
  one-decimal `go tool cover` display for its verdict, so 1,899/2,000
  statements (94.95%) was rounded to 95.0% and incorrectly passed. Regression
  `scripts/test-coverage-gate.sh` failed before the fix with
  `want exit 1, got 0`, then passed after comparing raw counts against the
  threshold. Exact-threshold, above/below, missing and malformed fixtures
  still passed. This addresses one reproduced gate defect, not proof of
  comprehensive equivalence to historical PR #757 or a real 95% measurement.
* `773f58040b8b274eaad9bf323d9ade8fd539561c`: paginated GitHub file metadata
  for all 79 closed-unmerged PRs was checked against GraphQL head and file
  counts, then compared to exact main blob IDs. Results, including per-path
  evidence, are in `HISTORICAL_PR_FILE_COMPARISON_2026-11-02.json` and its
  inventory report: 214 of 913 file entries matched, no PR had all entries
  matching. This is a comparison aid, not a behavioral disposition: all 79
  remain blocked pending substantive review and any necessary operator action.

**Real local service tests:** a disposable PostgreSQL 16.15 on a Unix socket
and Redis 7.4.7 on a Unix socket were used, with temporary codeload Go module
replacements and Go 1.27.3. No remote or production infrastructure was used.
At the then-current local working tree: PostgreSQL video queue and cost
integration race suites passed (`3.356s`, `2.789s`); four PostgreSQL
control-plane conformance/concurrency/migration cases passed (`1.139s`);
Redis control-plane conformance/concurrency/corruption/revision cases passed
(`1.033s`); PostgreSQL and Redis shared breaker integration cases passed
(`1.064s`, `1.023s`); PostgreSQL and Redis security-store contract/hash parity/
concurrent writers/reopen cases passed (`1.492s`). The local security-store
package race suite and vet passed (`34.493s`). This is not a full-repository
locked-module run, remote-service acceptance, Redis outage drill, or evidence
for GitHub CI. Browser acceptance was NOT_RUN: the local environment lacks
Playwright and a browser, and a real Gateway browser run has not been
established. Separate PR #823 is unchanged and still needs its own branch,
independent review, and exact-head browser/CI verification.

**External/action blockers:** the GitHub App needs workflow-write permission;
after authorized reconnection, push this fixed branch, verify its remote SHA,
and await complete CI, PostgreSQL/Redis integrations, FileStore/process
recovery and independent review. The 79 PRs still require behavior-level
review; the 95% fresh exact-candidate coverage, authorized two-hour soak,
external provider/organizational infrastructure acceptance, owner release
policy decision, and security comparison access remain unmet. Release is
**BLOCKED**.

#### Historical PR behavior review update

The prior 79-blocked tally is superseded by one verified disposition:
`f691ceb` documents [#807](https://github.com/ali-shortcuts/nexaroute/pull/807)
as `SUPERSEDED_BY_PR` [#808](https://github.com/ali-shortcuts/nexaroute/pull/808),
merged `dccd254467b6f89542441ea4c726f24f1c0b49b8` and reachable from exact
main `436c4e98618eed0c41e59059eae7eca5f05dd190` according to GitHub's compare
API. Five source/test/report blobs match #807; main's verification script
includes its OIDC self-test plus a later interrupt regression. Local
`python3 scripts/test-oidc-runner-acceptance.py` returned
`OIDC abort-summary regression: PASS` (its deliberate injected interrupt logs
a FAIL check as expected), `python3 scripts/oidc-runner-acceptance.py
--self-test` passed, and `bash scripts/test-vault-runner-acceptance.sh` passed
its 5/5 offline checks and policy/survival regression. These are offline
harness tests, **not** live organizational OIDC or Vault acceptance.

Current historical tally: 79 closed-unmerged examined at metadata/blob level;
1 superseded with behavior/successor evidence (#807), 78 blocked pending
behavior-level review and disposition, 0 recovered and 0 rejected. This is not
completion of the historical audit. #811's readiness and termination
implementation has a plausible successor #812, but later termination semantics
and behavioral equivalence still require examination; it remains blocked.
