# M1 remote-branch containment audit

**Audit date:** 2026-10-11  
**Repository:** `ali-shortcuts/nexaroute`  
**Base:** `origin/main` at `95924c572031f91311bbf817f970704d924e9386`  
**Method:** fetched/pruned `origin`, then tested whether each remote branch tip is an ancestor of `origin/main` using `git merge-base --is-ancestor <ref> origin/main`. Symbolic `origin/HEAD` and the `origin` symbolic alias are not branch tips and are excluded.

This is a point-in-time listing of remote refs. **No branch was deleted.** A branch being fully contained proves only that its current commit is reachable from main; it does not prove that no operator still needs the branch. The deletion column is a proposal for an owner to review, not an action taken.

## Remote branches fully contained in main

| Remote branch | Tip at audit | Proposal |
|---|---|---|
| `origin/chore/video-gateway-audit-hardening` | `376cd10` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/docs/phase1-reality` | `3a02525` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/fix/gateway-shutdown-lifecycle` | `07cad29` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/fix/virtual-key-policy-intersection` | `f5f415f` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/phase1-coverage-85` | `f263e94` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/wp1-virtual-key-policy` | `1a503e2` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/wp2-video-gateway` | `f5b74e6` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/wp2b-video-hardening` | `fce6214` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/wp4-rbac-sso` | `a3d6635` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/wp4-rbac-sso-runtime` | `2a1d9f1` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/wp5-egress-policy` | `81b888c` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/wp6-controlplane-reconcile` | `8646304` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/wp6-runtime-wiring` | `ac6517e` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/wp7-key-custody` | `c4fb68e` | Candidate for deletion after owner confirms it is no longer needed. |
| `origin/main` | `95924c5` | Keep; this is the audited base branch. |

**Count:** 15 contained refs including main; 14 non-main branch refs are deletion candidates subject to owner review.

## Branches not fully contained in main

These branches are not recommended for deletion by this audit. They may contain work not reachable from main; resolve their status with the owner before any cleanup.

| Remote branch | Tip at audit |
|---|---|
| `origin/chore/m1-dependency-audit` | `e27609e` — draft PR #239, current milestone work |
| `origin/fix/video-runtime-p0-hardening` | `8b6bf13` |
| `origin/fix/video-runtime-p0-hardening-v2` | `88e965c` |
| `origin/opencode/issue232-20261010142021` | `1bb893d` |
| `origin/opencode/issue233-20261010142842` | `fa318f0` |
| `origin/opencode/issue234-20261010142842` | `4c1cb7e` |
| `origin/phase0-baseline` | `d6a090b` |
| `origin/phase1-coverage-on-1b` | `0b23b91` |

**Count:** 8 non-contained branch refs, including the in-progress M1 PR branch.

## Safe cleanup follow-up

If cleanup is later authorized, re-fetch first, re-run the ancestry check against the then-current main, inspect associated pull-request/issue state and branch protection, and delete only individually approved refs. This report itself performs no remote mutation.
