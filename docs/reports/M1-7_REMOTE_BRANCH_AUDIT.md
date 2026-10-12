# M1-7 — Remote Branch Containment Audit

**Date:** 2026-10-11

**State:** DONE — all snapshot remote heads audited; no pre-existing remote branch deleted or modified

## Snapshot and method

Snapshot (UTC): `2026-10-11T08:48:33+00:00`. The inventory contains **42** remote heads, including `main`; the base is `origin/main` at `dcb9f9b3c0352d23f1dfa599ea58450b072406c0` with tree `d7ec0aa6741421f493890d3254a4f3640e71ab02`. The open-PR map contains 15 PRs (queried at 2026-10-11T08:48:19Z).

For each name returned by `git ls-remote --heads origin`, the fetched remote-tracking ref was required to match the remote SHA before comparison. `Tip ancestor = YES` means `git merge-base --is-ancestor origin/<branch> origin/main` succeeded, which proves the branch tip commit is contained in main history. `Tree = main` compares the two commit tree hashes. The `Main-only` and `Branch-only` columns are the first and second counts from `git rev-list --left-right --count origin/main...origin/<branch>` (behind and unique-to-branch commits respectively).

**Summary:** 15 tips (including `main`) are ancestors of `main`; 27 are not. Only `main` has a tree identical to `main`. 15 branch heads have open PRs. There are **14 deletion candidates** that are fully contained in `main` and have no open PR; 12 uncontained branches without an open PR are retained for owner review.

The `completion/m1-7-remote-branch-audit` row records its remote tip at the audit snapshot (`2b941d3`), before this evidence commit advances the branch. It is included in the 42-head snapshot and is protected by open PR #261.

## Branch-by-branch evidence

| Remote branch | Tip SHA | Tip ancestor of main | Tree equals main | Main-only | Branch-only | Open PR | Recommendation |
|---|---|---:|---:|---:|---:|---|---|
| `chore/completion-checklist` | `a20be0e855bf` | NO | NO | 0 | 5 | [#250](https://github.com/ali-shortcuts/nexaroute/pull/250) | Keep — open PR |
| `chore/m1-dependency-audit` | `e27609e06134` | NO | NO | 5 | 1 | — | Retain/review; unique commits |
| `chore/video-gateway-audit-hardening` | `376cd105b23d` | YES | NO | 92 | 0 | — | Propose deletion; fully contained |
| `ci/m1-pin-github-actions` | `8e33f7a42fe7` | NO | NO | 5 | 1 | — | Retain/review; unique commits |
| `completion/checklist-bootstrap` | `61ed040f6436` | NO | NO | 0 | 7 | [#254](https://github.com/ali-shortcuts/nexaroute/pull/254) | Keep — open PR |
| `completion/m1-1-coverage` | `46d513a453a1` | NO | NO | 0 | 2 | [#255](https://github.com/ali-shortcuts/nexaroute/pull/255) | Keep — open PR |
| `completion/m1-2-competitor-matrix` | `cee533ebf342` | NO | NO | 0 | 2 | [#256](https://github.com/ali-shortcuts/nexaroute/pull/256) | Keep — open PR |
| `completion/m1-3-benchmark-harness` | `063039a069ba` | NO | NO | 0 | 2 | [#257](https://github.com/ali-shortcuts/nexaroute/pull/257) | Keep — open PR |
| `completion/m1-4-hotspot-decision` | `90cd218e4ab7` | NO | NO | 0 | 2 | [#258](https://github.com/ali-shortcuts/nexaroute/pull/258) | Keep — open PR |
| `completion/m1-5-default-concurrency-test` | `a7113ca53a0a` | NO | NO | 0 | 2 | [#259](https://github.com/ali-shortcuts/nexaroute/pull/259) | Keep — open PR |
| `completion/m1-6-release-dry-run` | `841d31c7e013` | NO | NO | 0 | 4 | [#260](https://github.com/ali-shortcuts/nexaroute/pull/260) | Keep — open PR |
| `completion/m1-7-remote-branch-audit` | `2b941d3610b1` | NO | NO | 0 | 1 | [#261](https://github.com/ali-shortcuts/nexaroute/pull/261) | Keep — open PR |
| `docs/completion-checklist-initial` | `e02044ef21ed` | NO | NO | 0 | 3 | [#247](https://github.com/ali-shortcuts/nexaroute/pull/247) | Keep — open PR |
| `docs/m1-1-coverage` | `4030c85a4709` | NO | NO | 0 | 1 | [#251](https://github.com/ali-shortcuts/nexaroute/pull/251) | Keep — open PR |
| `docs/m1-2-competitor-evidence` | `01eb3b90ebb6` | NO | NO | 0 | 1 | [#252](https://github.com/ali-shortcuts/nexaroute/pull/252) | Keep — open PR |
| `docs/m1-remote-branch-audit` | `6a80f872c0c2` | NO | NO | 5 | 1 | — | Retain/review; unique commits |
| `docs/phase1-reality` | `3a02525223e4` | YES | NO | 71 | 0 | — | Propose deletion; fully contained |
| `fix/gateway-shutdown-lifecycle` | `07cad291cd5a` | YES | NO | 23 | 0 | — | Propose deletion; fully contained |
| `fix/video-runtime-p0-hardening` | `8b6bf134dfe0` | NO | NO | 44 | 25 | — | Retain/review; unique commits |
| `fix/video-runtime-p0-hardening-v2` | `88e965c6e19c` | NO | NO | 42 | 26 | — | Retain/review; unique commits |
| `fix/virtual-key-policy-intersection` | `f5f415f33d8f` | YES | NO | 84 | 0 | — | Propose deletion; fully contained |
| `main` | `dcb9f9b3c035` | YES | YES | 0 | 0 | — | Baseline |
| `opencode/issue232-20261010142021` | `1bb893d64183` | NO | NO | 9 | 1 | — | Retain/review; unique commits |
| `opencode/issue233-20261010142842` | `fa318f0584e2` | NO | NO | 8 | 1 | — | Retain/review; unique commits |
| `opencode/issue234-20261010142842` | `4c1cb7ecf9af` | NO | NO | 8 | 3 | — | Retain/review; unique commits |
| `opencode/issue242-20261011032653` | `e4ab8bed542d` | NO | NO | 2 | 1 | — | Retain/review; unique commits |
| `opencode/issue243-20261011032652` | `4a04ac9f43e9` | NO | NO | 2 | 1 | — | Retain/review; unique commits |
| `opencode/issue246-20261011033909` | `b0537ae72bd6` | NO | NO | 0 | 1 | [#248](https://github.com/ali-shortcuts/nexaroute/pull/248) | Keep — open PR |
| `perf/m1-3-benchmark-harness` | `86312608d72d` | NO | NO | 0 | 1 | [#253](https://github.com/ali-shortcuts/nexaroute/pull/253) | Keep — open PR |
| `phase0-baseline` | `d6a090ba0d70` | NO | NO | 98 | 4 | — | Retain/review; unique commits |
| `phase1-coverage-85` | `f263e9487e19` | YES | NO | 69 | 0 | — | Propose deletion; fully contained |
| `phase1-coverage-on-1b` | `0b23b91f0bd6` | NO | NO | 84 | 2 | — | Retain/review; unique commits |
| `test/m1-1-current-coverage` | `d530de095f40` | NO | NO | 0 | 1 | [#249](https://github.com/ali-shortcuts/nexaroute/pull/249) | Keep — open PR |
| `wp1-virtual-key-policy` | `1a503e2d7b2b` | YES | NO | 52 | 0 | — | Propose deletion; fully contained |
| `wp2-video-gateway` | `f5b74e6e7c69` | YES | NO | 45 | 0 | — | Propose deletion; fully contained |
| `wp2b-video-hardening` | `fce6214530eb` | YES | NO | 40 | 0 | — | Propose deletion; fully contained |
| `wp4-rbac-sso` | `a3d6635413f3` | YES | NO | 43 | 0 | — | Propose deletion; fully contained |
| `wp4-rbac-sso-runtime` | `2a1d9f121157` | YES | NO | 25 | 0 | — | Propose deletion; fully contained |
| `wp5-egress-policy` | `81b888c327d8` | YES | NO | 21 | 0 | — | Propose deletion; fully contained |
| `wp6-controlplane-reconcile` | `86463043440e` | YES | NO | 19 | 0 | — | Propose deletion; fully contained |
| `wp6-runtime-wiring` | `ac6517e3c3c5` | YES | NO | 10 | 0 | — | Propose deletion; fully contained |
| `wp7-key-custody` | `c4fb68ea6b98` | YES | NO | 16 | 0 | — | Propose deletion; fully contained |

## Deletion proposals — no deletions performed

- `chore/video-gateway-audit-hardening` — tip `376cd105b23d` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.
- `docs/phase1-reality` — tip `3a02525223e4` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.
- `fix/gateway-shutdown-lifecycle` — tip `07cad291cd5a` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.
- `fix/virtual-key-policy-intersection` — tip `f5f415f33d8f` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.
- `phase1-coverage-85` — tip `f263e9487e19` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.
- `wp1-virtual-key-policy` — tip `1a503e2d7b2b` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.
- `wp2-video-gateway` — tip `f5b74e6e7c69` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.
- `wp2b-video-hardening` — tip `fce6214530eb` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.
- `wp4-rbac-sso` — tip `a3d6635413f3` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.
- `wp4-rbac-sso-runtime` — tip `2a1d9f121157` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.
- `wp5-egress-policy` — tip `81b888c327d8` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.
- `wp6-controlplane-reconcile` — tip `86463043440e` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.
- `wp6-runtime-wiring` — tip `ac6517e3c3c5` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.
- `wp7-key-custody` — tip `c4fb68ea6b98` is an ancestor of `main`; no open PR references this branch. Proposed for deletion after operator review.

## Retain for review

- `chore/m1-dependency-audit` — tip `e27609e06134` has 1 branch-only commit(s), tree equal to main: NO; no open PR. Do not delete until an owner reviews the unique history.
- `ci/m1-pin-github-actions` — tip `8e33f7a42fe7` has 1 branch-only commit(s), tree equal to main: NO; no open PR. Do not delete until an owner reviews the unique history.
- `docs/m1-remote-branch-audit` — tip `6a80f872c0c2` has 1 branch-only commit(s), tree equal to main: NO; no open PR. Do not delete until an owner reviews the unique history.
- `fix/video-runtime-p0-hardening` — tip `8b6bf134dfe0` has 25 branch-only commit(s), tree equal to main: NO; no open PR. Do not delete until an owner reviews the unique history.
- `fix/video-runtime-p0-hardening-v2` — tip `88e965c6e19c` has 26 branch-only commit(s), tree equal to main: NO; no open PR. Do not delete until an owner reviews the unique history.
- `opencode/issue232-20261010142021` — tip `1bb893d64183` has 1 branch-only commit(s), tree equal to main: NO; no open PR. Do not delete until an owner reviews the unique history.
- `opencode/issue233-20261010142842` — tip `fa318f0584e2` has 1 branch-only commit(s), tree equal to main: NO; no open PR. Do not delete until an owner reviews the unique history.
- `opencode/issue234-20261010142842` — tip `4c1cb7ecf9af` has 3 branch-only commit(s), tree equal to main: NO; no open PR. Do not delete until an owner reviews the unique history.
- `opencode/issue242-20261011032653` — tip `e4ab8bed542d` has 1 branch-only commit(s), tree equal to main: NO; no open PR. Do not delete until an owner reviews the unique history.
- `opencode/issue243-20261011032652` — tip `4a04ac9f43e9` has 1 branch-only commit(s), tree equal to main: NO; no open PR. Do not delete until an owner reviews the unique history.
- `phase0-baseline` — tip `d6a090ba0d70` has 4 branch-only commit(s), tree equal to main: NO; no open PR. Do not delete until an owner reviews the unique history.
- `phase1-coverage-on-1b` — tip `0b23b91f0bd6` has 2 branch-only commit(s), tree equal to main: NO; no open PR. Do not delete until an owner reviews the unique history.

Branches with open PRs are marked **Keep** in the table. The report proposes cleanup only for tips proven to be in main history and without an open PR. No pre-existing remote branch was changed. **No GitHub branch-delete API, `git push --delete`, or other branch-deletion operation was run.**

## Verification

`./scripts/verify.sh` — **VERIFY PASS** on the M1-7 branch, including the clean test pass, race pass, vet, browser acceptance, fuzz checks, and linux amd64/arm64 builds. Raw output is committed below.

## Evidence files

- `docs/reports/m1-7-remote-branch-audit.raw.tsv` — complete snapshot, all 42 full tip SHAs, ancestry/tree hashes, ahead/behind counts, PR mapping, and proposals.
- `docs/reports/m1-7-verify.raw.txt` — complete `verify.sh` output ending in `VERIFY PASS`.
- `docs/reports/M1-7_REMOTE_BRANCH_AUDIT.md` — this summary and complete review table.
