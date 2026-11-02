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
