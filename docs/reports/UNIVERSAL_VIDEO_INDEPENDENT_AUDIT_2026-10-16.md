# Independent Technical Audit — NexaRoute Universal AI-to-Video

**Date:** 2026-10-16  
**Repository:** `ali-shortcuts/nexaroute`  
**Audited implementation:** PR #463, exact head `9c4fa40fef4b6f2b76c2dc0fbb7cf1a967951f1b`  
**Audit branch:** `audit/universal-video-2026-10-16`  
**Related issue:** #461, still open

## Executive conclusion

The Tier-A implementation is a real, working **programmatic video pipeline**, not native AI video generation. With an explicit or model-authored `video-plan.v1`, NexaRoute can render multi-scene H.264 MP4 output through FFmpeg, perform ffprobe and sampled-frame QC, optionally add local Piper narration/SRT subtitles, persist the validated output through the authenticated asset path, and reject unsupported native/reference/dialogue capabilities honestly.

It is **not ready to claim universal live-model compatibility or production-grade unattended operation** without further work. Live external planning-model benchmarking was not performed because this audit environment did not expose a configured NexaRoute provider set suitable for authorized, cost-controlled live calls. The PR's own issue evidence says live planning remained unverified.

Two defects were confirmed and fixed on this audit branch:

1. `agent.ParsePlan` accepted valid JSON followed by arbitrary trailing text or another JSON value, despite its documented strict behavior.
2. `render.RenderPlan` failed when given a relative job directory because the composer changed working directory while receiving relative concat-manifest/output paths.

One important resource-management finding remains documented rather than arbitrarily changed: `CodeRender.jobs` retains completed and failed jobs, and per-job work directories are not removed. This can cause unbounded memory and disk growth in a long-running process; retention must be designed consistently with polling, recovery, and asset-download semantics.

## A. Current implementation status

### Works with direct evidence

- Versioned declarative `video-plan.v1` schema with bounded scenes, elements, durations, dimensions, FPS, colors, cameras, transitions, and animation primitives.
- Strict unknown-field rejection during plan decoding.
- Bounded planning repair loop: three attempts by default, maximum five.
- Deterministic FFmpeg rendering using `color`/`gradients`, `drawtext`, `drawbox`, chart bars, `zoompan`, fade, and composition.
- Multi-scene rendering and concatenation.
- H.264/yuv420p MP4 output, dimensions and duration checked with ffprobe.
- Sampled-frame variance check to reject blank scenes.
- Authenticated video handler with bearer-token comparison, job polling, cancellation, and allow-listed asset downloads.
- Asset download path checks stored job asset IDs, regular files, size metadata, content type, path-safe filename handling, `nosniff`, and `no-store`.
- Optional local Piper narration and heuristic burned-in SRT composition when configured.
- Explicit rejection of native mode, reference assets, first/last frames, dialogue, callbacks, and unsupported image/multi-shot inputs for code-render.
- Honest metadata: `native_video=false`, `programmatic=true`, planning model, renderer, ffprobe QC, and optional narration tools.

### Partially implemented / environment-dependent

- The planner adapter is OpenAI Chat Completions-shaped. It can reuse the gateway data plane when configured that way, but it does not prove that every configured provider/model implements the same request, JSON-mode, response, or context-length semantics.
- The UI can show configured planning model names, but live model quality and compatibility are not measured automatically.
- Persian text is accepted as Unicode text, but the renderer's evidence is based on host fonts and FFmpeg drawtext. Correct Persian shaping, bidirectional layout, and visual typography were not proven by a live Persian MP4 in this audit. Narration depends on a configured Persian Piper voice.
- Cost reporting for code-render is a truthful zero video-ledger provider estimate, but it does not include local CPU, disk, electricity, or planning-token charges recorded by the normal gateway accounting.

### Does not implement

- Native photorealistic text-to-video generation.
- Image/reference-conditioned video for code-render.
- Provider-generated dialogue, music, sound effects, translation, or word-aligned subtitles.
- Vision-model quality judgment; QC is deterministic and intentionally limited.
- Durable/distributed code-render provider state across gateway processes.
- Live, fair comparative ranking of external planning models in CI or this audit run.

## B. Defect audit

| ID | Severity | Status | Finding and evidence |
|---|---|---|---|
| F-01 | Medium | **Fixed** | `internal/video/agent/agent.go:146-177`: `ParsePlan` decoded one JSON object but did not perform a second decode to require `io.EOF`. A response such as `<valid-plan> trailing` or `<valid-plan> {"another":true}` was accepted. Added end-of-input enforcement and `TestParsePlanRejectsTrailingContent`. |
| F-02 | Medium | **Fixed** | `internal/video/render/render.go:108-120`: relative `dir` values were passed to `composer.New(..., dir)`. `composer.Runner` executes with `cmd.Dir=dir`, so the relative concat manifest path was resolved relative to the working directory a second time and FFmpeg reported a missing input file. Reproduced independently with a two-scene render; fixed by converting the job directory to an absolute path before all render work. Added `TestRenderPlanResolvesRelativeJobDirectory`. |
| F-03 | Medium/High | **Open — document/design required** | `internal/video/providers/coderender.go:41-44,175-199,260-386`: completed, failed, and cancelled jobs remain in the in-memory `jobs` map; each job's work directory also remains on disk. Repeated jobs therefore grow memory and disk without a retention or cleanup mechanism. Do not apply arbitrary deletion: polling, restart semantics, recovery, and asset download need an explicit retention contract. |
| F-04 | Medium | **Open — compatibility boundary** | `internal/video/agent/openai_planner.go:64-151`: the adapter always posts `/v1/chat/completions`, optionally sends `response_format=json_object`, and expects `choices[0].message.content`. A model appearing in the planning selector is not proof that its provider supports this exact protocol or JSON mode. Direct non-gateway endpoints may accept an arbitrary model string. Compatibility probing/capability metadata should be explicit before advertising a model as eligible. |
| F-05 | Medium | **Open — capability quality** | Plan validation protects syntax and bounds but does not semantically verify that the plan preserves all facts or constraints in the original brief. A model can produce a valid but irrelevant plan; repair only receives validator defects, not a semantic brief-compliance verdict. |
| F-06 | Low/Medium | **Open — typography** | The renderer uses FFmpeg `drawtext` with host fonts. No shaping/bidi engine is present in the code-render path. Persian/Arabic captions may be displayed without correct joining or right-to-left ordering depending on FFmpeg/libfribidi/font behavior. This needs a dedicated visual regression test with a known Persian sample and a supported shaping strategy. |

No arbitrary model-generated code execution was found in the Tier-A path. The renderer consumes declarative plan fields and constructs its own FFmpeg arguments.

## C. Model benchmark results

### Live model coverage

No live planning model was ranked. The repository's CI and PR evidence explicitly uses a scripted planner for end-to-end tests, and live external planning remained unverified. This audit did not obtain credentials from unrelated secret files, logs, shell history, or arbitrary environment data, and did not make paid provider calls.

| Model | Provider/protocol | Live attempts | Request success | Valid-plan rate | Render success | Visual score | Latency/cost | Status |
|---|---|---:|---:|---:|---:|---:|---|---|
| DeepSeek / configured aliases | Not established from an authorized NexaRoute runtime config | 0 | — | — | — | — | — | **NOT TESTED — credentials/provider unavailable** |
| Claude | Not established from an authorized NexaRoute runtime config | 0 | — | — | — | — | — | **NOT TESTED — credentials/provider unavailable** |
| Gemini | Not established from an authorized NexaRoute runtime config | 0 | — | — | — | — | — | **NOT TESTED — credentials/provider unavailable** |
| OpenAI-compatible models | Adapter shape exists; no authorized NexaRoute live provider configuration established | 0 | — | — | — | — | — | **NOT TESTED — credentials/provider unavailable** |

The scripted planner tests are mock/provider-fixture evidence, not live-model measurements and are intentionally not presented as model benchmark results.

### Deterministic renderer evidence

A separate two-scene plan containing animated text, a shape, a chart, camera/transition effects, and a final composition was rendered independently after fixing F-02.

- MP4: `docs/audit-evidence/representative-audit-video.mp4`
- Plan: `docs/audit-evidence/representative-plan.json`
- Contact sheet: `docs/audit-evidence/representative-contact-sheet.png`
- Probe record: `docs/audit-evidence/ffprobe.json`

Observed media facts:

- Codec: `h264`
- Dimensions: `640x360`
- Frame rate: `12/1`
- Duration: `4.000000` seconds
- Size: `17,764` bytes
- Contact sheet visibly contains both the title scene and chart scene; no blank scene was observed.

## D. Rankings

Because no live model had an authorized, measured sample, no external model is ranked as a winner.

1. **Best overall end-to-end model:** not determined; live model sample size is zero.
2. **Best structured planner:** not determined; live model sample size is zero.
3. **Best Persian video model:** not determined; Persian live render was not run.
4. **Best consistent renderer:** the deterministic local renderer is the measured execution component, but this is not a model ranking.
5. **Best value:** not determined; no live provider cost/latency observations.
6. **Best incompletely evaluated model:** none; untested models are not ranked.

The practical default should therefore be the operator's most reliable **configured OpenAI-compatible planning deployment**, selected only after the benchmark suite in the audit brief is run through the actual gateway. Until then, the safest default is an explicit reviewed `video_plan`, not an unmeasured model brand.

## E. Media evidence

All evidence is local to the audit branch and contains no provider credentials or sensitive user data:

- `docs/audit-evidence/representative-audit-video.mp4` — deterministic renderer evidence, not a live-model result.
- `docs/audit-evidence/representative-contact-sheet.png` — frame sample from that MP4.
- `docs/audit-evidence/representative-plan.json` — exact plan used.
- `docs/audit-evidence/ffprobe.json` — observed codec, dimensions, frame rate, duration, and size.

## F. Changes and verification

### Changed source files

- `internal/video/agent/agent.go` — reject trailing content after the plan JSON.
- `internal/video/agent/agent_test.go` — regression test for trailing text and trailing JSON.
- `internal/video/render/render.go` — normalize the render job directory to an absolute path.
- `internal/video/render/render_test.go` — regression test for relative job directories.

### Exact verification performed

- `go test -count=1 ./internal/video/... ./cmd/gateway/... ./cmd/videogen/...` — **PASS** after the fixes.
- `go test -count=1 ./internal/video/agent` — **PASS** after parser fix.
- `go test -count=1 -v ./internal/video/render -run 'TestRenderPlanProducesPlayableMP4|TestValidateOutput'` — **PASS** on the original renderer acceptance tests.
- Independent renderer harness — **PASS** after F-02 fix; generated and validated the representative MP4.
- `ffprobe` on representative MP4 — **PASS**, observed H.264, 640x360, 12 fps, 4.000 seconds.
- `./scripts/verify.sh` on the final audit tree, including both fixes — **PASS** with exit code 0.
- GitHub CI on exact PR head — PR metadata showed successful CI/security checks including verify, vulnerability scan, CodeQL, fuzz smoke, and integration checks. These are CI results for the PR head, not proof of live-model generation.

### Environment limitations

- No authorized NexaRoute provider configuration/model inventory suitable for live calls was available in this sandbox.
- No paid/live external model calls were made.
- No Piper voice model was configured, so narration and Persian TTS were not live-tested.
- No production load test was run.

## G. Practical recommendation

Treat PR #463 as a promising **Tier-A programmatic renderer**, suitable for controlled experiments, explicit reviewed plans, explainers, charts, kinetic typography, and local rendering. Do not market it as universal native AI-to-video and do not select a default external planning model based on brand reputation or the selector list.

Before practical production use:

1. Define and implement job/work-directory retention with polling/download/recovery semantics and bounded disk usage.
2. Add provider/model capability probing for Chat Completions, JSON mode, context limits, and response shape.
3. Run the five-model benchmark suite with at least three attempts per actually configured model, recording end-to-end MP4 success separately from plan quality.
4. Add Persian/RTL visual fixtures and verify font/shaping behavior in the deployment image.
5. Rerun `./scripts/verify.sh` after the final renderer fix, then obtain fresh PR checks on the fix commit.
6. Keep the default user path conservative: reviewed `video_plan` or a measured planning model with an explicit compatibility status.
