# NexaRoute release-readiness reconciliation — 2026-11-09

> **Verdict: NOT RELEASE READY.** This is a current-state audit of the repository and its release policy, not a claim that every product path has been exercised. Do not publish `v0.25.0` from this snapshot.

## Audit identity

- Repository: `ali-shortcuts/nexaroute`
- Audited branch: `main`
- Audited main SHA: `15a14f92ad9fffa1beb11fcbc5b96a3b7a0158f7` (two documentation-only commits ahead of the previously audited application baseline `b9b5c0c097b5b64d6556029952893d0feacf87ef`; PR #876 added the historical audit report).
- Snapshot date: 2026-11-09
- Current release target documented in the repository: `v0.25.0`
- Current published stable release observed: `v0.24.0`
- GitHub tracker snapshot: 232 closed Issues, 2 open Issues; 641 closed PRs, 1 open PR (#877, this audit/diagnostic correction).
- The two open Issues are #652 and #654. The absence of open PRs is not evidence of release completion.

## Executive decision

The source tree is mature and has broad automated coverage, integration, security, container, browser, deployment, and release tooling. However, the release gate is deliberately stricter than "the latest CI is green". It requires:

1. Every required Issue #646–#654 to be closed with `state_reason=completed`.
2. A `NEXAROUTE_ACCEPTANCE_V1` record for each Issue, naming release `v0.25.0`, the exact candidate SHA, PASS, a successful manual-dispatch run ID, an artifact ID, and a summary.
3. The artifact to belong to that successful run and candidate SHA, with an independent reviewer checking the actual evidence.
4. All PR-required checks and release-candidate-only checks on the exact candidate SHA, including the validated two-hour soak.
5. The current `main` tip to equal the selected candidate SHA at the final gate.

The v0.25.0 exclusion manifest is intentionally empty. Its completion-only policy rejects open Issues and `not_planned` closure; an exclusion must not be used to hide incomplete acceptance.

## Latest exact-SHA automated checks

On current audited main SHA `15a14f92ad9fffa1beb11fcbc5b96a3b7a0158f7`:

| Check | Latest observed result | Evidence |
|---|---|---|
| Container build/smoke | PASS | [CI run 42587652970](https://github.com/ali-shortcuts/nexaroute/actions/runs/42587652970) |
| Bounded CI soak smoke | PASS | [CI run 42587652970](https://github.com/ali-shortcuts/nexaroute/actions/runs/42587652970) |
| Fuzz smoke | PASS | [CI run 42587652970](https://github.com/ali-shortcuts/nexaroute/actions/runs/42587652970) |
| PostgreSQL integration | PASS | [CI run 42587652970](https://github.com/ali-shortcuts/nexaroute/actions/runs/42587652970) |
| Redis integration | PASS | [CI run 42587652970](https://github.com/ali-shortcuts/nexaroute/actions/runs/42587652970) |
| CodeQL (Go) | PASS | [Security run 42587653033](https://github.com/ali-shortcuts/nexaroute/actions/runs/42587653033) |
| Go vulnerability scan | PASS | [Security run 42587653033](https://github.com/ali-shortcuts/nexaroute/actions/runs/42587653033) |
| Fresh Go coverage gate | PASS — **43,946 / 44,269 statements = 99.2704%** | [Coverage run 42587652923](https://github.com/ali-shortcuts/nexaroute/actions/runs/42587652923) |
| Full verification / verify job | RUNNING at audit time | [CI run 42587652970](https://github.com/ali-shortcuts/nexaroute/actions/runs/42587652970) |

The exact-SHA check-run API returned 11 checks: six completed CI checks above plus `verify` still running, two successful Security checks, one successful Coverage check, and an `implementation` check skipped by its own workflow. The skipped implementation check is not counted as a required pass. Because `verify` was still running, the ordinary exact-SHA gate was not PASS at audit time.

The current main SHA is two documentation-only commits ahead of the inspected application baseline `b9b5c0c`; PR #876 adds a dated audit report. The Doctor correction and current documentation reconciliation are in PR #877, not yet merged into main. A bounded CI soak smoke is not the release's sustained two-hour soak. The last recorded two-hour soak evidence remains on SHA `56c7ddfdcdb4869562d4b53187354d0033f688f0` and must be rerun on the frozen final candidate.

## Issue-by-issue acceptance reconciliation

This table distinguishes GitHub state from actual acceptance evidence. A closed label is not treated as proof.

| Issue | GitHub state | Evidence observed | Remaining release action |
|---|---|---|---|
| [#646 — FreeModel canaries](https://github.com/ali-shortcuts/nexaroute/issues/646) | CLOSED / completed | Issue body narrows v0.25.0 to two named model families through FreeModel's OpenAI-compatible endpoint; latest completion comment reports 7/7 scenarios for `gpt-5.6-terra` and 7/7 for `claude-sonnet-5-5`. | Verify the original run/artifact and approved cost/model-catalog inputs; add the required exact-candidate structured acceptance record. This proves only the selected FreeModel route/models, not direct OpenAI or Anthropic interoperability. |
| [#647 — Organizational OIDC](https://github.com/ali-shortcuts/nexaroute/issues/647) | CLOSED / completed | Auth0 preflight proves discovery, issuer/JWKS and configuration shape. The issue thread explicitly says interactive login, unmapped-user denial, logout/audit observation and provider-controlled key rotation remain unverified; a disclosure says the closure was automatic and false on the merits. | Complete the real organizational-tenant browser and negative-flow checks, retain sanitized evidence, reconcile the premature closure, and add exact-candidate metadata. |
| [#648 — Production-like Vault KV](https://github.com/ali-shortcuts/nexaroute/issues/648) | CLOSED / not_planned | A single-host, three-node Vault Raft KV exercise is reported as PASS for its narrowed local scope. The issue's closure disclosure says the scope-narrowing authorship is not attributable to the owner. | The v0.25.0 completion-only gate rejects `not_planned`. Complete the required authorized staging acceptance and close as completed; do not turn the exclusion manifest into a waiver. |
| [#649 — Vault Transit](https://github.com/ali-shortcuts/nexaroute/issues/649) | CLOSED / completed | Real Vault API and operator-lab evidence exists: local-loopback 50/50 and a single-host, three-node Raft exercise on ancestor SHA `debbe202cd5895e275528fe7c162f7b2969c29f7`. The issue's last disclosure disputes the closure and lists gaps in the evidence trail, including exact commands, external-verification update and an unavailable key-version test. | Reconcile the closure; complete missing Transit acceptance cases and evidence; decide whether operator-managed staging is required by the issue's original contract; bind a complete artifact and record to the final candidate. No production/HSM/KMS/HA claim follows from a single-host lab. |
| [#650 — Kubernetes/Helm](https://github.com/ali-shortcuts/nexaroute/issues/650) | CLOSED / completed | Live disposable-`kind` acceptance run 42490453403 on SHA `1d8ac1ba82ece8b589e940be3519a7bb4a9035f0` reports `failures=0 not_run=0`; the issue disclosure says closure authority is unresolved. | Preserve the real artifact and rerun acceptance on the final candidate SHA. Explicitly retain the boundary: disposable-kind success is not production-cluster certification or multi-host HA. |
| [#651 — Real ComfyUI inference](https://github.com/ali-shortcuts/nexaroute/issues/651) | CLOSED / completed | A comment reports a real ComfyUI 0.44.0 / Wan 2.1 1.3B MP4 inference with SHA-256 and Gateway checks PASS. The comment says the sanitized report is in a Colab workspace; a later disclosure says it is not committed or linked as a retrievable artifact and closure authority is pending. | Commit the sanitized report (no prompts/secrets), preserve its provenance and actual output metadata, verify Gateway retrieval and negative cases, and add exact-candidate acceptance record. Treat the current evidence as operator-reported until independently reviewable. |
| [#652 — Multi-host HA](https://github.com/ali-shortcuts/nexaroute/issues/652) | OPEN | PR #874 and merged PR #875 establish two Gateway replicas on distinct Kubernetes workers, single-Pod restart recovery, and same-instance PostgreSQL backup/restore. PostgreSQL and Redis are each one replica using `emptyDir`; Redis AOF is disabled. No cross-host database/Redis failover, durable persistence, multi-host RPO/RTO or long soak is proven. The current `main` `doctor` command still emits an outdated M4-2 warning for PostgreSQL/Redis security-store backends even though both adapters and real-service integration suites exist. This is a diagnostic false-positive, not evidence that the runtime admin-auth path fails closed; a bounded, read-only, redacted connectivity-probe correction and regression tests are included in PR #877 and remain subject to its exact-head CI. | Provision an authorized topology with durable/replicated PostgreSQL and Redis, test cold start, replica loss, both backend failures, reconnect, backup/restore, shared-state correctness and duplicate-job/ledger prevention; record measured RPO/RTO and soak. Merge the Doctor diagnostic correction after exact-head checks pass, then separately complete cross-host durable backend failover before claiming PostgreSQL-backed HA readiness. |
| [#653 — Coverage and soak](https://github.com/ali-shortcuts/nexaroute/issues/653) | CLOSED / completed | Coverage 99.3% and three-round/two-hour soak are recorded on SHA `56c7ddfdcdb4869562d4b53187354d0033f688f0` (run 42435913579 and 42435915263, artifact 13603355325 for soak). The issue thread says the PR merge auto-closed it despite an instruction to keep the issue open. | Rerun fresh coverage, stress and two-hour soak against the frozen final candidate SHA; validate summary and samples against the matching workflow artifact; reconcile the closure and add the exact-candidate acceptance record. |
| [#654 — Umbrella release tracker](https://github.com/ali-shortcuts/nexaroute/issues/654) | OPEN | The body still contains an older checklist saying #646–#653 are all open and has stale blocker notes. | Refresh from this matrix; keep open until #646–#653 are genuinely completed and their records/artifacts independently reviewed; close #654 last. |

## Release-blocking external dependencies

The code agent can complete repository-owned code, tests, docs, runbooks and validators. It cannot truthfully manufacture the following external evidence:

- An authorized organizational IdP tenant and test users for #647.
- A production-like/staging Vault KV environment and authorization for #648 if the original scope remains required.
- A Transit environment/policy/key scenario sufficient to close the remaining #649 acceptance conditions.
- A real ComfyUI host/model and retained sanitized evidence for #651 (the reported Colab run must be made reviewable).
- Two independent failure domains, durable PostgreSQL/Redis, a load-balanced Gateway endpoint and authorized backups for #652.
- Explicit provider account/model/price/budget inputs for any additional billable provider canary beyond the approved FreeModel scope.

No credentials, key material, customer prompts or private outputs should be committed. Do not create paid infrastructure or make billable calls without the explicit resource and spend authorization already required by repository policy.

## Repository-owned work that remains

1. Correct stale release-facing status in `docs/RELEASE_BLOCKERS.md`, `docs/EXTERNAL_VERIFICATION.md`, `docs/KNOWN_GAPS.md` and Issue #654 so that each status links to the current evidence and preserves historical boundaries.
2. Add a committed, sanitized ComfyUI evidence report and provenance link, or rerun the inference if the Colab evidence cannot be exported safely.
3. Correct the `doctor` security-store diagnostic so PostgreSQL/Redis selections are pinged read-only with bounded timeouts and redacted errors; keep the separate crash/backup/HA acceptance boundary explicit.
4. Harden and validate the two-host HA harness and run all possible offline/self-test cases; do not label same-host drills as multi-host.
5. Validate the Vault Transit runner against the actual operator-lab report, record exact commands and missing key-version/rollback cases, and bring `docs/EXTERNAL_VERIFICATION.md` in line with current evidence.
6. Ensure each acceptance workflow emits a provenance-bound sanitized artifact and a `NEXAROUTE_ACCEPTANCE_V1` record suitable for the strict gate.
7. Freeze one candidate SHA; rerun all required gates and live acceptance on that exact SHA; run the release-candidate architecture audit, extended fuzz, browser evidence and two-hour soak.
8. Run the release workflow in non-publishing `workflow_dispatch` mode, inspect all binaries, installer, SBOM, checksums and round-trip artifact, then run the strict gate immediately before an authorized tag publication.

## Safe closure policy

Do not close or reopen an Issue simply to make the dashboard green. Reconcile premature closures with the actual issue evidence and repository policy. Close #646–#653 as `completed` only when each acceptance contract is met and its exact-candidate record/artifact is independently checked. Close #654 last. Do not publish a version tag while any issue, acceptance record, exact-SHA gate or release-candidate gate is incomplete.

## Sources of truth

- [Release blocker runbook](../RELEASE_BLOCKERS.md)
- [Strict release gate](../RELEASE_GATE.md)
- [External verification runbook](../EXTERNAL_VERIFICATION.md)
- [Known product boundaries](../KNOWN_GAPS.md)
- [Latest main CI](https://github.com/ali-shortcuts/nexaroute/actions/runs/42585305605)
- [Latest main Security](https://github.com/ali-shortcuts/nexaroute/actions/runs/42585305552)
- [Latest main Coverage](https://github.com/ali-shortcuts/nexaroute/actions/runs/42585305683)
- [PR #874 — two-worker HA profile](https://github.com/ali-shortcuts/nexaroute/pull/874)
- [PR #875 — backend recovery evidence](https://github.com/ali-shortcuts/nexaroute/pull/875)
