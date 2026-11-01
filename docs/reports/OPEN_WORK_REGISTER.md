# Open work execution register

GitHub inventory at 2026-11-01; refresh it before acting. It listed **2 open
PRs, 0 drafts and 9 open issues**. No issue or PR has been closed as a result
of this register. This is not a release approval. The existing
[completion checklist](FINAL_COMPLETION_CHECKLIST.md) contains the detailed
acceptance and operator handoff; issue descriptions are authoritative.

## Pull requests

| PR | State, requirements and dependencies | Affected work, evidence and disposition |
|---|---|---|
| [#823](https://github.com/ali-shortcuts/nexaroute/pull/823) | OPEN, non-draft, head `58bacddbc59e8cf9a01bc59620128c93ea2d7e1a`; required PR checks green, no formal review. Supervision held it for chart state/window and scanner findings. Base `436c4e9`. | Overview chart, autopilot/body scanner, gate log and release docs (12 paths). #824 contains the changes with follow-up corrections, but #823 is **not an ancestor** of #824. Owner must review the content, sequencing and complete preservation before its merge or supersession; not closed. |
| [#824](https://github.com/ali-shortcuts/nexaroute/pull/824) | OPEN, non-draft, base `436c4e9`. Previous head `04d54d6329966c37d745cc757fad95fce77f145f` had green PR checks on synthetic merge `8b0235101de1956ad644ebeaf71aceeda1e888d0`; no formal review, and release-candidate evidence is not complete. Later commits need fresh review/checks. | Integrates #823 content plus workflow editor and fail-closed release gates. Current work adds atomic validation-to-publication checks, async editor protection and regression tests. Check the latest head and CI rather than transferring previous evidence. The #822 exclusion path is removed by this #824 follow-up in favor of a completed-only gate for #646–#654. This policy is not active on main until the follow-up merges. Formal review and PR merge sequencing remain outstanding. |

No other open PRs were returned by the paginated open-PR inventory. A clean
mergeability state is not an approval. Security, Dependabot and secret-scanning
alert listing APIs returned HTTP 403 for this integration; their alerts could
not be inventoried. Owner should grant appropriate read access or provide a
sanitized current alert list; never paste secrets into chat or issue comments.

## Issues

All nine remained OPEN. Each row identifies its primary files/workflow and
next action; existing fixture/local evidence is not external acceptance.

| Issue | Requirement, implementation/evidence and dependency | Final disposition |
|---|---|---|
| [#646](https://github.com/ali-shortcuts/nexaroute/issues/646) | Real provider canaries, streaming/tools/usage, auth and failover; `conformance-live-canary.yml`, provider acceptance harness. Needs authorized providers, models, credentials and explicit spending ceiling. | BLOCKED; no paid calls authorized. |
| [#647](https://github.com/ali-shortcuts/nexaroute/issues/647) | Real organizational OIDC discovery, PKCE, JWKS rotation, roles, denied users and audit; `oidc-runner-acceptance.yml` / `scripts/oidc-runner-acceptance.py`. Needs organization tenant and test accounts. | BLOCKED; local IdP is not acceptance. |
| [#648](https://github.com/ali-shortcuts/nexaroute/issues/648) | Production-like Vault KV, policy, rotation, outage and recovery; `vault-runner-acceptance.yml` / `scripts/vault-runner-acceptance.sh`. Needs authorized staging Vault and least-privilege secret references. | BLOCKED; dev-mode tests insufficient. |
| [#649](https://github.com/ali-shortcuts/nexaroute/issues/649) | Vault Transit implementation plus real round trips, negative cases and rotation/rewrap/rollback; `internal/secrets`, Vault acceptance workflow. Implementation/fixtures exist; needs authorized real Transit service and key policy. | BLOCKED; no real Transit evidence. |
| [#650](https://github.com/ali-shortcuts/nexaroute/issues/650) | Real Helm install, upgrade/rollback, health, security and persistence; `k8s-live-acceptance.yml`, charts. Earlier live attempt failed; corrected scripts need a new real cluster run. | BLOCKED; cluster required. |
| [#651](https://github.com/ali-shortcuts/nexaroute/issues/651) | Real ComfyUI inference, playable asset/hash and negative cases; `internal/video/providers`, `comfyui-accept`. Needs a real authorized host/model/workflow and resource allowance. | BLOCKED; fixture media is not inference. |
| [#652](https://github.com/ali-shortcuts/nexaroute/issues/652) | Independent hosts, shared PostgreSQL/Redis, failover, recovery and measured RPO/RTO; `ha-runner-acceptance.yml` / `scripts/ha-two-host-acceptance.py`. Needs actual separate hosts and shared services. | BLOCKED; same-host tests insufficient. |
| [#653](https://github.com/ali-shortcuts/nexaroute/issues/653) | Exact-final-SHA >=95% coverage, stress and >=2h sustained soak; `coverage-report.yml`, `soak.yml`, `scripts/check-soak-evidence.py`. Prior PR merge coverage gate passed, but exact percentage and final-SHA soak not established. | BLOCKED; authorized dispatch and artifact access required. |
| [#654](https://github.com/ali-shortcuts/nexaroute/issues/654) | Complete every acceptance and independent readiness review; `independent-environment.yml`, release-gate docs. Depends on all rows above, owner decisions, full UI/security review and exact-final-SHA evidence. | BLOCKED. |

## Sequencing and external action

Review and verify the latest #824 candidate, including the atomic store changes
and changed custom-Store contract, before any merge. Compare #823's actual
content and ancestry, preserve its useful work, then obtain the required owner
policy/review decision. Re-fetch main and rerun checks after any authorized
merge. Do not close #823 until every useful contribution is independently
confirmed in main; do not close any issue without its real acceptance evidence.

For manual workflow dispatch, this integration previously received HTTP 403.
The owner can enable the required Actions permission or run the documented
commands in [RELEASE_BLOCKERS.md](../RELEASE_BLOCKERS.md) after provisioning
and authorizing the systems above. Supply credentials only through an approved
secret channel, never in this register or chat. The final release remains
blocked until the required external evidence, exact-SHA checks and owner policy
decisions exist.
