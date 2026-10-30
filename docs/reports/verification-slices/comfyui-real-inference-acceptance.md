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
| Ambiguous submission (connection drops after `/prompt`) | exit `1`, exactly one POST, no resubmit, warning that the prompt may still be queued, remote cleanup `NOT_RUN` |
| Duplicate output filenames (harness) | exit `1`, download `FAIL` before any `/view`, no asset retained, workspace removed |
| Remote cleanup queue read fails | remote cleanup `FAIL`, local workspace still removed |
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
  `6ca0c68` (which includes merged #752, #754, #756, #741) with no
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

Verification on this head (base `6ca0c68`, Go 1.27.0, loopback
fixtures only — none of this is real inference):

- Focused plain on the final head: providers `TestComfyUI*` ok
  2.791 s, storage ok 1.139 s, API ok 0.607 s, gateway
  `TestComfyUIAccept*` ok 2.311 s. (Earlier, on the 02a43ac-based
  head: providers 3.917 s, storage 0.996 s, API retrieval 0.127 s,
  gateway accept 1.989 s; full `./internal/video/...` 13 packages
  ok on that head.)
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

## 2026-10-30 ambiguity/collision hardening + live re-check (NOT a live run)

This cycle re-checked the live prerequisites and closed the remaining
deterministic gaps named by the reopened issue. **Actual model inference
remains NOT_RUN/BLOCKED, and #651 must stay open.** No model was
downloaded, no paid compute was used, and no external or production
ComfyUI service was contacted.

Environment re-check (first-hand, 2026-10-30 ~10:19 UTC, names only —
no secret values were read or printed):

- `COMFYUI_URL`, `COMFYUI_BASE_URL`, `COMFYUI_API_URL`, `COMFYUI_HOST`,
  `COMFYUI_MODEL`, `COMFYUI_WORKFLOW`, and `NEXAROUTE_COMFYUI_URL`: all
  `UNSET`.
- `/etc/nexaroute`: does not exist.
- Nothing answered on `127.0.0.1:8188`; no ComfyUI listener was found.
- `ffprobe`: absent in this environment, so playability could not have
  been verified even if a server had existed.
- No owner authorization naming an instance, model, workflow,
  compute/budget limit, or secret-injection method exists on record.
- No ComfyUI/model/workflow versions can be recorded because no server
  or workflow was supplied.

Committed sanitized harness report:
`docs/reports/verification-slices/comfyui-acceptance-2026-10-30.json`.
It records `outcome: BLOCKED`, `inference_executed: false`, preflight
`BLOCKED`, and model inference `NOT_RUN`, with the fixture/dry-run
limitations attached. It is **not** acceptance evidence for real video
inference.

Code and test changes in this cycle (loopback fixtures only):

- **Ambiguous submission disclosed:** a submission that fails without
  a prompt ID may still have been queued server-side (transport drop,
  deadline, or 5xx). The harness now adds a warning that ComfyUI has no
  idempotency key and the queue must be checked before retrying, and
  the remote-cleanup reason says the same. A definite 4xx or
  `node_errors` rejection is not labelled ambiguous.
  `TestComfyUIAcceptAmbiguousSubmissionFailsClosedWithoutResubmit`
  proves exactly one POST, no resubmit, `FAIL`, no job/asset recorded,
  local cleanup `PASS`, remote cleanup `NOT_RUN`, and the warning.
- **Harness-level output collision:**
  `TestComfyUIAcceptDuplicateOutputFilenamesFailsClosed` proves a
  completion listing the same stored filename twice fails closed
  before any `/view` download, retains no asset, and removes the
  per-run workspace.
- **Cross-subfolder collision:** the adapter already keyed collisions
  by stored basename; `TestComfyUIDownloadRejectsSameFilenameAcrossSubfolders`
  now pins that two outputs named `same.mp4` under different
  subfolders are refused before any download.
- **Cleanup when the remote cancel itself fails:**
  `TestComfyUIAcceptCleanupRemoteErrorStillRemovesLocal` proves a
  failed queue read records remote cleanup `FAIL` but still removes
  the local partial workspace.
- **Defect fixed — failed `Put` could orphan video bytes:** the local
  store renamed the video file into place and then wrote its metadata
  sidecar. If that metadata write failed, `Put` returned an error
  without an asset, so `DownloadOutputs` rollback could not identify
  the file and left it orphaned under the failed job. `Local.Put` now
  removes the renamed video file when metadata serialization or
  writing fails, keeping `Put` atomic from the caller's perspective.
  New tests: `TestLocalPutRollsBackVideoWhenMetadataWriteFails` and
  `TestComfyUIDownloadRollsBackWhenLaterPersistenceFails`.
  Red proof on the pre-fix head (`7daa36d`, run in an in-repo
  worktree): the storage test failed with "failed Put left orphaned
  video bytes behind", and the provider test failed with
  `second.mp4` left behind; both PASS on the fixed head.

Verification on the fixed head (Go 1.25.0, loopback fixtures only —
none of this is real inference):

- `go test ./cmd/gateway/ -count=1 -timeout=600s` — PASS (60.416 s).
- `go test ./internal/video/providers/ ./internal/video/storage/ ./internal/video/api/ -count=1 -timeout=300s` — PASS.
- `go test -race ./internal/video/providers/ ./internal/video/storage/ ./internal/video/api/ -count=1 -timeout=360s` — PASS.
- `go test -race ./cmd/gateway/ -run 'TestComfyUIAccept' -count=1 -timeout=360s` — PASS.
- `go vet ./cmd/gateway ./internal/video/providers ./internal/video/storage ./internal/video/api` — PASS.
- `gofmt` clean on all touched files.
- `./scripts/verify.sh` — `VERIFY PASS` on this head. It ran the
  repository-wide unit/integration and race suites, vet, fuzz, release
  artifact/SBOM/coverage-gate checks, JS syntax checks, and both
  builds. The browser acceptance step was `SKIPPED` because Python
  `playwright` is not installed in this sandbox; the script reports
  that as a local skip, and CI's mandatory browser gate is separate.

Still missing for #651 (owner inputs required before any live run):

- A provisioned ComfyUI instance URL (HTTPS for non-loopback) with its
  exact ComfyUI version.
- A real compatible text-to-video model name/version and license, plus
  an API-format workflow whose prompt/width/height/frames node mapping
  is named explicitly.
- Written owner authorization naming the instance, model, workflow,
  compute/budget limit, and secret-injection method.
- A working `ffprobe`, and a decision on where the sanitized evidence
  report and any retained playable asset should be stored.

Until those exist, the live-inference criterion is `BLOCKED/NOT_RUN`;
fixtures, loopback tests, and harness code do not satisfy it and must
not be reported as real video inference.

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

Rebase note (2026-10-30): branch rebased onto main `28aed30` (merge of
#759); prior verification was performed on head `97e3872` (base
`6ca0c68`) and the `197c57b`/`410ce2a` rebases. Fresh CI on the new
head is authoritative for the rebased branch.

## 2026-10-30 follow-up: state reconciliation + residual negative gaps (local only, NOT a live run)

- **State reconciliation (no action taken, already correct):** #651 was
  closed 2026-10-30T05:10:17Z, one second after PR #748 merged, as a
  manual close (no commit id; #748's body said "do NOT close #651"
  and used "Related to", not a close keyword). It was reopened
  2026-10-30T10:11:49Z with an owner comment stating the closure was
  premature and that fixtures, loopback tests, and harness code do
  not close the issue. No owner-approved exclusion exists on the
  record, so no reopen action was needed and none was taken.
- **Gap audit against the nine required negative categories:** all
  nine are already covered by merged work and were NOT duplicated
  here — unavailable server, invalid workflow/node mapping, submit
  timeout with exactly-once submission, duplicate output filenames
  (exact and sanitized), oversized output (including the exact-limit
  boundary), partial-download rollback, cleanup (workspace +
  pending queue entry), cancellation (pending-only delete, running
  refusal, context cancellation), and fail-closed terminal job
  states (rejected, error, completed-without-MP4, stalled).
- **Defect fixed — case-insensitive duplicate output filenames:**
  the duplicate check compared stored names exactly, so two outputs
  differing only by case (`Clip.mp4` vs `clip.mp4`) passed the
  check. On Linux they are distinct files, but on
  case-insensitive filesystems (default macOS APFS, Windows) the
  sink's rename would silently overwrite the first asset while its
  record still claimed its own SHA-256 — the same
  self-inconsistency the exact-match check was built to prevent.
  **Fix:** `DownloadOutputs` now folds case for the duplicate
  comparison only; stored names themselves are unchanged, and the
  error still reports the supplied stored name. Red proof:
  `TestComfyUIDownloadRejectsCaseInsensitiveDuplicateOutputFilenames`
  FAILs on the pre-fix code (`DownloadOutputs error=<nil>, want
  duplicate rejection`) and PASSes with the fix, with zero `/view`
  requests and an empty store root. The exact-match and
  sanitized-collision tests still pass unchanged.
- **Pinning tests (pass on current code, no behavior change):**
  `TestComfyUICancelAbsentPromptRefusesWithoutDelete` (a prompt in
  neither queue is refused with no delete call — the harness
  cleanup path records this as BLOCKED, never a global interrupt);
  `TestComfyUIAcceptDownloadDeadlineFailsClosedWithCleanup` (a
  `/view` that never answers fails the harness run at its deadline:
  exit 1, download FAIL, exactly one submission, no retained asset,
  local cleanup PASS, remote cleanup BLOCKED because the prompt
  already completed and has no pending entry); and
  `TestHandlerComfyUIAssetMissingFileReturnsNotFound` (a stored
  record whose file is gone answers 404 `asset_not_found`, not a
  retryable 503).
- **Deliberately unchanged boundary:** the adapter trusts the
  provider's `video/mp4` content type and does not sniff media
  magic bytes. A server labeling non-media as `video/mp4` would be
  stored; playability is proven by the harness `ffprobe` check on
  the acceptance path, and the gateway serves SHA-256/size-verified
  bytes. This trust is the same for every provider, not a
  ComfyUI-specific defect, and sniffing would risk rejecting valid
  outputs — so no sniffing was added and no claim about it is made.
- **Live verdict unchanged (re-checked 2026-10-30, names only):**
  nothing on `127.0.0.1:8188`/`8189`; `COMFYUI_URL`,
  `COMFYUI_BASE_URL`, `COMFYUI_API_URL`, `COMFYUI_HOST`, and
  `COMFYUI_MODEL` all absent-or-empty; `/etc/nexaroute` does not
  exist; no operator authorization naming an instance, model, or
  workflow exists on record. Present, so not blockers: `ffprobe`,
  the deterministic fixture suites, and the `comfyui-accept`
  harness itself. Actual model inference remains NOT_RUN/BLOCKED,
  zero live calls, zero downloads, and #651 stays OPEN.
- **Verification on this head (base main `7daa36d`, Go 1.27.0,
  loopback fixtures only):** full packages — providers ok 107.6s,
  api ok 0.7s, storage ok 1.3s, gateway ok 289.7s; new tests
  `-race` — providers ok 1.5s, gateway ok 4.1s, api ok 1.3s;
  `go vet` clean on all four touched packages; `gofmt` clean on
  all five touched files. Full `./scripts/verify.sh` NOT run
  locally (whole-repo + browser step; CI is the gate).

## 2026-10-30 evening re-verification on merged main (local only, NOT a live run)

- **State reconciliation (no action taken, already correct):** #651 is
  OPEN (closedAt null, updated 2026-10-30T13:49:00Z). It was closed
  2026-10-30T05:10:17Z, one second after PR #748 merged, and reopened
  2026-10-30T10:11:49Z with an owner comment calling the closure
  premature, confirmed by the owner-order reopen comment
  2026-10-30T13:49:00Z. No owner-approved exclusion exists on the
  record, so no reopen comment was posted here and none was needed.
  PR #748 is MERGED (2026-10-30T05:10:16Z); PR #778 (case-insensitive
  duplicate filenames) is MERGED as main `7a2623a`.
- **Negative-suite re-verification on main `7a2623a` (Go 1.27.3,
  loopback fixtures only, first-hand test-name audit):** all nine
  required negative categories are present and passing — unavailable
  server (`TestComfyUIAcceptUnavailableServerFailsClosed`); invalid
  workflow/node mapping (`InvalidNodeMappingNeverContactsServer`,
  `MalformedWorkflowFilesRejected`, `NodeErrorsWithPromptIDRejected`);
  submit timeout with exactly-once submission (`SubmitTimeoutSubmitsExactlyOnce`
  at adapter and harness level, `AmbiguousSubmissionFailsClosedWithoutResubmit`,
  `StalledJobTimesOutWithoutResubmit`); duplicate output filenames
  (exact, sanitized, case-insensitive, same-name-across-subfolders);
  oversized output (`OversizedOutputLeavesNoPartialAsset`,
  `DownloadAtExactLimitPasses`); partial-download rollback
  (`DownloadPartialFailureCleansUpAndLeavesOtherJobIntact`,
  `DownloadRollsBackWhenLaterPersistenceFails`); cleanup
  (`CleanupRemoteErrorStillRemovesLocal`,
  `TimeoutCleansUpPendingPrompt`); cancellation
  (`PollInterruptedByCancellation`, `GetJobRespectsCancellation`,
  cancel-absent/cancel-running refusals); fail-closed terminal states
  (`HistoryFailureStatesAreFailClosed`, `RejectedJobFailsClosed`,
  `FailedInferenceFailsClosedWithoutResubmit`,
  `CompletedWithoutMP4FailsClosed`); plus gateway retrieval
  (`TestHandlerServesComfyUIAssetThroughGateway`, missing-file 404).
  No new negative test was added here because no genuine gap was
  found; duplicating the merged suite would be churn, not evidence.
- **Local results on `7a2623a`:** providers ComfyUI plain 1.884s
  exit 0; api handler 0.099s exit 0; gateway accept 2.938s exit 0;
  storage 1.071s exit 0; race subset (download / submit-timeout /
  ambiguous) providers 1.981s + gateway 2.004s exit 0. Full
  `./scripts/verify.sh` NOT run locally (whole-repo + browser step;
  CI is the gate).
- **Live verdict unchanged (re-checked 2026-10-30 evening, names
  only):** nothing listening on `127.0.0.1:8188`/`8189`;
  `COMFYUI_URL`, `COMFYUI_BASE_URL`, `COMFYUI_API_URL`,
  `COMFYUI_HOST`, `COMFYUI_MODEL` all absent-or-empty;
  `/etc/nexaroute` does not exist; no operator authorization naming
  an instance, model, workflow, or compute/budget limit exists on
  record. Zero live calls, zero model downloads, zero paid compute.
  Actual model inference remains NOT_RUN/BLOCKED and #651 stays
  OPEN. What would unblock it, exactly as before: written owner
  authorization naming a provisioned ComfyUI instance (URL), a
  compatible text-to-video model, an API-format workflow, and a
  compute/budget limit — then the smallest real inference via
  `nexaroute comfyui-accept --confirm-live` with the sanitized PASS
  report attached to #651.
