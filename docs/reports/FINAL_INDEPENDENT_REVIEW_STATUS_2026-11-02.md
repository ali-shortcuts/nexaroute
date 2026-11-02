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
