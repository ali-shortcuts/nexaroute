# Requirement-to-contract matrix (#413 / #502 item 3)

One map of the control plane's contract layers and the test that
enforces each seam. If a seam has no enforcing test, the matrix
says so instead of implying coverage.

## Layers and their enforcing tests

| Seam | Source of truth | Enforced by |
|---|---|---|
| Page registry ↔ view functions | `views()` in `internal/httpapi/web/app.js` | This matrix (page table below, drift-checked by `TestContractMatrixDrift`) |
| app.js call sites ↔ handler routes | `app.js` fetch/EventSource targets | `TestWebUIAPICallsResolveToHandlers` (every `/admin/` + `/v1/` call probed against real handlers) |
| Admin OpenAPI spec ↔ handlers ↔ permissions | `docs/api/admin-openapi.yaml` | M4-5 contract test (`admin_openapi_contract_test.go`): paths, methods, and the fail-closed authz matrix must agree; behavior wins over documentation |
| Payload schemas ↔ handlers | OpenAPI component schemas | Same M4-5 test (documented operations probed against the real handler; auth envelope, CSRF flow, audit limit) |
| UI copy ↔ localization dictionary | `bt()` dictionary in app.js (en/fa/ps) | i18n contract tests (`i18n_body_copy_contract_test.go`) |
| Design components ↔ CSS primitives | `docs/COMPONENTS.md` | `TestComponentCatalogContract`, token fence tests |

## Page registry (14 pages)

| Page id | View function |
|---|---|
| `overview` | `overviewView` |
| `providers` | `providersView` |
| `routing` | `routingView` |
| `models` | `modelsView` |
| `video` | `videoView` |
| `activity` | `activityView` |
| `playground` | `playgroundView` |
| `policies` | `policiesView` |
| `access` | `accessView` |
| `audit` | `auditView` |
| `cost` | `costView` |
| `evals` | `evalsView` |
| `observability` | `observabilityView` |
| `settings` | `settingsView` |

## Admin OpenAPI paths (44)

Every path documented in `docs/api/admin-openapi.yaml`. The M4-5
contract test proves each is implemented with the documented
methods and permissions; this list is drift-checked against the
spec by `TestContractMatrixDrift`.

- `/admin/auth/login`
- `/admin/auth/oidc/callback`
- `/admin/auth/status`
- `/admin/auth/logout`
- `/admin/api/snapshot`
- `/admin/api/csrf-token`
- `/admin/api/audit`
- `/admin/api/events/stream`
- `/admin/api/activity/requests`
- `/admin/api/probe`
- `/admin/api/autopilot`
- `/admin/api/route-preview`
- `/admin/api/providers`
- `/admin/api/providers/{id}`
- `/admin/api/provider-presets`
- `/admin/api/provider-check`
- `/admin/api/provider-test`
- `/admin/api/connection-test`
- `/admin/api/provider-discover`
- `/admin/api/compat`
- `/admin/api/compat/reset`
- `/admin/api/settings`
- `/admin/api/client-keys`
- `/admin/api/client-keys/{id}`
- `/admin/api/usage/identities`
- `/admin/api/config/history`
- `/admin/api/video/jobs/{id}/force-fail`
- `/admin/api/drain`
- `/admin/api/virtual-endpoints`
- `/admin/api/virtual-endpoints/{id}`
- `/admin/api/route-profiles`
- `/admin/api/route-profiles/{id}`
- `/admin/api/candidate-pools`
- `/admin/api/candidate-pools/{id}`
- `/admin/api/simple-routes`
- `/admin/api/simple-routes/{id}`
- `/admin/api/fallback-chains`
- `/admin/api/fallback-chains/{id}`
- `/admin/api/scorecards`
- `/admin/api/scorecards/{deployment}`
- `/admin/api/evaluation/suites`
- `/admin/api/evaluation/runs`
- `/admin/api/evaluation/run`
- `/admin/api/endpoint`
- `/admin/api/client-keys/{id}/rotate`

## Selector conventions (#502 item 2)

Stable hooks for tests and automation, in preference order:

1. **Element ids** on shell landmarks (inventory below — from
   `internal/httpapi/web/index.html`).
2. **`data-go="<page>"`** for navigation targets.
3. **`data-action="<command>"`** for commands (44 distinct
   actions in app.js at the time of writing; the set is
   discoverable by searching app.js for `data-action=`).
4. **`data-testid`** only when a surface has no semantic hook
   from 1–3 — add it with the surface, name it after the
   resource and state it exposes, and list it here in the same PR.
   No `data-testid` attributes exist yet because layers 1–3
   already cover the shell and primary flows; a browser-E2E suite
   (item 1) may add them per flow as it lands.

Shell id inventory:

- `auth-control`
- `brand-sub`
- `connection-dot`
- `connection-text`
- `content`
- `dialog-root`
- `lang-switch`
- `live-status`
- `main`
- `notice`
- `page-description`
- `page-title`
- `palette`
- `palette-input`
- `palette-list`
- `palette-open`
- `primary-nav`
- `skip-link`
- `theme-toggle`
- `version`

## Coverage, reported separately (#502 item 4)

- **Browser coverage:** NONE verified. The sandbox headless
  Chromium cannot reach the loopback test server (verified
  2026-10-18; see #502). Real-browser E2E remains item 1 of #502
  and is **UNVERIFIED** until it runs in an environment whose
  browser can load the gateway.
- **Server API integration:** covered — Go contract/integration
  tests probe real handlers (above), including postgres/redis
  integration suites in CI.
- **Mocks / fake providers:** used by server tests and CI only;
  never presented as production or live-provider success.
- **Live external-provider coverage:** NONE in this matrix's
  scope — live tenants remain under their own UNVERIFIED
  boundaries (see EXTERNAL_VERIFICATION.md).
