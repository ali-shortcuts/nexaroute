# Universal AI-to-Video — code-render work-directory retention (#461)

**Date:** 2026-10-19
**Issue:** #461 (Phase 22 — Any-model video: code-render provider)
**Related:** #463 / #465 (Tier-A implementation and audit fixes, already on `main`)
**Scope:** close the independent audit's remaining open finding — finished
code-render jobs and their per-job work directories were never reclaimed.

## Context

The Tier-A `code-render` provider is present on `main`: a configured
text/coding model authors a validated `video-plan.v1`, an FFmpeg renderer
executes it, deterministic QC gates the MP4, and the job runs on the existing
video lifecycle. The independent audit
(`docs/reports/UNIVERSAL_VIDEO_INDEPENDENT_AUDIT_2026-10-16.md`) confirmed the
implementation and named one open resource-management finding:

> `CodeRender.jobs` retains completed and failed jobs, and per-job work
> directories are not removed. Retention must be designed consistently with
> polling, recovery, and asset-download semantics before cleanup is added.

This change implements that contract; it does not re-implement or alter the
Tier-A protocol/renderer surface.

## What changed

- `internal/video/providers/coderender.go`
  - Every job now records its per-job scratch directory and the time it
    reached a terminal state (completed, failed, cancelled).
  - New `PruneTerminal(now)` removes terminal jobs older than `Retention` and
    deletes their work directories. Non-terminal jobs are never pruned,
    however old, so an in-flight render cannot lose its directory.
  - `Retention` defaults to `DefaultCodeRenderRetention` (24h); a
    non-positive value keeps everything, matching the job-store retention
    contract.
  - A best-effort sweep runs opportunistically on each `CreateJob`.
- `internal/video/runtime/runtime.go`
  - The existing hourly terminal-job sweeper also prunes code-render terminal
    jobs and work directories, and runs at startup.
- `internal/video/config.go`, `internal/config/config.go`, `cmd/gateway/main.go`
  - New `code_render_retention_hours` setting (default 24, max 8760, negative
    keeps forever), validated and mapped through to the provider.
- Docs: `docs/VIDEO_GATEWAY.md`, `docs/CONFIGURATION.md`, `docs/KNOWN_GAPS.md`,
  `configs/video.example.json`.

Why the window is long: the output MP4 lives in the work directory until the
orchestrator calls `DownloadOutputs`, which happens within seconds of a job
finishing. The retention window is far larger than any poll/download cycle, so
the persisted asset is always written through the asset sink before scratch
space is reclaimed.

## Deterministic regression tests

`internal/video/providers/coderender_retention_test.go` (no FFmpeg, no model,
no network):

- expired completed/failed/cancelled jobs and their directories are removed;
  a fresh terminal job and an old non-terminal job are retained;
- pruning is idempotent;
- a non-positive retention keeps everything;
- `NewCodeRender` installs a bounded default;
- a failed QC job publishes no asset through the sink (failure-path proof).

## Verification (this environment)

Commands and real outcomes:

- `go build ./...` — exit 0.
- `go vet ./internal/video/... ./internal/config/... ./cmd/gateway/...` — exit 0.
- `go test -race -count=1 ./internal/video/...` — exit 0 (all packages ok).
- `./scripts/verify.sh` — exit 0, `VERIFY PASS`; full log in
  `docs/reports/issue461-retention-verify-2026-10-19.raw.txt`.

## Unverified / blocked

- This environment has **no FFmpeg, ffprobe, or Piper** installed, so the
  render/narration end-to-end tests skip here. The `code-render` acceptance
  proof (real MP4 + ffprobe) is unchanged by this slice and was not re-run
  live here.
- The browser acceptance stage was skipped by `verify.sh` (no Chromium +
  Playwright in this runner).
- Live planning through a real external model remains **UNVERIFIED**; no paid
  calls were made, and no provider credentials were used.

No merge, release, tag, or configuration change was performed.
