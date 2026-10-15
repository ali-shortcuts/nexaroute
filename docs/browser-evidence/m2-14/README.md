# M2-14 browser evidence — capture matrix, runbook, flow index

## Capture matrix (what a full run produces)

For every page in the primary navigation, in every combination:

| Axis | Values |
|---|---|
| Theme | dark, light |
| Language | en (LTR), fa (RTL) |
| Viewport | desktop 1440x900, mobile 390x844 |

…plus an axe-core pass (wcag2a/wcag2aa/wcag22aa) per page in both
themes on desktop, and `manifest.json` with a SHA-256 per
screenshot. A re-run diffs against the committed manifest and
reports which shots changed — that report is the visual-diff
baseline (pixel-perfect diffing is intentionally not claimed).

## Running it

```sh
npm --prefix scripts/a11y install   # axe-core + playwright-core (dev-only)
node scripts/browser-evidence.mjs   # builds the gateway, captures into this directory
```

`CHROME_BIN` selects the browser; no browser download happens
(playwright-core drives the system Chromium).

## Status in the build sandbox: UNVERIFIED (browser-blocked)

The sandbox Chromium build refuses all loopback navigations
(`ERR_BLOCKED_BY_LOCAL_NETWORK_ACCESS_CHECKS`) — the full
strategy log is in `docs/browser-evidence/m2-11/README.md`. The
runner detects the `chrome-error://` landing and exits 2 with
that explanation, writing **no** screenshots rather than empty
evidence. The committed matrix above is produced on a standard
Chromium host (developer machine or browser-capable CI).

## Flow-evidence index (coverage that exists today, per flow)

Until the screenshots land, each UI flow is covered at the
handler level by suites that run in `scripts/verify.sh` and CI:

| UI flow | Covering tests |
|---|---|
| Sign-in, sessions, RBAC gating | `internal/httpapi/admin_auth_test.go`, `admin_authz_test.go`, `admin_throttle_test.go` |
| Provider create/edit/test | `internal/httpapi/coverage_admin_routes_test.go` |
| Route CRUD, virtual endpoints | `internal/httpapi/virtual_test.go` (`TestVirtualEndpoint*`) |
| Route-profile privacy | route-profile handlers in `internal/httpapi` (virtual + privacy suites) |
| Virtual keys: create/rotate/revoke, scopes, TPM | `internal/httpapi/client_keys_test.go` (`TestAdminClientKeyCreateRotateListNeverReturnsHash`, `TestVirtualKeyPolicyExpiryAndRevocation`), `client_keys_m38_test.go`, `client_keys_tpm_test.go` |
| Playground sends (data plane) | `internal/httpapi/coverage_openai_test.go`, `coverageAnthropic_wp4_test.go`, `claude_failover_e2e_test.go` |
| Drain / resume | `internal/httpapi/drain_test.go` (`TestGracefulDrainRejectsDataPlaneButKeepsHealth`) |
| Audit chain integrity | `internal/securitystore/chain_test.go`, `backend_contract_test.go`; audit API in `admin_auth_test.go` |
| UI shell + page wiring contracts | `internal/httpapi/control_plane_v2_test.go` + per-page asset tests (M2-2…M2-13 units) |
| Asset budgets + theme contrast | `internal/httpapi/web_assets_budget_test.go` |

These suites are the substitute evidence of record for the
flows; the screenshots, when produced on a capable host,
complement them — they do not replace them.
