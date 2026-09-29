# CI Browser-acceptance flake investigation (e37a694)

## Symptom
The CI "Browser acceptance test" step failed once on commit `e37a694`
(`fix(dashboard): validate every runtime setting control, not only the first`)
then passed on rerun with no code change.

## Root cause (confirmed by local reproduction)
`scripts/test-browser-e2e.py` injected two synthetic console events by
overwriting the page-global `snap.events` and calling `renderConsole()`,
then asserted on the `#consoleCount` filter counts.

Meanwhile the dashboard's own `tick()` loop (`internal/httpapi/web/app.js`)
calls `refresh()` every ~1.8s, and `refresh()` replaces the whole `snap`
object (`snap = s`) with live server state — which in the test environment
contains zero events. If a tick landed between the synthetic inject and the
filter clicks, the counts collapsed to "0 events" and the
`#consoleFilter button[data-f="errors"]` assertion failed.

Reproduction before fix (same machine, same code, sequential runs):
- RUN 1 EXIT=1 — `AssertionError: Locator expected to contain text '1' / Actual value: 0 events`
- RUN 2 EXIT=1 — same
- RUN 3 EXIT=0 — PASS
- RUN 4 EXIT=1 — same
- RUN 5 EXIT=0 — PASS

i.e. 3/5 failed — the classic single-failure-then-green-rerun signature.

## Fix (`scripts/test-browser-e2e.py` only, no workflow changes)
1. **Deterministic race removal:** pause the dashboard (`#pauseBtn`, the UI's
   own supported mechanism — `tick()` skips `refresh()` while paused)
   before injecting synthetic events, and resume afterwards. The existing
   pause/resume assertions that follow still start from the unpaused state,
   so their expectations are unchanged.
2. **Startup hardening (secondary, same step):**
   - readiness wait raised 20s → 60s (`NEXAROUTE_E2E_READY_TIMEOUT`),
     requiring two consecutive `/healthz` 200s so a cold `go run` compile
     on CI is not mistaken for readiness;
   - gateway stdout/stderr now stream to a temp log file instead of an
     unread `PIPE` (pipe-buffer deadlock), and the log tail is attached to
     every startup assertion;
   - bounded port-retry loop (`NEXAROUTE_E2E_STARTUP_ATTEMPTS=3`): a fresh
     free port is picked per attempt and `address already in use` /
     `cannot listen` exits are retried instead of failing the run;
   - Playwright `expect` default timeout 5s → 15s
     (`NEXAROUTE_E2E_EXPECT_TIMEOUT_MS`) and `#apiState` initial wait
     10s → 15s for loaded CI runners.

## Verification after fix (5 consecutive local runs)
- RUN 1 EXIT=0 — BROWSER E2E PASS
- RUN 2 EXIT=0 — BROWSER E2E PASS
- RUN 3 EXIT=0 — BROWSER E2E PASS
- RUN 4 EXIT=0 — BROWSER E2E PASS
- RUN 5 EXIT=0 — BROWSER E2E PASS

Full per-run logs were captured under `.artifacts/e2e-runs/run-<i>.log`
during verification; each ends with:
`BROWSER E2E PASS: startup/navigation, localization/theme, provider
create/edit/secret-preserve/secret-replace/delete, discovery
failure/manual model, simple route create/edit/delete, advanced-route
protection, settings validation/persistence, Connect output, observability
filters/auto-scroll, pause/resume, no JS page errors`.
