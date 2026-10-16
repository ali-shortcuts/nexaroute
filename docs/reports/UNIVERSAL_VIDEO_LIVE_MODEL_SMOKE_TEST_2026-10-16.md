# Universal AI-to-Video Live-Model Smoke Test

**Date:** 2026-10-16
**Repository:** `ali-shortcuts/nexaroute`
**Test branch:** `audit/live-model-smoke-2026-10-16`
**Source commit:** `d27669a953da318aa3a2de6b2d1531e1e16b42a4` (`origin/main`)
**Related issue:** #461
**Merged implementation:** PR #463
**Merged audit fixes:** PR #465

## Outcome

# LIVE TEST BLOCKED

A real end-to-end NexaRoute model test could not be started safely in this execution environment. The required NexaRoute runtime prerequisites were not available:

- The Go toolchain is not installed (`go` was not found), so the gateway and `videogen` binaries could not be built or started.
- No prebuilt `gateway` or `videogen` executable was present in the workspace.
- No non-example NexaRoute runtime configuration was present. The repository contains only example configurations; the video example has `code_render_enabled: false`, no planning model, and no runtime bearer token.
- A generic OpenAI-compatible environment is present in the sandbox, but it was not used directly. Calling it outside NexaRoute would bypass the gateway's provider routing, accounting, authentication, and video workflow, and would not satisfy this test.
- No authorized NexaRoute provider/model configuration and bounded spending allowance were established for this smoke test.

No paid model calls were made. No credentials, keys, bearer tokens, or sensitive provider responses were printed, stored, or published.

## Required path and result counts

The required path was:

`User brief → configured real model → valid video plan → NexaRoute renderer → playable MP4 → authenticated downloadable asset`

Because the gateway could not be started, the path was not entered.

| Measure | English attempts | Persian attempt |
|---|---:|---:|
| Real NexaRoute model requests | 0/3 | 0/1 |
| Valid model-generated plans | 0 | 0 |
| Successful NexaRoute renders | 0 | 0 |
| Authenticated downloadable live assets | 0 | 0 |
| Useful on-brief live MP4s | 0 | 0 |
| Paid calls | 0 | 0 |

No model identifier, latency, retry count, token usage, or cost can be reported as measured. No external model ranking was established.

## Requested test inputs

The prescribed English brief and Persian brief were **not submitted**, because doing so without an operational NexaRoute gateway and authorized provider configuration would not be a valid end-to-end test. No mock planner was substituted and no hand-written plan was counted as live-model evidence.

## Fresh environment inspection

The following checks were performed against the latest repository state:

```text
git fetch origin --prune                         PASS
git show --no-patch origin/main                  d27669a953da318aa3a2de6b2d1531e1e16b42a4
gh pr view 463                                    MERGED
gh pr view 465                                    MERGED
git cat-file checks for Universal Video files    PRESENT
command -v go                                     NOT FOUND
command -v ffmpeg                                 /usr/bin/ffmpeg
command -v ffprobe                                /usr/bin/ffprobe
find for gateway/videogen binaries               NONE FOUND
```

The current `origin/main` tree contains the relevant implementation, including `internal/video/providers/coderender.go`, `internal/video/agent/`, `internal/video/render/`, `internal/video/api/`, `internal/video/runtime/`, and the Universal Video documentation.

The inspected example configuration identifies providers such as OpenAI-compatible and Anthropic-compatible examples, but these are examples rather than an authorized runtime configuration. `configs/video.example.json` leaves `video.enabled`, `code_render_enabled`, and the planning model unset/disabled.

## Local-only media evidence

No live-model MP4 was generated. For a non-live sanity check only, the existing repository-owned deterministic renderer sample was inspected with `ffprobe`:

```bash
ffprobe -v error \
  -show_entries stream=codec_name,width,height,r_frame_rate \
  -show_entries format=duration,size \
  -of json docs/audit-evidence/representative-audit-video.mp4
```

Observed sample properties:

```json
{
  "streams": [
    {
      "codec_name": "h264",
      "width": 640,
      "height": 360,
      "r_frame_rate": "12/1"
    }
  ],
  "format": {
    "duration": "4.000000",
    "size": "17764"
  }
}
```

This file is deterministic renderer evidence from the earlier audit, **not** evidence of a real model request, a live plan, a live job, or an authenticated live asset download. It is not counted in the live result totals above.

## Tests not run

- `go test -count=1 ./internal/video/... ./cmd/gateway/... ./cmd/videogen/...` — blocked: Go is not installed.
- `./scripts/verify.sh` — blocked: Go is not installed.
- Gateway startup, authenticated planning, job creation, polling, asset download — not run: no gateway binary/toolchain or runtime configuration.
- Three identical English model attempts — not run.
- One Persian model attempt — not run.
- Persian/Arabic joining, RTL ordering, punctuation, font coverage, alignment, clipping, and readability — not tested.
- Production-scale load and long-running retention behavior — not tested.

Previously recorded CI and scripted-planner results from the merged implementation/audit are not substituted for this live-model test and are not presented as fresh local results.

## Practical next step

To unblock a valid smoke test, the owner must provide or enable all of the following in an authorized test environment:

1. A built NexaRoute `gateway` and `videogen` binary, or an installed Go toolchain capable of building them.
2. A non-production NexaRoute configuration with `video.enabled: true`, `code_render_enabled: true`, a configured planning model, and the normal provider routing credentials supplied through their intended environment mechanism.
3. A non-empty video bearer token and a local storage/work directory.
4. A bounded, approved test budget sufficient for at most three identical English attempts and one Persian attempt.
5. A supported font/Piper setup if Persian narration is to be tested.

Once those prerequisites are available, rerun the prescribed four attempts through the normal authenticated gateway path and record each model request, plan validation, render, download, ffprobe result, visual inspection, latency, retries, and cost separately.

## Safety and scope

No production configuration was changed. No external model was called. No application code was changed. No merge, release, deployment, issue closure, or credential exposure was performed.
