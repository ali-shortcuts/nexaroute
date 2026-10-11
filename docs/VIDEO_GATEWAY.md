# Video Gateway

The repository now contains an isolated `internal/video` domain and provider contract. The first implementation includes:

- versioned `VideoRequest`, `VideoJob`, asset, shot, capability, and cost models;
- bounded in-memory queue and job store with idempotency lookup;
- durable JSON job store with atomic replacement and restart recovery;
- explicit state-transition validation, persisted request manifests, and webhook deduplication;
- orchestrator skeleton with provider submission, bounded polling, cancellation, and budget gate;
- secure local filesystem asset sink with atomic writes, SHA-256 metadata, and traversal protection;
- deterministic FFmpeg normalize/concat helpers with command manifest;
- standalone HTTP handler for create/get/cancel/provider listing;
- a non-production `fake` provider used only by tests and local development.
- bearer-token authentication support in the standalone data-plane handler;
- timestamped HMAC webhook verification with replay-window checks.

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
- Configure the output node to emit an actual `.mp4`, `.webm`, `.mov`, or `.mkv` file. Image-only outputs are deliberately not reported as completed videos.
- The adapter currently supports text-to-video only. It does not claim audio/dialogue generation, remote webhooks, or safe interruption of an already-running ComfyUI prompt. A queued prompt can be removed; a running prompt is not globally interrupted because that could stop another user's work.
- The adapter's estimated provider API cost is USD 0 because it runs locally; that estimate excludes electricity, hardware depreciation, model downloads, and other infrastructure costs. It is not a guarantee of zero total operating cost.
- Plain HTTP is accepted only for loopback addresses. Use HTTPS for a remote ComfyUI endpoint.

## Important limits

When `video.enabled` is true, `cmd/gateway` creates the Video Runtime, mounts `/v1/video/` on the primary HTTP server, starts bounded workers, and cancels and joins the worker pool before shutdown returns. When it is false (the default), no video route or worker is created and the LLM data plane is unchanged. The JSON store is single-process and is not multi-node safe; use PostgreSQL/Redis only after an implementation and concurrency test exist. No real provider adapter is enabled or claimed verified. Provider credentials must come from environment variables when adapters are implemented; never use account rotation or quota bypass. The current CLI is still a local development tool and does not yet attach to a running gateway's durable store.

When enabled, runtime startup requires a non-empty bearer token from `video.auth_token_env`; the named environment variable must exist in the gateway process. The JSON job store, queue, and cost ledger remain single-process. Startup recovery re-enqueues queued jobs and resumes jobs with known provider job IDs; ambiguous submissions without a recorded provider job ID move to `needs_manual_action` rather than being resubmitted blindly.

Provider completion is not enough to mark a job complete: output files must be downloaded via the provider contract and persisted by the configured local asset sink with a URI, non-zero size, and SHA-256 metadata. The default per-file cap is 512 MiB; `video.max_asset_bytes` can be set from 1 byte through 8 GiB. This post-download sink cap does not replace streaming and Content-Length enforcement in a future real provider adapter because the current interface accepts byte slices.

Cost estimation errors or malformed estimates fail closed before admission. The in-process ledger separates reserved, estimated, and provider-reported actual spend, but it is not durable or distributed and cannot guarantee that actual provider billing equals the estimate. The `fake` provider writes a text placeholder that is explicitly not a playable video; it only validates local orchestration and storage.

Run focused tests with `go test ./internal/video/...` and the full suite with `go test ./...`.
