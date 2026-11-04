# Why the v0.25.0 release is blocked, and exactly what unblocks it

This page exists so nobody has to rediscover the answer. It states the current
blocking state, classifies every blocker by what kind of action can clear it,
and gives the literal command that produces the missing evidence.

Last updated: 2026-11-01, against `main` `436c4e98618eed0c41e59059eae7eca5f05dd190` (PRs #821 and #822 merged). A subsequent working branch is not covered by these main results.

## 1. What is *not* the problem

The following required checks passed on main `436c4e98618eed0c41e59059eae7eca5f05dd190` (CI run 41317289767, Security run 41317289831, Coverage run 41317289862). These checks do not establish product completeness:

| Check (required by ruleset `main-required-checks`, 9 contexts) | Result |
|---|---|
| `verify` | success |
| `container` | success |
| `fuzz-smoke` | success |
| `ci-soak-smoke` | success |
| `integration-postgres` | success |
| `integration-redis` | success |
| `Go vulnerability scan` | success |
| `CodeQL (Go)` | success |
| `Fresh Go coverage` (95.0% inclusive gate) | success |

The tree carries production Go with **no** `TODO`/`FIXME` markers, and every
acceptance command already exists: `nexaroute provider-accept`,
`nexaroute external-accept`, `nexaroute vault-accept`,
`nexaroute comfyui-accept`, `nexaroute conformance`. The existence of these commands does not establish that their acceptance criteria have been met.

## 2. The three real blocker classes

### Class A — the agent identity cannot dispatch workflows

A prior `gh workflow run` attempt from this environment returned:

```
HTTP 403: Resource not accessible by integration
(/repos/{owner}/{repo}/actions/workflows/{id}/dispatches)
```

All seven acceptance workflows are `workflow_dispatch`-only, and the GitHub App
acting on this repository previously had read-only Actions permission (recheck current permissions). This blocks agent-initiated workflow dispatch; real systems, credentials and
independent review are separate requirements.

The permission is also why `.github/workflows/*` cannot be edited (a push that
touches a workflow file is refused without the `workflows` permission), so no
workflow-side workaround is possible either.

**Fix:** repository owner → *Settings → GitHub Apps → (the Arena app) →
Configure → Permissions → Actions: Read and write*, then approve the
permission change for the installation. Alternatively run the same dispatch by
hand from the Actions tab (see §4).

### Class B — real external systems and credentials

These cannot be satisfied by any amount of code or permission:

| Issue | What is missing | Who can supply it |
|---|---|---|
| #646 | Paid provider authorization, endpoint, model/account, spend ceiling | Operator |
| #647 | An organizational OIDC tenant (issuer, client, redirect URI) | Operator |
| #648 | A staging/HA Vault with a least-privilege KV policy | Operator |
| #649 | A disposable Vault Transit instance, key and `encrypt`/`decrypt`/`rewrap` policy | Operator |
| #650 | A disposable Kubernetes cluster (kind or real) | Operator |
| #651 | A ComfyUI host with a real text-to-video model and workflow | Operator |
| #652 | Two independent hosts sharing PostgreSQL + Redis, behind one address | Operator |

### Class C — one real defect, already fixed but unvalidated live

`k8s-live-acceptance` **failed** on `89b608dd` (run 41259193878) at the
`Run live acceptance` step. The script fixes that followed — #809 (loopback
sidecar stub), #812 (readiness by revision, rollback port-forward, termination
precision), #813 (new ReplicaSet hash after upgrade/rollback), #817 (unbound
termination wait), #818 (preserve pod termination status) — are merged but have
**never run against a live cluster**. #650 stays open until they do.

## 3. Supplemental dispatches and their evidence limits

The operator must inspect each sanitized acceptance artifact. The status script
reads GitHub check runs and Actions run metadata; for `--rc` soak it also
validates a locally supplied summary. It does not authenticate the supplied
file against the artifact: the operator must independently verify provenance.

| Workflow | Inputs | Artifact must contain |
|---|---|---|
| `conformance-live-canary.yml` | `preset`, `model`, optional `base_url`/`auth_mode` | per-case PASS/FAIL/BLOCKED, provider/API version, date; no prompts or responses |
| `oidc-runner-acceptance.yml` | none | Disposable runner-local Keycloak only; **not** the organizational tenant required by #647. |
| `vault-runner-acceptance.yml` | none | Single-node runner-loopback Vault only; **not** staging/HA acceptance for #648 or #649. |
| `k8s-live-acceptance.yml` | `prev_tag=v0.24.0`, `with_postgres` | a `RESULT:` line with zero failures and zero `NOT_RUN` checks; upgrade, rollback, PVC persistence |
| `ha-runner-acceptance.yml` | none | Isolated containers on **one host**; not independent-host HA or multi-replica Redis failover required by #652. |
| `independent-environment.yml` | `full_soak` | a second, independent run of the standard gates |
| `soak.yml` | `rounds=3`, `duration=2h`, `timeout_minutes=180` | `summary.json` naming the commit, `sustained_seconds>=7200`, every planned stage, a NOT_RUN block |

`nexaroute comfyui-accept` (#651) is an operator CLI, not a workflow: it runs
from a host that can reach an authorized ComfyUI server.

## 4. Supplemental dispatch commands and operator runbook

Once the App has `actions:write` (or from a machine where `gh` is
`workflow`-scoped, or from the Actions tab → *Run workflow*):

```sh
CANDIDATE_SHA=$(gh api repos/ali-shortcuts/nexaroute/commits/main --jq .sha)

gh workflow run k8s-live-acceptance.yml --ref main -f prev_tag=v0.24.0
gh workflow run vault-runner-acceptance.yml --ref main
gh workflow run oidc-runner-acceptance.yml --ref main
gh workflow run ha-runner-acceptance.yml --ref main
gh workflow run soak.yml --ref main -f rounds=3 -f duration=2h -f timeout_minutes=180
gh workflow run independent-environment.yml --ref main

# No live provider dispatch here: the operator must first explicitly authorize
# the provider, model and monetary/token/request ceilings described below.
```

**Candidate binding:** the commands above use the mutable name `main` because
workflow dispatch requires a ref. First complete separately reviewed manifest
preparation, freeze changes to main, and record `CANDIDATE_SHA`. Verify each
run's `head_sha` equals that SHA, and re-check main before and after the runs;
a changed main or mismatched SHA invalidates the run for this candidate. Do
not reuse a PR merge checkout or an earlier main result. `CANDIDATE_SHA` in the example
is an observation, not proof that a later dispatch used it.

These dispatches are supplemental, not a complete external-acceptance
procedure. For #647, the IdP operator must configure an authorized
organizational tenant, registered callback and mapped/unmapped accounts, then
exercise the issue's PKCE, JWKS rotation, roles, logout, clock-skew and audit
criteria against that tenant. The runner-local workflow cannot do this.

For #648/#649, the Vault operator must provision an authorized staging Vault,
least-privilege KV and Transit policies and separate test keys, inject secrets
through the approved secret channel, run the documented live harness, and
coordinate rotation, revocation, outage, rewrap and rollback. See
[Vault KV acceptance](reports/verification-slices/vault-kv-acceptance.md)
and [external verification](EXTERNAL_VERIFICATION.md). Never upload tokens,
key bytes or unredacted configuration. The runner-loopback workflow does not
satisfy staging or HA requirements.

For #651, provision an authorized real ComfyUI model and workflow, run the
[real-inference procedure](reports/verification-slices/comfyui-real-inference-acceptance.md),
verify playable output, hash and gateway retrieval, and exercise failure and
cleanup cases. For #652, the infrastructure operator must provision two
independent hosts, shared PostgreSQL and Redis, a load-balancing endpoint and
recoverable backup, then coordinate cold-start, replica loss, both backend
outages and restore while recording topology, RPO and RTO. The single-host
runner does not satisfy this. For #650 inspect every live kind log and do not
claim production topology from it. For #653 inspect the exact-SHA soak summary
**and samples**, all stages and stress rounds; a successful smoke is not a
2-hour soak. Each operator must retain a sanitized, provenance-bound report for
independent review. External CLI reports need an authorized workflow/artifact
retention path for the strict gate; no such path is asserted here.

**Provider budget and stop conditions:** before any canary, the account owner
must specify in writing the provider, endpoint, models, maximum requests,
maximum tokens, maximum currency spend and time window; configure an actual
provider-side hard budget where available. If no enforceable ceiling exists,
use an explicitly approved conservative request/token limit and monitoring or
do not run. Stop immediately on approaching any limit, unexpected pricing,
authentication or permission anomalies, rate limits outside the approved test,
secret leakage, unexpected traffic, or a failed cleanup. Disable the test
credential and roll back test-only routing/configuration as appropriate; record
the stop and actual spend in the sanitized report. Never retry billable calls
without fresh authorization. This document supplies no spend authorization.

`k8s-live-acceptance.yml` refuses to run against a foreign `kubectl` context
(#777), so it needs a runner that actually has a cluster.

Then re-read the gate for the exact SHA:

```sh
bash scripts/release-gate-status.sh "$CANDIDATE_SHA" --rc --soak-summary /path/to/downloaded/summary.json
```

## 5. v0.25.0: completion only

The owner selected **complete every required scope** for v0.25.0. No signed or
unsigned exclusion may waive an open, `not_planned` or otherwise incomplete
blocker. The generic version-scoped exclusion mechanism from #822 remains in
place for other releases; it is not authorization to use exclusions here.
The historical v0.25.0 manifest remains unsigned and non-empty, so the strict
gate rejects it. An owner-reviewed release preparation must remove it or make
its `exclusions` array empty; that alone never supplies acceptance evidence.

Run each real authorized acceptance and attach sanitized evidence, then close
#646–#653 as completed only after independent review; close #654 last. Record
exact-SHA metadata as described in [RELEASE_GATE.md](RELEASE_GATE.md). Review
actual artifact contents and topology independently. The gate checks metadata,
not the truth or completeness of external acceptance.

## 6. What this gate will never do

- It will not accept an open blocker, with or without an exclusion.
- It will not read an exclusion written for a different version.
- It will not accept an exclusion with no reason, no approver, or no date.
- It will not accept an exclusion for an issue that is not a release blocker
  (typo guard), so the manifest can never quietly widen the scope.
- For other versions, an unused manifest does not fail an otherwise clean release.
  For v0.25.0 any nonempty or malformed manifest fails, even when all issues are closed.
