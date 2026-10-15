# M2-11 + M2-13 — WCAG 2.2 AA evidence and the embedded-asset budget

M2-13 (always-on Go gates over the embedded web FS):
- Per-file and total size budgets for the SPA assets.
- Embedded-assets-only: the shell may reference only local
  stylesheets/scripts; CSS must not @import or url() anything
  remote. (Provider base URLs in JS data are configuration,
  not asset loads.)

M2-11 (accessibility):
- A contrast gate: the CSS custom properties of both themes
  are parsed and every text-bearing foreground/background pair
  must meet WCAG 4.5:1. Building this gate found real defects:
  --subtle failed on every surface in BOTH themes (3.2-4.0 dark,
  2.7-3.0 light) and light-theme accent/good/warn failed on
  some surfaces; the palette is corrected in this unit.
- An automated axe-core scan (scripts/a11y-scan.mjs, raw CDP,
  no npm runtime deps beyond axe-core itself) drives a real
  Chromium over every page in both themes against a local
  gateway and fails on any serious/critical violation; first
  contentful paint is measured in the same run (localhost
  budget < 1s, M2-13).
- ENVIRONMENT BOUNDARY (recorded, not worked around): the
  Chromium build available in the build sandbox
  (meta-chromium 152) refuses every loopback navigation with
  ERR_BLOCKED_BY_LOCAL_NETWORK_ACCESS_CHECKS — via CDP target
  creation, Page.navigate, browser-UI initial tab, feature
  flags (LocalNetworkAccessChecks off, address-space
  overrides) and an Xvfb non-headless attempt (its debugging
  port never opens). The live in-browser axe run and the FCP
  measurement are therefore UNVERIFIED in this environment;
  the script detects the block and explains itself, and
  produces the full report on any standard Chromium host.
  docs/browser-evidence/m2-11/README.md carries the details.
