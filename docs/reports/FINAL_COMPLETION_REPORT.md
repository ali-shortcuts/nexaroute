# Final completion report — 2026-10-11

Repository: `ali-shortcuts/nexaroute`<br>
Base: `origin/main` at `dcb9f9b3c0352d23f1dfa599ea58450b072406c0`<br>
Working branch: `release/final-closeout-20261011`<br>
Final branch commit: `c0551a29b0f78905bed07450bd011a1a4c241057`<br>
Final release version: `v0.17.0`

No commit was made to `main`. No branch or tag was deleted. No GitHub release was created.

## Done

### WP6 — local runtime store path

- Fixed startup migration so an existing local control-plane snapshot is preserved instead of being replaced by the bootstrap marker or needlessly revision-bumped.
- Added tests for existing-state migration, restart recovery, and concurrent optimistic-revision writes in `cmd/gateway/controlplane_runtime_test.go`.
- Targeted result:

```text
ok   github.com/ali-shortcuts/nexaroute/cmd/gateway       8.324s
ok   github.com/ali-shortcuts/nexaroute/internal/controlplane 0.030s
ok   github.com/ali-shortcuts/nexaroute/internal/httpapi  9.840s
```

- Race result:

```text
ok   github.com/ali-shortcuts/nexaroute/cmd/gateway       9.791s
ok   github.com/ali-shortcuts/nexaroute/internal/controlplane 1.041s
ok   github.com/ali-shortcuts/nexaroute/internal/httpapi 17.329s
```

### WP3 — aggregate coverage

Fresh command on the closeout tree:

```text
go test -count=1 -coverprofile=/tmp/final.cover ./...
...
total: (statements) 85.0%
```

The requested **85.0% threshold passes**. No coverage-padding tests were added.

### Specs/014 evidence

- Deterministic real-request failover browser evidence passed:

```text
LIVE VISUAL AGENT E2E PASS
```

  It used Chromium, the real gateway, two local HTTP upstreams, a deterministic first failure and second success, request correlation, and generated evidence under `specs/014-control-plane-rebuild/live-visual-agent/evidence/`.
- Performance evidence was regenerated in `lva-performance.json`; the report records idle, active-failover, and success DOM/heap measurements.
- Epoch/cursor reconciliation evidence remains covered by the SSE resume/epoch tests and `docs/reports/a9-sse-resume-evidence.md`.

### WP8 — clean clone

Remote branch clone: `release/final-closeout-20261011`<br>
Clean clone SHA: `c0551a29b0f78905bed07450bd011a1a4c241057`

Raw acceptance result:

```text
=== CLEAN CLONE ===
## release/final-closeout-20261011
c0551a29b0f78905bed07450bd011a1a4c241057
=== verify.sh ===
VERIFY PASS
=== smoke-local.sh ===
SMOKE PASS — local runtime, UI, admin persistence and token-count fallback are operational.
=== test-install.sh ===
Prepared v0.17.0 in dist/ with gateway, videogen, installer, config and docs. Nothing published.
INSTALL PASS
INSTALLER E2E PASS
```

`test-install.sh` was made clean-clone-safe: it builds local `v0.17.0` fixtures when ignored `dist/` artifacts are absent.

### Version consistency

Updated to `v0.17.0` across README, Dockerfile default build argument, release build default, release notes, installer fixtures, CI packaging invocation, and `/version` default/tests. The stale `v0.17.0-rc.1` release notes were replaced by `docs/releases/v0.17.0.md`; stale v0.6/v0.7 references were removed from active source/docs/scripts.

### Release workflow

Dispatched without publishing:

```text
Run: 38134261987
URL: https://github.com/ali-shortcuts/nexaroute/actions/runs/38134261987
headSha: c0551a29b0f78905bed07450bd011a1a4c241057
verify: success
publish: skipped
conclusion: success
```

The workflow’s `verify` job passed. The `publish` job was skipped because this was a branch `workflow_dispatch`; no tag or release was created.

## Gate evidence

Local raw result summary:

```text
=== smoke-local ===
SMOKE PASS — local runtime, UI, admin persistence and token-count fallback are operational.
=== build-release v0.17.0 ===
Prepared v0.17.0 in dist/ with gateway, videogen, installer, config and docs. Nothing published.
=== release-smoke ===
RELEASE SMOKE PASS — gateway, embedded UI, Video Studio route, fake provider and async job admission are operational.
=== test-install ===
INSTALL PASS
INSTALLER E2E PASS
```

Full verification raw terminal result ended with:

```text
== real browser control-plane acceptance ==
BROWSER E2E PASS
== real browser Live Visual Agent acceptance ==
LIVE VISUAL AGENT E2E PASS
== linux amd64 build ==
== linux arm64 build ==
VERIFY PASS
```

## Not done / not verified

- **Docker image build: BLOCKED.** `docker: command not found`; no Docker CLI/daemon was available in the sandbox.
- **Live Redis interoperability: NOT VERIFIED.** Redis client contracts and lease/token-bucket primitives exist, but no live Redis service was available.
- **Live PostgreSQL interoperability: NOT VERIFIED.** SQL store contracts/tests exist, but no live PostgreSQL service/driver was available.
- **Live OIDC identity provider: NOT VERIFIED.**
- **Live OS keyring/KMS interoperability: NOT VERIFIED.**
- **Real commercial video provider: NOT VERIFIED.** Release smoke uses the documented development fake provider.
- **Human visual acceptance with a configured real provider: NOT VERIFIED.** Automated Chromium evidence is deterministic and local; it is not a human visual sign-off.
- The control-plane runtime closeout verifies the merged local file-store wiring and its migration/restart/concurrency behavior. It does not claim live external backend wiring where the repository intentionally refuses unsafe driver/client fallback.

## Branch containment recommendations — no deletion performed

The requested M1 audit listed these **14 contained non-main branches**. Recommendation for each: **retain until the owner explicitly approves deletion**, then re-fetch and re-audit immediately before any individual deletion.

| Branch | Tip at M1 audit | Recommendation |
|---|---:|---|
| `chore/video-gateway-audit-hardening` | `376cd10` | Candidate for deletion after owner approval |
| `docs/phase1-reality` | `3a02525` | Candidate for deletion after owner approval |
| `fix/gateway-shutdown-lifecycle` | `07cad29` | Candidate for deletion after owner approval |
| `fix/virtual-key-policy-intersection` | `f5f415f` | Candidate for deletion after owner approval |
| `phase1-coverage-85` | `f263e94` | Candidate for deletion after owner approval |
| `wp1-virtual-key-policy` | `1a503e2` | Candidate for deletion after owner approval |
| `wp2-video-gateway` | `f5b74e6` | Candidate for deletion after owner approval |
| `wp2b-video-hardening` | `fce6214` | Candidate for deletion after owner approval |
| `wp4-rbac-sso` | `a3d6635` | Candidate for deletion after owner approval |
| `wp4-rbac-sso-runtime` | `2a1d9f1` | Candidate for deletion after owner approval |
| `wp5-egress-policy` | `81b888c` | Candidate for deletion after owner approval |
| `wp6-controlplane-reconcile` | `8646304` | Candidate for deletion after owner approval |
| `wp6-runtime-wiring` | `ac6517e` | Candidate for deletion after owner approval |
| `wp7-key-custody` | `c4fb68e` | Candidate for deletion after owner approval |

The same audit listed these **8 non-contained branches**. Recommendation: **do not delete; retain and resolve with the owner/PR state first**.

| Branch | Tip at M1 audit | Recommendation |
|---|---:|---|
| `chore/m1-dependency-audit` | `e27609e` | Retain; draft PR #239 / milestone work |
| `fix/video-runtime-p0-hardening` | `8b6bf13` | Retain; tree/PR review required |
| `fix/video-runtime-p0-hardening-v2` | `88e965c` | Retain; tree/PR review required |
| `opencode/issue232-20261010142021` | `1bb893d` | Retain; tree/issue review required |
| `opencode/issue233-20261010142842` | `fa318f0` | Retain; tree/issue review required |
| `opencode/issue234-20261010142842` | `4c1cb7e` | Retain; tree/issue review required |
| `phase0-baseline` | `d6a090b` | Retain; baseline history must be preserved |
| `phase1-coverage-on-1b` | `0b23b91` | Retain; coverage branch review required |

A fresh fetch found additional remote refs beyond that point-in-time M1 list. They were not deleted; the M1 requested 14/8 list above remains unchanged for owner review.

## Final status

**Done:** local WP6 migration/restart/concurrency evidence; aggregate coverage threshold; deterministic browser failover; performance and cursor/epoch evidence; v0.17.0 version cleanup; clean-clone acceptance; successful non-publishing release workflow dispatch.<br>
**Not done / not verified:** Docker build, live Redis/Postgres/OIDC/KMS/keyring/real-video interoperability, and human visual sign-off with a real provider.<br>
**Administrative safety:** no main push, no branch deletion, no tag creation, no release publication.


---

# NexaRoute master completion — progress report

Updated 2026-11-02. **Overall: BLOCKED; this is not a release authorization.** This report distinguishes evidence from outstanding work. Local commits and reports are not changes integrated into main.

## Baseline and current references

- Starting checkpoint: `ddc0546907fe94288d471c703ecaa7cd3442a79b`; earlier `63236a22698a11782ca434cad0c05454b12b4454` and `17d7329ee7bcd7fd56419e3efd55d84254a3d7ee` verified ancestors.
- Latest tested code commit: `ce7eb49eb9c36c2f7cb6e2c4c493f79c7a3605cc` (this report and the persistence report are documentation changes after it; use `git rev-parse HEAD` for the latest local SHA).
- Main: `436c4e98618eed0c41e59059eae7eca5f05dd190`. #824 head: `05a3e966965dae5147f5f0efd22089e82034ff78`. #823 head: `58bacddbc59e8cf9a01bc59620128c93ea2d7e1a`. Verified 2026-11-02; may subsequently change.
- #824 remote verify: **FAIL**, [run 41394087286](https://github.com/ali-shortcuts/nexaroute/actions/runs/41394087286/job/125111865107). Local continuation has not been pushed. Required check results cannot be transferred between SHAs.
- At code commit `ce7eb49`, the local diff versus main contained 82 changed files; the diff versus #824 remote head contained 28. The earlier 66-file inventory is not an inventory of the current local continuation and must be reconciled by behavioral review; file counts alone do not establish completeness.
- New merges during this continuation: **none**. Historical #808 merged as `dccd254467b6f89542441ea4c726f24f1c0b49b8`; this is evidence for #807's supersession, not a new merge.

## Work and verification

| Area | Status | Evidence / gap |
|---|---|---|
| Coverage threshold | PASS, targeted local | Exact integer comparisons and 94.95%, 94.99%, 95.00%, higher and precision regressions passed on `19456135f22bc8f2c57f05b3ca152766357249a7`. No fresh whole-repository exact-final coverage: **NOT_RUN**. |
| Persistence | BLOCKED | Real local PostgreSQL/Redis targeted tests, acknowledgement-loss and race tests passed on the SHA recorded in `PERSISTENCE_RELIABILITY_ACCEPTANCE.md`. New prune concurrency regression reproduced a failure and passed 10 repetitions after fix on `ce7eb49`; full NexaFlow race suite and vet also passed with temporary dependency replacements. Power loss, comprehensive failover and mixed-version compatibility remain unverified. |
| Canonical verification | BLOCKED | `./scripts/verify.sh` attempted on `ce7eb49` with `GOPROXY=off`. Early checks passed, but uncached locked dependencies caused package setup failures and the command timed out. The full suite did not complete. |
| Historical PRs | BLOCKED | All 79 initially inventoried in `HISTORICAL_PR_DISPOSITION_2026-11-02.md`; #807 behaviorally evidenced as superseded by #808, 78 await behavioral disposition. Blob comparisons do not prove functional equivalence. |
| Backend and security | BLOCKED | Some persistence and release-path fixes are local; a comprehensive subsystem audit, actual CodeQL alert comparison and exact-head CI are outstanding. Ruleset `26200123` was confirmed active with nine required checks. Publication-control gaps remain. |
| Frontend/dashboard | BLOCKED | Local chart and scanner changes plus #823 review patch are not proof of fixes on #823. Full page/control, accessibility, localization, mobile and real-browser acceptance remains outstanding. |
| External issues #646–654 | BLOCKED | Real authorized providers/IdP/Vault/Kubernetes/ComfyUI/HA and final-candidate coverage/soak required. No local fixture represents live acceptance. All must remain open until their actual criteria are met. |
| Two-hour final-RC soak | NOT_RUN | Prior smoke tests are not a two-hour soak. |
| Independent review / merge / release | BLOCKED | No independent approval or authorized final gate. #822/#824 release-policy conflict requires an owner decision. No tag, deployment or publication has been authorized. |

## Integration access and next verification

The GitHub App previously rejected a fast-forward containing workflow changes. The owner should reconnect/authorize **Contents: Read and write** and **Workflows: Read and write**; **Actions: Read and write** is needed for manual dispatch. After authorization, fetch and compare current remote refs, preserve history, fast-forward only if still safe, and run all required checks on the actual resulting remote head. Do not force-push or bypass restrictions. #823 must be fixed on its own existing branch or an explicitly documented successor; the local patch is not landed evidence. Obtain genuine independent review before any authorized merge, then rerun applicable checks on the exact integration/main SHA.

### Recovered snapshot continuation — 2026-11-02

Environment reset left HEAD at `436c4e9` and retained the prior work as uncommitted files, while prior reported local objects including `ce7eb49` and `39e8ba5` were absent. Snapshot `2aa28d2` and merge `178c29d` preserve the recovered files and actual #824 remote history; they do not recreate the missing commit identities. See `PERSISTENCE_RELIABILITY_ACCEPTANCE.md` for backups and exact test scope. Latest tested code SHA `265468b19762657a89a50e04ab8ddad59281846c` includes a red/green null-record corruption fix and a red/green directory-close failure fix. Targeted real disposable PostgreSQL/Redis races and full NexaFlow package race pass on that SHA using temporary replacement modules; this is not locked-dependency full CI. All work remains local. Remote #824 verify remains failed and release remains **BLOCKED**. Use `git rev-parse HEAD` for the current documentation-inclusive local SHA.

### Checksum-verified local run — 2026-11-02

At `bb01781d717a8d88be1fa76f31160600ae6f027a`, canonical verify exited 0 but explicitly skipped browser acceptance; a separate real-browser run subsequently passed four suites on that SHA using Chromium 153 rather than the CI-pinned browser. Fresh local Go coverage passed at 42,865/44,474 (96.38215587%, profile SHA-256 `c52778a5332d55695339c3b9b18d5fd03920ba91ddf068e84fd12a4444070611`). Locked dependencies were checksum-verified against repository go.sum via a reconstructed temporary local proxy, not official proxy/SumDB; selected real disposable PostgreSQL/Redis race tests and 20 repetitions of the previous reconciliation regression passed. See the persistence acceptance report for details. These results are not remote CI or evidence for a later SHA. Current local commit objects and backup reconstruction are documented there; original previously reported local objects are unavailable in this checkout. Release remains BLOCKED.
