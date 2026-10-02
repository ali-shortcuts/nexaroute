# v0.14.0 Tasks

## R1/R2

- [x] Clone stable main and create greenfield branch.
- [x] Inventory admin routes, config structs, snapshot, SSE, gateway and embed architecture.
- [x] Compare six serious router/gateway products using 80 supplied public URLs.
- [x] Reconcile research with product spec and anti-patterns.

## Frontend foundation

- [x] Remove legacy top-level navigation and obsolete presentation layers.
- [x] Add new tokens, semantic state styles, responsive behavior and reduced-motion handling.
- [x] Centralize API/auth, SSE and lifecycle teardown in one controller.
- [x] Build shell with Overview, Providers, Routing, Activity and Settings.

## Product surfaces

- [x] Providers empty/add/detail flow with discovery, per-model test, selection, connection test and persistence wiring.
- [x] Simple route list/builder/detail and contextual Advanced surface.
- [x] Connect route setup with actual gateway/auth/model state.
- [x] Calm Overview with backend event-backed activity and no synthetic movement.
- [x] Activity request/event list with filter.
- [x] Settings intent-first plus Expert Controls boundary.
- [ ] Full running-provider browser E2E and screenshot evidence.

## Verification

- [x] Frontend syntax and shell contract tests.
- [x] Go unit/integration/race tests.
- [x] Provider, routing, Connect, SSE and persistence backend coverage preserved.
- [ ] Capture all required screenshots from real configured UI.
- [x] Security smoke: no provider secret values in admin provider response.
- [x] Build and local/public page smoke.
- [ ] Docker and installer checks.
- [ ] Complete convergence matrix with human visual acceptance.
- [ ] Update/open PR; do not merge or release.
