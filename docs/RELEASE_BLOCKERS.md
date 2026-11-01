# Why the v0.25.0 release is blocked, and exactly what unblocks it

This page exists so nobody has to rediscover the answer. It states the current
blocking state, classifies every blocker by what kind of action can clear it,
and gives the literal command that produces the missing evidence.

Last updated: 2026-11-01, against `main` `b1eebc105340a85b39d051510320bd06390737ce`.

## 1. What is *not* the problem

The code is complete and green. Re-checked first-hand on `b1eebc1`:

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
`nexaroute comfyui-accept`, `nexaroute conformance`. Nothing has to be
implemented to finish the release.

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

Every workflow writes a sanitized artifact. The release gate never reads a
"green badge" — it reads the artifact.

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
bash scripts/release-gate-status.sh "$(git rev-parse v0.25.0)" --rc
```

## 5. The two legitimate ways to ship

The repository's policy is explicit: *a new stable release must wait for each
remaining scope to be completed **or explicitly excluded with reviewed,
truthful release notes***. Both paths are supported by
`.github/scripts/check-final-release-gate.sh`.

### Path 1 — complete the scopes (preferred)

Run the workflows in §4 against a real environment, attach each sanitized
report to its issue, close it as `completed`, then close #654. The gate then
passes with no exclusions and the manifest file is ignored entirely.

### Path 2 — documented, owner-approved exclusions

1. Review `docs/releases/v0.25.0.md` § *Scope exclusions* and confirm every
   statement is true.
2. In `docs/releases/v0.25.0-exclusions.json`, set `approved_by` (the owner
   login) and `approved_at` (`YYYY-MM-DD`) on **every** entry. The gate fails
   closed while any entry is unsigned — that is deliberate.
3. Close each blocker as `not_planned`, linking the exclusion reason. **An
   open issue can never be excluded** — the gate checks that first.
4. Merge the manifest and the notes to `main`, then tag:

   ```sh
   git tag -a v0.25.0 -m "NexaRoute v0.25.0"
   git push origin v0.25.0
   ```

The release workflow then re-runs `check-final-release-gate.sh` on the tag
commit, prints one `::warning::` per exclusion, and publishes. The published
notes must keep the exclusion list intact.

## 6. What this gate will never do

- It will not accept an open blocker, with or without an exclusion.
- It will not read an exclusion written for a different version.
- It will not accept an exclusion with no reason, no approver, or no date.
- It will not accept an exclusion for an issue that is not a release blocker
  (typo guard), so the manifest can never quietly widen the scope.
- It will not let an unused or stale manifest fail an otherwise clean release.
