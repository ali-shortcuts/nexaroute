# Umbrella matrix — remaining NexaRoute acceptance and quality work (DRAFT, 2026-10-30)

> **Status of this document:** DRAFT for supervisor/owner review. It does NOT edit
> issue #654's body, does NOT close anything, and does NOT change any gate to
> verified. Every row was re-verified against live GitHub state during the
> session that wrote it (see "Verification basis" at the end). Where a live
> portion is blocked, the missing input is named exactly; nothing blocked is
> reported as done.
>
> **Base commit:** main `7daa36da962bf3031b353487f760506cd60aedb2`
> (merge of PR #764, 2026-10-30T06:25:20Z).
> **Open PRs at verification time:** none (`gh pr list --state open` empty).
> **Branch protection at verification time:** main is UNPROTECTED
> (`GET /branches/main/protection` → 404 "Branch not protected").
> See the companion draft `docs/reports/branch-protection-proposal-2026-10-30.md`.

## Rule: #654 closes LAST

Issue #654 (umbrella) may be closed only after every child row below is either
CLOSED on genuine evidence or covered by an explicit, owner-approved,
reviewed exclusion recorded in the release decision. No child is closed by
this draft, and no silent waiver is assumed anywhere in it.

## Matrix

| Issue | State (live) | Merged evidence on main | What remains / missing input |
|---|---|---|---|
| #646 — real LLM provider canaries | OPEN | PR #743 merged 2026-10-30T00:29:40Z as `8fb9943` (head `2e9ffcb`): `internal/providers/livecanary/canary_timeout_ratelimit_test.go` (+115, loopback tests only — per-call timeout + 429 handling). No production code changed. | **BLOCKED — live portion.** All provider dialects live-BLOCKED; rate-limit/outage live runs NOT_RUN; live failover NOT_RUN; zero live calls, zero paid calls. Missing input: explicit owner authorization + exact resource/budget scope + operator credentials/provisioning for the real providers. Fixture/loopback evidence is NOT live acceptance. |
| #647 — live organizational OIDC | OPEN | No live-tenant evidence merged. Local conforming IdP / loopback self-test tooling only (see PR #666 lineage and the OIDC operator runbook work; none of it is a real tenant run). | **BLOCKED — live portion.** Missing input: operator-owned organizational IdP tenant + credentials + written authorization naming the tenant. Local IdP results must not be presented as organizational acceptance. |
| #648 — production-like Vault KV | OPEN | PR #750 merged 2026-10-30T00:36:03Z as `8ac484e` (head `b73e5bb`): helper HTTP status verification + 1 MiB GET cap (`cmd/gateway/vault_accept.go`, `scripts/key-provider-vault-kv.sh`, helper-status tests). Hardening only, not staging acceptance. | **BLOCKED — staging portion.** Missing input: written authorization + operator-owned non-loopback HTTPS Vault + non-dev/HA storage + dedicated empty KV v2 mount + least-privilege tokens + manual policy review. Staging gate NOT_RUN. |
| #649 — real Vault Transit (nxs2) | OPEN | nxs2 implementation IS on main (Transit encrypt/decrypt/rewrap, strict nxs2 schema, ADR 0007). PR #755 merged 2026-10-30T01:54:32Z as `4b830cd`: re-check record only — status stays NOT_RUN/BLOCKED, no code change to the Transit path. | **BLOCKED — live portion.** Live steps (a)–(h) all NOT_RUN. Missing input: operator-authorized disposable Transit instance + dedicated key + least-privilege encrypt/decrypt/rewrap-only policy + token-injection path. Zero live Vault calls to date for this gate. |
| #650 — live Kubernetes/Helm | OPEN | PR #762 merged 2026-10-30T05:39:42Z as `ccce752` (head `b581c89`): safe-failure phases + `--self-test` in `scripts/k8s-acceptance.sh` (+390/−18), `docs/reports/K8S_ACCEPTANCE_2026-10-30.md` (+193), `docs/DEPLOYMENT.md` updates, CI runs `--offline-only` + `--self-test`. | **BLOCKED — live portion.** Live safe-failure phases UNEXECUTED (NOT_RUN). Missing input: owner-authorized disposable cluster (kind/kubectl/docker present, cluster creation authorized). Offline + self-test evidence is NOT a live deployment. Chart staleness note (from prior audits): `deploy/helm/nexaroute/Chart.yaml` 0.1.0/0.22.0 vs release line — verify on current main before any live run; do not bump the chart without release-policy approval. |
| #651 — real ComfyUI inference | OPEN (reopened) | PR #748 merged 2026-10-30T05:10:16Z as `48db094` (head `18e87e7`): harness extension — gateway-path retrieval test, submit/download timeout exactly-once tests, traversal containment, duplicate-output fix via `storage.StoredName`. Harness/tooling only. | **Premature closure RECONCILED, live gate still unmet.** Timeline verified from issue events: closed 2026-10-30T05:10:17Z (immediately after #748 merged) and **reopened 2026-10-30T10:11:49Z** — before this mission turn, so no reopen action was needed from this draft. Real end-to-end model inference remains NOT_RUN/BLOCKED. Missing input: provisioned ComfyUI server + authorized compatible text-to-video model/workflow + GPU/CPU allocation + operator authorization. No weights downloaded, no paid calls. |
| #652 — multi-host HA / failover / backup / RPO/RTO | OPEN (reopened by audit) | No HA acceptance merged. Audit finding stands: prior auto-close did not match the criteria. | **BLOCKED / NOT_RUN.** Multi-host failover NOT_RUN; backup/restore NOT_RUN; no measured RPO/RTO values exist — none are claimed here. Missing input: owner-authorized multi-host staging (PostgreSQL/Redis) + failover/backup drill authorization. |
| #653 — 95% coverage + stress/soak follow-up | OPEN | Coverage leg MERGED: PR #756 merged 2026-10-30T01:09:33Z as `02a43ac` (head `b8b0f20`): 95% repository coverage gate (`scripts/coverage-gate.sh`, `scripts/test-coverage-gate.sh`, `coverage-report.yml`, `verify.sh` gate-regression checks) — this merge closed issue #663 (closed 2026-10-30T01:09:35Z, 2 s later) as a separate issue. PR #759 merged 2026-10-30T03:44:17Z as `28aed30a` (head `816d714`): covered/total statement counts in gate output. PR #751 merged 2026-10-30T00:33:22Z as `796f8b4`: KNOWN_GAPS updated to the 2c5f583 CI measurement, 95.6% (41,704/43,612, artifact 12920986741, run 40926334325, Go 1.27.3). PR #753 (duplicate of #751) CLOSED unmerged as superseded. | **OPEN — scope not fully resolved.** Coverage measurement + enforcement are on main, but #653's own stated scope also covers stress/soak acceptance, which is #664's evidence (see #664 row) and is not yet current on this main commit. #653 closes only when its full stated scope and dependencies are resolved or explicitly excluded by the owner. Do not close #653 on the coverage number alone. |
| #654 — umbrella tracker | OPEN | This draft matrix + the companion protection proposal (DRAFT PR, not merged). Prior gate-audit PR #742 merged (EXTERNAL_VERIFICATION.md audit section); #654's body checklist was previously reconciled to live states except a stale #649 line noted by auditors — the supervisor updates the body after reviewing this draft. | **Closes LAST.** All rows above must be CLOSED or explicitly owner-excluded first. This draft deliberately does not edit #654's body. |
| #664 — long-duration soak on current main | OPEN, soak IN FLIGHT | PR #758 merged 2026-10-30T02:58:18Z as `197c57b`: soak duration passed to the evidence validator (validator now checks dispatched duration). Prior soak success on `ccce752` (run 41011189189) does NOT bind to current main and is NOT claimed here. | **IN PROGRESS, not complete.** Two Soak runs are in flight on SHA `7daa36d`: run 41040133996 (workflow_dispatch, started 2026-10-30T10:21:24Z, on main — subject to the soak workflow's cancel-in-progress rule) and run 41040550445 (workflow_dispatch, dispatched 2026-10-30T10:24:45Z on dedicated branch `soak-run-7daa36d`, rounds=3, duration=2h, timeout 180 min, expected completion ~12:40Z, hard cap ~13:24Z). Neither has completed at verification time. #664 stays OPEN until a run completes AND its artifact summary validates against commit `7daa36d` + 7200 s sustained duration + all stages. A cancelled or partial run is not a pass. |
| #765 — NexaFlow production Gateway wiring | OPEN (new) | PR #764 merged 2026-10-30T06:25:20Z as `7daa36d` (head `429eaec`): standalone Studio port + ClientGone evidence reconciliation. Its Boundaries section states explicitly that the browser acceptance (15 cases) ran against a pre-wired in-process test server only, and **the production gateway still has no `AttachNexaflow` caller** — production-gateway browser acceptance is NOT_RUN until the separate wiring decision is made. Issue #765 created 2026-10-30T10:18:39Z to track exactly that gap (opt-in flag, default off). | **IN PROGRESS by the NexaFlow lane — not duplicated here.** This draft contains no NexaFlow wiring code and no browser-acceptance claim. The in-process browser suite must not be presented as production Gateway acceptance. #765 closes only when the opt-in production wiring lands on main AND a production-gateway browser run passes (or the owner records an explicit exclusion). |

## Post-merge CI on the base commit (all SUCCESS)

Push-event runs on `7daa36d`, 2026-10-30T06:25:23Z, all `completed/success`:

- CI — run 41013985867: `verify`, `container`, `ci-soak-smoke`, `fuzz-smoke`,
  `integration-postgres`, `integration-redis`.
- Security — run 41013985905: `Go vulnerability scan`, `CodeQL (Go)`.
- Coverage report — run 41013985974: `Fresh Go coverage`.
- Browser evidence — run 41013986017: `evidence`.

In-flight at verification time (NOT green, NOT counted above): Soak run
41040133996 (`in_progress` on `7daa36d`) and the opencode `implementation`
check runs triggered by `issue_comment` at ~10:13Z. Neither is a merge gate
and neither is proposed as a required check (see the protection proposal).

## Verification basis (what was re-checked live for this draft)

- `git rev-parse origin/main` = `7daa36da962bf3031b353487f760506cd60aedb2`;
  `gh pr list --state open` empty; `git ls-remote --heads origin` shows no
  matrix/protection branch (no duplicate lane; opencode branches in flight
  are unrelated scopes and were not touched).
- Issue states via `gh issue view` (#646–#654 OPEN; #663 CLOSED
  2026-10-30T01:09:35Z; #664 OPEN; #765 OPEN, created 2026-10-30T10:18:39Z).
- #651 events via the issue-events API: closed 2026-10-30T05:10:17Z,
  reopened 2026-10-30T10:11:49Z.
- Merged PRs via `gh pr view` (#743, #748, #750, #751, #755, #756, #758,
  #759, #762, #764 merged; #753, #761 closed unmerged).
- Branch protection via read-only `GET /branches/main/protection` → 404.
  No protection was applied, enabled, or modified by this draft.
- Post-merge runs via `gh run list --branch main` and `gh run view --json jobs`
  for runs 41013985867, 41013985905, 41013985974, 41013986017.

## Limitations of this draft

- This is a documentation draft, not acceptance evidence. It records merged
  tooling/hardening as tooling/hardening; it upgrades no live gate.
- Coverage figures cited here are the ones already recorded on main via
  #751/#756/#759; this draft did not re-run a repo-wide coverage measurement.
- Live rows (#646–#652, #664's completion, #765's production run) can only
  move when the named owner authorization/resource arrives and the run is
  observed. Until then they stay BLOCKED / IN PROGRESS, however green CI is.
