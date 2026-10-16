# Video Gateway

The repository now contains an isolated `internal/video` domain and provider contract. The first implementation includes:

- versioned `VideoRequest`, `VideoJob`, asset, shot, capability, and cost models;
- bounded in-memory queue and job store with idempotency lookup;
- durable JSON job store with atomic replacement and restart recovery;
- explicit state-transition validation, persisted request manifests, and webhook deduplication;
- orchestrator with per-shot provider submission, durable checkpoints, bounded polling, cancellation, aggregate cost admission, and final episode composition;
- secure local filesystem asset sink with atomic writes, SHA-256 metadata, and traversal protection;
- deterministic FFmpeg normalize/concat helpers with command manifest;
- standalone HTTP handler for create/get/cancel/provider listing;
- a non-production `fake` provider used only by tests and local development.
- bearer-token authentication support in the standalone data-plane handler;
- timestamped HMAC webhook verification with replay-window checks.

## Universal AI-to-Video (code-render)

The `code-render` provider lets **any configured text or coding model** — DeepSeek, Claude, Gemini, OpenAI-compatible, or another configured deployment — create a real video, even though the model itself has no video-generation capability. The model acts as the creative planner; NexaRoute's deterministic renderer does the execution:

`brief -> planning model -> validated video plan (video-plan.v1) -> scene renderer (FFmpeg) -> optional Piper narration/subtitles -> QC gate (ffprobe + frame checks) -> downloadable MP4`

This is **programmatic video**: animated text and titles, motion graphics, charts, timelines, and diagrams. It is not native generative video — a text model does not gain a photorealistic video head. Requests for native generation are rejected with a pointer to a native provider (for example ComfyUI) instead of being silently downgraded. Creation modes are reported truthfully in job metadata (`usage_metadata.native_video=false`, `programmatic=true`, the planning model, and the tools used).

Enable it in the existing `video` configuration object:

```json
{
  "video": {
    "enabled": true,
    "auth_token_env": "NEXAROUTE_VIDEO_TOKEN",
    "code_render_enabled": true,
    "code_render_planner_model": "deepseek-chat",
    "code_render_max_duration_seconds": 120
  }
}
```

- `code_render_planner_base_url` defaults to this gateway's own data plane on loopback, so the planning call reuses the existing provider configuration, routing, failover, budgets, and credentials. Point it at another OpenAI-compatible endpoint only when needed; plain HTTP is loopback-only. If that endpoint needs a bearer token, name its environment variable in `code_render_planner_api_key_env` (the value is never logged or returned).
- `code_render_planner_models` optionally pins the selectable planning models; when empty, the gateway advertises its configured public model names, deployment models, and aliases. The user picks the planning model per job (`model_preference`); the engine never silently overrides that choice.
- The planning model returns a declarative plan (schema `video-plan.v1`): ordered scenes with durations that must sum to the requested total, solid/gradient backgrounds, camera moves (`zoom_in`, `zoom_out`, `pan_left`, `pan_right`), transitions, text/shape/chart elements with bounded animations, captions, and narration text. Malformed output is repaired through bounded corrective prompts (3 attempts by default), then the job fails explicitly. The plan is data, never code: unknown fields are rejected, and no field can name a path, URL, or command.
- A client may instead supply a reviewed plan directly as `video_plan` in the create request, or preview one first via `POST /v1/video/universal/plan` (same bearer auth) — Video Studio's **Generate plan for review** uses this endpoint, and the edited plan is validated again when the job is submitted.
- Rendering is local and deterministic (FFmpeg `color`/`gradients` sources with `drawtext`/`drawbox`/`zoompan`, composed through the existing composer). A DejaVu/Noto/Liberation font must be installed, or set `NEXAROUTE_VIDEO_FONT` to a font file. Completion requires the QC gate: H.264 codec, plan-matching resolution and duration, and non-blank sampled frames for every scene. A failed check fails the job with the defect recorded; no asset is published.
- Narration and burned-in subtitles work when Piper is configured (`piper_enabled`), reusing the local Piper integration: the plan's narration (or the request's `narration_text`) is synthesized, timed SRT captions are built from the narration/captions, and both are composed into the final MP4. Narration longer than the video fails the job explicitly.
- Cost: rendering is local and the provider reports a USD 0 video-ledger estimate. The planning model's token usage is metered by the gateway's normal request accounting when the planner call passes through it — it is not free, and it is not double-counted in the video ledger. Local rendering still consumes compute and electricity.
- Limits: total duration is capped by `code_render_max_duration_seconds` (default 120s) and by the plan contract (32 scenes, 120s per scene, 24 elements per scene). Reference/first/last-frame assets, dialogue, music generation, and image elements are not supported in this baseline and are rejected explicitly. Provider state is in-process: a gateway restart during rendering leaves the job resumable-or-manual under the normal recovery rules and never resubmits the planning call (it happens once, before job acceptance).
- Test video: enable the provider, open Video Studio, choose `code-render`, pick a planning model, enter a brief (for example "A 6-second explainer: title slide, then a bar chart of quarterly growth"), queue the job, and download `episode.mp4` from the completed job card. From the API: `POST /v1/video/jobs` with `provider_preference: "code-render"`, `model_preference: "<a configured model>"`, `prompt`, `duration_seconds`, `aspect_ratio`, and `creation_mode: "programmatic"`.

## Local, no-provider-fee generation with ComfyUI

The optional `comfyui` adapter lets the Video Gateway submit a saved ComfyUI **API-format workflow** to a local ComfyUI server. This avoids a commercial video API bill, but it still requires compatible models, GPU/CPU resources, disk space, and electricity. NexaRoute does not download models or invent a working workflow for a particular model family.

Add the following fields to the existing `video` object in the NexaRoute config. Replace the node IDs with the IDs in your own ComfyUI workflow:

```json
{
  "video": {
    "enabled": true,
    "auth_token_env": "NEXAROUTE_VIDEO_TOKEN",
    "comfyui_enabled": true,
    "comfyui_base_url": "http://127.0.0.1:8188",
    "comfyui_workflow_path": "comfyui-api-workflow.json",
    "comfyui_model": "local-video",
    "comfyui_max_duration_seconds": 10,
    "comfyui_prompt_node_id": "6",
    "comfyui_negative_node_id": "7",
    "comfyui_width_node_id": "8",
    "comfyui_height_node_id": "8",
    "comfyui_frames_node_id": "12",
    "comfyui_fps_node_id": "12",
    "comfyui_seed_node_id": "12"
  }
}
```

- Export the workflow from ComfyUI using **Save (API Format)** and place it at the configured path (relative paths resolve from the NexaRoute config directory).
- The prompt, width, height, and frame-count node IDs are required. By default their input names are `text`, `width`, `height`, and `length`; FPS defaults to `frame_rate`, seed to `noise_seed`, and negative prompt to `text`. Override the corresponding `comfyui_*_input` setting if your workflow uses different input names.
- Configure the output node to emit an actual `.mp4` file. Other formats and image-only outputs are deliberately not reported as completed videos.
- The adapter currently supports one text-to-video workflow per job and MP4 output only. It rejects multi-shot plans, reference/first/last-frame assets, narration/subtitle fields, callbacks, and unsupported negative-prompt/seed/FPS settings rather than silently ignoring them. It does not claim audio/dialogue generation, remote webhooks, or safe interruption of an already-running ComfyUI prompt. A queued prompt can be removed; a running prompt is not globally interrupted because that could stop another user's work.
- The adapter's estimated provider API cost is USD 0 because it runs locally; that estimate excludes electricity, hardware depreciation, model downloads, and other infrastructure costs. It is not a guarantee of zero total operating cost.
- Plain HTTP is accepted only for loopback addresses. Use HTTPS for a remote ComfyUI endpoint.

## Local ComfyUI setup

The production adapter in this repository connects to a ComfyUI API server on your machine. It does not download models or invent a workflow for you: export a workflow in ComfyUI's API format, make sure it produces a video output, and configure the node IDs used for prompt, width, height, and frame count. The configured workflow file must be readable by the NexaRoute gateway process.

1. Install FFmpeg with H.264 support on Ubuntu:

   ```bash
   sudo apt-get update
   sudo apt-get install -y ffmpeg
   ffmpeg -hide_banner -encoders | grep libx264
   ```

2. Start ComfyUI with its API enabled and keep it bound to loopback for local use (default API URL `http://127.0.0.1:8188`). The adapter refuses plain HTTP to non-loopback hosts.

3. Copy `configs/video.example.json` and configure the `video` section. Set `enabled: true`, `comfyui_enabled: true`, `auth_token_env: "NEXAROUTE_VIDEO_TOKEN"`, and point `comfyui_workflow_path` to your exported API workflow. Fill `comfyui_prompt_node_id`, `comfyui_width_node_id`, `comfyui_height_node_id`, and `comfyui_frames_node_id` with the exact IDs from that workflow. Set the optional negative-prompt, FPS, and seed node IDs only when those inputs exist. The default input names are `text`, `width`, `height`, `length`, `frame_rate`, and `noise_seed`; change them when your workflow uses different names.

4. Set the same non-empty bearer token in the gateway process environment. For a local shell, generate one with `openssl rand -hex 32`; for a service, configure it in the service's secret environment rather than putting the token in the JSON file. Paste that token into Video Studio's session token field or export it as `NEXAROUTE_VIDEO_TOKEN` for the CLI.

5. Start/restart the gateway, then verify the configured provider and submit a short test job:

   ```bash
   videogen providers
   videogen create --file request.json --dry-run
   videogen create --file request.json
   videogen job-get --id JOB_ID
   videogen asset-download --id JOB_ID --asset-id ASSET_ID --out ./episode.mp4
   ```

The ComfyUI adapter currently supports silent text-to-video only; it does not generate provider-side dialogue, sound effects, or music. Multi-shot jobs can optionally add local Piper narration and script-derived SRT captions after the clips are saved, then compose the final MP4 using FFmpeg. The configured `comfyui_max_duration_seconds` is a per-clip limit, not the total episode duration. Model/workflow requirements and licenses are managed by your local ComfyUI installation.

## Local narration and burned-in subtitles

The optional Piper adapter adds local speech synthesis without a paid API. Install Piper in a dedicated virtual environment and download a voice model from the official catalogue. The model and its matching `.onnx.json` sidecar are both required.

```bash
python3 -m venv ~/.local/share/nexaroute/piper-venv
~/.local/share/nexaroute/piper-venv/bin/pip install --upgrade pip piper-tts
mkdir -p ~/.local/share/nexaroute/voices
VOICE_NAME="choose-a-voice-from-the-Piper-catalogue"
~/.local/share/nexaroute/piper-venv/bin/python -m piper.download_voices --data-dir ~/.local/share/nexaroute/voices "$VOICE_NAME"
```

Set these fields in the gateway's `video` configuration:

```json
{
  "piper_enabled": true,
  "piper_binary": "/home/USER/.local/share/nexaroute/piper-venv/bin/piper",
  "piper_model_path": "/home/USER/.local/share/nexaroute/voices/VOICE_NAME.onnx"
}
```

Replace `USER` and `VOICE_NAME` with real values. If the gateway runs as a service or container, the binary and both model files must be readable inside that runtime. Restart the gateway after changing the configuration. Piper's official catalogue lists Persian (`fa_IR`) among supported languages, but voice quality, gender, and model licensing vary; review the selected voice's model card before publishing. The adapter invokes the local CLI once per episode, so the model is loaded for each narration job.

In Video Studio, enter either the full `narration_text` or per-shot `narration_segment` values in the shot-plan JSON—not both. Select a subtitle language to generate an SRT file and burn the captions into the final MP4. Captions are timed heuristically from text length; they are not word-aligned and are not translated. The synthesized WAV and SRT are stored as source assets and can be downloaded through the same authenticated asset endpoint.

Narration audio must fit inside the requested episode duration. If Piper produces speech longer than the total shot duration, the job fails with a clear error instead of silently truncating the narration; shorten the text or increase shot durations. Without Piper enabled, narration/subtitle requests fail before any video provider is submitted. Ambient sound, dialogue generation, music generation, and automatic translation remain unsupported.

## Asset downloads

The authenticated endpoint `GET /v1/video/jobs/{job_id}/assets/{asset_id}` streams an asset only when its ID is present in that job's persisted output/source asset list. Downloads are constrained to the configured local storage root, regular files only, and the configured size limit. Responses support HTTP range requests and are sent as attachments with `nosniff` and `no-store` headers. Job JSON uses route-relative asset URIs; it does not disclose the server's absolute filesystem paths. A bearer token configured by `video.auth_token_env` is required for job and asset requests.

Use the CLI to save a completed output without exposing the server's local path:

```bash
videogen job-get --id JOB_ID
videogen asset-download --id JOB_ID --asset-id ASSET_ID --out ./episode.mp4
```

The `ASSET_ID` comes from the job's `output_assets` or `source_assets`. The command reads the bearer token from `NEXAROUTE_VIDEO_TOKEN` by default, creates the local output with mode `0600`, and refuses to overwrite an existing file.

## Multi-shot Gateway request

A multi-shot request uses one provider/model for the episode. `duration_seconds` must equal the sum of the individual shot durations, and every shot must fit the selected provider's per-clip limit. The Gateway stores each shot checkpoint and returns one final MP4 asset after composition.

Example request file for `videogen create --file episode-request.json`:

```json
{
  "project_id": "persepolis-history",
  "episode_id": "persepolis-001",
  "prompt": "",
  "mode": "text_to_video",
  "provider_preference": "comfyui",
  "duration_seconds": 8,
  "aspect_ratio": "16:9",
  "shot_plan": [
    {
      "shot_id": "arrival",
      "duration_seconds": 4,
      "prompt": "Wide establishing shot of Persepolis at sunrise, historically grounded ancient Persian architecture",
      "camera_movement": "slow cinematic pan"
    },
    {
      "shot_id": "walkthrough",
      "duration_seconds": 4,
      "prompt": "A time-travelling guide walks toward the monumental staircase at Persepolis",
      "subject_action": "walks forward while looking at the reliefs"
    }
  ]
}
```

Set `NEXAROUTE_URL` to the Gateway base URL and `NEXAROUTE_VIDEO_TOKEN` to the bearer token configured by `video.auth_token_env`. The CLI reads the token from the environment and does not print it. Do not include `shot_plan[].output_asset` in a client request; output assets are server-owned checkpoints and caller-supplied values are discarded.

## Gateway-side narration and captions

To enable narration inside Video Studio, install Piper and a compatible ONNX voice model locally, then configure the Gateway's `video` object:

```json
{
  "video": {
    "piper_enabled": true,
    "piper_binary": "piper",
    "piper_model_path": "models/fa_IR-voice-medium.onnx"
  }
}
```

The model's companion `.onnx.json` sidecar must be next to the model. Relative model paths resolve from the NexaRoute config directory. NexaRoute does not download Piper or voice models. Restart after changing config. Provide a narration script in Video Studio or set `narration_segment` on individual shots; the orchestrator persists the narration WAV before composition and reuses it after a retry. Select **Generate captions from narration script** to persist an SRT checkpoint and burn approximate same-language captions into the final MP4. Caption timing is estimated from text length and does not translate or align words to speech.

## Local offline narration with Piper

The standalone `videogen narrate` command can synthesize a narration WAV locally with a user-installed Piper CLI and ONNX voice model. It makes no network requests, requires no Gateway token, and downloads no voice/model files. Install Piper and a compatible voice model separately; keep the model and its companion configuration file together when required by that voice.

```bash
videogen narrate --text-file story-fa.txt --model /path/to/fa_IR-voice-medium.onnx --out narration.wav
```

Use `--piper-bin /path/to/piper` when the executable is not named `piper`. The text input must be non-empty UTF-8, the model must be a readable regular file, and the output must end in `.wav`. The command invokes Piper directly without a shell, bounds diagnostic output, applies a 30-minute timeout, validates the RIFF/WAVE header, writes the output with mode 0600, and atomically replaces the destination only after successful synthesis. A failed synthesis leaves an existing destination unchanged.

Piper and the voice model are local dependencies, not bundled or downloaded by NexaRoute. Quality, language coverage, and runtime depend on the chosen model and hardware. The local synthesis command is a CLI step; it is not yet a Video Studio action or a Gateway-side persisted job stage.

For a single local narration-and-editing command, use `videogen render --file render.json --piper-bin /path/to/piper`. The JSON manifest contains the normal episode fields (`shots`, `music_path`, `subtitle_path`, `output_path`, `width`, `height`, `fps`) plus `narration_text_path`, `piper_model_path`, and `narration_output_path`. Supply all three narration fields together. The command first writes the WAV checkpoint, then runs FFmpeg composition; if composition fails, the narration WAV remains available for a retry. Alternatively, omit all three fields and provide an existing `narration_path`.

Example:

```json
{
  "shots": ["assets/shot-01.mp4", "assets/shot-02.mp4"],
  "narration_text_path": "story-fa.txt",
  "piper_model_path": "/models/fa_IR-voice-medium.onnx",
  "narration_output_path": "audio/narration.wav",
  "music_path": "audio/music.wav",
  "subtitle_path": "subtitles/fa.srt",
  "output_path": "renders/episode.mp4",
  "width": 1920,
  "height": 1080,
  "fps": 24
}
```


## Local episode assembly

After generated shot clips and optional audio/subtitle files exist on disk, the local CLI can assemble them into one MP4:

```bash
videogen compose --file episode.json
```

Example `episode.json`:

```json
{
  "shots": ["assets/shot-01.mp4", "assets/shot-02.mp4"],
  "narration_path": "audio/narration.wav",
  "music_path": "audio/music.wav",
  "subtitle_path": "subtitles/fa.srt",
  "output_path": "renders/episode.mp4",
  "width": 1920,
  "height": 1080,
  "fps": 24,
  "narration_volume": 1.0,
  "music_volume": 0.12
}
```

Paths are resolved relative to the current working directory. Install FFmpeg with the `libx264` encoder or set `NEXAROUTE_FFMPEG` to its executable path. This local command does not require a Video Gateway token. A zero volume field selects its default (narration 1.0, music 0.12). The Gateway preflights FFmpeg and `libx264` before admitting multi-shot work, so a missing encoder fails before any provider job is submitted.

For a Gateway request with more than one `shot_plan` entry, the orchestrator validates and estimates each shot separately, persists each provider job ID and output asset, then composes the ordered clips into an MP4. If configured, the narration WAV and SRT captions are persisted as separate checkpoints and reused on restart or composition retry. If a crash leaves a shot in the pre-submission state without a durable provider job ID, the job moves to manual action rather than risking a duplicate billable submission. One episode currently uses one provider/model, and each individual shot must fit that provider's limits.

When `video.piper_enabled` is true, the Gateway can synthesize narration for multi-shot episodes and optionally burn in approximate same-language SRT captions. Captions use estimated timing based on text length; they are not translated or word-aligned. Provider-generated dialogue, sound effects, and music remain unsupported. The local `videogen render`/`compose` commands can still mix user-supplied music and burn in an existing SRT. Cancellation during local FFmpeg composition is intentionally rejected to avoid a race that could publish a partial result.

## Important limits

When `video.enabled` is true, `cmd/gateway` creates the Video Runtime, mounts `/v1/video/` on the primary HTTP server, starts bounded workers, and cancels and joins the worker pool before shutdown returns. When it is false (the default), no video route or worker is created and the LLM data plane is unchanged. The JSON store is single-process and is not multi-node safe; use PostgreSQL/Redis only after an implementation and concurrency test exist. The opt-in ComfyUI adapter is local text-to-video only and requires a user-supplied API-format workflow; no commercial provider adapter is bundled. ComfyUI does not require provider API credentials. Any future external-provider credentials must come from environment variables; never use account rotation or quota bypass. The `videogen` Gateway commands attach to a running Gateway through the authenticated API and use its configured job store; `videogen compose` is a local FFmpeg command and does not require a Gateway token.

When enabled, runtime startup requires a non-empty bearer token from `video.auth_token_env`; the named environment variable must exist in the gateway process. The JSON job store, queue, and cost ledger remain single-process. Startup recovery re-enqueues queued jobs, resumes known single-shot provider jobs, and resumes multi-shot jobs from per-shot checkpoints or the composition stage. Ambiguous submissions without a recorded provider job ID move to `needs_manual_action` rather than being resubmitted blindly.

Provider completion is not enough to mark a job complete: output files must be downloaded via the provider contract and persisted by the configured local asset sink with a URI, non-zero size, and SHA-256 metadata. The default per-file cap is 512 MiB; `video.max_asset_bytes` can be set from 1 byte through 8 GiB. This post-download sink cap does not replace streaming and Content-Length enforcement in a future real provider adapter because the current interface accepts byte slices.

Cost estimation errors or malformed estimates fail closed before admission. The in-process ledger separates reserved, estimated, and provider-reported actual spend, but it is not durable or distributed and cannot guarantee that actual provider billing equals the estimate. The `fake` provider writes a text placeholder that is explicitly not a playable video; it only validates local orchestration and storage.

When enabled, runtime startup requires a non-empty bearer token from `video.auth_token_env`; the named environment variable must exist in the gateway process. The JSON job store, queue, and cost ledger remain single-process. Startup recovery re-enqueues queued jobs and resumes jobs with known provider job IDs; ambiguous submissions without a recorded provider job ID move to `needs_manual_action` rather than being resubmitted blindly.

Provider completion is not enough to mark a job complete: output files must be downloaded via the provider contract and persisted by the configured local asset sink with a URI, non-zero size, and SHA-256 metadata. The default per-file cap is 512 MiB; `video.max_asset_bytes` can be set from 1 byte through 8 GiB. This post-download sink cap does not replace streaming and Content-Length enforcement in a future real provider adapter because the current interface accepts byte slices.

Cost estimation errors or malformed estimates fail closed before admission. The in-process ledger separates reserved, estimated, and provider-reported actual spend, but it is not durable or distributed and cannot guarantee that actual provider billing equals the estimate. The `fake` provider writes a text placeholder that is explicitly not a playable video; it only validates local orchestration and storage.

Run focused tests with `go test ./internal/video/...` and the full suite with `go test ./...`.
