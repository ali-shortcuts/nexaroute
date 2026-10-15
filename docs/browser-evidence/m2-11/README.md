# M2-11 browser evidence — how to produce it, and the sandbox boundary

## Running the scan

```sh
npm --prefix scripts/a11y install   # fetches axe-core 4.10.2 (dev-only)
node scripts/a11y-scan.mjs          # builds the gateway, scans, writes axe-report.json here
```

The script builds `./cmd/gateway`, serves it on `127.0.0.1:18741`
with a minimal fixture config, drives Chromium over the DevTools
protocol (no puppeteer dependency), visits **every page in the
primary nav in both themes**, runs axe-core with the
`wcag2a`/`wcag2aa`/`wcag22aa` tag set, and fails on any
**serious or critical** violation. It also records
first-contentful-paint and enforces the M2-13 localhost budget
(FCP < 1000 ms). Set `CHROME_BIN` to choose the browser binary.

Always-on gates that do not need a browser live in
`internal/httpapi/web_assets_budget_test.go`: asset size budgets,
embedded-assets-only, and WCAG contrast ratios computed from the
theme variables for both themes (that gate found and fixed real
`--subtle`/accent/good/warn contrast failures in this unit).

## Status in the build sandbox: UNVERIFIED (browser-blocked)

The Chromium build available in the sandbox
(`/opt/meta-chromium/chrome`, 152.0.7977.82) refuses **every**
navigation to the locally served UI with
`ERR_BLOCKED_BY_LOCAL_NETWORK_ACCESS_CHECKS` ("initiated by a
public page to connect to devices or servers on your local
network"). Verified strategies that all hit the same block:

- CDP `Target.createTarget` with the gateway URL
- CDP `Page.navigate` after attach
- the gateway URL as the browser's initial tab (browser-UI initiated)
- `--disable-features=LocalNetworkAccessChecks,PrivateNetworkAccessChecks,...`
- `--ip-address-space-overrides=127.0.0.1:public` (with and without
  `--enable-features=LocalNetworkAccessIpAddressSpaceOverrides`)
- Xvfb non-headless launch (the debugging port never opens)

The scan script detects the `chrome-error://` landing page and
aborts with this explanation rather than producing an empty
report. The live axe run and the FCP number must be produced on
a standard Chromium/Chrome host (developer machine or a CI image
with a regular browser); nothing about the result is claimed
until then.
