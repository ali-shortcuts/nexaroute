# Final completion report

Date: 2026-10-10  
Repository: `ali-shortcuts/nexaroute`  
Final main: `032c394336586e77233e6a96e93f368cdd0d6346`

## Acceptance matrix

| WP | State | PR / merge SHA | Notes |
|---|---|---|---|
| WP1 | DONE | #221 / recorded in prior project evidence | Virtual-key policy merged and retained. |
| WP2 | DONE | #222 / recorded in prior project evidence | Video gateway integration merged. |
| WP2b | DONE | #226 / recorded in prior project evidence | Runtime hardening, recovery and shutdown fixes merged. |
| WP4b | DONE | #224 / recorded in `docs/reports/WP4B_REVIEW.md` | OIDC/PKCE, RBAC, sessions, CSRF and durable local audit merged and reviewed. |
| WP5 | DONE | #228 / `b84f689` parent merge recorded by GitHub | Provider egress policy merged; external provider interoperability is not claimed. |
| WP6 | PARTIAL | #229 / `32e50af2234aaf57c5b911903980260588caffd7` | Reconciliation health contract and control-plane primitives merged. Runtime backend selection/migration and live SQL/Redis verification remain. |
| WP7 | DONE (scope-limited) | #230 / `032c394336586e77233e6a96e93f368cdd0d6346` | File, environment, command/KMS and Linux keyring custody paths are implemented and tested. External service interoperability is unverified. |
| WP3 | PARTIAL | No separate final PR | Aggregate coverage is 84.9%, not 85.0%; no padding tests were used. |
| WP8 | PARTIAL | This report | Final evidence and handoff are written. Branch deletion was deliberately not performed without the required per-branch tree audit. |

## Gate results

All final-main checks below passed:

- formatting (`gofmt`)
- `go vet ./...`
- `go test -race -count=1 ./...`
- `./scripts/verify.sh`
  - shell syntax and formatting
  - clean unit/integration tests
  - browser control-plane acceptance: PASS
  - Live Visual Agent acceptance: PASS
  - bounded fuzz checks: PASS
  - Linux amd64 and arm64 builds: PASS
- `./scripts/smoke-local.sh`: PASS
- `./scripts/build-release.sh v0.7.0`: PASS; artifacts prepared locally, nothing published
- `./scripts/test-install.sh`: PASS; clean install, checksum rejection, upgrade flow and config preservation
- `git diff --check`: PASS

Coverage command:

```text
go test -count=1 -coverprofile=/tmp/final.cover ./...
go tool cover -func=/tmp/final.cover | tail -1
=> total: 84.9% statements
```

No `.cover` file was committed.

## Security and negative-test summary

- Existing WP4/WP5 security evidence remains in the repository, including `docs/reports/WP4B_REVIEW.md`.
- WP7 rejects unknown providers, malformed environment keys, malformed command output, short keys, unavailable keyrings, and local rotation under external custody.
- Key values are not placed in command arguments or ordinary error text.
- Missing/wrong key behavior remains fail-closed.
- Live external IdP, Redis, SQL, OS keyring daemon, and KMS interoperability were not available and are explicitly **unverified**.

## Branches

No remote branches were deleted during this run. Branches known to be merged/contained include `wp6-controlplane-reconcile` and `wp7-key-custody`, plus the previously merged feature branches listed in the handoff. `phase0-baseline` and any branch not proven contained by a fresh tree/ancestor comparison must be retained until that review is performed.

## Remaining work

1. Finish WP6 runtime wiring: select durable backends from configuration, migrate current local/in-memory state without loss, add crash/upgrade/backup/restore tests, and run real external services if available.
2. Raise meaningful aggregate coverage from 84.9% to at least 85.0%, prioritizing currently weak packages rather than adding padding.
3. Run a fresh clean-clone acceptance and perform the per-branch containment audit before deleting any remote branches.
4. Keep the `UNVERIFIED` labels for live third-party IdPs, external Redis/Postgres, keyring daemons, and KMS until actually exercised.

`docs/reports/HANDOFF.md` is the authoritative continuation checkpoint.
