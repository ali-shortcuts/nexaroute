# v0.25.0 completion checklist — blocked, not a release approval

Rechecked 2026-11-01. This is a status and operator handoff, not an approval,
a signed exclusion, a complete audit, or evidence for a later SHA. Determine
current main and both PR heads again before relying on any historical result.

## Integration and engineering

- Baseline main at recheck: `436c4e98618eed0c41e59059eae7eca5f05dd190`.
  #823 head: `58bacddbc59e8cf9a01bc59620128c93ea2d7e1a` (open and held).
  Its 12 changed paths are all present among #824's changes relative to that
  baseline. #824 includes the chart, body limit and release-gate log fixes,
  with additional lifecycle, time-window and scanner corrections and tests.
- #823 is functionally superseded **for those reviewed changes on this
  baseline**, not modified, merged or closed. Owner must review both diffs,
  choose an approved merge order, and decide #823's disposition. A changed head
  or base invalidates the comparison. Do not merge or close either PR from
  this checklist.
- #824 has chart window/state and workflow-editor regressions, bounded 4 KiB
  autopilot input, and gate parsing/provenance fixes. The AST scanner checks
  direct `json.NewDecoder`/`io.ReadAll` of `r.Body` and some bounds, **not**
  aliases, arbitrary reader APIs, interprocedural flows or general taint.
  A lexical inventory found 28 `r.Body`/form-related lines under `internal`;
  it is not a complete security audit. In particular, the NexaFlow direct
  read has a top-level 1 MiB `MaxBytesReader`, and the two video JSON decoders
  use 2 MiB and 1 MiB wrappers. Other aliases and paths still need review.
- Full repository authentication, authorization, SSRF, secrets, concurrency,
  retry, storage, routing, recovery, container and dependency audit: **NOT
  COMPLETE**. CI CodeQL/govulncheck are useful but do not establish completion.

## Dashboard and test evidence

- The chart reads the bounded lifecycle feed, uses a half-open rolling window,
  and distinguishes succeeded, failed, active, stale, cancelled and other.
  Regression tests include future/edge timestamps, count reconciliation,
  localization keys and failed-refresh retry loops. Workflow regressions cover
  typed JSON and save/publish races. These are targeted, not a complete product
  audit of every control, permission state, keyboard interaction, translation,
  page or device.
- Historical #824 head `3b32d9f12eaa19a88f2dd65c03e4a818a137e50f` tested
  PR merge checkout `a3f89df783dd7c952f71b57db5e600325f2726b5`: CI
  [41325082280](https://github.com/ali-shortcuts/nexaroute/actions/runs/41325082280),
  Security [41325082270](https://github.com/ali-shortcuts/nexaroute/actions/runs/41325082270),
  Coverage [41325082319](https://github.com/ali-shortcuts/nexaroute/actions/runs/41325082319),
  Real-system [41325082273](https://github.com/ali-shortcuts/nexaroute/actions/runs/41325082273),
  Redis [41325082359](https://github.com/ali-shortcuts/nexaroute/actions/runs/41325082359)
  passed. Browser artifact `13091212132` includes explicitly fixture-fed chart
  screenshots; other test fixtures are described by the suite. Coverage gate
  passed at its 95% threshold, artifact `13090533485`; exact percentage was
  **NOT independently measured** because artifact storage could not be read.
  These results do not transfer to later commits or final main.
- CI real-system tests use two local Gateway processes with PostgreSQL and a
  real Secret Service round trip; they do not establish independent-host HA,
  provider, production Vault, live Kubernetes or ComfyUI acceptance. Local
  Chromium is missing NSS/NSPR libraries and some Go modules are uncached;
  local browser and complete local verification are not claimed as passing.
- UI inventory, all-page live-backend interaction review, accessibility,
  responsive visual review and human screenshot review: **INCOMPLETE**.

## Acceptance register

All issues below were OPEN at recheck. Criteria are authoritative in each
linked issue; descriptions below summarize, not replace them. Unless otherwise
specified no external acceptance evidence exists for the later working tree.
Local/fixture tests and historical reports are in
[EXTERNAL_VERIFICATION.md](../EXTERNAL_VERIFICATION.md) and
[RELEASE_BLOCKERS.md](../RELEASE_BLOCKERS.md); they are not transferred to a
new candidate. Never record secrets or private data in artifacts or issues.

| Issue | Remaining acceptance and operator input | Status |
|---|---|---|
| [#646](https://github.com/ali-shortcuts/nexaroute/issues/646) | Authorized provider/model and spending limit; real streaming, tools, usage, auth/rate/failover cases and sanitized per-provider reports. Fixtures exist; untested providers remain unverified. | BLOCKED |
| [#647](https://github.com/ali-shortcuts/nexaroute/issues/647) | Organizational IdP, claims/role mapping and authorized test accounts; PKCE, key rotation, negative, logout and audit evidence. Local IdP is not the tenant. | BLOCKED |
| [#648](https://github.com/ali-shortcuts/nexaroute/issues/648) | Staging/non-dev Vault KV with least-privilege credential, outage/rotation and recovery; dev mode is not HA acceptance. | BLOCKED |
| [#649](https://github.com/ali-shortcuts/nexaroute/issues/649) | Authorized real Vault Transit/key/policy; round trips, fail-closed negatives, rotation/rewrap and rollback. Implementation/fixtures are not external acceptance. | BLOCKED |
| [#650](https://github.com/ali-shortcuts/nexaroute/issues/650) | Disposable real cluster, secret references and storage; Helm upgrade/rollback, probes, security, persistence and sanitized evidence. Rendering alone is insufficient. | BLOCKED |
| [#651](https://github.com/ali-shortcuts/nexaroute/issues/651) | Authorized ComfyUI host, compatible real model/workflow and resource budget; real playable output/hash and negative cases. Fixture media is not inference. | BLOCKED |
| [#652](https://github.com/ali-shortcuts/nexaroute/issues/652) | Independent hosts and shared PostgreSQL/Redis, authorized failure/recovery and backup drill, shared-state checks and measured RPO/RTO. Same-host tests do not satisfy it. | BLOCKED |
| [#653](https://github.com/ali-shortcuts/nexaroute/issues/653) | Exact-final-SHA fresh coverage percentage >=95%, stress and validated >=2h sustained soak. Prior PR-merge gate pass is historical; current exact percentage and long soak NOT_MEASURED/NOT_RUN. | BLOCKED |
| [#654](https://github.com/ali-shortcuts/nexaroute/issues/654) | All individual acceptance, complete readiness review, exact candidate evidence and owner decisions; cannot be inferred from green CI. | BLOCKED |

## Owner and operator handoff

1. Choose #823/#824 disposition and merge order; no automated merge here.
2. Supply permitted disposable infrastructure, security approvals, providers,
   exact spending limit, test accounts and secret references out of band. Use
   commands in [RELEASE_BLOCKERS.md](../RELEASE_BLOCKERS.md) and per-issue
   procedures; review each artifact, topology and SHA independently.
3. On the exact authorized final candidate after any merge, run required CI,
   security, fresh coverage, browser, integration and real-system workflows,
   manual audit tools and two-hour soak. The current identity cannot dispatch
   manual workflows (HTTP 403). Download the matching soak report **and sample
   file**, verify origin against its run, and use `--soak-summary` as described
   in [RELEASE_GATE.md](../RELEASE_GATE.md). Read the actual coverage artifact.
4. Review unresolved issues, any policy/exclusion decision, UI and security
   findings. Exclusions are unsigned; manifest text alone proves no approval.
   Do not close issues, sign exclusions, tag, release or call this ready while
   mandatory conditions remain unsatisfied.
