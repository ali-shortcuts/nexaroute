# Publication control-plane inventory — BLOCKED

Snapshot 2026-11-02: main `436c4e98618eed0c41e59059eae7eca5f05dd190`,
#824 `155a1ac60434b9bc907b7f8c6cb0a3a059990900`, #823
`58bacddbc59e8cf9a01bc59620128c93ea2d7e1a`. #824 contains `4611d63` as an
ancestor; this report predates any follow-up commits. Both PRs were open and
had no formal reviews or review threads. All conclusions must be rechecked
against fresh refs and administrative settings.

## Scope and limits

Fetched full reachable Git history, all five public tags and 107 advertised
branch heads into non-branch inventory refs; 2,069 commits were reachable.
Scanned 132 distinct reachable `.github/workflows/*.yml` blobs with a
conservative keyword inventory, inspected privileged matches and each tag's
release workflow, and inspected the current scripts and workflow definitions.
This is not proof about deleted/unreachable refs, externally installed GitHub
Apps, cloud IAM, secrets, marketplace action internals, indirect shell commands,
or automation outside this repository. Historical workflow reachability also
depends on GitHub event semantics, current settings and actor permissions.
No local scan can establish their absence.

## Version tag release workflows

All five tag commits are ancestors of the observed main. Every tag has
`.github/workflows/release.yml` triggered by `push` of `v*`, with a publish
job requesting `contents: write`, no job environment and `gh release create`.
The tag SHA is checked only as an ancestor of main. Historical scripts do not
contain the current v0.25.0 completion gate. The listed commit is the peeled
commit containing the workflow, not a new release candidate.

| Tag | Commit | Additional privilege | Risk |
|---|---|---|---|
| v0.20.2 | `d094bf996155857d1fd6bf18e94450af73def4fd` | write GitHub token | An allowed new `v*` tag at this commit can run the historical publisher; no current gate. |
| v0.21.0 | `2b2fa337e619cca6a83dda42f3cf2c61a0df1a5e` | write GitHub token | Same. |
| v0.22.0 | `2d45f0d893af8545785e31156841d0c8f8cb3007` | write token and `id-token: write` for Sigstore | Same, plus OIDC token issuance; cloud trust policy is unknown. |
| v0.23.0 | `05003db9326b01464a368723d8ab31671b40dc8f` | write token and OIDC | Same. |
| v0.24.0 | `ac5d7aa36e0b0c129ff615fc9a9bfb191f016796` | write token and OIDC | Same. |

Existing tag refs do not by themselves cause a fresh push event. Creation or
movement of another matching tag at an old commit is the identified risk;
actual ability to create/move tags depends on actor rights and rules. The
current main release workflow still has `push.tags: ["v*"]`, a publish job
with write token and OIDC, and no protected environment. Manual dispatch is a
non-publishing dry run in that file; a different historical or external path
must not be assumed safe. No privileged publication was attempted.

## Other repository paths

| Path / definition | Trigger, ref and access | Operation and unresolved boundary |
|---|---|---|
| Main `.github/workflows/browser-evidence-pr.yml` at `436c4e9` (also v0.23/v0.24) | `workflow_run` from Browser evidence on main; `contents: write`, `pull-requests: write`, Actions read | Downloads an artifact, validates it, pushes a `bot/browser-evidence-*` branch and opens a PR. Not a release publisher, but write token can perform repository operations beyond its intended commands; artifact and external action trust remain review items. |
| Main `.github/workflows/opencode.yml` at `436c4e9` (also all five tags) | owner-command `issue_comment` / `pull_request_review_comment`; job `id-token: write` | Executes an external agent action; no GitHub write permission explicitly granted to that job, but OIDC issuance and external service trust require inspection. Owner-command YAML alone is not cloud IAM protection. |
| Main `.github/workflows/conformance-live-canary.yml` at `436c4e9` (also v0.23/v0.24) | `workflow_dispatch`; `contents: read`; `conformance-live` environment; three referenced canary secrets | Performs billable provider calls. Environment listing returned zero visible environments and a direct lookup returned 404; secret locations and actual protection are **unknown**, not proven safe. Requires authorized model, hard spend/request caps and stop conditions. |
| Main `.github/workflows/k8s-live-acceptance.yml` at `436c4e9` | dispatch, read token | Creates and destroys its own disposable kind cluster with fake secrets; not production deployment. Operator-run Helm/kubectl commands outside GitHub remain a deployment path. |
| Main `vault-runner-acceptance.yml`, `oidc-runner-acceptance.yml`, `ha-runner-acceptance.yml` | dispatch, read token | Runner-local disposable Vault/Keycloak/single-host topology, not real staging credentials or production acceptance. Operator live acceptance and credentials outside repo remain out of scope. |
| Other main workflows: `audit`, `browser-evidence`, `ci`, `coverage-report`, `flake-hunt-acceptance`, `independent-environment`, `integration-redis`, `quality-gate`, `real-system-acceptance`, `security`, `soak` | Main/PR pushes, workflow_dispatch or scheduled events as defined in files; explicit read permissions where declared | CI, artifact upload and test/container builds. Container build uses no registry push. External action code, runner/registry behavior and administrative policy are not independently verified. |
| Historical `bootstrap-source.yml` at `f7f37df4652d06aba7f8e3b3ba61ca3b3589dfe9`; `hardening-finalize.yml` at `ce827ebd8feddb23d85c4176ad373196b15abfc7` | Historical path-filtered push / dispatch; `contents: write` | Both attempted `git push ... HEAD:main`; removed from current main. Their historical presence and possible execution on other refs require centralized Actions and branch protection review. Do not assume retired files are centrally disabled. |
| Historical `format-hardening.yml` at `3449b2b7c24c08dd4a6ed7b1b9836f6917e3e015` | Push to hardening branch; `contents: write` | Pushes formatted commits to that branch; removed from main. |
| Historical `gemini-cli.yml`, `jules.yml` at `2574ac36030f692178cdb01960907a122a15d81c` | owner-command issue comments; external action and API-key secrets; Gemini requested contents/issues/PR write | Removed from current main; remote provider access and historical/default-branch event execution need separate GitHub and secret inventory review. |
| Manual/third-party automation | Human, GitHub App, PAT, cloud identity, package/deployment service | Release/tag/asset operations, container publishing or deployment may be performed outside repository workflows. Actor/credential inventory and cloud configuration were not accessible. |

### Remaining current-main workflow definitions

The entries below are definitions at main `436c4e9`; all are repository
`contents: read`, except `flake-hunt-acceptance` also has `checks: read`.
These are not identified as publication paths by this inspection; third-party
actions, runner behavior and operator actions still need independent review.

| Workflow | Events / refs | Intended operation |
|---|---|---|
| `audit.yml` | `workflow_dispatch` | Architecture/quality audit artifacts. |
| `browser-evidence.yml` | dispatch; path-filtered main push | Browser screenshots/artifacts; feeds privileged `browser-evidence-pr.yml`. |
| `ci.yml` | main push, PR, dispatch | Verify, tests and container builds; `build-push-action` is build-only. |
| `coverage-report.yml` | main push, PR, dispatch | Go coverage artifact. |
| `flake-hunt-acceptance.yml` | configured branch push | Reads check metadata and tests. |
| `independent-environment.yml` | dispatch | Disposable test services and verification artifacts. |
| `integration-redis.yml` | PR, dispatch | Disposable Redis integration. |
| `quality-gate.yml` | path-filtered main push and PR | Offline model-quality tests; some action refs are mutable tags. |
| `real-system-acceptance.yml` | PR, dispatch | Runner PostgreSQL/Secret Service acceptance, not multi-host acceptance. |
| `security.yml` | main push/PR, schedule, dispatch | CodeQL and vulnerability scan. |
| `soak.yml` | main push, dispatch | Stress and soak artifacts; not automatically a validated two-hour candidate soak. |

GitHub's workflow list also includes active records for deleted `diag-release`,
`format-hardening` and multiple `tmp-*` workflows. An active workflow record
is not proof a deleted definition can run; owner must check its current
execution and dispatch behavior. No current local composite actions or reusable
workflows were found, but external action internals and historical indirect
invocations were not exhaustively verified. Ordinary action references include
un-pinned tags in `quality-gate.yml` and older workflows; these need separate
supply-chain review. This inventory is not a complete external credential or
publication inventory.

## Observed settings and unknowns

- The readable active ruleset `main-required-checks` (26200123) targets only
  `refs/heads/main`, protecting deletion, non-fast-forward and nine checks.
  It has a role bypass in pull-request mode. The returned ruleset list showed
  no tag-target ruleset; legacy/tag protections, parent/organization controls
  and actual actor capabilities remain unverified (403/404 on related APIs).
- Repository visibility is public. API reports repository permissions
  `admin: true`, but integration requests for branch protection, Actions
  permissions, default token permissions, repository secret names and immutable
  release setting returned HTTP 403. Do not treat the nominal admin field as
  sufficient authority. No settings were changed.
- Visible environment listing returned `total_count: 0`; direct
  `conformance-live` lookup returned 404. Visibility/permission limitations
  mean this does not prove environment or secret absence.
- Existing published releases v0.21.0–v0.24.0 report `immutable: false`.
  Whether immutable releases are enabled for future releases is unknown.
- OIDC subject customization API returned `use_default: true` and
  `use_immutable_subject: true`. This describes GitHub-issued subject format,
  not cloud-provider trust policies or protection against arbitrary branches.

## Owner-only controls and verification

Do not configure or change these without the authorized owner. Each item is
**NOT VERIFIED / NOT CONFIGURED BY THIS AUDIT**:

1. Inventory repository, organization, environment and external publication
   credentials and installed apps. Remove publication credentials from
   unrestricted repository/organization secret scope only after rotation and
   owner-approved migration. Verify names/scopes in Settings or authorized
   APIs without exposing values. Default `GITHUB_TOKEN` is a separate
   credential: restricting stored secrets does **not** restrict a historical
   job's requested `contents: write` permission.
2. Create `release-publication` environment with authorized required
   reviewers, prevent self-review, disable administrative bypass where the
   plan supports it, and allow only the protected controller branch `main`;
   do not allow arbitrary tags. Verify environment, reviewers, bypass and
   deployment-ref policy through GitHub Settings/API and a harmless negative
   test. GitHub plan/visibility may limit environment protections.
3. Configure active tag creation/update/deletion rules for release tags with
   only a narrowly scoped release GitHub App bypass; inspect effective rules
   and test with a nonproduction test tag under an approved procedure. Also
   inventory ruleset bypass actors and branch protections. Do not modify or
   delete existing release tags as a test.
4. Bind cloud/OIDC trust to the exact repository identity, trusted ref,
   protected environment, audience and trusted workflow identity/immutable
   revision where supported; inspect each provider's actual trust policy and
   run authorized negative tests for tag and noncontroller refs. GitHub's OIDC
   settings alone do not establish cloud trust.
5. Determine and test a **central** control that denies legacy tag workflows a
   write-capable GitHub token and publication authority. Repository default
   read-only token permissions do not necessarily cap explicit workflow/job
   `contents: write`; an environment only gates jobs that name it. If the
   available GitHub plan cannot enforce a central cap/event restriction, this
   is an unresolved architecture blocker—not something a YAML `if:` fixes.
   Restrict human/App release creation and asset changes outside the controller
   as far as GitHub's actual controls permit, and independently verify them.
6. Enable immutable releases for future releases where supported and verify
   the setting and a separately authorized test. This protects future published
   releases/tags, **not** execution of old tag workflows or the period before
   publication. Existing releases reported mutable.

## Proposed controller — not implemented or approved

A protected `main` workflow dispatched only on the trusted ref is easier to
review and update; a separately protected reusable workflow pinned by full SHA
adds an immutable workflow identity but still requires a safe caller, central
credential restrictions and careful upgrade policy. No approach is secure
solely because a YAML `if:` says so. Current GitHub configuration and external
trust controls are not verified, so no controller has been activated.

A future controller should validate the exact current-main candidate and
completion-only gate in an unprivileged job, independently verify the actual
sanitized acceptance reports and artifact digests, then require a protected
environment approval by an authorized non-self reviewer. Mint a narrowly
scoped release-App token only after approval; the privileged job must use a
reviewed controller definition and never execute scripts, hooks or workflow
instructions from the candidate/tag. Recheck candidate SHA and gate after the
approval and immediately before publishing. Handle main advancing during the
run as a hard failure. Separately prevent old workflows' GITHUB_TOKEN and
manual privileged actors from bypassing this controller. Human final owner
approval remains mandatory. Until live negative tests show every central
boundary is enforced, release remains BLOCKED.

The optional `scripts/release-control-assessment.py` is a conservative local
fixture/snapshot assessor, **not a guard or proof of GitHub settings**. It
intentionally flags historical privileged workflows even when a manually
supplied snapshot claims protections. `scripts/test-release-control-assessment.py`
exercises its refusal cases; its passing tests do not establish actual
credentials, rules, OIDC trust, reviewers or event enforcement.

## Completion-gate pagination revalidation

The installed GitHub CLI was 2.23.0. A real read of issue #646 comments with
`per_page=2` returned five pages and ten comments, matching the issue API's
count at that instant. This demonstrated multi-page parsing only; #646 has no
qualifying acceptance record and no live acceptance was asserted. The updated
checker validates every Link relation and its repository/issue/page binding,
fetches each page at a constructed endpoint, compares counts and `updated_at`
before and after, and rejects duplicate IDs, malformed pages and failed calls.
Offline fixtures test broken links, changing issue metadata and partial output.
A count match is not a transactional API snapshot and cannot prove report
contents or protect against malicious API responses; transient comment changes
can cause a fail-closed retry. See [Actions execution protections](ACTIONS_EXECUTION_PROTECTIONS.md)
for the current availability finding and owner verification runbook.

The assessment script takes `--snapshot` pointing to a non-secret JSON object;
the fixture schema is illustrated by `snapshot()` in its test. Its
`manual-dispatch-privilege` finding means a workflow contains both constructs,
not that its particular publish job is reachable by dispatch. A hypothetical
passing snapshot is self-reported, cannot attest to live GitHub or cloud
settings, and must never be used as release approval.
