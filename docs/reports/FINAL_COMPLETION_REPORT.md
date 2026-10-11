# Final completion report — 2026-10-11

Repository: `ali-shortcuts/nexaroute`  
Base: `origin/main` at `dcb9f9b3c0352d23f1dfa599ea58450b072406c0`  
Working branch: `release/final-closeout-20261011`  
Final branch commit: `c0551a29b0f78905bed07450bd011a1a4c241057`  
Final release version: `v0.17.0`

No commit was made to `main`. No branch or tag was deleted. No GitHub release was created.

## Done

### WP6 — local runtime store path

- Fixed startup migration so an existing local control-plane snapshot is preserved instead of being replaced by the bootstrap marker or needlessly revision-bumped.
- Added tests for existing-state migration, restart recovery, and concurrent optimistic-revision writes in `cmd/gateway/controlplane_runtime_test.go`.
- Targeted result:

```text
ok   github.com/ali-shortcuts/nexaroute/cmd/gateway       8.324s
ok   github.com/ali-shortcuts/nexaroute/internal/controlplane 0.030s
ok   github.com/ali-shortcuts/nexaroute/internal/httpapi  9.840s
```

- Race result:

```text
ok   github.com/ali-shortcuts/nexaroute/cmd/gateway       9.791s
ok   github.com/ali-shortcuts/nexaroute/internal/controlplane 1.041s
ok   github.com/ali-shortcuts/nexaroute/internal/httpapi 17.329s
```

### WP3 — aggregate coverage

Fresh command on the closeout tree:

```text
go test -count=1 -coverprofile=/tmp/final.cover ./...
...
total: (statements) 85.0%
```

The requested **85.0% threshold passes**. No coverage-padding tests were added.

### Specs/014 evidence

- Deterministic real-request failover browser evidence passed:

```text
LIVE VISUAL AGENT E2E PASS
```

  It used Chromium, the real gateway, two local HTTP upstreams, a deterministic first failure and second success, request correlation, and generated evidence under `specs/014-control-plane-rebuild/live-visual-agent/evidence/`.
- Performance evidence was regenerated in `lva-performance.json`; the report records idle, active-failover, and success DOM/heap measurements.
- Epoch/cursor reconciliation evidence remains covered by the SSE resume/epoch tests and `docs/reports/a9-sse-resume-evidence.md`.

### WP8 — clean clone

Remote branch clone: `release/final-closeout-20261011`  
Clean clone SHA: `c0551a29b0f78905bed07450bd011a1a4c241057`

Raw acceptance result:

```text
=== CLEAN CLONE ===
## release/final-closeout-20261011
c0551a29b0f78905bed07450bd011a1a4c241057
=== verify.sh ===
VERIFY PASS
=== smoke-local.sh ===
SMOKE PASS — local runtime, UI, admin persistence and token-count fallback are operational.
=== test-install.sh ===
Prepared v0.17.0 in dist/ with gateway, videogen, installer, config and docs. Nothing published.
INSTALL PASS
INSTALLER E2E PASS
```

`test-install.sh` was made clean-clone-safe: it builds local `v0.17.0` fixtures when ignored `dist/` artifacts are absent.

### Version consistency

Updated to `v0.17.0` across README, Dockerfile default build argument, release build default, release notes, installer fixtures, CI packaging invocation, and `/version` default/tests. The stale `v0.17.0-rc.1` release notes were replaced by `docs/releases/v0.17.0.md`; stale v0.6/v0.7 references were removed from active source/docs/scripts.

### Release workflow

Dispatched without publishing:

```text
Run: 38134261987
URL: https://github.com/ali-shortcuts/nexaroute/actions/runs/38134261987
headSha: c0551a29b0f78905bed07450bd011a1a4c241057
verify: success
publish: skipped
conclusion: success
```

The workflow’s `verify` job passed. The `publish` job was skipped because this was a branch `workflow_dispatch`; no tag or release was created.

## Gate evidence

Local raw result summary:

```text
=== smoke-local ===
SMOKE PASS — local runtime, UI, admin persistence and token-count fallback are operational.
=== build-release v0.17.0 ===
Prepared v0.17.0 in dist/ with gateway, videogen, installer, config and docs. Nothing published.
=== release-smoke ===
RELEASE SMOKE PASS — gateway, embedded UI, Video Studio route, fake provider and async job admission are operational.
=== test-install ===
INSTALL PASS
INSTALLER E2E PASS
```

Full verification raw terminal result ended with:

```text
== real browser control-plane acceptance ==
BROWSER E2E PASS
== real browser Live Visual Agent acceptance ==
LIVE VISUAL AGENT E2E PASS
== linux amd64 build ==
== linux arm64 build ==
VERIFY PASS
```

## Not done / not verified

- **Docker image build: BLOCKED.** `docker: command not found`; no Docker CLI/daemon was available in the sandbox.
- **Live Redis interoperability: NOT VERIFIED.** Redis client contracts and lease/token-bucket primitives exist, but no live Redis service was available.
- **Live PostgreSQL interoperability: NOT VERIFIED.** SQL store contracts/tests exist, but no live PostgreSQL service/driver was available.
- **Live OIDC identity provider: NOT VERIFIED.**
- **Live OS keyring/KMS interoperability: NOT VERIFIED.**
- **Real commercial video provider: NOT VERIFIED.** Release smoke uses the documented development fake provider.
- **Human visual acceptance with a configured real provider: NOT VERIFIED.** Automated Chromium evidence is deterministic and local; it is not a human visual sign-off.
- The control-plane runtime closeout verifies the merged local file-store wiring and its migration/restart/concurrency behavior. It does not claim live external backend wiring where the repository intentionally refuses unsafe driver/client fallback.

## Branch containment recommendations — no deletion performed

The requested M1 audit listed these **14 contained non-main branches**. Recommendation for each: **retain until the owner explicitly approves deletion**, then re-fetch and re-audit immediately before any individual deletion.

| Branch | Tip at M1 audit | Recommendation |
|---|---:|---|
| `chore/video-gateway-audit-hardening` | `376cd10` | Candidate for deletion after owner approval |
| `docs/phase1-reality` | `3a02525` | Candidate for deletion after owner approval |
| `fix/gateway-shutdown-lifecycle` | `07cad29` | Candidate for deletion after owner approval |
| `fix/virtual-key-policy-intersection` | `f5f415f` | Candidate for deletion after owner approval |
| `phase1-coverage-85` | `f263e94` | Candidate for deletion after owner approval |
| `wp1-virtual-key-policy` | `1a503e2` | Candidate for deletion after owner approval |
| `wp2-video-gateway` | `f5b74e6` | Candidate for deletion after owner approval |
| `wp2b-video-hardening` | `fce6214` | Candidate for deletion after owner approval |
| `wp4-rbac-sso` | `a3d6635` | Candidate for deletion after owner approval |
| `wp4-rbac-sso-runtime` | `2a1d9f1` | Candidate for deletion after owner approval |
| `wp5-egress-policy` | `81b888c` | Candidate for deletion after owner approval |
| `wp6-controlplane-reconcile` | `8646304` | Candidate for deletion after owner approval |
| `wp6-runtime-wiring` | `ac6517e` | Candidate for deletion after owner approval |
| `wp7-key-custody` | `c4fb68e` | Candidate for deletion after owner approval |

The same audit listed these **8 non-contained branches**. Recommendation: **do not delete; retain and resolve with the owner/PR state first**.

| Branch | Tip at M1 audit | Recommendation |
|---|---:|---|
| `chore/m1-dependency-audit` | `e27609e` | Retain; draft PR #239 / milestone work |
| `fix/video-runtime-p0-hardening` | `8b6bf13` | Retain; tree/PR review required |
| `fix/video-runtime-p0-hardening-v2` | `88e965c` | Retain; tree/PR review required |
| `opencode/issue232-20261010142021` | `1bb893d` | Retain; tree/issue review required |
| `opencode/issue233-20261010142842` | `fa318f0` | Retain; tree/issue review required |
| `opencode/issue234-20261010142842` | `4c1cb7e` | Retain; tree/issue review required |
| `phase0-baseline` | `d6a090b` | Retain; baseline history must be preserved |
| `phase1-coverage-on-1b` | `0b23b91` | Retain; coverage branch review required |

A fresh fetch found additional remote refs beyond that point-in-time M1 list. They were not deleted; the M1 requested 14/8 list above remains unchanged for owner review.

## Final status

**Done:** local WP6 migration/restart/concurrency evidence; aggregate coverage threshold; deterministic browser failover; performance and cursor/epoch evidence; v0.17.0 version cleanup; clean-clone acceptance; successful non-publishing release workflow dispatch.  
**Not done / not verified:** Docker build, live Redis/Postgres/OIDC/KMS/keyring/real-video interoperability, and human visual sign-off with a real provider.  
**Administrative safety:** no main push, no branch deletion, no tag creation, no release publication.
