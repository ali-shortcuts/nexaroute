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
- WP4 — PARTIAL; only HTTP route-permission enforcement and log audit notes exist in `wp4-rbac-sso-runtime`. OIDC authorization-code+PKCE, issuer/audience/signature/expiry/state/nonce verification, role mapping, server-side sessions/revocation, and a mock OIDC provider test are not implemented. Draft PR #224 is open and intentionally not mergeable as WP4 completion. Do not mark it ready or merge until the outstanding identity/session criteria are implemented and tested.
- WP5 — NOT STARTED; no changes exist on local branch/worktree `wp5-provider-egress` at checkpoint. Implement host/CIDR/port allow-list, fail-closed private/metadata protections at dial time, DNS-rebinding pinning, safe redirect/proxy behavior, per-provider overrides, validation, tests, docs, and metrics. Do not claim it done without the redirect, resolver-rebinding, and IP-literal bypass tests.
- WP6 — NOT STARTED; adapters exist in `internal/controlplane`, but they are not yet selected/wired into runtime. Preserve/migrate Phase 1 state and test upgrade, crash-safety, concurrency, backup/restore. Real external SQL/Redis stays unverified unless run.
- WP7 — NOT STARTED; file key provider remains the only production provider. Implement keyring/KMS abstraction, fake-KMS tests and crash-safe rotation before marking done.
- WP8 — NOT STARTED; no final clean-clone acceptance, final docs sweep, final completion report, or branch deletion pass has been performed. Do not delete `phase0-baseline`: earlier branch comparison showed it is not contained in `main` (four branch-only commits at that comparison). Recheck every branch/tree before deletion; branches `phase1-coverage-85`, `docs/phase1-reality`, and the WP1/WP2 branches appeared behind/contained at the recorded snapshot, but verify again against current `main` first.

WP4 branch commit `419af43` was pushed and draft PR #224 opened; it has not been merged. No tags, releases, repository settings, secrets, or unrelated repositories were touched. Next concrete step: implement the remaining identity/session criteria on this PR branch or a separately reviewed follow-up, then continue WP3/WP5–WP8 with separate branches and full gates before each PR. `FINAL_COMPLETION_REPORT.md` must not be written as a completed report until all WPs have been honestly classified and final-main validation is done.


## Latest execution checkpoint — 2026-10-10T09:36+04:30

This section supersedes earlier statements above about the current PR/checkpoint; the earlier text is retained as history.

- Live `main` remained `c4c022c45bee3813fb8eebc9921a018f295785e3` at the latest check. PR #224 (`wp4-rbac-sso-runtime`) is open; it has not been merged. The current local branch is the PR head plus local gate follow-up changes; do not merge until the complete required gate and fresh GitHub checks are green.
- WP4b(1) route enforcement is implemented in the branch: all currently registered Admin API families are mapped, unknown paths deny, the legacy key/keyless loopback path maps to the explicit break-glass owner, and existing tests cover forged role headers, unauthenticated identities, viewer-write denial, and missing/cross-origin CSRF. The web UI's mutations go through those Admin APIs. This does not implement multi-user role assignment or OIDC; those remain separate WP4b(2) work.
- Initial full local gate passed through all steps, with 77.5% total coverage, but the post-commit CI `verify` failed in a gateway shutdown integration test. A health-check response body that was closed without draining was found in `internal/desktop.WaitReady`; draining was added, along with body draining in two gateway lifecycle tests. Both lifecycle tests then passed 20 consecutive race-enabled runs.
- The latest full gate run (raw output: `docs/reports/wp4b-runtime-gates.raw.txt`) passed gofmt, vet, `go test -race -count=1 ./...`, `verify.sh` (including browser and Live Visual Agent acceptance), `smoke-local.sh`, v0.7.0 build-release, and test-install. It failed at the required uncached coverage run because `TestRuntimeFakeProviderEndToEnd` in `internal/video/runtime` races TempDir cleanup: `Runtime.Close()` closes the queue but does not wait for the worker/store write to finish. Reproduced with `go test -count=20 ./internal/video/runtime -run TestRuntimeFakeProviderEndToEnd` (four failures). This is a WP2b runtime lifecycle issue, not evidence of an RBAC behavior failure.
- **WP4b(1) status: BLOCKED pending WP2b's video-runtime cleanup fix and a successful complete gate.** Keep PR #224 open and do not merge it until the block is removed, the PR is refreshed with current `main` without rebase, all required gates pass, and all fresh GitHub checks complete successfully.
- Next step (per requested order): create `wp2b-video-hardening` from current `main`; compare the two video branches (v2 is newer), carry the correct unique changes only, fix gofmt and the worker/store shutdown race, run the required video/TLS/encrypted-config integration tests, update video docs honestly, and run the full gate before opening/merging the WP2b PR. After WP2b is merged, return to WP4b(1), merge `main` into its branch (no rebase), and re-run its complete gate.
- No PR has been merged or branch deleted during this checkpoint. Coverage is measured at 77.5% on the earlier successful gate and remains below 85%; the most recent coverage run failed before a trustworthy final total could be recorded. No coverage profile was committed.


## Latest execution checkpoint — 2026-10-10T09:44+04:30

This checkpoint supersedes the earlier Phase 1-only status above. Worktree `wp2b-video-hardening` was created from `main` at `c4c022c45bee3813fb8eebc9921a018f295785e3`. I compared the older `fix/video-runtime-p0-hardening` and newer `fix/video-runtime-p0-hardening-v2` trees. Because their ancestry is not linear and v2 carries historical WP4-only differences, the active branch was built from the final v2-vs-main tree diff; duplicate/historical commits were not replayed, and no WP4 files were imported.

WP2b changes harden the opt-in video runtime: bearer-token environment configuration is mandatory and fail-closed, asset limits and local persistence are validated, job output/cost handling is safer, and `Runtime.Close()` cancels and joins the worker pool so asynchronous writes cannot race temp-directory or store cleanup. A duplicate `TestVideoEnabledRequiresAuthEnvAndBoundsAssetSize` definition in the v2 tree was removed as an exact copy; its original assertion set remains intact. A test assertion calling `Read` through a concrete `*storage.Local` was corrected to invoke the method directly. The configuration reference, README, security/operations docs, capability matrix, known gaps, and video gateway guide were swept to describe the real boundaries; no real external provider is claimed verified.

The latest complete local gate passed: gofmt; video tests under the race detector; the TLS + encrypted-config + video gateway integration test; `go vet ./...`; `go test -race -count=1 ./...`; `scripts/verify.sh` including browser and Live Visual Agent acceptance; local smoke; v0.7.0 release build; clean-install/upgrade/checksum installer E2E; uncached coverage; and `git diff --check`. Raw evidence is `docs/reports/wp2b-video-gates.raw.txt`. Total statement coverage measured **75.5%**—below the 85% target, which remains honest follow-up work for WP3; do not add padding tests. The separate repeated runtime check passed 20 race-enabled runs.

WP2b is ready for one new PR from `wp2b-video-hardening` to `main` after a final full gate on the documentation-final tree and fresh GitHub checks. Do not merge until GitHub checks are green, and preserve the repository's merge-commit-only policy. The older video PR #225 must not remain as a second active WP2b PR once the replacement PR is opened; keep historical branch refs intact unless the later branch audit proves they are safe to remove.

WP4b(1) remains **BLOCKED**, not completed or merged. Its PR #224 has server-side Admin route authorization but awaits WP2b landing and a successful complete gate against the resulting `main`; afterward merge `main` into the WP4 branch (no rebase), rerun the complete gate, and require fresh GitHub checks before merge. OIDC/PKCE, state/nonce/issuer/audience/signature/expiry validation, role mapping, and server-side session/revocation remain outstanding WP4b(2) scope.


## PR routing checkpoint — 2026-10-10T09:47+04:30

The replacement WP2b PR is open: **#226** (`wp2b-video-hardening` → `main`, initial head `bff1ab1fce3979a06746c8d9c0ef0c1e2840d0be`). The initial `verify`, vulnerability scan, and CodeQL checks were in progress when inspected. The prior v2 PR **#225 was closed unmerged** as superseded; its branch history remains intact. Do not merge #226 unless every check for its latest head completes successfully, and use a merge commit only. PR #224 remains open and unmerged for the documented WP4b(1) block.


## Latest WP4 continuation checkpoint — 2026-10-10T10:06+04:30

This checkpoint supersedes earlier status statements above where they conflict.

- Live `main` is `73977e7113949fc6b7083ab22db68c95b5e5047d` (merge PR #226 / WP2b). PR #224 remains open as a draft on `wp4-rbac-sso-runtime`.
- The latest `main` was merged into the WP4 branch without rebase. Merge commit: `6a3dce1` (`Merge remote-tracking branch 'origin/main' into wp4-rbac-sso-runtime`). Three documentation conflicts were resolved by preserving the current WP2b/Phase-2 statements together with accurate WP4 boundaries and the preceding handoff history.
- After that merge, `./scripts/verify.sh` passed in full on Go 1.26.8: shell syntax, formatting, all unit/integration tests, `go vet`, full race suite, browser control-plane E2E, Live Visual Agent E2E, two bounded fuzz targets, and Linux amd64/arm64 builds for gateway and videogen. Fresh GitHub checks still need to run after pushing this merge commit.
- WP4b(1) (route permission enforcement) is refreshed and locally verified. WP4 as a whole remains **PARTIAL**: no OIDC/PKCE validation, role-claim mapping, identity-provider login, server-side session/revocation, mock OIDC conformance suite, or durable structured audit store. PR #224 must remain draft and unmerged until the identity/session scope is implemented and tested; do not claim SSO or multi-user RBAC.
- Coverage goal remains unresolved: a fresh post-merge `go test -coverprofile` measurement reports 75.6%, below 85%. WP5 provider-egress policy, WP6 runtime-integrated durable stores, and WP7 keyring/KMS remain distinct open work packages.
- No release, tag, repository setting, secret, or unrelated repository was changed.


## WP4 OIDC/RBAC implementation checkpoint — 2026-10-10T11:17+04:30

This checkpoint supersedes earlier WP4 status statements above where they conflict. Earlier notes are preserved as historical handoffs.

- `main` remains `73977e7113949fc6b7083ab22db68c95b5e5047d`; the current work is on `wp4-rbac-sso-runtime`, derived from that main through the existing merge commit (no rebase). PR #224 remains open and draft.
- The formerly partial route-level RBAC branch now includes maintained OIDC Authorization Code + PKCE, verified issuer/audience/JWKS/nonce/state/browser binding, strict endpoint URL policy, allowlisted viewer/operator/admin mapping, server-side opaque sessions and CSRF, absolute/idle expiry and rotation/revocation, opt-in non-downgrade break-glass, durable local bbolt sessions/audit, web login/logout, and updated configuration/security/operations docs.
- Added end-to-end tests using an in-process OIDC provider, middleware/actual-handler permission tests for all three roles, audit failure/durability coverage, secure-cookie/expiry tests, and config URL/role validation tests. No live third-party IdP was configured or tested.
- Final local gates on this source tree passed: `gofmt`, `go test ./...`, `go test -race ./...`, `go vet ./...`, `./scripts/verify.sh` (browser control-plane acceptance, Live Visual Agent, bounded fuzz, Linux amd64/arm64 builds), `./scripts/smoke-local.sh`, `./scripts/build-release.sh v0.7.0`, `./scripts/test-install.sh` (installer/checksum/upgrade/config preservation), and `git diff --check`.
- Exact coverage measurement: `go test -coverprofile=... ./...` + `go tool cover -func=...` = **75.6% aggregate statement coverage**, versus the repository's documented 85% target. No explicitly defined narrower package subset or executable 85% CI gate was found. The target is still **not met**; do not claim WP4 acceptance or mark the PR ready.
- Remaining blockers to acceptance: raise the repository-wide measurement to at least 85%; push the final branch state and require fresh GitHub checks on that exact head. Keep PR #224 draft/unmerged until both blockers are closed. Do not merge, release/tag, alter repository settings, or change/delete any other work.
- Detailed implementation, security boundary, and verification evidence: `docs/reports/WP4_RBAC_HTTP_PARTIAL.md` and `docs/reports/WP4_RBAC_SSO_DESIGN.md`.


## WP4 remote-check remediation checkpoint — 2026-10-10T11:34+04:30

- Remote PR checks for the prior head `ef777138961794cee2a25fcc2df3a7d0949f008c` failed in two places: Docker runtime `/healthz` could not connect, and govulncheck reported reachable GO-2026-4945 through `go-jose/v4@v4.0.5`. CodeQL and CodeQL Go passed. The prior head must not be used as evidence of clean CI.
- The current local source tree upgrades `go-jose/v4` to `v4.1.4` (the Go vulnerability record's fixed release), raises the minimum Go version to 1.24, updates the Docker builder, and prints container logs if the Docker health probe fails. The first attempt to chmod the copied `/config` directory did not fix the runtime; the later CI run on `4ea44b0` identified that `/config` was the DB's non-private parent. The current worktree correction moves the DB to a private child directory created as `0700`. Local `govulncheck` on Go 1.26.9 reports 0 reachable vulnerabilities; the full local tests, race, vet, verify, smoke, release build, installer E2E, and diff checks passed before this final path adjustment.
- Local measured coverage remains **75.6%**, below the 85% repo-wide target. This is still an acceptance blocker. The sandbox has no Docker daemon, so only fresh GitHub CI can confirm the Docker runtime fix.
- At the time of this checkpoint these corrections are in the worktree and the remote head is still `ef77713`; commit/push them, then require new CI and security checks on the resulting exact PR head. Keep PR #224 draft/unmerged, and make no release or unrelated repository changes.


## WP4 Docker private-store-path fix — 2026-10-10T11:45+04:30

This checkpoint supersedes the preceding remote-check notes where they describe the Docker remediation as pending without its root cause. Remote checks on `4ea44b01786fe52b25b2db127b03a3ec846b7cd2` passed the Go vulnerability scan and CodeQL, but the CI Docker runtime still failed. Its newly captured logs proved `/config` was the security DB's parent and was not mode `0700`, so the store correctly failed closed. The worktree now moves both the config example and the implicit default to `security/nexaroute-security.db`; `securitystore.Open` creates the missing leaf directory with mode `0700`. A regression test verifies this behavior under a writable, non-private parent, and a route test verifies the default path. Documentation now explains this storage layout; the ineffective Docker `COPY --chmod` attempt was removed. The CI failure logger remains enabled.

After this private-path change, the complete local suite passed on Go 1.26.9: `go test ./...`, race, vet, govulncheck (0 reachable), `scripts/verify.sh`, local smoke, release packaging, installer E2E, module verification/tidy, doccheck, and diff checks. Measured repo-wide statement coverage remains **75.6%**, below the required 85%. Remote branch head is still `4ea44b0` while this fix is in the worktree; commit/push it, then require fresh CI/Docker/security checks on that exact head. PR #224 stays draft and unmerged.
