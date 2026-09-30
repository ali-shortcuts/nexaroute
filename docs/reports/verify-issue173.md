# Verify report — issue #173 (F8: bound client SSE retry + malformed-frame status)

- Audit base: `76525d8e3e51248a92ac16d087db50e13e2c1d20` (privacy fix verified).
- Branch base at work start: `4888331` (main tip then).
- Rebased onto `origin/main` = `e0150e3` (Issue #176 F12, touches only
  `internal/httpapi/eval_live_test.go` — no overlap, rebase clean).
- Work commit (post-rebase): `441c05a`
  `feat(web): bound SSE retry with jitter, malformed-frame status (#173)`.
- Prior state proof: `internal/httpapi/web/app.js` retry was
  `Math.min(8000, 1500 + consecutiveFailures * 1000)` (capped, no jitter);
  frame `catch {}` silently dropped malformed frames; no `sseStats`,
  malformed counter, or degraded signal existed (grep for
  `sseStats|malformed|jitter` in `internal/httpapi/web` found nothing).

## Change (client-only, `consumeLiveEvents`/fallback path)

`internal/httpapi/web/app.js` (+118/−13):
- `sseRetryDelayMs(failures, randFn)`: `min(8000, 1500 + n*1000)` base with
  subtractive jitter up to 500 ms, floored at 250 ms → always in
  `[250, 8000]`; deterministic under injected `rand`.
- `sseParseFrame(frame)`: heartbeats/frames without `data:` skipped as before;
  `data:` frames with invalid JSON bump `malformedFrames` and are skipped —
  valid events keep flowing. Never logs raw payloads (no `console.*` in file).
- `NexaRoute.sseStats()`: `{transport, state, consecutiveFailures,
  malformedFrames, lastRetryMs, lastError}` — counts/transport only.
- Degraded path: `sseNoteFailure` sets `reconnecting` + throttled toast
  (one per 15 s, fake-clock injectable); `sseNoteOpen` resets failures and
  toasts `Live events reconnected` exactly once per degradation episode.
- `stopLiveEvents` / `sseResetStats` (+ test hooks `_sseHooks`,
  `_consumeLiveEvents`, `_liveSeq`) for cleanup and determinism.
- Server resume protocol (`?since=liveSeq`) and snapshot polling fallback
  unchanged; retry loop null-safe after stop.

`internal/httpapi/web/sse_client.test.mjs` (new): deterministic harness that
loads the real `app.js` in a stubbed DOM sandbox with fake clock/rand/sleep.
Covers reconnect cap, malformed frames, successful recovery, cleanup.

## Test evidence (real output)

`node --check internal/httpapi/web/app.js` → pass (also run by verify.sh).

`node --test --test-reporter=spec internal/httpapi/web/sse_client.test.mjs`:
```
▶ F8 client SSE retry + malformed-frame status
  ✔ retry delay stays bounded, capped, jittered and deterministic
  ✔ malformed frames bump the counter; valid events still flow; no payload leak
  ✔ successful recovery resets failures and signals exactly once
  ✔ cleanup stops the retry loop and resets status
ℹ tests 4 / pass 4 / fail 0
```
Deterministic retry sequence with `rand=()=>0`: `[2500, 3500, 4500, 5500,
6500]`, capped at 8000 from failure 7. Stream test: `liveSeq` 41→42 across
one malformed frame, `malformedFrames=1`, retry `2144ms` in bounds, no marker
leak in toasts/stats/logs.

`gofmt -l .` → empty. `go build ./...` → ok. `go vet ./...` → ok.

`bash scripts/verify.sh` → `VERIFY PASS`, exit 0 (128 `ok` lines, 0 `FAIL`):
clean + `-shuffle=on -count=10` unit/integration, `go vet`, clean + shuffle
race passes, `node --check` both web files, short fuzz suites, benchmark
smoke, linux amd64+arm64 builds.

## Remaining limitations

- `scripts/verify.sh` browser acceptance skipped by the script itself:
  `WARN: Chromium + Python Playwright unavailable`. The Node SSE harness above
  exercises the production stream/parse/retry path instead.
- The new harness is not wired into `scripts/verify.sh` or CI workflows
  (workflows untouched per issue rules; kept scope minimal). Run it with
  `node --test internal/httpapi/web/sse_client.test.mjs`.
