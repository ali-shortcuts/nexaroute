# Phase 1 handoff — 2026-10-09

Completed on `phase1b-on-1a`:
- stacked Phase 1b commits on top of Phase 1a with conflict resolution;
- canonical security-document reconciliation;
- encrypted-loader/config CLI migration and read-only tests;
- real TLS plus encrypted-config gateway startup test;
- all mandated local gates, including release-fixture-backed installer E2E;
- PR #215 opened against `phase1a-encrypted-secrets`.

Separate client-auth body-preservation branch/PR:
- branch `phase1a-client-auth-body-fix`;
- PR #214 against `phase1a-encrypted-secrets`;
- raw evidence in `CLIENT_AUTH_BODY_FIX_2026-10-09.txt`.

Remaining:
- review/merge PR #211, then #214 and stacked #215 in the intended order;
- coverage remains 77.4%, below the requested 85% target; do not pad tests;
- wait for GitHub CI on #214/#215 and resolve any CI-only failures;
- do not start Phase 2 until the Phase 1 PRs are merged and explicitly confirmed.


## Continuation checkpoint — remaining control-plane work (2026-10-10)

Verified repository: `ali-shortcuts/nexaroute`; `main` at `c4c022c45bee3813fb8eebc9921a018f295785e3` when checked. No open PRs were present at that check. The latest merged PRs are #221 (WP1, merge `14cf26a294c7cd329fe5e64b8f0e259a6785d259`), #222 (WP2, merge `6e58fcab0cf2076b199e74b7fbfc12d9007e297c`), and #223 (WP4 authorization core/design, merge `c4c022c45bee3813fb8eebc9921a018f295785e3`). Their GitHub checks were completed successfully. Existing WP1/WP2 reports and evidence are `docs/reports/WP1_VIRTUAL_KEY_POLICY.md`, `docs/reports/WP2_VIDEO_GATEWAY.md`, `docs/reports/wp1-virtual-key-policy-gates.raw.txt`, and `docs/reports/wp2-video-gateway-gates.raw.txt`.

### Work completed in this continuation

On local branch `wp4-rbac-sso-runtime` (based on the verified `main` above), a partial WP4 implementation maps registered Admin API route families to server-side `internal/authz` permissions, denies unmapped Admin API paths, treats the legacy static key/keyless loopback as an explicit break-glass owner, and logs state-changing/forbidden Admin requests without request bodies or credentials. Negative mapping and middleware tests were added. README, SECURITY, OPERATIONS, KNOWN_GAPS, and CAPABILITY_MATRIX were updated to describe this as partial, not SSO. The detailed status is `docs/reports/WP4_RBAC_HTTP_PARTIAL.md`.

The full local WP4-partial gate completed successfully after correcting two issues found in the first attempt (gofmt and a legacy 405 expectation). Raw output: `docs/reports/wp4-rbac-http-gates.raw.txt`; per-function coverage output: `docs/reports/wp4-rbac-http-cover-functions.raw.txt`. The successful run included gofmt (empty), vet, race tests, `verify.sh` including browser and Live Visual Agent acceptance, local smoke, v0.7.0 release build, installer tests, coverage, and `git diff --check`. Coverage on this post-video code line is 77.5% total; this is below the requested 85% target and differs from the older pre-video 79.6% baseline.

### Current status and next actions

- WP1 — DONE/merged as PR #221; merge SHA above.
- WP2 — DONE/merged as PR #222; merge SHA above.
- WP3 — NOT DONE; latest measured total 77.5%, target 85.0%. The per-function report above identifies uncovered areas. Add meaningful tests only and remeasure after WP4–WP7.
- WP4 — PARTIAL; only HTTP route-permission enforcement and log audit notes exist in `wp4-rbac-sso-runtime`. OIDC authorization-code+PKCE, issuer/audience/signature/expiry/state/nonce verification, role mapping, server-side sessions/revocation, and a mock OIDC provider test are not implemented. No WP4 runtime PR has been opened or merged. Start by inspecting `git status`, commit this branch's intended files (do not stage generated artifacts), push it, and open a clearly marked **draft/partial** PR if useful; do not merge it while the WP4 acceptance criteria remain unmet.
- WP5 — NOT STARTED; no changes exist on local branch/worktree `wp5-provider-egress` at checkpoint. Implement host/CIDR/port allow-list, fail-closed private/metadata protections at dial time, DNS-rebinding pinning, safe redirect/proxy behavior, per-provider overrides, validation, tests, docs, and metrics. Do not claim it done without the redirect, resolver-rebinding, and IP-literal bypass tests.
- WP6 — NOT STARTED; adapters exist in `internal/controlplane`, but they are not yet selected/wired into runtime. Preserve/migrate Phase 1 state and test upgrade, crash-safety, concurrency, backup/restore. Real external SQL/Redis stays unverified unless run.
- WP7 — NOT STARTED; file key provider remains the only production provider. Implement keyring/KMS abstraction, fake-KMS tests and crash-safe rotation before marking done.
- WP8 — NOT STARTED; no final clean-clone acceptance, final docs sweep, final completion report, or branch deletion pass has been performed. Do not delete `phase0-baseline`: earlier branch comparison showed it is not contained in `main` (four branch-only commits at that comparison). Recheck every branch/tree before deletion; branches `phase1-coverage-85`, `docs/phase1-reality`, and the WP1/WP2 branches appeared behind/contained at the recorded snapshot, but verify again against current `main` first.

No changes were pushed or merged during this checkpoint, and no tags, releases, repository settings, secrets, or unrelated repositories were touched. Next concrete step: inspect the clean/dirty status of `wp4-rbac-sso-runtime`, commit only its intentional source/docs/evidence files, then continue the remaining work in the mandated order with a separate branch and a full gate before each PR. `FINAL_COMPLETION_REPORT.md` must not be written as a completed report until all WPs have been honestly classified and final-main validation is done.
