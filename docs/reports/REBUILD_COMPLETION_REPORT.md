# NexaRoute Rebuild Completion Report

**Date:** 2026-10-01  
**Branch:** `rebuild/v0.13.0-complete`  
**Repository:** `ali-shortcuts/nexaroute`

## Executive result

The authorized end-to-end rebuild work is complete in the working branch. The implementation already present on the branch was re-audited against the supplied brief, the remaining acceptance gap was fixed, and the full local verification matrix was rerun.

The one functional issue found during the continuation pass was a stale browser acceptance step: the new control-plane intentionally hides CLI/Connect from top-level navigation, while the test still clicked the hidden `data-tab="cli"` button. The test now follows the intended user journey: **Routing → Advanced → Connect**.

## Completed implementation areas

### Control-plane UX and routing

- Overview/control-plane shell uses a focused default navigation with Connect/CLI and advanced configuration kept contextual.
- Routing has a simple route builder with automatic and ordered modes, model search, select-all/clear-all, edit/delete, and backend-authoritative persistence.
- Advanced route objects remain accessible without making the default experience overwhelming.
- Provider onboarding supports guided connection, model discovery, manual fallback, connection check, model test, write-only secret editing, and contextual provider search.
- Live topology/ring behavior is data-driven: ambient motion was removed and traffic animation is tied to real route events.
- Model-node telemetry is additive and provenance/freshness-aware; absent values are omitted rather than guessed.
- CLI/Connect rendering escapes public model values and does not expose provider credentials.
- Accessibility and operator ergonomics include skip-link, language/theme controls, keyboard navigation, ARIA state, dialog Escape handling, and live status regions.

### Backend and security hardening

- Provider/base URL validation rejects malformed hosts and embedded credentials where the contract requires credential separation.
- Proxy URL compatibility was preserved while retaining strict base URL validation.
- Regression tests cover embedded provider/proxy URL credentials and the compatibility boundary.
- Known-gap/audit documentation was reconciled with the implemented behavior and remaining trust boundaries.

### Release and delivery

- Release build fixtures were prepared for `v0.13.0-rc1` (nothing was published).
- A dedicated branch was created: `rebuild/v0.13.0-complete`.
- Installer lifecycle, clean install, checksum rejection, architecture selection, upgrade/reinstall, persistence, write-only secrets, crash recovery, and browser readiness were exercised.

## Verification evidence

| Gate | Result | Evidence |
|---|---:|---|
| Go unit/integration suite | PASS | `go test ./... -count=1` |
| Go vet | PASS | `go vet ./...` inside verify flow |
| Race suite | PASS | verify flow and soak race checks |
| Formatting/diff hygiene | PASS | `gofmt`, `git diff --check` |
| Frontend syntax/tests | PASS | Node syntax checks and `node --test internal/httpapi/web/*.test.mjs` |
| Browser acceptance | PASS | startup/navigation, localization/theme, provider CRUD, discovery/manual fallback, routing, Connect output, XSS safety, observability filters, pause/resume, no JS page errors |
| Stress checks | PASS | router, probe/recovery, event state, HTTP admission, log rotation, evaluation |
| Soak checks | PASS | 3 repeated rounds, hot reload, recovery, race-enabled soak |
| Installer/release lifecycle | PASS | clean install, checksum rejection, upgrade/reinstall, persistence, crash recovery |
| Local release build | PASS | amd64 and arm64 fixtures |

## Environment limitations

These are not application test failures, but could not be marked PASS in this sandbox:

1. **Docker gate unavailable:** Docker is not installed in the active sandbox, so the Docker build/run gate could not be executed here.
2. **Vulnerability scan toolchain mismatch:** `govulncheck` with the sandbox's Go 1.23.12 reported standard-library advisories associated with that old toolchain. The downloaded Go 1.27.1 archive itself failed to compile because its runtime source files are internally inconsistent (`traceexp.go`/`tracebuf.go` redeclaration and missing symbols). The repository's normal Go 1.23 test suite remains green; the Go 1.27 CI scan should be run on the CI runner or a clean Go 1.27 installation.

No source or configuration changes were made to suppress either limitation.

## Changed in the continuation pass

- `scripts/test-browser-e2e.py`: updated two CLI acceptance steps to use the contextual Routing → Connect path introduced by the control-plane UX.

## Recommended next action

Merge or review the local branch, then run the existing GitHub Actions matrix (especially the Go 1.27 security job and Docker job) in a clean runner before publishing `v0.13.0`.
