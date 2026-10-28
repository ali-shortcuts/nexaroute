# Browser acceptance

This is the mandatory browser gate and how it differs from the optional
screenshot evidence. The rule that governs both: **a skipped browser
suite is never a successful browser acceptance run.**

## Mandatory gate (CI)

The CI `verify` job's **Browser acceptance test** step provisions the
pinned browser and runs `scripts/run-browser-acceptance.py`, which
executes the three existing suites in order, one status per suite:

| Suite | Script | PASS marker |
| --- | --- | --- |
| browser-e2e | `scripts/test-browser-e2e.py` | `BROWSER E2E PASS` |
| browser-e2e-502 | `scripts/test-browser-e2e-502.py` | `BROWSER E2E #502 PASS` |
| live-visual-agent | `scripts/test-live-visual-agent.py` | `LIVE VISUAL AGENT E2E PASS` |

The runner prints one `BROWSER ACCEPTANCE: <suite>: PASS|FAIL|SKIPPED`
line per suite, writes `summary.json` into the diagnostics directory,
and the step uploads that directory as an artifact and copies the
summary into the job summary. Exit codes: `0` all PASS, `1` any FAIL,
`2` any SKIPPED (and no FAIL). The job therefore fails on a skip.

What the suites cover, on a fresh loopback gateway with probe
disabled and no real provider credential:

- Clean first startup and the empty-provider / empty-route / empty-activity states.
- Provider add/edit, model discovery (discovered + manual), route creation, ordered fallback inspection, and a real fallback drill (one loopback upstream failing, one answering).
- Provider secret hygiene: saved secrets never render in the edit UI, never return from read APIs, never appear in browser requests except the single explicit replacement request, and clear/replace are distinct operations.
- Navigation (all sidebar pages, mobile 390px, title/offset consistency), the real theme-toggle click, the real language-switch click (Persian renders `dir="rtl"`, `lang="fa"` and restores `ltr`/`en`), and settings-tab navigation.
- Network/API failure and recovery: a 500 snapshot surfaces as Unavailable/Offline and recovers to Connected/Live on reload, Video Studio failure scenarios, malformed payloads, and the video-disabled state with no global banner.
- Browser console errors (asserted empty at section boundaries), failed-request and page-error capture, and the configured admin mode: keyless emergency loopback status, hidden sign-in control, and denial (401) of a DNS-rebinding Host header.

## Pinned dependencies

- Python Playwright: `playwright==1.55.0` in the CI step. The version pins the browser revision `playwright install --with-deps chromium` downloads, so re-runs provision the same browser.
- Node evidence tooling: `playwright-core` is pinned to `1.55.0` and `axe-core` to `4.10.2` in `scripts/a11y/package.json`.
- Chromium resolution for the suites, in order: `NEXAROUTE_CHROMIUM_BIN`, a system `chromium`/`google-chrome` on PATH, then Playwright's bundled full Chromium binary. The last step matters because `playwright install` does not put a binary on PATH; the old `which()`-only check reported a skip on a correctly provisioned machine.

No production security setting is weakened for these tests. In
particular the admin cookie and loopback-trust rules are untouched;
the suites run in the keyless emergency loopback mode the gateway
already provides, and a configured admin key / OIDC tenant is not
simulated here (see "Still not covered" below).

## Optional evidence (not a gate)

`scripts/browser-evidence.mjs` (workflow `Browser evidence`) captures
theme/language/viewport screenshots and axe results. It is optional
evidence generation, never a merge gate and never a substitute for
the acceptance gate above. Its exit contract is `0` captured,
`1` axe serious/critical violations, `2` environment blocked (no
screenshots by design). The companion `browser-evidence-pr` workflow
only proposes fixture PRs from successful runs on `main`.

## Running locally

```sh
python3 -m venv .venv-browser
.venv-browser/bin/pip install "playwright==1.55.0"
.venv-browser/bin/python -m playwright install --with-deps chromium
# Go must be on PATH (the suites invoke `go run ./cmd/gateway`).
.venv-browser/bin/python scripts/run-browser-acceptance.py
```

The runner prints the diagnostics directory; per-suite redacted logs
and `summary.json` land there. To run one suite directly, set
`NEXAROUTE_CHROMIUM_BIN` to the Playwright Chromium binary first
(the runner does this for you when it runs all three).

If a dependency is missing, the runner reports SKIPPED with the exact
remedy above and exits `2`. Do not work around a skip by editing a
suite to pass without a browser, and do not mark a skipped run as
passed in a report.

## Failure diagnostics and redaction

On failure the diagnostics directory holds, per suite, the redacted
stdout/stderr log and `summary.json`. The main suite additionally
preserves, under the evidence directory, its redacted gateway log
(last 256 KiB; the gateway logs every response with status >= 400, so
throttled 429s are visible there), the `/admin/api/` responses the
page observed (path and status only, never bodies or headers), and
gateway liveness, `/healthz` status and exit code
(`gateway-diagnostics.json`). That set exists because the first
failure of this gate left only a traceback: the dashboard was stuck
at Unavailable with no record of whether its snapshot fetch had been
stubbed, throttled, or never answered. Redaction replaces test canary
values, bearer tokens, `x-admin-key` values, cookie headers and
`api_key` values with `[REDACTED]` before anything is written, and
assertion messages in the suites never interpolate a secret value
(a failing assertion used to print the canary itself). The runner
never reads browser cookies, localStorage, sessionStorage or form
values, so none of those can reach a log or artifact. Screenshots
taken by a mandatory run go under the diagnostics directory, not
into the repository checkout; direct suite runs keep their historical
evidence paths.

## Defects found by actually running the suites

Two pre-existing test-suite defects surfaced during verification
and are fixed in this change (no production code touched):

- Playwright route glob: a trailing `**` does not cross `/`, so
  `**/v1/video/jobs**` matched the jobs list URL but never
  `/v1/video/jobs/<id>`. The #414 jobs dispatcher therefore never
  stubbed tracked-job fetches; they hit the real gateway (which
  has no such job), returned 404, and the #246 console assertion —
  the first console check after #414 — failed on them. Both jobs
  dispatchers (issue #414 and issue #246) now register the list
  pattern and the `**/v1/video/jobs/**` pattern. Verified with an
  isolated route-matching probe before the fix was applied.
- Transient success window: the visual agent holds `success` for
  only `TERMINAL_SETTLE_MS` (4000 ms) before returning to `idle`.
  The live-visual suite took a slow full-page screenshot between
  the terminal event and the success assertion, so under load the
  window closed first and the check failed with status `idle`
  even though the failover journey had succeeded. The suite now
  asserts success promptly after awaiting the response; the
  active state is still recorded in `lva-performance.json`, and
  the failover journey is asserted from snapshot event kinds.
- Admin-bucket starvation after the DNS-rebinding probe (the CI
  failure on this PR: `#connection-text` expected `Connected`,
  observed `Unavailable` at the snapshot-recovery assertion). The
  dashboard refreshes only on page load, and its seven parallel
  admin fetches share the gateway's per-IP admin token bucket
  (capacity 90, refill 1.5/s) with every other loopback caller.
  The rebinding probe is an unauthenticated admin request with no
  auth error, so the middleware charges it the full bucket
  (the F11 anti-oracle rule); when the bucket holds >= 90 at that
  moment it drains to ~0, and the recovery reload's refresh is
  throttled with 429s, leaving the dashboard at Unavailable. The
  gateway behavior is deliberate and unchanged -- no security
  setting is weakened. The suite now reloads until Connected or
  a 60s deadline instead of asserting once after a single 5s
  window: each attempt is a real refresh, and a genuinely broken
  recovery still fails at the deadline. Reproduced deterministically
  against a loopback gateway: rebinding probe, then seven parallel
  refresh fetches all 429; after ~6s of refill, all 200.

## Still not covered (honest boundary)

- A configured admin key exercised through the browser emergency modal, and a live OIDC tenant login: the suites assert the keyless emergency mode they boot in. A key-configured browser login and any live tenant remain unverified here.
- Live upstream providers: all provider traffic in these suites is loopback fakes or browser-boundary mocks for discovery/check/test. No paid call, no real credential.
- The sandbox's own Chromium build (`/opt/meta-chromium/chrome`) refuses loopback navigations; that is why the local remedy uses stock Playwright Chromium, which navigates loopback normally. The sandbox limitation is an environment property, not a product defect, and it is never worked around by weakening a security check.
