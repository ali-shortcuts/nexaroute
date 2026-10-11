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

## Local episode assembly building block

The `internal/video/composer.Runner.ComposeEpisode` helper assembles already-generated local shot files into an MP4: it normalizes each shot to a common resolution/FPS, concatenates shots in the supplied order, optionally mixes narration with looping background music, and optionally burns in an SRT subtitle file. It uses temporary files beside the destination, preserves the MP4 extension for FFmpeg's muxer detection, and removes temporary artifacts on return.

The helper requires FFmpeg on the host and accepts an `EpisodeSpec` JSON shape with `shots` (ordered local file paths), optional `narration_path`, `music_path`, and `subtitle_path`, required `output_path`, `width`, `height`, and `fps`, plus optional `narration_volume` and `music_volume`. A zero audio-volume field selects its default (narration 1.0, music 0.12).

This is currently a local composition building block, **not yet a persistent Video Gateway episode job or a Video Studio action**. The gateway does not yet create/checkpoint one provider job per shot and automatically hand all shot assets to this composer.

## Important limits

When `video.enabled` is true, `cmd/gateway` creates the Video Runtime, mounts `/v1/video/` on the primary HTTP server, starts bounded workers, and cancels and joins the worker pool before shutdown returns. When it is false (the default), no video route or worker is created and the LLM data plane is unchanged. The JSON store is single-process and is not multi-node safe; use PostgreSQL/Redis only after an implementation and concurrency test exist. No real provider adapter is enabled or claimed verified. Provider credentials must come from environment variables when adapters are implemented; never use account rotation or quota bypass. The current CLI is still a local development tool and does not yet attach to a running gateway's durable store.

When enabled, runtime startup requires a non-empty bearer token from `video.auth_token_env`; the named environment variable must exist in the gateway process. The JSON job store, queue, and cost ledger remain single-process. Startup recovery re-enqueues queued jobs and resumes jobs with known provider job IDs; ambiguous submissions without a recorded provider job ID move to `needs_manual_action` rather than being resubmitted blindly.

Provider completion is not enough to mark a job complete: output files must be downloaded via the provider contract and persisted by the configured local asset sink with a URI, non-zero size, and SHA-256 metadata. The default per-file cap is 512 MiB; `video.max_asset_bytes` can be set from 1 byte through 8 GiB. This post-download sink cap does not replace streaming and Content-Length enforcement in a future real provider adapter because the current interface accepts byte slices.

Cost estimation errors or malformed estimates fail closed before admission. The in-process ledger separates reserved, estimated, and provider-reported actual spend, but it is not durable or distributed and cannot guarantee that actual provider billing equals the estimate. The `fake` provider writes a text placeholder that is explicitly not a playable video; it only validates local orchestration and storage.

Run focused tests with `go test ./internal/video/...` and the full suite with `go test ./...`.
