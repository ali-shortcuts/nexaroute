## Summary

Implements PRIVACY P3 dashboard UI changes as specified in issue #234.

## Changes

### Provider Cards & Detail
- Added data handling badge showing `trains_on_data` (yes=warning, no=safe, unknown=neutral) and retention
- Accessible text labels (not color-only) with tooltips
- Note displayed when present

### Add/Edit Provider Dialog
- Added "Data handling" section with:
  - `trains_on_data` select (unknown/yes/no)
  - `retention` select (unknown/none/limited)
  - Note input (max 200 chars, no control characters)
- Saved through existing provider save endpoint
- Secrets behavior unchanged

### Routing Page
- Shows route profile `privacy` setting (Any/No training)
- Clear warning message when route requires `no_training` but has no eligible deployments
- Success message when `no_training` route has eligible deployments

### Activity Page
- Shows "excluded by privacy requirement: N" when event carries `privacy_excluded` field

### Layout & Accessibility
- Single page title (no repeated headings)
- Consistent top spacing
- Works at 390px wide with no horizontal overflow
- Dark and light theme support
- WCAG AA contrast compliance

## Testing
- Extended browser e2e test (`scripts/test-browser-e2e.py`) with:
  - 3 providers of different data-handling classes (yes/no/unknown)
  - Badge visibility verification for all three values
  - Edit-and-save persistence test
  - No console errors
  - No horizontal overflow at 390px viewport
- Screenshots (1440x900 and 390x844, dark and light) committed under `docs/browser-evidence/privacy/`
- All Go tests pass: `gofmt -l .`, `go vet ./...`, `go test -race ./...`
- JS syntax check passes: `node --check`

## Evidence
Screenshots at `docs/browser-evidence/privacy/`:
- providers-1440x900-dark.png, providers-1440x900-light.png
- providers-390x844-dark.png, providers-390x844-light.png
- routing-1440x900-dark.png, routing-1440x900-light.png
- routing-390x844-dark.png, routing-390x844-light.png
- activity-1440x900-dark.png, activity-1440x900-light.png
- activity-390x844-dark.png, activity-390x844-light.png

Commit: 3a4a4ce