# Independent Technical Audit — NexaRoute Universal AI-to-Video

**Date:** 2026-10-16
**Repository:** `ali-shortcuts/nexaroute`
**Implementation audited:** PR #463, merged into `main` as `dfdbe2359ab6fb3cde623fedd022ccb62fe2efc0`
**Repair branch:** `audit/fix-universal-video-merge-2026-10-16`
**Related issue:** #461, still open
**Superseded audit PR:** #464, based on the pre-merge implementation history

## Executive conclusion

The Tier-A implementation present in `main` is a real, working **programmatic video pipeline**, not native AI video generation. With an explicit or model-authored `video-plan.v1`, NexaRoute can render multi-scene H.264 MP4 output through FFmpeg, perform ffprobe and sampled-frame QC, optionally add local Piper narration/SRT subtitles, persist validated output through the authenticated asset path, and reject unsupported native/reference/dialogue capabilities honestly.

PR #463 is confirmed present in `main`; the original audit PR #464 had an unnecessarily broad comparison because it was based on the pre-merge history. This repair branch is based directly on current `origin/main` and contains only the two audit fixes, their regression tests, the report, and reproducible representative evidence.

The implementation is **not ready to claim universal live-model compatibility or production-grade unattended operation** without further work. No live external planning-model benchmark was performed: no authorized NexaRoute provider configuration was available for cost-controlled calls, and no paid calls were made.

Two defects were confirmed and fixed:

1. `agent.ParsePlan` accepted valid JSON followed by arbitrary trailing text or another JSON value.
2. `render.RenderPlan` could fail for a relative job directory because composition resolved paths after changing the command working directory.

One important resource-management finding remains open: `CodeRender.jobs` retains completed and failed jobs, and per-job work directories are not removed. Retention must be designed consistently with polling, recovery, and asset-download semantics before cleanup is added.

## A. Main-tree and history verification

The following facts were checked against the live repository before editing:

- PR #463 is **MERGED** into `main` at `dfdbe2359ab6fb3cde623fedd022ccb62fe2efc0`.
- `origin/main` contains the Universal Video implementation and tests, including:
  - `internal/video/agent/agent.go`
  - `internal/video/agent/agent_test.go`
  - `internal/video/render/render.go`
  - `internal/video/render/render_test.go`
  - `internal/video/providers/coderender.go`
  - `internal/video/plan.go`
  - `internal/video/runtime/coderender_runtime_test.go`
  - `docs/VIDEO_GATEWAY.md`
  - `docs/KNOWN_GAPS.md`
- PR #464 remains open at its old pre-merge-based head `1126b24dba2f229371f10766bbd94a86dfde5012`.
- The clean repair branch was created from current `origin/main`; the original implementation was not reintroduced or duplicated.

## B. Current implementation status

### Verified in the source tree and evidence

- Versioned declarative `video-plan.v1` schema with bounded scenes, elements, durations, dimensions, FPS, colors, cameras, transitions, and animation primitives.
- Strict unknown-field rejection during plan decoding.
- Bounded planning repair loop: three attempts by default, maximum five.
- Deterministic FFmpeg rendering using colors/gradients, `drawtext`, `drawbox`, chart bars, `zoompan`, fade, and composition.
- Multi-scene rendering and concatenation.
- H.264/yuv420p MP4 output, dimensions and duration checked with ffprobe.
- Sampled-frame variance check to reject blank scenes.
- Authenticated video handler with bearer-token comparison, job polling, cancellation, and allow-listed asset downloads.
- Optional local Piper narration and heuristic burned-in SRT composition when configured.
- Explicit rejection of native mode, reference assets, first/last frames, dialogue, callbacks, and unsupported image/multi-shot inputs for code-render.
- Honest metadata: `native_video=false`, `programmatic=true`, planning model, renderer, ffprobe QC, and optional narration tools.

### Known boundaries and open issues

- The planner adapter is OpenAI Chat Completions-shaped. A configured model is not proof that its provider supports the same request, JSON-mode, response, or context-length semantics.
- A syntactically valid plan may still fail to satisfy the original brief semantically; deterministic validation does not establish on-brief quality.
- Persian/Arabic shaping and right-to-left rendering need dedicated visual regression coverage. No Persian MP4 or Persian Piper voice was tested here.
- Cost reporting for code-render is a truthful zero video-ledger provider estimate, but local CPU, disk, and planning-token accounting require separate interpretation.
- Native photorealistic text-to-video, image/reference-conditioned code-render video, provider-generated dialogue/music/SFX, vision-model QC, and durable distributed provider state are outside this Tier-A slice.
- No arbitrary model-generated code execution was found in the Tier-A path; the renderer consumes declarative plan fields and constructs its own FFmpeg arguments.

## C. Defect audit

| ID | Severity | Status | Finding and evidence |
|---|---|---|---|
| F-01 | Medium | **Fixed** | `internal/video/agent/agent.go`: `ParsePlan` now performs a second decode and requires `io.EOF`, rejecting valid JSON followed by text or a second JSON value. Regression coverage is in `internal/video/agent/agent_test.go` (`TestParsePlanRejectsTrailingContent`). |
| F-02 | Medium | **Fixed** | `internal/video/render/render.go`: `RenderPlan` converts the job directory to an absolute path before filesystem and FFmpeg composition work. `internal/video/render/render_test.go` uses a relative directory with a real multi-scene render and probes the resulting MP4 (`TestRenderPlanResolvesRelativeJobDirectory`). |
| F-03 | Medium/High | **Open — design required** | Completed, failed, and cancelled jobs remain in the in-memory `jobs` map and work directories remain on disk. Define retention, polling, recovery, and download semantics before implementing cleanup. |
| F-04 | Medium | **Open — compatibility boundary** | Model selection does not prove Chat Completions, JSON mode, response shape, or context-limit compatibility. Add explicit probing/capability metadata before advertising a deployment as eligible. |
| F-05 | Medium | **Open — quality boundary** | Syntax and bounds validation do not establish semantic compliance with the user's brief. Add a bounded, evidence-producing brief-compliance check/repair path. |
| F-06 | Low/Medium | **Open — typography** | FFmpeg `drawtext` host-font behavior does not constitute a Persian/Arabic shaping guarantee. Add deployment-specific RTL visual fixtures. |

## D. Model benchmark results

**No live external models tested; no model ranking established.**

No live planning model was ranked. DeepSeek, Claude, Gemini, and OpenAI-compatible deployments had zero live attempts because no authorized NexaRoute provider configuration was available. No credentials were obtained from unrelated files, logs, or shell history, and no paid provider calls were made.

| Model family | Live attempts | Result |
|---|---:|---|
| DeepSeek / configured aliases | 0 | Not tested — provider configuration unavailable |
| Claude | 0 | Not tested — provider configuration unavailable |
| Gemini | 0 | Not tested — provider configuration unavailable |
| OpenAI-compatible deployments | 0 | Not tested — authorized live configuration unavailable |

Scripted planners and mock endpoints in existing tests are fixture evidence, not live-model measurements.

## E. Representative media evidence

The committed evidence is deterministic renderer evidence, not a live-model result:

- MP4: `docs/audit-evidence/representative-audit-video.mp4`
- Plan: `docs/audit-evidence/representative-plan.json`
- Contact sheet: `docs/audit-evidence/representative-contact-sheet.png`
- Probe record: `docs/audit-evidence/ffprobe.json`

Freshly verified in this execution with `ffprobe`:

- Codec: `h264`
- Dimensions: `640x360`
- Frame rate: `12/1`
- Duration: `4.000000` seconds
- Size: `17,764` bytes

The contact sheet contains both the title scene and chart scene; no blank scene was observed. Disposable per-scene intermediate renders are intentionally not retained in this focused repair branch.

## F. Verification classification

### Freshly verified in this execution

- `git fetch origin --prune` completed successfully.
- PR #463 merge state, merge commit, PR #464 state, branch head, and relevant source-file presence were checked through Git and GitHub.
- A clean worktree was created from `origin/main`; the focused audit commit applied without conflicts.
- The final committed MP4 was inspected with:

  ```bash
  ffprobe -v error \
    -show_entries stream=codec_name,width,height,r_frame_rate \
    -show_entries format=duration,size \
    -of json docs/audit-evidence/representative-audit-video.mp4
  ```

  It reported H.264, 640x360, 12/1, 4.000000 seconds, and 17,764 bytes.

- The final branch contains the parser and relative-directory fixes plus their regression tests, without duplicating the merged Universal Video implementation.

### Previously recorded, not re-run in this execution

The earlier audit branch recorded passing results for the focused Go tests, the broader video/runtime tests, the independent renderer harness, and `scripts/verify.sh`. Those results remain historical evidence and are not represented as fresh runs here.

### Not run or blocked in this execution

- Go tests and `scripts/verify.sh`: blocked because the sandbox does not have the Go toolchain (`go: command not found`).
- Live external model calls: not run; no authorized provider configuration and no paid calls.
- Piper narration/Persian TTS, Persian/RTL visual rendering, production load, and long-running retention behavior: not tested.
- GitHub checks for the eventual repair commit: must be inspected after the repair branch is pushed and the PR is opened.

## G. Recommendation

Treat PR #463 as a **Tier-A programmatic renderer** suitable for controlled experiments, explicit reviewed plans, explainers, charts, kinetic typography, and local rendering. Do not market it as universal native AI-to-video and do not choose a default external planning model based only on its brand or selector presence.

Before production use:

1. Define job/work-directory retention with polling, download, recovery, and bounded disk semantics.
2. Add provider/model capability probing for protocol, JSON mode, context limits, and response shape.
3. Run a live benchmark with at least three attempts per actually configured model, recording plan validity and end-to-end MP4 success separately.
4. Add Persian/RTL visual fixtures and verify shaping in the deployment image.
5. Add semantic brief-compliance QC and bounded repair evidence.
6. Keep the default path conservative: reviewed `video_plan` or a deployment with explicit measured compatibility.
