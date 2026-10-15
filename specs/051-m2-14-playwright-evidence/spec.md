# M2-14 — Playwright all-page/flow evidence, responsive themes, a11y, diffs

Deliverables:
- scripts/browser-evidence.mjs: a Playwright (playwright-core,
  system Chromium, no browser download) evidence runner that
  boots a fixture gateway and captures every nav page across
  themes (dark/light) x languages (en + fa/RTL) x viewports
  (1440x900 desktop, 390x844 mobile) into
  docs/browser-evidence/m2-14/, runs axe per page, and writes a
  manifest.json with a SHA-256 per screenshot — re-runs report
  which shots changed (the diff baseline).
- docs/browser-evidence/m2-14/README.md: the capture matrix,
  run instructions, and a flow-evidence index mapping each UI
  flow to the handler-level test suites that cover it today.

ENVIRONMENT BOUNDARY (same as M2-11, recorded not worked
around): the sandbox Chromium build refuses all loopback
navigations (ERR_BLOCKED_BY_LOCAL_NETWORK_ACCESS_CHECKS), so
the screenshots themselves are UNVERIFIED in this environment;
the runner detects the block and aborts with the explanation
rather than writing empty evidence. On a standard Chromium
host it produces the full set.
