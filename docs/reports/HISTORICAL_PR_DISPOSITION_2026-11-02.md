# Historical PR disposition inventory — 2026-11-02

Baseline main: `436c4e98618eed0c41e59059eae7eca5f05dd190`. GitHub GraphQL `pullRequest` metadata queried for all 79 numbers on 2026-11-02; each returned `CLOSED`, `merged=false`, no merge commit. A closed/unmerged status alone does not establish whether behavior landed by another route.

**Status: in progress.** Rows marked blocked require behavior-level comparison against the stated main, regression evidence, and an authorized decision where recovery or rejection is needed. Successors and equivalence are not inferred from title or ancestry alone.

Cross-branch recovery, creating a new branch, PR merge, and issue closure are outside the current session constraints; an authorized operator must perform those actions after review.

| PR | GitHub title | Disposition | Specific evidence still needed |
|---|---|---|---|
| [#814](https://github.com/ali-shortcuts/nexaroute/pull/814) | docs: refresh release-readiness evidence through 2026-11-01 | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#811](https://github.com/ali-shortcuts/nexaroute/pull/811) | fix: K8s acceptance readiness polling and termination timing | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#807](https://github.com/ali-shortcuts/nexaroute/pull/807) | fix: OIDC abort summary and Vault post-revocation survival check | SUPERSEDED_BY_PR | [#808](https://github.com/ali-shortcuts/nexaroute/pull/808) merged as `dccd254467b6f89542441ea4c726f24f1c0b49b8`, ancestor of main `436c4e9` (GitHub compare: ahead, behind_by 0). The five OIDC/Vault source, test and report blobs match #807 on main; `scripts/verify.sh` retains the self-test and adds the abort regression. Local OIDC interrupt regression and Vault policy/survival harness self-tests pass. These offline tests are not external IdP or Vault acceptance. |
| [#781](https://github.com/ali-shortcuts/nexaroute/pull/781) | nexaflow: wire production gateway to AttachNexaflow behind opt-in flag (issue #765, candidate — hold for owner choice) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#779](https://github.com/ali-shortcuts/nexaroute/pull/779) | nexaflow: wire production gateway to AttachNexaflow behind opt-in flag (issue #765) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#771](https://github.com/ali-shortcuts/nexaroute/pull/771) | NexaFlow wiring pushed; tests pass, PR pending. | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#770](https://github.com/ali-shortcuts/nexaroute/pull/770) | docs: umbrella matrix + branch protection proposal (issue 654, mission G; draft, nothing applied) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#768](https://github.com/ali-shortcuts/nexaroute/pull/768) | docs: issue664 soak-on-7daa36d dispatch record + coverage-gate verification (results pending) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#761](https://github.com/ali-shortcuts/nexaroute/pull/761) | nexaflow: reconcile #733 studio port onto current main (successor to #733) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#757](https://github.com/ali-shortcuts/nexaroute/pull/757) | ci: enforce 95% Go coverage gate with pinned Go 1.27.3 | BLOCKED_PENDING_DEPENDENCY | PR head `0b2b9026fe92eae4e2047100ff033457f7f44da2` proposes a Python validator and 16-case suite. Main `436c4e9` instead has `scripts/coverage-gate.sh` plus tests, pins coverage Go 1.27.3 and requires Fresh Go coverage. They are not byte-identical; equivalence of malformed/mismatched profiles and successor merge identity remain unproven. Main's gate incorrectly passed 1899/2000 statements due to rounded output; a local follow-up fixes that case but is not on main. |
| [#753](https://github.com/ali-shortcuts/nexaroute/pull/753) | docs: update KNOWN_GAPS coverage measurement to main 2c5f583 (95.63% CI) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#747](https://github.com/ali-shortcuts/nexaroute/pull/747) | docs: update KNOWN_GAPS coverage measurement to main 8d61086 (93.0%) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#740](https://github.com/ali-shortcuts/nexaroute/pull/740) | test: wave-2 coverage to 95% + Wave3 502->504 fix (related to #663) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#733](https://github.com/ali-shortcuts/nexaroute/pull/733) | nexaflow: port #690 studio UX onto the canonical #693 API (no second engine) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#716](https://github.com/ali-shortcuts/nexaroute/pull/716) | test: repair stale signatures and failure expectations | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#706](https://github.com/ali-shortcuts/nexaroute/pull/706) | fix(httpapi): record native Anthropic SSE usage incl. cache tokens | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#694](https://github.com/ali-shortcuts/nexaroute/pull/694) | feat: synthesize x-opencode-session for opted-in OpenCode providers | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#691](https://github.com/ali-shortcuts/nexaroute/pull/691) | nexaflow: phase 8 end-to-end runtime, durable store, admin API, UI | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#690](https://github.com/ali-shortcuts/nexaroute/pull/690) | nexaflow: visual workflow studio (Phase 6) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#688](https://github.com/ali-shortcuts/nexaroute/pull/688) | nexaflow: Phase 5 integration — runtime engine, completed adapters, instantiable templates | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#672](https://github.com/ali-shortcuts/nexaroute/pull/672) | docs: revise ADR 0007 Vault Transit envelope design from security review | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#671](https://github.com/ali-shortcuts/nexaroute/pull/671) | OIDC operator runbook + secret-free live preflight; live tenant BLOCKED | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#666](https://github.com/ali-shortcuts/nexaroute/pull/666) | OIDC local tests expanded; live-tenant checklist BLOCKED; all checks pass. | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#655](https://github.com/ali-shortcuts/nexaroute/pull/655) | docs: propose Vault Transit envelope-key architecture | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#522](https://github.com/ali-shortcuts/nexaroute/pull/522) | fix(i18n): translate task labels in Activity feed | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#517](https://github.com/ali-shortcuts/nexaroute/pull/517) | feat(provider): truthful preset catalog and model onboarding (#394) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#514](https://github.com/ali-shortcuts/nexaroute/pull/514) | Supervisor takeover: deterministic flake fixes + formal 50+5 acceptance gate (#404) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#499](https://github.com/ali-shortcuts/nexaroute/pull/499) | feat(autopilot): canary rollback with fail-closed signal coverage | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#491](https://github.com/ali-shortcuts/nexaroute/pull/491) | feat(provider): Phase 2 provider catalog and model onboarding (#394) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#464](https://github.com/ali-shortcuts/nexaroute/pull/464) | audit: harden universal video plan parsing and relative renders | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#456](https://github.com/ali-shortcuts/nexaroute/pull/456) | Restore #451 remainder: chat system-image drop + #447 regression pins (closed unmerged) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#451](https://github.com/ali-shortcuts/nexaroute/pull/451) | Fix Responses response decoder empty-arguments drift (#447 follow-up) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#369](https://github.com/ali-shortcuts/nexaroute/pull/369) | fix(release): SBOM gate falsely rejects unlinked go.mod modules — v0.22.0 dry-run blocker | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#356](https://github.com/ali-shortcuts/nexaroute/pull/356) | fix(web): repair app.js tail broken by the #342/#344 landings | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#318](https://github.com/ali-shortcuts/nexaroute/pull/318) | feat: M4-2a PostgreSQL security store adapter (sessions + audit chain) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#317](https://github.com/ali-shortcuts/nexaroute/pull/317) | feat: M4-6b opt-in OTLP span export with GenAI attributes | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#306](https://github.com/ali-shortcuts/nexaroute/pull/306) | fix: harden per-client TPM admission and retry guidance | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#281](https://github.com/ali-shortcuts/nexaroute/pull/281) | fix(egress): close SSRF gaps and add table-driven matrix (M3-4, part 1) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#280](https://github.com/ali-shortcuts/nexaroute/pull/280) | fix(privacy): route token counts through virtual policies | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#273](https://github.com/ali-shortcuts/nexaroute/pull/273) | release: v0.17.1 integration (fix #246, release hardening, tests, evidence) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#253](https://github.com/ali-shortcuts/nexaroute/pull/253) | perf(bench): repair gateway benchmark for c=256 | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#252](https://github.com/ali-shortcuts/nexaroute/pull/252) | docs: verify competitor matrix evidence | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#251](https://github.com/ali-shortcuts/nexaroute/pull/251) | test: record current aggregate coverage result | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#250](https://github.com/ali-shortcuts/nexaroute/pull/250) | docs: add full completion checklist | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#249](https://github.com/ali-shortcuts/nexaroute/pull/249) | test: record current aggregate coverage result | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#247](https://github.com/ali-shortcuts/nexaroute/pull/247) | docs: start full completion checklist | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#225](https://github.com/ali-shortcuts/nexaroute/pull/225) | fix(video): harden authentication, job recovery, outputs, and budgets | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#214](https://github.com/ali-shortcuts/nexaroute/pull/214) | Fix client auth preservation for large request bodies | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#212](https://github.com/ali-shortcuts/nexaroute/pull/212) | Phase 1b: built-in TLS/mTLS, Admin CSRF, and config CLI | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#209](https://github.com/ali-shortcuts/nexaroute/pull/209) | Upgrade gateway Connect UX and API endpoint visibility | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#207](https://github.com/ali-shortcuts/nexaroute/pull/207) | rebuild: complete v0.14.0 product correction UI | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#203](https://github.com/ali-shortcuts/nexaroute/pull/203) | F1 render isolation done, VERIFY PASS | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#202](https://github.com/ali-shortcuts/nexaroute/pull/202) | fix(frontend): teardown dashboard resources on pagehide | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#201](https://github.com/ali-shortcuts/nexaroute/pull/201) | fix(provider): replace-key UX on current main | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#196](https://github.com/ali-shortcuts/nexaroute/pull/196) | Issue #81 routing benchmark done | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#194](https://github.com/ali-shortcuts/nexaroute/pull/194) | Issue #175 done: lifecycle teardown on pagehide. All checks pass. PR opened. | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#192](https://github.com/ali-shortcuts/nexaroute/pull/192) | B5b provider replace-key UX implemented & tested | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#181](https://github.com/ali-shortcuts/nexaroute/pull/181) | Coverage up in 4/5 pkgs; gateway blocked | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#71](https://github.com/ali-shortcuts/nexaroute/pull/71) | fix: audit findings batch 1 (top-15 list) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#70](https://github.com/ali-shortcuts/nexaroute/pull/70) | feat(dashboard): telemetry panels, node popover, readable tool errors (B3-B4, B6) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#68](https://github.com/ali-shortcuts/nexaroute/pull/68) | feat: SSE resume tests, routing benchmarks, competitor comparison (A9-A11) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#67](https://github.com/ali-shortcuts/nexaroute/pull/67) | fix: provider edit shows header names and proxy structure without secrets (B5) | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#65](https://github.com/ali-shortcuts/nexaroute/pull/65) | test: failover and resilience evidence A1-A6 | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#46](https://github.com/ali-shortcuts/nexaroute/pull/46) | docs(v0.12.0): add traceability matrix, security review, event stream contract, and operations manual | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#40](https://github.com/ali-shortcuts/nexaroute/pull/40) | UI/UX: NexaRoute Control Plane v2 | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#38](https://github.com/ali-shortcuts/nexaroute/pull/38) | WIP: Control Plane v2 — provider-first UX and simple routing | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#35](https://github.com/ali-shortcuts/nexaroute/pull/35) | Integrate deep audit fixes into final hardening | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#34](https://github.com/ali-shortcuts/nexaroute/pull/34) | WIP: add model resilience supervisor | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#31](https://github.com/ali-shortcuts/nexaroute/pull/31) | Fix compatibility tests using internal model IDs | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#26](https://github.com/ali-shortcuts/nexaroute/pull/26) | Fix Responses-native routing and reload/cache correctness | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#21](https://github.com/ali-shortcuts/nexaroute/pull/21) | Fix lossy stateful Responses routing | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#20](https://github.com/ali-shortcuts/nexaroute/pull/20) | Fix hedge attempt accounting and candidate skipping | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#17](https://github.com/ali-shortcuts/nexaroute/pull/17) | Fix v0.6 runtime version and protocol diagnostics | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#16](https://github.com/ali-shortcuts/nexaroute/pull/16) | Fix hedge attempt accounting and candidate skipping | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#13](https://github.com/ali-shortcuts/nexaroute/pull/13) | Beta 0.6.1-beta.3: unified endpoint, client keys and Ubuntu launcher | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#12](https://github.com/ali-shortcuts/nexaroute/pull/12) | Fix universal installer and release workflow | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#10](https://github.com/ali-shortcuts/nexaroute/pull/10) | Fix hedged-race latency, UI preset/edit regressions, version drift and dead code | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#8](https://github.com/ali-shortcuts/nexaroute/pull/8) | Add exact routed usage accounting and pricing telemetry | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |
| [#5](https://github.com/ali-shortcuts/nexaroute/pull/5) | NexaRoute v0.3: guardrails, power UI, upstream logical errors, one-command installer | BLOCKED_PENDING_DEPENDENCY | Compare the change described by this PR against exact main; establish behavioral equivalence or identify a safe successor and tests. |

## File-level comparison supplement

GitHub `pulls/{number}/files` was queried with pagination for each of the 79
PRs; returned file counts and PR heads were cross-checked against GitHub
GraphQL `changedFiles` and `headRefOid`. No count/head mismatch was observed.
Each returned non-removed PR file blob was compared with the same path in the
exact main tree `436c4e98618eed0c41e59059eae7eca5f05dd190`. The raw path,
PR blob, main blob (or null), file status and match flag are recorded in
`HISTORICAL_PR_FILE_COMPARISON_2026-11-02.json`. 214 of 913 entries matched;
no PR had all entries match. Removed files are conservatively recorded as
non-matches, not proof a deletion was not carried forward. A non-match does
not mean a feature is missing: later edits or independent implementations may
preserve behavior. A blob match also does not prove integration, execution or
review. Thus this scan narrows the remaining work but does **not** establish
any final disposition.

| PR | Changed file entries | Identical current-path blobs | Still need behavior review |
|---|---:|---:|---|
| [#814](https://github.com/ali-shortcuts/nexaroute/pull/814) | 1 | 0 | Yes |
| [#811](https://github.com/ali-shortcuts/nexaroute/pull/811) | 1 | 0 | Yes |
| [#807](https://github.com/ali-shortcuts/nexaroute/pull/807) | 6 | 5 | Completed for this PR; see successor evidence above |
| [#781](https://github.com/ali-shortcuts/nexaroute/pull/781) | 9 | 0 | Yes |
| [#779](https://github.com/ali-shortcuts/nexaroute/pull/779) | 8 | 0 | Yes |
| [#771](https://github.com/ali-shortcuts/nexaroute/pull/771) | 14 | 3 | Yes |
| [#770](https://github.com/ali-shortcuts/nexaroute/pull/770) | 2 | 0 | Yes |
| [#768](https://github.com/ali-shortcuts/nexaroute/pull/768) | 1 | 0 | Yes |
| [#761](https://github.com/ali-shortcuts/nexaroute/pull/761) | 14 | 12 | Yes |
| [#757](https://github.com/ali-shortcuts/nexaroute/pull/757) | 3 | 0 | Yes |
| [#753](https://github.com/ali-shortcuts/nexaroute/pull/753) | 1 | 0 | Yes |
| [#747](https://github.com/ali-shortcuts/nexaroute/pull/747) | 1 | 0 | Yes |
| [#740](https://github.com/ali-shortcuts/nexaroute/pull/740) | 27 | 18 | Yes |
| [#733](https://github.com/ali-shortcuts/nexaroute/pull/733) | 13 | 7 | Yes |
| [#716](https://github.com/ali-shortcuts/nexaroute/pull/716) | 6 | 4 | Yes |
| [#706](https://github.com/ali-shortcuts/nexaroute/pull/706) | 0 | 0 | Yes |
| [#694](https://github.com/ali-shortcuts/nexaroute/pull/694) | 9 | 0 | Yes |
| [#691](https://github.com/ali-shortcuts/nexaroute/pull/691) | 42 | 4 | Yes |
| [#690](https://github.com/ali-shortcuts/nexaroute/pull/690) | 42 | 16 | Yes |
| [#688](https://github.com/ali-shortcuts/nexaroute/pull/688) | 12 | 0 | Yes |
| [#672](https://github.com/ali-shortcuts/nexaroute/pull/672) | 1 | 0 | Yes |
| [#671](https://github.com/ali-shortcuts/nexaroute/pull/671) | 3 | 2 | Yes |
| [#666](https://github.com/ali-shortcuts/nexaroute/pull/666) | 4 | 0 | Yes |
| [#655](https://github.com/ali-shortcuts/nexaroute/pull/655) | 1 | 0 | Yes |
| [#522](https://github.com/ali-shortcuts/nexaroute/pull/522) | 1 | 0 | Yes |
| [#517](https://github.com/ali-shortcuts/nexaroute/pull/517) | 10 | 2 | Yes |
| [#514](https://github.com/ali-shortcuts/nexaroute/pull/514) | 18 | 9 | Yes |
| [#499](https://github.com/ali-shortcuts/nexaroute/pull/499) | 25 | 6 | Yes |
| [#491](https://github.com/ali-shortcuts/nexaroute/pull/491) | 11 | 1 | Yes |
| [#464](https://github.com/ali-shortcuts/nexaroute/pull/464) | 14 | 7 | Yes |
| [#456](https://github.com/ali-shortcuts/nexaroute/pull/456) | 4 | 1 | Yes |
| [#451](https://github.com/ali-shortcuts/nexaroute/pull/451) | 5 | 1 | Yes |
| [#369](https://github.com/ali-shortcuts/nexaroute/pull/369) | 2 | 0 | Yes |
| [#356](https://github.com/ali-shortcuts/nexaroute/pull/356) | 3 | 0 | Yes |
| [#318](https://github.com/ali-shortcuts/nexaroute/pull/318) | 11 | 4 | Yes |
| [#317](https://github.com/ali-shortcuts/nexaroute/pull/317) | 17 | 8 | Yes |
| [#306](https://github.com/ali-shortcuts/nexaroute/pull/306) | 7 | 0 | Yes |
| [#281](https://github.com/ali-shortcuts/nexaroute/pull/281) | 16 | 9 | Yes |
| [#280](https://github.com/ali-shortcuts/nexaroute/pull/280) | 3 | 0 | Yes |
| [#273](https://github.com/ali-shortcuts/nexaroute/pull/273) | 86 | 67 | Yes |
| [#253](https://github.com/ali-shortcuts/nexaroute/pull/253) | 23 | 0 | Yes |
| [#252](https://github.com/ali-shortcuts/nexaroute/pull/252) | 1 | 0 | Yes |
| [#251](https://github.com/ali-shortcuts/nexaroute/pull/251) | 1 | 0 | Yes |
| [#250](https://github.com/ali-shortcuts/nexaroute/pull/250) | 1 | 0 | Yes |
| [#249](https://github.com/ali-shortcuts/nexaroute/pull/249) | 2 | 0 | Yes |
| [#247](https://github.com/ali-shortcuts/nexaroute/pull/247) | 1 | 0 | Yes |
| [#225](https://github.com/ali-shortcuts/nexaroute/pull/225) | 20 | 0 | Yes |
| [#214](https://github.com/ali-shortcuts/nexaroute/pull/214) | 4 | 0 | Yes |
| [#212](https://github.com/ali-shortcuts/nexaroute/pull/212) | 20 | 9 | Yes |
| [#209](https://github.com/ali-shortcuts/nexaroute/pull/209) | 4 | 0 | Yes |
| [#207](https://github.com/ali-shortcuts/nexaroute/pull/207) | 4 | 0 | Yes |
| [#203](https://github.com/ali-shortcuts/nexaroute/pull/203) | 2 | 0 | Yes |
| [#202](https://github.com/ali-shortcuts/nexaroute/pull/202) | 2 | 0 | Yes |
| [#201](https://github.com/ali-shortcuts/nexaroute/pull/201) | 5 | 1 | Yes |
| [#196](https://github.com/ali-shortcuts/nexaroute/pull/196) | 2 | 1 | Yes |
| [#194](https://github.com/ali-shortcuts/nexaroute/pull/194) | 2 | 0 | Yes |
| [#192](https://github.com/ali-shortcuts/nexaroute/pull/192) | 4 | 1 | Yes |
| [#181](https://github.com/ali-shortcuts/nexaroute/pull/181) | 7 | 5 | Yes |
| [#71](https://github.com/ali-shortcuts/nexaroute/pull/71) | 20 | 0 | Yes |
| [#70](https://github.com/ali-shortcuts/nexaroute/pull/70) | 10 | 0 | Yes |
| [#68](https://github.com/ali-shortcuts/nexaroute/pull/68) | 5 | 0 | Yes |
| [#67](https://github.com/ali-shortcuts/nexaroute/pull/67) | 4 | 0 | Yes |
| [#65](https://github.com/ali-shortcuts/nexaroute/pull/65) | 8 | 0 | Yes |
| [#46](https://github.com/ali-shortcuts/nexaroute/pull/46) | 4 | 3 | Yes |
| [#40](https://github.com/ali-shortcuts/nexaroute/pull/40) | 3 | 0 | Yes |
| [#38](https://github.com/ali-shortcuts/nexaroute/pull/38) | 7 | 0 | Yes |
| [#35](https://github.com/ali-shortcuts/nexaroute/pull/35) | 13 | 8 | Yes |
| [#34](https://github.com/ali-shortcuts/nexaroute/pull/34) | 22 | 0 | Yes |
| [#31](https://github.com/ali-shortcuts/nexaroute/pull/31) | 2 | 0 | Yes |
| [#26](https://github.com/ali-shortcuts/nexaroute/pull/26) | 29 | 0 | Yes |
| [#21](https://github.com/ali-shortcuts/nexaroute/pull/21) | 5 | 0 | Yes |
| [#20](https://github.com/ali-shortcuts/nexaroute/pull/20) | 4 | 0 | Yes |
| [#17](https://github.com/ali-shortcuts/nexaroute/pull/17) | 2 | 0 | Yes |
| [#16](https://github.com/ali-shortcuts/nexaroute/pull/16) | 4 | 0 | Yes |
| [#13](https://github.com/ali-shortcuts/nexaroute/pull/13) | 46 | 0 | Yes |
| [#12](https://github.com/ali-shortcuts/nexaroute/pull/12) | 9 | 0 | Yes |
| [#10](https://github.com/ali-shortcuts/nexaroute/pull/10) | 74 | 0 | Yes |
| [#8](https://github.com/ali-shortcuts/nexaroute/pull/8) | 21 | 0 | Yes |
| [#5](https://github.com/ali-shortcuts/nexaroute/pull/5) | 62 | 0 | Yes |

**Totals:** 79 metadata-verified closed/unmerged; 78 blocked pending technical disposition; 1 superseded with successor, exact merge and behavior evidence (#807 → #808); 0 recovered or rejected. This inventory is not proof that all historical patches were read or that the mission is complete.
