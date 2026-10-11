# M2-1 UI Regression Verification

**Status:** DONE
**Date:** 2026-10-11
**Branch:** `completion/m2-1-ui-regression-check`

## Scope and result

Verified the existing UI regressions against the real Go gateway and Chromium browser, strengthened the browser assertions, fixed the remaining light-theme hero contrast defect, and committed versioned screenshots under [`docs/browser-evidence/m2-1/`](../browser-evidence/m2-1/).

## Acceptance results

| Requirement | Browser evidence | Result |
|---|---|---|
| Exactly one visible page title on every page | Exact visible `h1`/`h2` match on Overview, Providers, Routing, Video Studio, Activity, and Settings | PASS — one on each page |
| Consistent page/title top offsets (±2 px) | Measured at 1440×900 after clearing transient notices | PASS — title top 53.9375 px on all six pages; content top 147.734375 px on all six pages; each range is 0 px |
| Empty-state CTA separated from its description | Measured the real Video Studio `.video-empty` paragraph-to-button gap | PASS — 16 px (minimum required: 12 px) |
| All mobile navigation destinations reachable at 390 px | Each of six nav items checked for visible/enabled state, non-zero/in-bounds geometry, clickability, and no horizontal overflow | PASS |
| Privacy badges have correct semantic styles | Visible provider data-handling labels mapped to classes and computed colors in both themes | PASS — `Trains on data` → `warn`; `Does not train` → `good`; `Training unknown` → `neutral`; all three colors are distinct in dark and light themes |
| Screenshot proof of all three badge states in both themes | Two full-page Providers captures show the warning, safe, and neutral badges together | PASS |

### Measured badge colors

| State | CSS class | Dark theme | Light theme |
|---|---|---|---|
| Yes / warning | `warn` | `rgb(243, 199, 109)` | `rgb(162, 103, 9)` |
| No / safe | `good` | `rgb(88, 212, 154)` | `rgb(22, 134, 90)` |
| Unknown / neutral | `neutral` | `rgb(148, 163, 184)` | `rgb(75, 91, 110)` |

## Fixes made

- Added explicit light-theme text colors for the dark-gradient Overview and Video Studio hero panels, their headings/eyebrows/orbit labels, and ghost buttons. Visual review had found these inherited dark text colors that were not readable on the dark hero backgrounds.
- Configured the browser fixture with the local development fake video provider and a test-only session token. This exercises the real local Video Studio API without contacting an external provider and avoids the disabled-feature 404 toast contaminating screenshots.
- Strengthened `scripts/test-browser-e2e.py`: it now checks the real Video Studio empty state, measures title and content offsets, verifies every rendered privacy-label/class mapping and pairwise-distinct theme colors, and can direct its screenshots to a versioned evidence directory.

## Browser evidence

There are **26 screenshots**: 24 standard viewport captures (six pages × 1440×900 and 390×844 × dark/light) plus two full-page Providers captures (one per theme; 1440 px viewport, 1020 px full-page image) to show all three privacy states together. See the [evidence index](../browser-evidence/m2-1/README.md).

Key proof images:

- [Providers — privacy badges, dark theme (full page)](../browser-evidence/m2-1/providers-privacy-badges-fullpage-dark.png)
- [Providers — privacy badges, light theme (full page)](../browser-evidence/m2-1/providers-privacy-badges-fullpage-light.png)
- [Overview — mobile, light theme](../browser-evidence/m2-1/overview-390x844-light.png)
- [Video Studio — mobile, light theme](../browser-evidence/m2-1/video-390x844-light.png)

## Validation

- `python3 -m py_compile scripts/test-browser-e2e.py` — PASS
- `git diff --check` — PASS
- `python3 scripts/test-browser-e2e.py` with Chromium — PASS; see [`m2-1-browser-e2e.raw.txt`](m2-1-browser-e2e.raw.txt)
- `./scripts/verify.sh` with Go 1.27.2 — PASS, including the browser and Live Visual Agent E2E gates, Go tests, `go vet`, the clean race pass, short fuzz checks, and Linux amd64/arm64 builds; see [`m2-1-verify.raw.txt`](m2-1-verify.raw.txt)
