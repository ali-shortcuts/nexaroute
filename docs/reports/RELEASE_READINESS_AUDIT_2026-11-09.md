# NexaRoute release-readiness audit — 2026-11-09

## Scope

This audit covers the current `main` after merge commit `b9b5c0c097b5b64d6556029952893d0feacf87ef`:

- repository cleanliness and open pull requests
- offline acceptance and release-gate regression suites
- exact-main CI status
- currently open GitHub issues
- release blockers that require owner-authorized external systems

## Current repository state

- Current main: `b9b5c0c097b5b64d6556029952893d0feacf87ef`
- Open PRs at audit time: none
- Latest release: `v0.24.0`
- Target `v0.25.0`: completion-only policy; no exclusions permitted
- Working tree: clean

## Offline verification

The following checks passed on the checked-out main:

```text
final release-gate regression tests: PASS
HA documentation consistency: PASS
HA two-host harness self-test: PASS
release-gate status regression tests: PASS
v0.25.0 completion-policy tests: PASS
integration-report validator tests: PASS
soak-evidence validator tests: PASS (32 tests)
Python compilation and shell syntax checks: PASS
git diff --check: PASS
```

The merged PR #875 also passed all required PR checks on its exact head, including CI verify, coverage, security, PostgreSQL/Redis integration, real-system acceptance, container, fuzz smoke and soak smoke.

## Current open issues

Only these issues are currently open:

| Issue | Status | Current disposition |
|---|---|---|
| [#652](https://github.com/ali-shortcuts/nexaroute/issues/652) | OPEN | Partial iximiuz acceptance is recorded. Gateway placement/recovery, PostgreSQL/Redis single-Pod recovery, PostgreSQL backup/temporary restore and measured local recovery are evidenced. Multi-host PostgreSQL/Redis failover, durable persistence, durable RPO/RTO and long soak remain unverified. M4-2 remains a warning. |
| [#654](https://github.com/ali-shortcuts/nexaroute/issues/654) | OPEN | Umbrella issue. It cannot close until every required acceptance issue has current, exact-SHA evidence and the owner-approved release process is complete. |

Issues #646–#651 and #653 are not currently open in GitHub, but their historical closure state and evidence must still be reconciled against the strict `v0.25.0` completion gate before a release tag is considered.

## Release verdict

**NOT READY / BLOCKED.**

This is not caused by a failing local regression suite. The strict release policy requires current, structured, exact-SHA acceptance records for all release blockers and completion of the owner-authorized external gates. The remaining evidence classes include live provider checks, organizational IdP, staging Vault boundaries, real Kubernetes acceptance on the exact candidate, real ComfyUI inference, true two-host PostgreSQL/Redis HA with RPO/RTO, and the current-main soak/coverage evidence required by the release procedure.

The repository is in a usable development state and the code-level gates are green where executed or already passed on the exact PR head. It must not be tagged or published as `v0.25.0` until the external acceptance prerequisites and strict release gate pass.
