# Branch protection proposal for `main` — checks-only variant (DRAFT, 2026-10-30)

> **Status of this document:** DRAFT + PROPOSAL ONLY. **Nothing in this
> document was applied.** Branch protection was inspected read-only only
> (`GET /branches/main/protection` → 404 "Branch not protected" at
> verification time). The supervisor applies the payload in §5 after review;
> this draft's author does not apply, enable, or modify any protection,
> does not merge, close, or comment on any issue/PR.
>
> **Base commit:** main `7daa36da962bf3031b353487f760506cd60aedb2`
> (merge of PR #764, 2026-10-30T06:25:20Z).
> **Companion draft:** `docs/reports/umbrella-matrix-2026-10-30.md`
> (same draft PR; #654 closes last and is not edited here).

## 1. Owner constraints this proposal implements

From the owner's authorization update for this work:

1. **Require verified, deterministic CI, Security, and applicable coverage
   checks ONLY.** Paid provider acceptance and external-infrastructure
   tests must NOT become ordinary PR checks.
2. **Preserve authorized maintainer access** and document a
   recovery/bypass procedure.
3. **The ruleset must not prevent all merges or lock out the owner.**
4. **Test the ruleset against the existing PR workflow** (§6).
5. **Record the exact rule configuration and required check names** (§5).

## 2. Check contexts verified live — verbatim strings

### 2a. Post-merge push runs on `7daa36d` (all `completed/success`, 2026-10-30T06:25:23Z)

| Workflow (run id) | Job check-run names, verbatim |
|---|---|
| CI (41013985867) | `verify`, `container`, `ci-soak-smoke`, `fuzz-smoke`, `integration-postgres`, `integration-redis` |
| Security (41013985905) | `Go vulnerability scan`, `CodeQL (Go)` |
| Coverage report (41013985974) | `Fresh Go coverage` |
| Browser evidence (41013986017) | `evidence` |

### 2b. PR-head check runs on #764's head `429eaec` (all `completed/success`)

`verify`, `container`, `ci-soak-smoke`, `fuzz-smoke`,
`integration-postgres`, `integration-redis` (×2 — two workflows, same
name), `Go vulnerability scan`, `CodeQL (Go)`, `Fresh Go coverage`,
`CodeQL` (bare), `Real Secret Service Store/Load`,
`Two real Gateway processes against PostgreSQL`.

Note the differences between 2a and 2b — they are trigger-scope effects,
not data errors, and they decide §3:

- `evidence` appears on main push but NOT on the PR head.
- `CodeQL` (bare), `Real Secret Service Store/Load`,
  `Two real Gateway processes against PostgreSQL`, and the second
  `integration-redis` appear on the PR head but NOT on the main push
  listing.

### 2c. What was NOT verified — stated explicitly, not invented

- No check context in §5 is invented. Every required name below appears
  verbatim in §2a and §2b.
- The bare `CodeQL` check run's origin is NOT a workflow job
  (`security.yml` jobs are `govulncheck` → `Go vulnerability scan` and
  `codeql` → `CodeQL (Go)`); it is consistent with the
  `codeql-action/analyze` upload check, but that origin was not confirmed
  from a job listing, and the bare name did NOT appear in the `7daa36d`
  push check-run listing retrieved. It is therefore NOT required (§4).
- No context for a paid-provider or live-external acceptance job is
  proposed, because no such job runs as an ordinary PR check and the
  owner forbids making one so (§1.1).

## 3. Trigger-scope analysis (workflow files on main `7daa36d`)

This section is the reason the required set is 8 names and not "everything
green". A required check that never reports on a PR blocks that PR
forever, so trigger scope matters as much as greenness.

| Workflow | Trigger on main | Runs on every PR? | Consequence for required checks |
|---|---|---|---|
| `ci.yml` (CI) | `push` branches `[main]`; `pull_request` (no paths filter); `workflow_dispatch` | YES | All 6 job names are safe to require. |
| `security.yml` (Security) | `push` branches `[main]`; `pull_request` branches `[main]`; schedule; `workflow_dispatch` | YES, with one caveat (§3.1) | Both job names are safe to require for same-repo branches. |
| `coverage-report.yml` (Coverage report) | `push` branches `[main]`; `pull_request` **paths-filtered to `**/*.go`, `go.mod`, `go.sum`, `.github/workflows/coverage-report.yml`**; `workflow_dispatch` | NO — Go-path PRs only | `Fresh Go coverage` MUST NOT be required yet: it does not run on docs-only PRs, so requiring it would permanently block every docs-only PR (including the draft PR carrying this proposal). The 95% gate itself lives in this workflow (`coverage-gate.sh … 95.0`), so the gate also only executes on Go-path PRs. See §4 for the phase-2 unblocker. |
| `browser-evidence.yml` (Browser evidence) | `push` branches `[main]` with paths filter; `workflow_dispatch` | NO — no `pull_request` trigger | `evidence` MUST NOT be required: it never reports on a PR head. Post-merge browser evidence stays a push-time signal, not a merge gate. |
| `real-system-acceptance.yml` (Real-system acceptance) | `pull_request` (no paths filter); `workflow_dispatch` | YES (PRs only) | Jobs `Two real Gateway processes against PostgreSQL` and `Real Secret Service Store/Load` DO run on every PR — but they are real-service integration tests (disposable PostgreSQL service, disposable D-Bus keyring). Per owner constraint §1.1 they are EXCLUDED from the required set: they stay visible CI signals, but they are not merge-blocking required checks. Revisit only on explicit owner word. |
| `integration-redis.yml` (Redis control-plane integration) | `pull_request` (no paths filter); `workflow_dispatch` | YES (PRs only) | Its job name collides with CI's `integration-redis`. Requiring the name `integration-redis` already gates both instances on PR heads (both reported success on `429eaec`). No separate context exists or is needed. |
| `opencode.yml` | `issue_comment` / `pull_request_review_comment` | NO | `implementation` check runs MUST NOT be required (comment-triggered lanes; many are `in_progress`/`skipped` on `7daa36d`). |
| `soak.yml` (Soak) | `workflow_dispatch`; `push` paths-filtered | NO | `soak` MUST NOT be required (long dispatch-only job; currently `in_progress` on `7daa36d`). |
| `browser-evidence-pr.yml` | `workflow_run` | NO | `propose-fixture-pr` MUST NOT be required (follow-up workflow, not a PR gate). |

### 3.1 Security caveat: fork PRs and the CodeQL job condition

`security.yml`'s `codeql` job carries:

```yaml
if: github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository
```

i.e. it is skipped on PRs from forks. All current lanes use same-repo
branches, so `CodeQL (Go)` reports on every lane PR. For a future fork
PR, the supervisor must confirm at apply time how a skipped required
check behaves (see the fork-PR item in §6, step 4) before treating the
ruleset as fork-safe. This caveat does not block the same-repo lane
workflow, and it is NOT a reason to drop `CodeQL (Go)` from the set.

## 4. Recommendation: checks-only now; review requirement is an owner decision

**Recommended: apply the checks-only variant in §5 and do NOT add a
required-review rule in this step.**

Reason, stated plainly: every recent merge in this program landed with
zero formal GitHub reviews — #743 (`8fb9943`), #748 (`48db094`),
#750 (`8ac484e`), #751 (`796f8b4`), #755 (`4b830cd`), #756 (`02a43ac`),
#758 (`197c57b`), #759 (`28aed30a`), #762 (`ccce752`), #764
(`7daa36d`). Requiring reviews would change the lane merge process
itself: the next lane PR could not merge until someone reviews it, and
this repository currently has exactly one collaborator (see §7), so
there is no independent reviewer pool to satisfy such a rule. A review
requirement is therefore NOT a safe default to smuggle into a
"protection" change — it needs an explicit supervisor/owner decision
about who reviews, whether the owner self-reviewing counts, and how
lane throughput is preserved. Until that decision exists, checks-only
is the variant that protects `main` without halting the program.

**Phase-2 candidates (each needs its own decision + workflow change,
none applied here):**

1. `Fresh Go coverage` — unblocker: widen `coverage-report.yml`'s
   `pull_request` trigger to all PRs (remove the paths filter) OR
   accept that docs-only PRs can never merge. Until one of those is
   chosen and landed, do not require it.
2. `Real Secret Service Store/Load` + `Two real Gateway processes
   against PostgreSQL` — unblocker: explicit owner word overriding
   §1.1 (they are real-service tests) plus a stability observation
   period across PR types.
3. Bare `CodeQL` — unblocker: confirm its origin and that it reports
   stably on both push and PR events; until then the job-level
   `CodeQL (Go)` is the requireable CodeQL signal.

## 5. EXACT API payload — checks-only variant (PROPOSAL ONLY, NOT APPLIED)

Apply with `PUT /repos/ali-shortcuts/nexaroute/branches/main/protection`.
This payload is the complete configuration; nothing else is proposed.

```json
{
  "required_status_checks": {
    "strict": true,
    "contexts": [
      "verify",
      "container",
      "ci-soak-smoke",
      "fuzz-smoke",
      "integration-postgres",
      "integration-redis",
      "Go vulnerability scan",
      "CodeQL (Go)"
    ]
  },
  "enforce_admins": false,
  "required_pull_request_reviews": null,
  "restrictions": null
}
```

Reading of each field:

- `required_status_checks.strict: true` — "require branches to be up
  to date before merging": a PR head must include the latest `main`
  (rebase/merge-update + re-run checks) before it can merge. This is
  the existing lane practice made enforceable.
- `contexts` — exactly the 8 names verified in §2a/§2b that run on
  every PR without a paths filter. Order is cosmetic; GitHub matches
  by exact string. (The legacy `contexts` form is used deliberately:
  no `app_id` scoping is needed, so the newer `checks` array form
  adds nothing.)
- `enforce_admins: false` — administrators are NOT subject to the
  required checks (see §7: this is the owner's bypass; it is what
  keeps §1.3 true).
- `required_pull_request_reviews: null` — no review requirement is
  configured (see §4 for why this is deliberate, not an omission).
- `restrictions: null` — no push allowlist is configured (see §7).

Example invocation for the supervisor (placeholder token only — no
real credential is recorded in this document):

```bash
curl -sS -X PUT \
  -H "Accept: application/vnd.github+json" \
  -H "Authorization: Bearer <OWNER_OR_ADMIN_TOKEN>" \
  https://api.github.com/repos/ali-shortcuts/nexaroute/branches/main/protection \
  --data @branch-protection-payload.json
# where branch-protection-payload.json contains exactly the JSON above.
```

Expected success response: HTTP 200 with the protection object echoing
the 8 contexts, `strict: true`, `enforce_admins.enabled: false`, and
no `required_pull_request_reviews` block. Any other response (422 for
an unknown context, 403/404 for permissions) means STOP: do not retry
with a guessed context name — re-verify the context against a live
check run first.

## 6. Step-by-step verification plan (for the supervisor, AFTER applying)

Do these in order. If any step fails, use §8 (rollback) and report the
failure; do not weaken the payload to force a pass.

1. **API inspection (immediate).**
   `GET /repos/ali-shortcuts/nexaroute/branches/main/protection` must
   return 200 (no longer 404) and the body must list exactly the 8
   contexts from §5, `strict: true`, `enforce_admins.enabled: false`,
   and no review requirement. Record the full response in the
   supervisor's ledger. Also `GET /repos/ali-shortcuts/nexaroute/rulesets`
   must still show no conflicting ruleset (verified empty before
   applying).
2. **Docs-only test PR (the trap case).** Open a trivial docs-only PR
   (e.g. one comment line in an existing report) from a same-repo
   branch. Expected: the 8 required contexts appear and run; `Fresh Go
   coverage` does NOT appear (paths filter) and must NOT block the
   merge; `evidence` does NOT appear and must NOT block. Merge it only
   when all 8 are green. If the merge is blocked by an absent context,
   the payload is wrong — roll back (§8) and fix the payload, not the
   branch.
3. **Go-touching test PR (the gate case).** Open a PR touching one
   `*.go` file (e.g. a comment-only Go change, or reuse a real lane
   PR). Expected: all 8 required contexts PLUS `Fresh Go coverage`
   appear (paths filter passes); the coverage gate step runs inside
   that workflow. Merge only when all required contexts are green.
4. **Negative test (the block case).** On a throwaway PR, force a
   failure in one required context (e.g. break `verify` only) and
   confirm GitHub refuses the merge while it is red, then restore
   green and confirm the block lifts. This proves the rule actually
   gates, not just decorates. Do not use admin bypass to merge the
   red PR as the "pass" — that proves nothing.
5. **Fork-PR behavior (the caveat case).** Confirm how a skipped
   `CodeQL (Go)` (fork condition, §3.1) interacts with the required
   set — either with a real fork test PR or from GitHub's current
   documented behavior at apply time — and record the answer in the
   ledger before calling the ruleset fork-safe. Until then, describe
   the ruleset as verified for same-repo branches only.
6. **First real lane PR under protection.** Watch the first genuine
   lane PR merge end-to-end under the rule (head up to date via
   `strict`, all 8 green, merge commit on `main`, post-merge push
   runs green). Record its number and merge SHA in the ledger. Only
   after this step is the ruleset "tested against the existing PR
   workflow" in the sense of §1.4.

## 7. Maintainer access and bypass note

Verified live (read-only) at proposal time:

- Repository owner: `ali-shortcuts`; default branch `main`; public.
- Collaborators (complete list): exactly one — `ali-shortcuts`,
  role `admin`, permissions `admin=true, push=true, pull=true`.
- Existing branch rulesets: none. Existing branch protection: none
  (404 before applying).

Consequences of the §5 payload:

- **The owner is never locked out.** With `enforce_admins: false`,
  repository administrators bypass the required status checks: the
  owner can still merge a PR whose checks are pending or failing, and
  can still push directly to `main`, exactly as today. This is the
  documented admin-bypass semantic and the reason §1.3 holds.
- **No new allowlist narrows access.** `restrictions: null` means no
  push allowlist is added; nobody's existing access is reduced by an
  allowlist entry. (Standard protected-branch semantics still apply
  to any future non-admin collaborator: they would land via PRs, not
  direct pushes. There is currently no such collaborator.)
- **If the owner is ever the only person who can act, the owner can
  still act.** That includes removing the protection entirely via §8.
  No step in §5 or §6 requires a second human.

## 8. Rollback procedure (exact calls; restores the prior state)

Prior state before applying: **no branch protection** (`GET` → 404)
and **no rulesets**. Rollback therefore means deleting the protection
object, not "restoring" a previous payload (there is none to restore).

**To remove protection entirely (the rollback):**

```bash
curl -sS -X DELETE \
  -H "Accept: application/vnd.github+json" \
  -H "Authorization: Bearer <OWNER_OR_ADMIN_TOKEN>" \
  https://api.github.com/repos/ali-shortcuts/nexaroute/branches/main/protection
# Expected: HTTP 204 No Content. Afterwards, GET .../branches/main/protection
# must return 404 "Branch not protected" again — that 404 IS the verified
# prior state, not an error.
```

**To roll back only a bad context (if one context name proves wrong
after applying):** re-`PUT` the §5 payload with that single context
removed — never with a guessed replacement name — then re-run §6
steps 1–3. If more than one context proves wrong, prefer full DELETE
above and re-propose, rather than iterating on a live protected branch.

**Rollback triggers (any one suffices):** a docs-only PR is blocked by
an absent context; a required context never reports on a real lane PR;
the owner cannot merge or push; a 422/403 on apply; or any §6 step
fails and cannot be explained from a live check run.

## 9. What this proposal deliberately does NOT do

- It does not require `Fresh Go coverage`, `evidence`, `CodeQL`
  (bare), the Real-system acceptance jobs, `soak`, `implementation`,
  or `propose-fixture-pr` — each exclusion has its trigger-scope or
  owner-constraint reason in §3/§4.
- It does not add a review requirement, a push allowlist, linear
  history, signed commits, or conversation-resolution rules. Each
  would be a separate decision with its own workflow impact.
- It does not claim the 95% coverage gate runs on every PR: the gate
  runs inside `coverage-report.yml` (Go-path PRs + pushes to `main`);
  `verify.sh`'s gate-regression tests (`test-coverage-gate.sh`) run
  on every PR via CI `verify`, but those test the gate script — they
  are not the gate itself. The distinction is preserved here so the
  supervisor does not over-claim gate coverage.
- It does not change any workflow file. Widening the coverage trigger
  (the §4 phase-2 unblocker) is a workflow PR of its own, not part of
  this protection change.
