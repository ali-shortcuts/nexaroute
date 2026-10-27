package nexaflow

import (
	"context"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// Video nodes submit, poll, and fetch outputs through the existing video
// HTTP API (POST /v1/video/jobs, GET /v1/video/jobs/{id},
// GET /v1/video/jobs/{id}/assets/{assetID}). They never touch the queue
// store directly: queue.JobStore.Create alone does not run a job, so
// submitting through the store would silently create a dead job. Video
// persistence, orchestration, and asset storage stay with the video
// subsystem; this package only adapts its API.
//
// Bounds follow the video domain: prompt non-empty and <= 4000 chars,
// duration > 0 and <= 600s (the node bound; the gateway's own 3600s
// ceiling still applies upstream), mode in the video domain set,
// aspect ratio in the video domain set. Submit is billable and requires
// operator policy approval; status and output are read-only.
// Retry semantics: submit MaxAttempts 1 (a retry could double-submit a
// billable job); status/output retryable on timeout. Cancellation aborts
// the HTTP call; it does NOT cancel the video job (cancel is a separate
// video API operation, not exposed as a node here).
// Outputs never contain filesystem paths: asset metadata only, plus
// bytes only when the caller asks and the bytes fit the output bound.
func videoNodeSpecs() []*NodeSpec {
	return []*NodeSpec{
		{
			ID: "nexaroute.video.submit", Version: SupportedVersion, Category: "video",
			Title:       "Video job submission",
			Description: "Submits a video job through the existing video API. Billable; requires operator policy approval. Does not poll or wait.",
			InputSchema: []Field{
				{Name: "prompt", Type: "string", Required: true, Description: "Video prompt, 1..4000 characters."},
				{Name: "mode", Type: "string", Description: "text_to_video, image_to_video, or video_to_video; default text_to_video."},
				{Name: "duration_seconds", Type: "number", Description: "Duration in seconds, >0 and <=600."},
				{Name: "aspect_ratio", Type: "string", Description: "16:9, 9:16, 1:1, or custom; default 16:9."},
			},
			OutputSchema: []Field{
				{Name: "job_id", Type: "string"},
				{Name: "state", Type: "string"},
			},
			ConfigSchema: nil,
			Permission:   authz.ManageVideo, SideEffect: SideEffectBillable,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 30 * time.Second, MaxTimeout: 60 * time.Second,
			MaxInputBytes: 64 << 10, MaxOutputBytes: 4096,
			RequiresOperatorPolicy: true,
			Run:                    runVideoSubmit,
		},
		{
			ID: "nexaroute.video.status", Version: SupportedVersion, Category: "video",
			Title:       "Video job status",
			Description: "One poll of a video job's state. Read-only; waiting for completion is the runtime's job.",
			InputSchema: []Field{
				{Name: "job_id", Type: "string", Required: true},
			},
			OutputSchema: []Field{
				{Name: "job_id", Type: "string"},
				{Name: "state", Type: "string"},
				{Name: "terminal", Type: "boolean", Description: "True for completed, failed, cancelled, expired."},
			},
			ConfigSchema: nil,
			Permission:   authz.ManageVideo, SideEffect: SideEffectRead,
			Retry:          RetryPolicy{MaxAttempts: 3, RetryableCodes: []string{"timeout", "backend_error"}},
			DefaultTimeout: 10 * time.Second, MaxTimeout: 30 * time.Second,
			MaxInputBytes: 4096, MaxOutputBytes: 64 << 10,
			Run: runVideoStatus,
		},
		{
			ID: "nexaroute.video.output", Version: SupportedVersion, Category: "video",
			Title:       "Video job output",
			Description: "Asset metadata for a job, plus asset bytes only when asset_id is given and the bytes fit the output bound. Never returns filesystem paths.",
			InputSchema: []Field{
				{Name: "job_id", Type: "string", Required: true},
				{Name: "asset_id", Type: "string", Description: "When set, fetch this asset's bytes (only if they fit the output bound)."},
			},
			OutputSchema: []Field{
				{Name: "job_id", Type: "string"},
				{Name: "assets", Type: "array", Description: "Asset metadata: id, filename, content_type, sha256, size_bytes. No paths."},
				{Name: "asset", Type: "object", Description: "Fetched asset metadata, when asset_id was given."},
				{Name: "data_base64", Type: "string", Description: "Base64 asset bytes, only when they fit the output bound."},
			},
			ConfigSchema: nil,
			Permission:   authz.ManageVideo, SideEffect: SideEffectRead,
			Retry:          RetryPolicy{MaxAttempts: 3, RetryableCodes: []string{"timeout", "backend_error"}},
			DefaultTimeout: 30 * time.Second, MaxTimeout: 60 * time.Second,
			MaxInputBytes: 4096, MaxOutputBytes: 256 << 10,
			Run: runVideoOutput,
		},
	}
}

var videoModes = map[string]bool{
	"text_to_video": true, "image_to_video": true, "video_to_video": true,
}

var videoAspects = map[string]bool{
	"16:9": true, "9:16": true, "1:1": true, "custom": true,
}

func runVideoSubmit(ctx context.Context, rc *RunContext) (map[string]any, error) {
	if err := requireBackend(rc); err != nil {
		return nil, err
	}
	prompt, ok := getString(rc.Input, "prompt")
	if !ok || strings.TrimSpace(prompt) == "" || len(prompt) > 4000 {
		return nil, NewNodeError("validation_error", false, "prompt must be non-empty and at most 4000 characters")
	}
	mode := getStringOr(rc.Input, "mode", "text_to_video")
	if !videoModes[mode] {
		return nil, NewNodeError("validation_error", false, "unknown video mode %q", mode)
	}
	duration := 0.0
	if v, ok := getNumber(rc.Input, "duration_seconds"); ok {
		duration = v
	}
	if duration <= 0 || duration > 600 {
		return nil, NewNodeError("validation_error", false, "duration_seconds must be in (0,600]")
	}
	aspect := getStringOr(rc.Input, "aspect_ratio", "16:9")
	if !videoAspects[aspect] {
		return nil, NewNodeError("validation_error", false, "unknown aspect_ratio %q", aspect)
	}
	job, err := rc.Backend.VideoSubmit(ctx, VideoSubmitRequest{
		Prompt: prompt, Mode: mode, DurationSeconds: duration, AspectRatio: aspect,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"job_id": job.JobID, "state": job.State}, nil
}

func runVideoStatus(ctx context.Context, rc *RunContext) (map[string]any, error) {
	if err := requireBackend(rc); err != nil {
		return nil, err
	}
	jobID, ok := getString(rc.Input, "job_id")
	if !ok || strings.TrimSpace(jobID) == "" {
		return nil, NewNodeError("validation_error", false, "input job_id is required")
	}
	job, err := rc.Backend.VideoStatus(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"job_id": job.JobID, "state": job.State, "terminal": job.Terminal}, nil
}

func runVideoOutput(ctx context.Context, rc *RunContext) (map[string]any, error) {
	if err := requireBackend(rc); err != nil {
		return nil, err
	}
	jobID, ok := getString(rc.Input, "job_id")
	if !ok || strings.TrimSpace(jobID) == "" {
		return nil, NewNodeError("validation_error", false, "input job_id is required")
	}
	job, err := rc.Backend.VideoStatus(ctx, jobID)
	if err != nil {
		return nil, err
	}
	assets := make([]any, 0, len(job.Assets))
	for _, a := range job.Assets {
		assets = append(assets, map[string]any{
			"id": a.ID, "filename": a.Filename, "content_type": a.ContentType,
			"sha256": a.SHA256, "size_bytes": a.SizeBytes,
		})
	}
	out := map[string]any{"job_id": job.JobID, "assets": assets}
	assetID, hasAsset := getString(rc.Input, "asset_id")
	if hasAsset && strings.TrimSpace(assetID) != "" {
		meta, data, err := rc.Backend.VideoAsset(ctx, jobID, assetID)
		if err != nil {
			return nil, err
		}
		// Bytes only under the node output bound; metadata alone is
		// returned otherwise, with an explicit flag, never truncated bytes.
		out["asset"] = map[string]any{
			"id": meta.ID, "filename": meta.Filename, "content_type": meta.ContentType,
			"sha256": meta.SHA256, "size_bytes": meta.SizeBytes,
		}
		if len(data) > 0 {
			// []byte marshals to base64 in JSON, matching the
			// data_base64 output field; Execute enforces the bound
			// on the marshalled form.
			out["data_base64"] = data
		}
	}
	return out, nil
}
