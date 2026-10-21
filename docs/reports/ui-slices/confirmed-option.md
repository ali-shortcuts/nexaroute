# UI slice — confirmed inert option (issue #610)

Parent audit: #593. Narrow functional UI audit: one confirmed defect, one
focused PR. No full-UI-audit claims.

## The defect

**Control:** Settings → Routing → "Default strategy" select
(`#setting-strategy`, rendered by `settingsRoutingCards` in
`internal/httpapi/web/app.js`).

**Path:** rendered select → `save-settings` handler → `PUT
/admin/api/settings` → `config.Validate()` → `router.applyStrategy`.

**Failure:** the select offered `adaptive` as a routing strategy. That
strategy was never implemented:

- `internal/router/router.go` `applyStrategy` has no `case "adaptive"`;
  the value falls through to `default` (plain health-rank + score sort),
  behaving identically to having no strategy at all.
- No adaptive ordering logic exists anywhere in the tree (the only
  "adaptive" in the router is the distinct `adaptive_round_robin`
  strategy; `internal/decision/policy` "adaptive" is a benchmark label,
  not wired to the strategy).
- Yet `config.Validate()` accepted `"adaptive"`, `docs/CONFIGURATION.md`
  listed it under "Available strategies", and the UI offered it —
  so an operator selecting it got silently default behavior with no
  indication anything was off.

This is the same defect class as the round-1 bug-hunt finding
(dead `fallback_on_unknown_model`, #589): a UI/documented/validated
option with no backend behavior behind it.

## Reproduction (before fix)

1. `TestUnimplementedAdaptiveStrategyRejected`
   (`internal/config/config_test.go`): set
   `cfg.Routing.Strategy = "adaptive"` and call `Validate()` —
   **failed** before the fix (validation returned nil).
2. Static: `settingsRoutingCards` rendered
   `<option …>adaptive</option>`; `applyStrategy` has no
   `case "adaptive"`.

## Fix

Chose removal with truthful labeling over implementation: a real
adaptive routing engine is a roadmap-scale feature, out of scope for
this slice; the issue explicitly permits removing/disabling with
truthful explanatory text.

- `internal/httpapi/web/app.js`: the select now offers exactly
  `ready_mesh` and `ready_queue` (the strategies with dedicated router
  behavior). A current value outside that set (e.g. a legacy `adaptive`
  config) renders as a disabled, selected option labeled
  `<value> — not implemented, pick a supported strategy`
  (new `set_strategy_unsupported` key in the EN/FA/PS body dictionary).
- `internal/config/config.go`: `Validate()` rejects `"adaptive"`
  (fail-closed). This covers file config, env-derived config, and the
  admin `PUT /admin/api/settings` path (`mutateConfig` →
  `applyConfigLocked` → `Validate()`).
- `docs/CONFIGURATION.md`: removed `adaptive` from the available
  strategies list, with a note explaining the removal.
- `internal/httpapi/i18n_body_copy_contract_test.go`: removed the now
  unused `"adaptive"` body-copy allowlist entry.
- `internal/router/router_test.go`: renamed the two misleadingly-named
  `TestAdaptive*` tests to `TestLegacyAdaptiveStrategy*` — they set the
  legacy value directly and document that the router treats it as
  default ordering.

## Test outcome

- `TestUnimplementedAdaptiveStrategyRejected`: FAIL before fix →
  PASS after (true red/green).
- `TestStrategySelectOffersOnlyImplementedStrategies` (new, in
  `settings_controls_contract_test.go`): asserts the select offers no
  static `adaptive` option, offers exactly the implemented set, and
  labels legacy values as unsupported — PASS (would fail pre-fix on all
  three assertions).
- `TestRoutingStrategiesValidate`, `TestSettingsControlsContract`,
  `TestBodyCopyI18nContract` (dictionary parity 573×3 incl. the new
  key): PASS.
- Full `scripts/verify.sh` + fresh CI/Security on the PR SHA: see PR.

## Remaining UI surface not reviewed

This slice traced Provider onboarding, Routing, Settings, Models, and
the command palette end-to-end (all 51 rendered `data-action` values
have handlers; all `data-go` targets are valid pages; all sampled
handlers reach real backend endpoints; the preset catalog's
verified/partial statuses are computed from implemented dialects and
test-enforced). Not deeply traced in this slice: Video page flows,
Activity/Audit/Evals/Observability/Playground/Cost/Access pages, and
RTL visual rendering. No claim of a full UI audit is made.
