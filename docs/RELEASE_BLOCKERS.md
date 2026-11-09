# Why the v0.25.0 release is blocked, and exactly what unblocks it

This page exists so nobody has to rediscover the answer. It states the current
blocking state, classifies every blocker by what kind of action can clear it,
and gives the literal command that produces the missing evidence.

Last updated: 2026-11-09, candidate branch based on exact `origin/main` `66a2fabe1e32a1d98403aa6d13797d9a584fd4d3`. The new local Vault Transit run is bound to candidate code SHA `35407ad0a0946de65bfcccc5ba3cd4b3779eb2d1`; historical GitHub results below remain tied to their own SHAs. This candidate update does not assert that those historical checks are current on the newer base.

## 1. What is *not* the problem

Historical required-check results below were recorded on SHA `1d8ac1ba82ece8b589e940be3519a7bb4a9035f0`; they are not asserted current on base SHA `66a2fabe1e32a1d98403aa6d13797d9a584fd4d3`. Separately, `./scripts/verify.sh` passed locally on candidate code SHA `35407ad0a0946de65bfcccc5ba3cd4b3779eb2d1` (unit/integration, vet, race, browser, fuzz and amd64/arm64 builds); this is not GitHub Actions evidence. These checks do not establish product completeness:

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
| `Fresh Go coverage` (95.0% inclusive gate) | **99.29295896%** (43,956 / 44,269 statements; gate PASS) |

The repository-owned acceptance commands already exist: `nexaroute provider-accept`,
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
| #647 | Complete interactive OIDC login and the remaining mapped/unmapped-user, logout, audit and provider-key-rotation observations against the approved test tenant | Operator |
| #649 | Operator-managed staging acceptance beyond the actual Vault 1.18.5 local-loopback PASS; environment-specific staging policy/topology and production operations remain unverified | Operator |
| #651 | A provisioned ComfyUI server, compatible real text-to-video model and workflow for an actual inference | Operator |
| #652 | Two independent hosts sharing PostgreSQL + Redis, behind one address, plus an authorized backup/restore drill | Operator |

Issue-state caveats are separate from missing infrastructure: #648 is closed with `state_reason=not_planned`, which the v0.25.0 completion-only gate rejects. #647 and #653 are closed as completed, but their latest disclosure comments challenge the merits or authority of those closures. #650 has a passing current-main technical run, but its closure authority is also recorded as pending owner disposition. Do not infer acceptance from the GitHub state alone; see §5.

### Class C — Kubernetes disposable-cluster acceptance now passes on current main; issue disposition is separate

The earlier failure on `89b608dd` (run 41259193878) is historical and is superseded for the disposable-cluster technical acceptance by [PR #869](https://github.com/ali-shortcuts/nexaroute/pull/869), merged as current main `1d8ac1ba82ece8b589e940be3519a7bb4a9035f0`.

The post-merge [Kubernetes live acceptance run 42490453403](https://github.com/ali-shortcuts/nexaroute/actions/runs/42490453403) ran on that exact SHA. Its inspected [raw artifact 13621468111](https://github.com/ali-shortcuts/nexaroute/actions/runs/42490453403/artifacts/13621468111) records the upgraded release Pod Ready, the PostgreSQL-backed Gateway Ready, denial of pod-list access for the release ServiceAccount through SubjectAccessReview, and `RESULT: failures=0 not_run=0`. The job completed successfully. See [the retained acceptance report](reports/K8S_LIVE_ACCEPTANCE_2026-11-08.md).

This is a PASS for the exercised disposable `kind` workflow on SHA `1d8ac1b`; it is not proof of production-cluster readiness, independent-host failure tolerance, or measured multi-host RPO/RTO, and it does not satisfy #652. Issue #650 is currently closed/completed, but [its latest disclosure](https://github.com/ali-shortcuts/nexaroute/issues/650#issuecomment-6722959520) says the closure authority is unattributable and keep-closed versus reopen remains pending the owner's disposition. This document records the technical evidence without deciding that issue-state question.

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

For #648/#649, the Vault operator must still provision an authorized staging
Vault, least-privilege KV and Transit policies and separate test keys, inject
secrets through the approved secret channel, and coordinate the environment-
specific acceptance. The actual Vault 1.18.5 local-loopback Transit run covers
migration, Gateway startup, invalid ciphertext, an unavailable key version,
rotation, rewrap and rollback; see the [retained #649 report](reports/verification-slices/vault-transit-acceptance-2026-11-09.md)
and [JSON summary](reports/verification-slices/vault-transit-acceptance-2026-11-09.json).
The KV procedure remains in [Vault KV acceptance](reports/verification-slices/vault-kv-acceptance.md).
Never upload tokens, key bytes or unredacted configuration. Local or runner-
loopback evidence does not satisfy staging, HA, production, HSM or KMS claims.

For #651, provision an authorized real ComfyUI model and workflow, run the
[real-inference procedure](reports/verification-slices/comfyui-real-inference-acceptance.md),
verify playable output, hash and gateway retrieval, and exercise failure and
cleanup cases. For #652, the infrastructure operator must provision two
independent hosts, shared PostgreSQL and Redis, a load-balancing endpoint and
recoverable backup, then coordinate cold-start, replica loss, both backend
outages and restore while recording topology, RPO and RTO. The single-host
runner does not satisfy this. For #650, the post-merge run 42490453403 and artifact 13621468111 provide a passing disposable-kind result on SHA `1d8ac1b`; inspect the retained [acceptance report](reports/K8S_LIVE_ACCEPTANCE_2026-11-08.md). If the final release candidate is a different SHA, rerun and inspect acceptance on that exact frozen candidate. Never claim production topology from a hosted-runner `kind` result. For #653 inspect the exact-SHA soak summary
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
The v0.25.0 manifest (`docs/releases/v0.25.0-exclusions.json`) is now empty, so
the strict gate's manifest check passes; the earlier unsigned proposal survives
only in git history. An empty manifest never supplies acceptance evidence, and
any later non-empty or malformed manifest makes the gate refuse again.

Run each required acceptance and attach sanitized evidence, then close #646–#653 as completed only after independent review; close #654 last. Record exact-SHA metadata as described in [RELEASE_GATE.md](RELEASE_GATE.md). Review actual artifact contents and topology independently. The gate checks metadata, not the truth or completeness of external acceptance.

**2026-11-08 tracker-state reconciliation:** #646 is closed/completed after the recorded FreeModel canary passed 7/7 scenarios for each selected model; preserve the owner-selected provider/protocol scope and bind final metadata to the release candidate. #647 is closed/completed, but its latest disclosure says the Auth0 run was discovery/JWKS/config-shape preflight only; interactive login, unmapped-user denial, logout/audit and provider-controlled key rotation remain unverified, so the closure is not release evidence pending owner disposition. #648 is closed `not_planned`; v0.25.0's completion-only gate refuses that state even though the narrowed single-host Vault KV evidence is recorded. #649, #651 and #652 remain open for Transit-specific acceptance, real ComfyUI inference and independent-host HA respectively. #650 has the passing disposable-kind run above, while its closure attribution remains pending owner disposition. #653 has valid coverage/soak evidence on `56c7ddfdcdb4869562d4b53187354d0033f688f0` (10/10 stages, 7,209 seconds hot-reload, 28,802 resource samples), but its latest disclosure records another automatic closure and pending owner disposition; its soak evidence is not a substitute for a fresh exact-candidate soak after the final main freeze. #654 remains open and must be closed last.

## 6. What this gate will never do

- It will not accept an open blocker, with or without an exclusion.
- It will not read an exclusion written for a different version.
- It will not accept an exclusion with no reason, no approver, or no date.
- It will not accept an exclusion for an issue that is not a release blocker
  (typo guard), so the manifest can never quietly widen the scope.
- For other versions, an unused manifest does not fail an otherwise clean release.
  For v0.25.0 any nonempty or malformed manifest fails, even when all issues are closed.
