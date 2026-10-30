# ComfyUI real-inference acceptance automation (#651)

Parent issue: [#651](https://github.com/ali-shortcuts/nexaroute/issues/651).

## Current status

**Actual model inference: NOT_RUN (BLOCKED) in this repository environment.**
No provisioned ComfyUI server, compatible text-to-video model, or GPU/CPU
resource allocation was available to this agent. The deterministic tests use
loopback HTTP fixtures; fixture output is **not** real video inference and is
not recorded as acceptance evidence.

This slice adds the opt-in automation and its negative-path regression tests.
It does not promote ComfyUI from `UNVERIFIED` and does not claim live model
interoperability.

## Command

```bash
nexaroute comfyui-accept \
  --config /etc/nexaroute/config.json \
  --confirm-live \
  --out docs/reports/verification-slices/comfyui-acceptance-$(date -u +%Y%m%dT%H%M%SZ).json
```

The command:

- Refuses to run without `--confirm-live`, before loading a target or making a
  network request, and writes no report in that case.
- Reads only the existing `video.comfyui_*` configuration. It has no built-in
  ComfyUI endpoint, does not download models, and does not call any paid
  service.
- Requires a readable workflow and valid prompt/width/height/frames node
  mapping before contacting the server.
- Requires an installed `ffprobe` (override with `--ffprobe`). Without it,
  playability cannot be proven and the run is `BLOCKED` rather than `PASS`.
- Reads ComfyUI `/system_stats` to record the ComfyUI, Python, and PyTorch
  versions plus RAM/VRAM constraints.
- Submits one minimal text-to-video workflow, polls until completion (bounded
  by `--timeout`, default `10m`), downloads through the provider, persists
  through the same local asset store used by the gateway, then reads the asset
  back from disk to verify non-zero size, video metadata, matching SHA-256, and
  a decodable video stream via `ffprobe`.
- Removes a partial per-run asset workspace and attempts to delete a pending
  ComfyUI queue entry when submission, polling, download, or verification
  fails. It never sends ComfyUI's global interrupt for a running prompt.
- Writes an atomic `0600` JSON report with `PASS`/`FAIL`/`BLOCKED`/`NOT_RUN`
  checks, sanitized endpoint metadata, workflow SHA-256, prompt SHA-256 (not
  prompt text), server versions/resources, job state, asset facts, and cleanup
  status.

Exit codes: `0` PASS, `1` FAIL, `2` usage/config error, `3` BLOCKED.
`NOT_RUN` is a check status, not an exit code. A successful real run keeps the
verified output under the configured `--asset-dir` (default
`.artifacts/comfyui-acceptance/`) for operator review. Do not commit large
media outputs; commit only the sanitized report/runbook evidence.

`--asset-dir` is the acceptance retention root, not automatically the
gateway's `video.storage_root`. The harness verifies the same local store
implementation (containment inside the root, regular 0600 files under 0700
directories) through its `comfyui:asset_path` check, but the
`comfyui-accept` command itself calls the provider directly and never
exercises the gateway HTTP asset-serving path or its
`video.auth_token_env` check. Since the 2026-10-29 extension, that
gateway path is covered by a gateway-stack test instead
(`TestHandlerServesComfyUIAssetThroughGateway`: authorized retrieval,
missing/wrong token rejection, traversal rejection, tamper detection —
see the 2026-10-29 section). Do not serve `--asset-dir` over
unauthenticated HTTP; a live gateway-path serving check is a separate
operator step, and the report must not claim it.

## What closes #651

An operator with a provisioned installation must run the command above with a
real compatible text-to-video workflow and attach the sanitized report to the
issue. The report must show:

- `outcome: PASS` and `inference_executed: true`;
- non-empty server version/resource metadata;
- a completed job with a prompt ID;
- one or more persisted assets with `content_type: video/mp4`, non-zero
  `size_bytes`, a 64-hex-digit SHA-256, ComfyUI `node_id`/`filename` metadata,
  and an `ffprobe` codec/width/height/duration;
- cleanup `local_status: PASS` (retained verified output) or an explicit
  operator note when a fixture/loopback target was used.

If any item is absent, the issue remains open with the missing evidence named.

## Deterministic negative coverage (hermetic, loopback only)

`cmd/gateway/comfyui_accept_test.go` covers:

| Scenario | Expected behavior |
|---|---|
| No `--confirm-live` | exit `2`, zero upstream calls, no report |
| ComfyUI not provisioned | exit `3`, `BLOCKED`, inference `NOT_RUN` |
| Unavailable server | exit `1`, `FAIL`, no fabricated version or asset |
| Invalid workflow/node mapping | exit `1`, preflight `FAIL`, server never contacted |
| Timeout | exit `1`, poll `FAIL`, pending prompt deleted, partial workspace removed |
| Failed inference (`status_str: error`) | exit `1`, poll + inference `FAIL`, job recorded as `failed`, no asset, exactly one submission, pending prompt deleted |
| Transient poll error | poll falls back to the queue and retries `GetJob` only; the prompt is submitted exactly once, then the run can still PASS |
| Oversized output | exit `1`, download `FAIL`, no partial local asset retained |
| Completed without MP4 | exit `1`, fail-closed, no asset reported |
| Successful fixture lifecycle | exit `0`, report/PASS/checks, retained fixture asset verified for size/metadata/SHA-256, `asset_path` PASS (inside the acceptance root, regular 0600 file), report contains no prompt text |

`internal/video/providers/comfyui_test.go` additionally covers invalid node
mapping variants, unavailable server, context deadline, `/system_stats`
parsing, output-size limits, malformed history, and safe cancellation. These
tests prove the adapter and harness contract; they do **not** prove real model
inference.

## Local verification (2026-10-27)

- `go test ./... -count=1 -timeout=540s` — PASS.
- `go test -race ./cmd/gateway ./internal/video/providers -count=1 -timeout=360s` — PASS.
- `./scripts/verify.sh` — `VERIFY PASS` (the browser acceptance step was
  skipped because Chromium and Python Playwright are unavailable in this
  environment; this is unrelated to ComfyUI inference).
- Actual ComfyUI model inference was not run. These commands verify only the
  adapter contract and the opt-in harness, not live model interoperability.

## Independent review follow-up (2026-10-27, PR #669 follow-up commit)

The lane's harness was reviewed against the six items in issue #651. Three
gaps were closed in this follow-up, without changing the live-run verdict:

- Failed inference had no harness test: `status_str: error` is now covered
  (job recorded `failed`, inference `FAIL`, no asset, exactly one submission,
  pending prompt deleted, workspace removed).
- Poll retries had no no-resubmit proof: a transient history failure now
  covered — the harness falls back to the queue and retries `GetJob` only, so
  the prompt is still submitted exactly once.
- Item 6 had no explicit check: `comfyui:asset_path` now asserts every
  persisted asset resolves inside the acceptance root as a regular 0600 file,
  and the success test asserts the retained file mode, the root prefix, and
  that the report contains no prompt text (only length + SHA-256).
- The runbook's exit-code line wrongly listed `4 NOT_RUN`; corrected to
  `0/1/2/3`, with `NOT_RUN` as a check status only.

Verification on the follow-up head (worktree at PR #669 head `8b89bac` plus
this commit):

- `go test -race ./cmd/gateway/ -run TestComfyUIAccept -count=1` — PASS
  (including the two new tests and the extended success test).
- `go test ./cmd/gateway/ -count=1` (full package) — PASS.
- `go test ./internal/video/providers/ -run TestComfyUI -count=1` — PASS.
- Red proof for the new check: with the old `comfyui_accept.go`, the extended
  success test FAILs with `asset_path status="", want PASS`; with the new
  code it PASSes. The failed-inference and transient-poll tests are regression
  coverage for behaviour the adapter already had, not bug fixes.
- `gofmt` clean, `go vet` clean on the two touched packages.
- NOT verified locally on this head: full `go test ./...` (a dispatch-timeout
  interrupted that run; its outcome is unknown, not a pass) and a fresh
  `./scripts/verify.sh` run. Those gates are left to CI on the new head.
- Actual ComfyUI model inference remains NOT_RUN/BLOCKED: no provisioned
  server, model, or workflow was available, no model was downloaded, and no
  paid or public inference endpoint was called.

## 2026-10-28 regression extension (PR #669 follow-up, NOT a live run)

One adapter defect was found and fixed, with red/green proof:

- **Defect:** `CreateJob` ignored a non-empty `node_errors` payload when the
  server also returned a `prompt_id`, so a rejected workflow could be
  treated as an accepted job and polled. **Fix:** a `node_errors` value
  other than empty/`null`/`{}` now fails the submission closed. The
  payload itself is not echoed in the error (it may embed workflow input
  values). Red proof: `TestComfyUINodeErrorsWithPromptIDRejected` FAILed
  on the old code (`CreateJob error=<nil>`) and PASSes with the fix.

New deterministic regression coverage (fixtures only — none of this is
real inference):

- Adapter (`internal/video/providers/comfyui_regression_test.go`):
  malformed workflow files (syntax error, top-level array/string,
  node without an inputs object, null inputs, oversized file),
  server-rejected jobs (HTTP 400 with `node_errors`, HTTP 500, exactly
  one submission), unknown node class deferring to server validation
  (the adapter has no server node registry, so it must not invent an
  allowlist), history failure states (execution error, images-only and
  webm-only completion), stalled jobs absent from history and queue,
  running-prompt cancellation refusing without any delete, download
  rejection of image content types / empty bodies / HTTP 404, the
  exact-limit download boundary, cancelled-context status reads, and
  malformed-history fallback to the queue.
- Harness (`cmd/gateway/comfyui_accept_regression_test.go`):
  polling interrupted by context cancellation (deadline and
  pre-cancelled contexts, no completion, no resubmit), a stalled job
  timing out at the harness deadline with exactly one submission and
  fail-closed cleanup, and running-prompt cleanup recording BLOCKED
  without a global interrupt.

Verification on this head (Go 1.25.1, local): `go test -race
./internal/video/providers/ -run TestComfyUI` PASS, `go test -race
./cmd/gateway/ -run TestComfyUIAccept` PASS, `gofmt`/`go vet` clean on
the touched packages. Full `go test ./...` and `./scripts/verify.sh`
were not yet run on this head (see the final report); CI on the
updated PR is the gate for those.

## 2026-10-29 harness extension (local gaps closed, NOT a live run)

Gap audit against the #651 local requirements. Already covered by the
tests cited above and NOT duplicated here: unreachable server, invalid
workflow/node mapping, poll timeout, oversized output, failed-job states,
poll safe-retry (no resubmit), adapter cancellation, gateway-level cancel,
and cleanup of a partial workspace plus a pending queue entry. Four
genuine gaps were found and closed; one of them was a production defect.

- **Defect fixed — duplicate output filenames overwrote stored assets:**
  `DownloadOutputs` passed every server-supplied filename through the
  sink, which keys stored files by sanitized basename, not by node. Two
  outputs sanitizing to the same basename (two nodes returning `same.mp4`,
  or `../same.mp4` plus `same.mp4`) therefore overwrote each other while
  the first asset's record still claimed its own SHA-256/size — a
  self-inconsistent persisted set. **Fix:** `DownloadOutputs` now
  rejects the whole download before any `/view` request when two outputs
  share a `filepath.Base` filename (`duplicate video output filename`).
  Red proof: `TestComfyUIDownloadRejectsDuplicateOutputFilenames` FAILs
  on the pre-fix code (`DownloadOutputs error=<nil>, want duplicate
  rejection`) and PASSes with the fix.
- **Gateway-path retrieval (was untested):** the `comfyui-accept` command
  still calls the provider directly and never exercises gateway HTTP
  serving — that part is unchanged. What is new is a gateway-stack test
  proving retrieval *through NexaRoute* for a ComfyUI-metadata asset:
  `TestHandlerServesComfyUIAssetThroughGateway` serves a stored asset
  (provider=comfyui, node_id, filename, subfolder, type) over
  `GET /v1/video/jobs/{id}/assets/{assetID}` and asserts byte-identical
  body, `video/mp4`, `nosniff`/`no-store` headers, missing token → 401,
  wrong token → 401, path-traversal asset names (`..`, `%2e%2e`,
  `..%2f..%2fsecret`, embedded traversal) → 404 each, unknown job → 404,
  and same-size tampering of the stored file → 500
  `asset_integrity_error` (recorded SHA-256 checked before serving).
- **Download sanitization (was untested at the adapter level):**
  `TestComfyUIDownloadKeepsTraversalFilenamesInsideStoreRoot` drives
  server-supplied filenames `../escape.mp4`, `/tmp/abs.mp4`, and
  `..\..\win.mp4` through `DownloadOutputs` into a real local store and
  asserts every stored URI stays inside the root, no stored filename
  contains a separator, and no file escapes beside the root. Sink
  containment itself needed no fix.
- **Submit-timeout exactly-once (was untested):**
  `TestComfyUICreateJobSubmitTimeoutSubmitsExactlyOnce` (adapter) and
  `TestComfyUIAcceptSubmitTimeoutSubmitsExactlyOnce` (harness) prove a
  `/prompt` call cut off by the caller's context deadline errors instead
  of inventing success, submits exactly once, and leaves no retained
  asset; the harness case records FAIL with local cleanup PASS and
  remote cleanup NOT_RUN (no prompt ID exists to cancel). The adapter
  has no retry loop, so exactly-once holds by construction; these tests
  pin it.
- **Download timeout (was untested):**
  `TestComfyUIDownloadTimeoutFailsClosed` proves a `/view` download cut
  off by the caller's context deadline errors with nothing persisted in
  the store.
- **Fixture labeling pinned:** the success test now also asserts the
  report's limitations contain an entry with both `loopback` and
  `harness evidence only`, so a future refactor cannot silently drop
  the marking that keeps a fixture PASS from reading as real inference.
  No labeling defect was found; the pin guards the existing behavior.

Test-fixture note: the first versions of the new timeout tests
deadlocked in `httptest.Server.Close` because their fake handlers
blocked on the request context alone and the client transport does not
reliably tear the server-side connection down on a client deadline.
That was a fixture defect, not a product defect (the product calls
errored promptly in every run). The fixtures now release the handler
through a test-controlled channel. The same client-disconnect cleanup
family is independently visible in Wave3 httpapi tests; no Wave3 code
was touched here.

Verification on this head (base `bb5865f`, Go 1.27.0, loopback
fixtures only — none of this is real inference):

- New tests plain: providers 4/4 PASS (1.006s), harness 2/2 PASS
  (0.833s), gateway API 1/1 PASS (0.181s).
- New tests `-race`: providers ok 2.690s, gateway ok 3.567s, API ok
  1.460s.
- Full packages: `go test -count=1 ./cmd/gateway/` ok 207.442s;
  `go test -count=1 ./internal/video/...` all 13 packages ok
  (providers 35.902s, audio 32.434s, render 21.420s).
- `gofmt` clean on all touched files; `go vet` clean on `cmd/gateway`,
  `internal/video/providers`, `internal/video/api`,
  `internal/video/storage`.
- `./scripts/verify.sh` NOT run locally: it runs the whole repository
  plus a browser step that cannot run in this sandbox, and the scoped
  full-package runs above already took ~6 minutes under sibling
  contention. CI on the PR is the gate for it.
- Secrets scan of the diff: clean (fixture literals and test-token
  assertions only; no credential values, prompts, or media).
- Actual ComfyUI model inference remains NOT_RUN/BLOCKED (see below);
  no model was downloaded, no paid or public inference endpoint was
  called, no production instance was touched.

## 2026-10-30 rebase + partial-download cleanup (local only, NOT a live run)

- **Rebase:** the five 2026-10-29 commits rebased cleanly onto main
  `02a43ac` (which includes merged #752, #754, #756) with no
  conflicts and no content change to the original fixes; the diff
  against main is still exactly the original 7 files plus the new
  changes below.
- **Old verify failure, diagnosed, not duplicated:** the only failing
  check on the old PR head was `verify`, and its only failing test
  was `TestIssue663ProvidersScheduleRecoveryQueueFullCallback`
  (internal/probe, CI run 40915396200, job 123594150864:
  "queue-full timer callback did not publish a
  recovery_queue_full event", task cleared but event not yet
  visible). Root cause is a clear-then-publish ordering race in
  `internal/probe/engine.go`: the timer callback calls
  `clearRecoveryTask` and only then `bus.Add`, so a test that polls
  task clearance and snapshots the bus immediately can observe the
  clear before the publish under CI scheduling load. That race is
  already fixed on main by merged PR #752 (test-only: the callback
  test polls for the event within the same 2 s bound instead of
  snapshotting once; assertion unchanged). No probe production code
  was changed here and no second fix was opened: a
  publish-before-clear production reorder was considered and
  deliberately not taken, because it would duplicate a just-merged
  decision and the recovery outcome itself (task cleared, event
  follows) is unchanged either way. On the rebased tree the
  callback test passes (see verification below).
- **Defect fixed — partial download left orphaned assets:**
  `DownloadOutputs` persisted each output as it arrived, so a
  download that failed on a later asset (oversized, HTTP error,
  timeout, non-video) returned an error but left the earlier
  assets persisted in the store — an orphaned partial set that a
  retry could mistake for, or overwrite under, a completed job.
  The job itself was never marked complete (the orchestrator and
  the harness both treat a download error as failed/resumable),
  but the partial files leaked. **Fix:** `DownloadOutputs` now
  wraps the existing per-asset loop; on any error it best-effort
  deletes exactly the assets that call persisted, on a detached
  context so a timed-out download can still clean up, and still
  returns `nil` assets plus the original error. The sink
  capability is optional (`assetDeleter` type assertion), so
  existing sinks are unaffected. `storage.Local.Delete` validates
  the URI inside the store root (lexical and symlink-resolved,
  same as `Open`) and removes the named path itself — never
  following a symlink — plus its `.json` sidecar.
  Red proof: `TestComfyUIDownloadPartialFailureCleansUpAndLeavesOtherJobIntact`
  FAILs on the pre-fix adapter (left `first.mp4` and
  `first.mp4.json` under the failed job's directory, both cases)
  and PASSes with the fix, for a later oversized asset and for a
  later HTTP-500 asset; in both cases an unrelated job's asset in
  the same store reads back byte-identical, metadata included.
  `TestLocalDeleteRemovesAssetAndMetadata` and
  `TestLocalDeleteRefusesOutsideRoot` pin the delete primitive:
  file plus sidecar removed, second delete fails, an
  outside-root URI is refused and its file untouched.

Verification on this head (base `02a43ac`, Go 1.27.0, loopback
fixtures only — none of this is real inference):

- Focused plain: providers `TestComfyUI*` ok 3.917 s, storage ok
  0.996 s, API gateway-retrieval test ok 0.127 s, gateway
  `TestComfyUIAccept*` ok 1.989 s.
- Focused `-race`: providers `TestComfyUI*` ok 10.275 s,
  storage (full package) ok 4.211 s, API (full package) ok 1.967 s,
  gateway `TestComfyUIAccept*` ok 5.752 s.
- Probe callback test on the rebased tree (includes merged #752):
  `TestIssue663ProvidersScheduleRecoveryQueueFullCallback`
  `-count=20` ok 0.611 s; earlier on the pre-rebase tree it also
  passed `-count=50` plain and `-count=20 -race` for the
  `ScheduleRecovery*` set.
- `gofmt` clean on all touched files; `go vet` clean on
  `internal/video/providers`, `internal/video/storage`, and
  `internal/probe`.
- `./scripts/verify.sh` is not run locally (whole repository plus
  a browser step that cannot run in this sandbox); CI on the PR
  is the gate for it. Actual ComfyUI model inference remains
  NOT_RUN/BLOCKED (see below); no model was downloaded, no paid
  or public inference endpoint was called, no production
  instance was touched.

Re-checked 2026-10-30 05:58 +0430 (names only, no values read):
nothing listening on `127.0.0.1:8188` or `127.0.0.1:8189`;
`COMFYUI_URL`, `COMFYUI_BASE_URL`, `COMFYUI_API_URL`,
`COMFYUI_HOST`, and `COMFYUI_MODEL` all absent-or-empty;
`/etc/nexaroute` does not exist; no gateway config points at a
ComfyUI instance; no operator authorization naming an instance,
model, or workflow exists on record. Present, so not blockers:
`ffprobe` 8.1.2, the deterministic fixture suites, and the
`comfyui-accept` harness itself. The verdict is unchanged:
NOT_RUN/BLOCKED, and #651 stays OPEN.

## Live prerequisites still missing (2026-10-28, checked first-hand)

Actual model inference remains NOT_RUN/BLOCKED, and #651 stays OPEN
until it is demonstrated. Missing, exactly:

- No ComfyUI server reachable (nothing on `127.0.0.1:8188`; no
  docker/kubectl to provision one in this environment).
- No operator-supplied compatible text-to-video model or API-format
  workflow, and no gateway config pointing at one (`/etc/nexaroute`
  does not exist here; no `comfyui_*` config was supplied).
- No GPU/CPU resource allocation for a real inference run.
- Present, so not blockers: `ffprobe`/`ffmpeg` binaries, the
  deterministic fixture suites above, and the `comfyui-accept`
  harness itself.

No model weights were downloaded, no customer data was used, no
production instance was touched, and no fixture output is reported
as real inference anywhere in this update.

Re-checked 2026-10-29 (names only, no values read): nothing listening
on `127.0.0.1:8188` or `127.0.0.1:8189`; `COMFYUI_URL`,
`COMFYUI_BASE_URL`, and `COMFYUI_API_URL` all absent-or-empty;
`/etc/nexaroute` does not exist; no gateway config points at a ComfyUI
instance; no operator authorization naming an instance, model, or
workflow exists on record. Present, so not blockers: `ffprobe` 8.1.2,
the deterministic fixture suites, and the `comfyui-accept` harness
itself. The verdict is unchanged: NOT_RUN/BLOCKED.

## Limitations

- The command cannot distinguish a real ComfyUI instance from a fixture served
  on loopback. Its report records `evidence_class: loopback` and says a PASS is
  real inference only when the operator-provisioned target is a real model.
- `ffprobe` proves a decodable video stream, not subjective content quality or
  that the requested prompt was rendered correctly.
- A running ComfyUI prompt cannot be cancelled without the global interrupt;
  the harness reports that cleanup as `BLOCKED` instead of claiming success.
- ComfyUI supplies no idempotency key for a submission: the harness submits
  exactly once per run, but a manual retry after an ambiguous submission
  failure (network drop after the server accepted the prompt) may create a
  second prompt. Check the ComfyUI queue before re-running.
- A completion listing two outputs with the same filename is refused
  whole (nothing written) instead of letting the second output
  overwrite the first; the operator must disambiguate the workflow.
- Workflow/model licenses, model files, and resource allocation remain the
  operator's responsibility.
