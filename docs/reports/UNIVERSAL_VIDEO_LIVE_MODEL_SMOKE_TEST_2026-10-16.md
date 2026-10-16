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

**Follow-up status:** Go and local compilation blockers were removed; the live test remains blocked by missing authorized provider credentials, video bearer token, and bounded budget. Follow-up source: `bf3eef1bc8ced1640087a9de59485400a15e040c` (`origin/main`).

A real end-to-end NexaRoute model test could not be started safely in this execution environment. The initial smoke-test execution did not have the required NexaRoute runtime prerequisites. A follow-up environment preparation was performed and is recorded below; it removed the toolchain/binary blocker but did not establish an authorized live provider configuration.
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

## Initial smoke-test environment inspection

The following checks were performed during the initial blocked run against that repository state:

```text
git fetch origin --prune                         PASS
git show --no-patch origin/main                  d27669a953da318aa3a2de6b2d1531e1e16b42a4 (initial run)
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

## Follow-up environment preparation and fresh local verification

A follow-up run installed the latest official Go 1.27.2 toolchain available from `go.dev/dl`, verified its published SHA-256 checksum, and built both binaries from the current `origin/main` source. The build succeeded:

- `go build -trimpath -o /home/ubuntu/nexaroute-live-build/gateway ./cmd/gateway` — PASS.
- `go build -trimpath -o /home/ubuntu/nexaroute-live-build/videogen ./cmd/videogen` — PASS.
- `go test -count=1 ./internal/video/... ./cmd/gateway/... ./cmd/videogen/...` — PASS; all listed packages passed.
- `./scripts/verify.sh` — PASS, including clean unit/integration tests, vet, race pass, release/SBOM checks, browser acceptance, fuzz smoke, and amd64/arm64 builds. The script banner reported `go1.26.8`; the direct build/test shell used verified Go 1.27.2.
- A temporary non-production configuration passed `gateway config validate` and `gateway config dry-run`; dry-run explicitly opened no listener and made no upstream requests.

A bounded gateway startup attempt with that temporary configuration failed before serving, with the redacted precise error:

```text
cannot initialize video gateway: video authentication token environment variable "UNCONFIGURED_VIDEO_TOKEN" is missing or empty
```

This confirms the remaining blocker is configuration/authorization, not inability to compile the current source. No real model request was made.

## Tests not run

- Gateway startup with a real authorized configuration, authenticated planning, job creation, polling, and asset download — blocked: no authorized provider/model configuration, bearer token, or approved spending allowance is available.
- Three identical English model attempts — not run.
- One Persian model attempt — not run.
- Persian/Arabic joining, RTL ordering, punctuation, font coverage, alignment, clipping, and readability — not tested.
- Production-scale load and long-running retention behavior — not tested.

Previously recorded CI and scripted-planner results from the merged implementation/audit are not substituted for this live-model test and are not presented as fresh local results.

## Practical next step

To unblock a valid smoke test, the owner must provide or enable all of the following in an authorized test environment:

1. A non-production NexaRoute configuration with `video.enabled: true`, `code_render_enabled: true`, and a configured planning model.
2. Normal provider routing credentials supplied through the intended secret mechanism.
3. A non-empty video bearer token and a local storage/work directory.
4. A bounded, approved test budget sufficient for at most three identical English attempts and one Persian attempt.
5. A supported font/Piper setup if Persian narration is to be tested.

Once those prerequisites are available, rerun the prescribed four attempts through the normal authenticated gateway path and record each model request, plan validation, render, download, ffprobe result, visual inspection, latency, retries, and cost separately.

## Safety and scope

No production configuration was changed. No external model was called. No application code was changed. No merge, release, deployment, issue closure, or credential exposure was performed.
