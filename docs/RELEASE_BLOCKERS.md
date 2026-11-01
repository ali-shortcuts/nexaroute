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

`gh workflow run` from this environment returns:

```
HTTP 403: Resource not accessible by integration
(/repos/{owner}/{repo}/actions/workflows/{id}/dispatches)
```

All seven acceptance workflows are `workflow_dispatch`-only, and the GitHub App
acting on this repository has read-only Actions permission. **Every issue that
only needs a rerun is stuck behind this one permission.**

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

## 3. What each dispatch produces

The operator must inspect each sanitized acceptance artifact. The status script
reads GitHub check runs and Actions run metadata; for `--rc` soak it also
validates a locally supplied summary. It does not authenticate the supplied
file against the artifact: the operator must independently verify provenance.

| Workflow | Inputs | Artifact must contain |
|---|---|---|
| `conformance-live-canary.yml` | `preset`, `model`, optional `base_url`/`auth_mode` | per-case PASS/FAIL/BLOCKED, provider/API version, date; no prompts or responses |
| `oidc-runner-acceptance.yml` | none | discovery pinning, state/nonce/PKCE, JWKS + rotation, role mapping, denied unmapped user, logout, audit excerpts |
| `vault-runner-acceptance.yml` | none | KV put/get round-trip, startup decrypt, wrong/revoked token fail-closed, rotation, outage; and Transit encrypt/decrypt/rewrap, invalid ciphertext, revoked key version, no local fallback |
| `k8s-live-acceptance.yml` | `prev_tag=v0.24.0`, `with_postgres` | a `RESULT:` line with zero failures and zero `NOT_RUN` checks; upgrade, rollback, PVC persistence |
| `ha-runner-acceptance.yml` | none | nine scenarios; two hosts; measured failover RPO/RTO |
| `independent-environment.yml` | `full_soak` | a second, independent run of the standard gates |
| `soak.yml` | `rounds=3`, `duration=2h`, `timeout_minutes=180` | `summary.json` naming the commit, `sustained_seconds>=7200`, every planned stage, a NOT_RUN block |

`nexaroute comfyui-accept` (#651) is an operator CLI, not a workflow: it runs
from a host that can reach an authorized ComfyUI server.

## 4. The exact commands

Once the App has `actions:write` (or from a machine where `gh` is
`workflow`-scoped, or from the Actions tab → *Run workflow*):

```sh
MAIN=$(gh api repos/ali-shortcuts/nexaroute/commits/main --jq .sha)

gh workflow run k8s-live-acceptance.yml --ref main -f prev_tag=v0.24.0
gh workflow run vault-runner-acceptance.yml --ref main
gh workflow run oidc-runner-acceptance.yml --ref main
gh workflow run ha-runner-acceptance.yml --ref main
gh workflow run soak.yml --ref main -f rounds=3 -f duration=2h -f timeout_minutes=180
gh workflow run independent-environment.yml --ref main

# The three that need real credentials, only after the operator authorizes them:
gh workflow run conformance-live-canary.yml --ref main -f preset=openai -f model=<model>
```

`k8s-live-acceptance.yml` refuses to run against a foreign `kubectl` context
(#777), so it needs a runner that actually has a cluster.

Then re-read the gate for the exact SHA:

```sh
bash scripts/release-gate-status.sh "$CANDIDATE_SHA" --rc --soak-summary /path/to/downloaded/summary.json
```

## 5. The only supported release path: complete every blocker

There is exactly one supported path to a stable release: **complete every
required acceptance scope**.

1. Finish the repository-local implementation and tests for each blocker.
2. Run its authorized acceptance workflow against the required real system.
3. Attach sanitized evidence to the corresponding issue. Include the exact
   tested SHA, workflow/run URL, artifact identifiers, acceptance results,
   and measured values where applicable.
4. Have a reviewer verify that the evidence satisfies the issue's complete
   acceptance criteria.
5. Close the issue with `state_reason=completed` only after that review.
6. Repeat for every required issue #646–#653.
7. Close umbrella issue #654 last, after the full readiness review.
8. Run the full release-candidate gate on the exact candidate SHA. Publish only
   after every mandatory check and external acceptance requirement passes and
   the repository owner authorizes publication.

Closing an issue as `not_planned`, `duplicate`, with a missing reason, or by
any other non-completed state is **not** a release completion. It will fail the
gate even if the issue has extensive comments or a previously approved
exclusion entry.

The release workflow re-runs `check-final-release-gate.sh` on the tag commit.
Only after the gate and all other release checks pass should the owner create
and push the release tag and publish the release.

## 6. What this gate will never do

- It will never accept an open, reopened, or not-planned required issue.
- It will never accept a missing, null, or unknown issue state reason.
- It will never accept a version-scoped exclusion, waiver, or owner-approved
  manifest as a substitute for actual completion.
- It will reject any `docs/releases/*-exclusions.json` file.
- It will never convert fixture, runner-local, or simulated evidence into
  external acceptance.
- It will never close issues, approve policy exceptions, or publish a release
  automatically.
