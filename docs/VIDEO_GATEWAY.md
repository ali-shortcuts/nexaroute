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

## Important limits

When `video.enabled` is true, `cmd/gateway` creates the Video Runtime, mounts `/v1/video/` on the primary HTTP server, starts bounded workers, and closes the queue during shutdown. When it is false (the default), no video route or worker is created and the LLM data plane is unchanged. The JSON store is single-process and is not multi-node safe; use PostgreSQL/Redis only after an implementation and concurrency test exist. No real provider adapter is enabled or claimed verified. Provider credentials must come from environment variables when adapters are implemented; never use account rotation or quota bypass. The current CLI is still a local development tool and does not yet attach to a running gateway's durable store.

Run focused tests with `go test ./internal/video/...` and the full suite with `go test ./...`.
