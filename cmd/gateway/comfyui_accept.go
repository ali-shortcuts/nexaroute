package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

const (
	comfyAcceptSchema     = "nexaroute.comfyui-acceptance.v1"
	comfyAcceptDefaultTTL = 10 * time.Minute
	comfyAcceptMaxTTL     = 60 * time.Minute
	comfyAcceptMaxPoll    = 30 * time.Second
)

type comfyAcceptReport struct {
	Schema            string              `json:"schema"`
	GeneratedAt       string              `json:"generated_at"`
	Mode              string              `json:"mode"`
	EvidenceClass     string              `json:"evidence_class"`
	Outcome           string              `json:"outcome"`
	InferenceExecuted bool                `json:"inference_executed"`
	Bounds            comfyAcceptBounds   `json:"bounds"`
	Target            comfyAcceptTarget   `json:"target"`
	Request           comfyAcceptRequest  `json:"request"`
	Server            *comfyAcceptServer  `json:"server,omitempty"`
	Job               *comfyAcceptJob     `json:"job,omitempty"`
	Checks            []comfyAcceptCheck  `json:"checks"`
	Limitations       []string            `json:"limitations,omitempty"`
	Warnings          []string            `json:"warnings,omitempty"`
	Assets            []comfyAcceptAsset  `json:"assets,omitempty"`
	Cleanup           *comfyAcceptCleanup `json:"cleanup,omitempty"`
}

type comfyAcceptBounds struct {
	JobTimeoutSeconds  float64 `json:"job_timeout_seconds"`
	PollIntervalMillis int64   `json:"poll_interval_ms"`
}

type comfyAcceptTarget struct {
	BaseURL            string  `json:"base_url,omitempty"`
	Model              string  `json:"model,omitempty"`
	WorkflowFile       string  `json:"workflow_file,omitempty"`
	WorkflowSHA256     string  `json:"workflow_sha256,omitempty"`
	WorkflowBytes      int64   `json:"workflow_bytes,omitempty"`
	PromptNodeID       string  `json:"prompt_node_id,omitempty"`
	WidthNodeID        string  `json:"width_node_id,omitempty"`
	HeightNodeID       string  `json:"height_node_id,omitempty"`
	FramesNodeID       string  `json:"frames_node_id,omitempty"`
	NegativeNodeID     string  `json:"negative_node_id,omitempty"`
	FPSNodeID          string  `json:"fps_node_id,omitempty"`
	SeedNodeID         string  `json:"seed_node_id,omitempty"`
	MaxDurationSeconds float64 `json:"max_duration_seconds,omitempty"`
	MaxOutputBytes     int64   `json:"max_output_bytes,omitempty"`
	AssetDirectory     string  `json:"asset_directory,omitempty"`
	FFprobe            string  `json:"ffprobe,omitempty"`
}

type comfyAcceptRequest struct {
	DurationSeconds float64 `json:"duration_seconds"`
	FPS             int     `json:"fps"`
	Width           int     `json:"width,omitempty"`
	Height          int     `json:"height,omitempty"`
	PromptBytes     int     `json:"prompt_bytes"`
	PromptSHA256    string  `json:"prompt_sha256"`
}

type comfyAcceptServer struct {
	ComfyUIVersion string              `json:"comfyui_version,omitempty"`
	PythonVersion  string              `json:"python_version,omitempty"`
	PyTorchVersion string              `json:"pytorch_version,omitempty"`
	OS             string              `json:"os,omitempty"`
	RAMTotalBytes  int64               `json:"ram_total_bytes,omitempty"`
	RAMFreeBytes   int64               `json:"ram_free_bytes,omitempty"`
	Devices        []comfyAcceptDevice `json:"devices,omitempty"`
}

type comfyAcceptDevice struct {
	Name           string `json:"name,omitempty"`
	Type           string `json:"type,omitempty"`
	Index          int    `json:"index,omitempty"`
	VRAMTotalBytes int64  `json:"vram_total_bytes,omitempty"`
	VRAMFreeBytes  int64  `json:"vram_free_bytes,omitempty"`
}

type comfyAcceptJob struct {
	PromptID  string  `json:"prompt_id,omitempty"`
	State     string  `json:"state,omitempty"`
	Progress  float64 `json:"progress,omitempty"`
	Polls     int     `json:"polls"`
	LatencyMS int64   `json:"latency_ms,omitempty"`
}

type comfyAcceptAsset struct {
	ID          string                 `json:"id,omitempty"`
	Filename    string                 `json:"filename,omitempty"`
	ContentType string                 `json:"content_type,omitempty"`
	URI         string                 `json:"uri,omitempty"`
	SizeBytes   int64                  `json:"size_bytes,omitempty"`
	SHA256      string                 `json:"sha256,omitempty"`
	Metadata    map[string]string      `json:"metadata,omitempty"`
	Probe       *comfyAcceptAssetProbe `json:"probe,omitempty"`
}

type comfyAcceptAssetProbe struct {
	Codec   string  `json:"codec,omitempty"`
	Width   int     `json:"width,omitempty"`
	Height  int     `json:"height,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	Size    int64   `json:"size,omitempty"`
}

type comfyAcceptCheck struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type comfyAcceptCleanup struct {
	RemoteAttempted bool   `json:"remote_attempted"`
	RemoteStatus    string `json:"remote_status"`
	RemoteReason    string `json:"remote_reason,omitempty"`
	LocalStatus     string `json:"local_status"`
	LocalReason     string `json:"local_reason,omitempty"`
}

func runComfyUIAccept(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("comfyui-accept", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath(), "path to the NexaRoute JSON config containing video.comfyui_* settings")
	outPath := fs.String("out", "", "redacted acceptance JSON report path (default under .artifacts/comfyui-acceptance)")
	assetDir := fs.String("asset-dir", ".artifacts/comfyui-acceptance", "root directory for retained acceptance outputs")
	timeout := fs.Duration("timeout", comfyAcceptDefaultTTL, "overall acceptance deadline (max 60m)")
	pollInterval := fs.Duration("poll-interval", 2*time.Second, "ComfyUI status polling interval (max 30s)")
	prompt := fs.String("prompt", "NexaRoute ComfyUI acceptance: a single red cube rotating on a plain background", "minimal text prompt for the acceptance workflow")
	duration := fs.Float64("duration", 1, "requested clip duration in seconds")
	fps := fs.Int("fps", 24, "requested frames per second")
	width := fs.Int("width", 0, "requested width in pixels; 0 uses the workflow's aspect-ratio default")
	height := fs.Int("height", 0, "requested height in pixels; 0 uses the workflow's aspect-ratio default")
	maxOutput := fs.Int64("max-output-bytes", 0, "maximum downloaded asset size; 0 uses video.max_asset_bytes or the adapter default")
	ffprobe := fs.String("ffprobe", "ffprobe", "ffprobe executable used to verify the downloaded media is playable")
	confirmLive := fs.Bool("confirm-live", false, "required acknowledgment that this submits a real workflow to the configured ComfyUI server")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*confirmLive {
		fmt.Fprintln(stderr, "nexaroute comfyui-accept: refusing to run without --confirm-live.")
		fmt.Fprintln(stderr, "This command submits a real workflow to ComfyUI and may consume GPU/CPU time. It does not download models itself.")
		return 2
	}
	if *timeout <= 0 || *timeout > comfyAcceptMaxTTL {
		fmt.Fprintf(stderr, "nexaroute comfyui-accept: --timeout must be between 1ns and %s\n", comfyAcceptMaxTTL)
		return 2
	}
	if *pollInterval <= 0 || *pollInterval > comfyAcceptMaxPoll {
		fmt.Fprintf(stderr, "nexaroute comfyui-accept: --poll-interval must be between 1ns and %s\n", comfyAcceptMaxPoll)
		return 2
	}
	if strings.TrimSpace(*prompt) == "" {
		fmt.Fprintln(stderr, "nexaroute comfyui-accept: --prompt must not be empty")
		return 2
	}
	if *duration <= 0 || *fps < 1 {
		fmt.Fprintln(stderr, "nexaroute comfyui-accept: --duration must be positive and --fps must be at least 1")
		return 2
	}

	cfg, err := config.LoadBase(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "nexaroute comfyui-accept: cannot load config: %v\n", err)
		return 2
	}
	if strings.TrimSpace(*outPath) == "" {
		*outPath = filepath.Join(".artifacts", "comfyui-acceptance", "comfyui-acceptance-"+time.Now().UTC().Format("20060102T150405Z")+".json")
	}

	report := newComfyAcceptReport(cfg, *assetDir, *ffprobe, *timeout, *pollInterval)
	report.Request = comfyAcceptRequest{
		DurationSeconds: *duration,
		FPS:             *fps,
		Width:           *width,
		Height:          *height,
		PromptBytes:     len([]byte(*prompt)),
		PromptSHA256:    comfyAcceptSHA256([]byte(*prompt)),
	}

	code := runComfyUIAcceptChecks(&report, cfg, *configPath, *assetDir, *ffprobe, *prompt, *duration, *fps, *width, *height, *maxOutput, *timeout, *pollInterval)
	finalizeComfyAcceptReport(&report)
	if err := writeComfyAcceptReport(*outPath, report); err != nil {
		fmt.Fprintf(stderr, "nexaroute comfyui-accept: cannot write report: %v\n", err)
		return 2
	}
	printComfyAcceptSummary(stdout, report, *outPath)
	return code
}

func runComfyUIAcceptChecks(report *comfyAcceptReport, cfg config.Config, configPath, assetDir, ffprobePath, prompt string, duration float64, fps, width, height int, maxOutput int64, timeout, pollInterval time.Duration) int {
	preflightStart := time.Now()
	if !cfg.Video.Enabled || !cfg.Video.ComfyUIEnabled {
		addComfyAcceptCheck(report, "comfyui:preflight", externalStatusBlocked,
			"video.enabled and video.comfyui_enabled must both be true; no ComfyUI target was contacted", preflightStart)
		addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusNotRun,
			"actual model inference was not run because no ComfyUI target is provisioned", time.Time{})
		return 3
	}
	videoCfg := comfyAcceptVideoConfig(cfg.Video, configPath)
	if strings.TrimSpace(videoCfg.ComfyUIBaseURL) == "" || strings.TrimSpace(videoCfg.ComfyUIWorkflowPath) == "" {
		addComfyAcceptCheck(report, "comfyui:preflight", externalStatusBlocked,
			"video.comfyui_base_url and video.comfyui_workflow_path are required", preflightStart)
		addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusNotRun,
			"actual model inference was not run because the ComfyUI target is incomplete", time.Time{})
		return 3
	}
	report.Target.BaseURL = sanitizeAcceptanceURL(videoCfg.ComfyUIBaseURL)
	report.Target.Model = videoCfg.ComfyUIModel
	report.Target.WorkflowFile = filepath.Base(videoCfg.ComfyUIWorkflowPath)
	report.Target.PromptNodeID = videoCfg.ComfyUIPromptNodeID
	report.Target.WidthNodeID = videoCfg.ComfyUIWidthNodeID
	report.Target.HeightNodeID = videoCfg.ComfyUIHeightNodeID
	report.Target.FramesNodeID = videoCfg.ComfyUIFramesNodeID
	report.Target.NegativeNodeID = videoCfg.ComfyUINegativeNodeID
	report.Target.FPSNodeID = videoCfg.ComfyUIFPSNodeID
	report.Target.SeedNodeID = videoCfg.ComfyUISeedNodeID
	report.Target.MaxDurationSeconds = videoCfg.ComfyUIMaxDurationSeconds

	workflowData, err := os.ReadFile(videoCfg.ComfyUIWorkflowPath)
	if err != nil {
		addComfyAcceptCheck(report, "comfyui:preflight", externalStatusBlocked,
			"ComfyUI workflow is not readable: "+err.Error(), preflightStart)
		addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusNotRun,
			"actual model inference was not run because the workflow is unavailable", time.Time{})
		return 3
	}
	report.Target.WorkflowBytes = int64(len(workflowData))
	report.Target.WorkflowSHA256 = comfyAcceptSHA256(workflowData)

	if maxOutput <= 0 {
		maxOutput = cfg.Video.MaxAssetBytes
	}
	if maxOutput <= 0 {
		maxOutput = 512 << 20
	}
	report.Target.MaxOutputBytes = maxOutput

	ffprobeResolved, err := exec.LookPath(strings.TrimSpace(ffprobePath))
	if err != nil {
		addComfyAcceptCheck(report, "comfyui:preflight", externalStatusBlocked,
			"ffprobe executable is unavailable; playability cannot be verified without inventing a PASS", preflightStart)
		addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusNotRun,
			"actual model inference was not run because playability verification is unavailable", time.Time{})
		return 3
	}
	report.Target.FFprobe = ffprobeResolved

	provider, err := providers.NewComfyUI(videoCfg, maxOutput)
	if err != nil {
		addComfyAcceptCheck(report, "comfyui:preflight", externalStatusFail,
			"ComfyUI workflow/node mapping is invalid: "+err.Error(), preflightStart)
		addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusNotRun,
			"actual model inference was not run because the workflow/node mapping failed validation", time.Time{})
		return 1
	}
	addComfyAcceptCheck(report, "comfyui:preflight", externalStatusPass,
		"workflow is readable and every configured required node input exists", preflightStart)

	runCtx, cancelRun := context.WithTimeout(context.Background(), timeout)
	defer cancelRun()

	statsStart := time.Now()
	statsCtx, cancelStats := context.WithTimeout(runCtx, 30*time.Second)
	stats, err := provider.SystemStats(statsCtx)
	cancelStats()
	if err != nil {
		addComfyAcceptCheck(report, "comfyui:system_stats", externalStatusFail,
			"ComfyUI server is unavailable or did not return /system_stats: "+err.Error(), statsStart)
		addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusNotRun,
			"actual model inference was not run because the server preflight failed", time.Time{})
		return 1
	}
	report.Server = comfyAcceptServerFromStats(stats)
	addComfyAcceptCheck(report, "comfyui:system_stats", externalStatusPass,
		"ComfyUI /system_stats responded with server and resource metadata", statsStart)

	request := video.VideoRequest{
		ProjectID:       "comfyui-acceptance",
		Prompt:          prompt,
		Mode:            video.ModeTextToVideo,
		DurationSeconds: duration,
		FPS:             fps,
		OutputFormat:    "mp4",
	}
	if width > 0 && height > 0 {
		request.AspectRatio = video.AspectCustom
		request.Width, request.Height = width, height
	} else {
		request.AspectRatio = video.Aspect16x9
	}
	if err := provider.ValidateRequest(request); err != nil {
		addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusFail,
			"minimal acceptance request is invalid for this workflow: "+err.Error(), time.Time{})
		return 1
	}

	if err := os.MkdirAll(assetDir, 0o700); err != nil {
		addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusFail,
			"cannot create acceptance asset root: "+err.Error(), time.Time{})
		return 1
	}
	runDir, err := os.MkdirTemp(assetDir, "run-")
	if err != nil {
		addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusFail,
			"cannot create acceptance asset workspace: "+err.Error(), time.Time{})
		return 1
	}
	report.Target.AssetDirectory = runDir
	sink, err := storage.NewLocal(runDir, maxOutput)
	if err != nil {
		_ = os.RemoveAll(runDir)
		addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusFail,
			"cannot initialize acceptance asset store: "+err.Error(), time.Time{})
		return 1
	}

	submitStart := time.Now()
	job, err := provider.CreateJob(runCtx, request)
	if err != nil {
		addComfyAcceptCheck(report, "comfyui:submit", externalStatusFail,
			"workflow submission failed: "+err.Error(), submitStart)
		comfyAcceptFailClosed(report, runDir, nil)
		return 1
	}
	report.InferenceExecuted = true
	report.Job = &comfyAcceptJob{PromptID: job.ProviderJobID}
	addComfyAcceptCheck(report, "comfyui:submit", externalStatusPass,
		"ComfyUI accepted the workflow and returned a prompt ID", submitStart)

	pollStart := time.Now()
	status, polls, pollErr := pollComfyAcceptJob(runCtx, provider, job.ProviderJobID, pollInterval)
	report.Job.Polls = polls
	report.Job.LatencyMS = time.Since(pollStart).Milliseconds()
	if pollErr != nil {
		if status.ProviderJobID != "" {
			report.Job.State = string(status.State)
			report.Job.Progress = status.Progress
		}
		reason := "job did not complete: " + pollErr.Error()
		if errors.Is(pollErr, context.DeadlineExceeded) {
			reason = "job timed out before completion: " + runCtx.Err().Error()
		}
		addComfyAcceptCheck(report, "comfyui:poll", externalStatusFail, reason, pollStart)
		if strings.EqualFold(string(status.State), string(video.StateFailed)) && status.Error != "" {
			addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusFail,
				"ComfyUI reported workflow failure: "+status.Error, time.Time{})
		} else {
			addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusFail,
				"actual model inference did not complete: "+reason, time.Time{})
		}
		comfyAcceptFailClosed(report, runDir, provider)
		return 1
	}
	report.Job.State = string(status.State)
	report.Job.Progress = status.Progress
	addComfyAcceptCheck(report, "comfyui:poll", externalStatusPass,
		"ComfyUI reported a completed workflow", pollStart)
	if len(status.Outputs) == 0 {
		addComfyAcceptCheck(report, "comfyui:download", externalStatusFail,
			"completed workflow returned no video outputs", time.Time{})
		addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusFail,
			"workflow completed but produced no MP4 video output", time.Time{})
		comfyAcceptFailClosed(report, runDir, provider)
		return 1
	}
	for i := range status.Outputs {
		meta := make(map[string]string, len(status.Outputs[i].Metadata)+3)
		for k, v := range status.Outputs[i].Metadata {
			meta[k] = v
		}
		meta["project_id"], meta["job_id"] = request.ProjectID, "comfyui-acceptance-"+time.Now().UTC().Format("20060102T150405Z")
		status.Outputs[i].Metadata = meta
	}

	downloadStart := time.Now()
	assets, err := provider.DownloadOutputs(runCtx, status, sink)
	if err != nil {
		addComfyAcceptCheck(report, "comfyui:download", externalStatusFail,
			"output download or persistence failed: "+err.Error(), downloadStart)
		addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusFail,
			"workflow completed but its output could not be downloaded or persisted as a valid asset", time.Time{})
		comfyAcceptFailClosed(report, runDir, provider)
		return 1
	}
	if len(assets) == 0 {
		addComfyAcceptCheck(report, "comfyui:download", externalStatusFail,
			"provider returned no persisted output assets", downloadStart)
		addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusFail,
			"workflow completed but no persisted video asset was produced", time.Time{})
		comfyAcceptFailClosed(report, runDir, provider)
		return 1
	}
	addComfyAcceptCheck(report, "comfyui:download", externalStatusPass,
		fmt.Sprintf("downloaded and persisted %d video asset(s) through the local asset store", len(assets)), downloadStart)

	probe := &render.Renderer{FFprobe: ffprobeResolved}
	verifyStart := time.Now()
	for _, asset := range assets {
		verified, verifyErr := verifyComfyAcceptAsset(runCtx, sink, probe, asset)
		if verifyErr != nil {
			addComfyAcceptCheck(report, "comfyui:asset_integrity", externalStatusFail, verifyErr.Error(), verifyStart)
			addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusFail,
				"workflow output failed read-back integrity or playability verification", time.Time{})
			report.Assets = append(report.Assets, verified)
			comfyAcceptFailClosed(report, runDir, provider)
			return 1
		}
		report.Assets = append(report.Assets, verified)
	}
	addComfyAcceptCheck(report, "comfyui:asset_integrity", externalStatusPass,
		"read back every persisted asset, verified non-zero size, video metadata, and matching SHA-256", verifyStart)
	addComfyAcceptCheck(report, "comfyui:playability", externalStatusPass,
		"ffprobe decoded at least one video stream from every persisted asset", verifyStart)
	addComfyAcceptCheck(report, "comfyui:model_inference", externalStatusPass,
		"actual configured ComfyUI workflow inference completed and produced a verified MP4 asset", time.Time{})
	report.Cleanup = &comfyAcceptCleanup{
		RemoteAttempted: false, RemoteStatus: externalStatusNotRun,
		RemoteReason: "workflow completed; no remote queue entry remained",
		LocalStatus:  externalStatusPass,
		LocalReason:  "verified acceptance asset retained for operator inspection; temporary files were promoted atomically",
	}
	return 0
}

func newComfyAcceptReport(cfg config.Config, assetDir, ffprobe string, timeout, pollInterval time.Duration) comfyAcceptReport {
	base := ""
	if cfg.Video.ComfyUIEnabled {
		base = cfg.Video.ComfyUIBaseURL
	}
	return comfyAcceptReport{
		Schema:        comfyAcceptSchema,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Mode:          "live",
		EvidenceClass: acceptanceEvidenceClass(base),
		Bounds: comfyAcceptBounds{
			JobTimeoutSeconds:  timeout.Seconds(),
			PollIntervalMillis: pollInterval.Milliseconds(),
		},
		Target: comfyAcceptTarget{
			AssetDirectory: assetDir,
			FFprobe:        ffprobe,
		},
		Checks: []comfyAcceptCheck{},
		Limitations: []string{
			"Actual model inference is NOT_RUN/BLOCKED unless a provisioned ComfyUI server with a real compatible text-to-video workflow is supplied by the operator.",
			"A fixture or loopback stub can prove this harness and the adapter contract, but its output is not real model inference and must not be reported as such.",
			"Playability means ffprobe found a decodable video stream; human visual acceptance is still required for content quality.",
		},
	}
}

func comfyAcceptVideoConfig(cfg config.VideoConfig, configPath string) video.Config {
	workflowPath := strings.TrimSpace(cfg.ComfyUIWorkflowPath)
	if workflowPath != "" && !filepath.IsAbs(workflowPath) {
		workflowPath = filepath.Join(filepath.Dir(configPath), workflowPath)
	}
	return video.Config{
		Enabled:                   cfg.Enabled,
		ComfyUIEnabled:            cfg.ComfyUIEnabled,
		ComfyUIBaseURL:            cfg.ComfyUIBaseURL,
		ComfyUIWorkflowPath:       workflowPath,
		ComfyUIModel:              cfg.ComfyUIModel,
		ComfyUIMaxDurationSeconds: cfg.ComfyUIMaxDurationSeconds,
		ComfyUIPromptNodeID:       cfg.ComfyUIPromptNodeID,
		ComfyUIPromptInput:        cfg.ComfyUIPromptInput,
		ComfyUINegativeNodeID:     cfg.ComfyUINegativeNodeID,
		ComfyUINegativeInput:      cfg.ComfyUINegativeInput,
		ComfyUIWidthNodeID:        cfg.ComfyUIWidthNodeID,
		ComfyUIWidthInput:         cfg.ComfyUIWidthInput,
		ComfyUIHeightNodeID:       cfg.ComfyUIHeightNodeID,
		ComfyUIHeightInput:        cfg.ComfyUIHeightInput,
		ComfyUIFramesNodeID:       cfg.ComfyUIFramesNodeID,
		ComfyUIFramesInput:        cfg.ComfyUIFramesInput,
		ComfyUIFPSNodeID:          cfg.ComfyUIFPSNodeID,
		ComfyUIFPSInput:           cfg.ComfyUIFPSInput,
		ComfyUISeedNodeID:         cfg.ComfyUISeedNodeID,
		ComfyUISeedInput:          cfg.ComfyUISeedInput,
	}
}

func pollComfyAcceptJob(ctx context.Context, provider *providers.ComfyUI, id string, interval time.Duration) (video.ProviderJobStatus, int, error) {
	var last video.ProviderJobStatus
	var lastErr error
	polls := 0
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return last, polls, fmt.Errorf("%w (last status error: %v)", err, lastErr)
			}
			return last, polls, err
		}
		polls++
		status, err := provider.GetJob(ctx, id)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return last, polls, fmt.Errorf("%w (last status error: %v)", ctx.Err(), lastErr)
			}
		} else {
			last = status
			lastErr = nil
			switch status.State {
			case video.StateCompleted:
				return status, polls, nil
			case video.StateFailed, video.StateCancelled, video.StateExpired:
				reason := status.Error
				if reason == "" {
					reason = "provider reported " + string(status.State)
				}
				return status, polls, errors.New(reason)
			}
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if lastErr != nil {
				return last, polls, fmt.Errorf("%w (last status error: %v)", ctx.Err(), lastErr)
			}
			return last, polls, ctx.Err()
		case <-timer.C:
		}
	}
}

func verifyComfyAcceptAsset(ctx context.Context, sink *storage.Local, probe *render.Renderer, asset video.Asset) (comfyAcceptAsset, error) {
	verified := comfyAcceptAsset{
		ID: asset.ID, Filename: asset.Filename, ContentType: asset.ContentType,
		URI: asset.URI, SizeBytes: asset.SizeBytes, SHA256: asset.SHA256,
		Metadata: asset.Metadata,
	}
	if asset.SizeBytes <= 0 {
		return verified, errors.New("persisted asset has a non-positive size")
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(asset.ContentType)), "video/") {
		return verified, fmt.Errorf("persisted asset has non-video content type %q", asset.ContentType)
	}
	if len(asset.SHA256) != 64 {
		return verified, errors.New("persisted asset has no valid SHA-256")
	}
	if _, err := hex.DecodeString(asset.SHA256); err != nil {
		return verified, errors.New("persisted asset has a malformed SHA-256")
	}
	raw, err := sink.Read(asset)
	if err != nil {
		return verified, fmt.Errorf("read back persisted asset: %w", err)
	}
	if int64(len(raw)) != asset.SizeBytes {
		return verified, fmt.Errorf("asset size mismatch: record %d bytes, read %d bytes", asset.SizeBytes, len(raw))
	}
	if sum := comfyAcceptSHA256(raw); !strings.EqualFold(sum, asset.SHA256) {
		return verified, errors.New("asset SHA-256 does not match the bytes read back from disk")
	}
	if !strings.EqualFold(strings.TrimSpace(asset.Metadata["provider"]), "comfyui") {
		return verified, errors.New("asset metadata is missing provider=comfyui")
	}
	if strings.TrimSpace(asset.Metadata["node_id"]) == "" {
		return verified, errors.New("asset metadata is missing the ComfyUI node_id")
	}
	if strings.TrimSpace(asset.Metadata["filename"]) == "" {
		return verified, errors.New("asset metadata is missing the ComfyUI filename")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info, err := probe.Probe(probeCtx, asset.URI)
	if err != nil {
		return verified, fmt.Errorf("verify playable media: %w", err)
	}
	if info.Codec == "" || info.Size <= 0 {
		return verified, errors.New("ffprobe returned no decodable video stream")
	}
	verified.Probe = &comfyAcceptAssetProbe{
		Codec: info.Codec, Width: info.Width, Height: info.Height, Seconds: info.Seconds, Size: info.Size,
	}
	return verified, nil
}

func comfyAcceptFailClosed(report *comfyAcceptReport, runDir string, provider *providers.ComfyUI) {
	cleanup := &comfyAcceptCleanup{RemoteStatus: externalStatusNotRun}
	if provider != nil && report.Job != nil && strings.TrimSpace(report.Job.PromptID) != "" {
		cleanup.RemoteAttempted = true
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := provider.CancelJob(cleanupCtx, report.Job.PromptID)
		cancel()
		switch {
		case err == nil:
			cleanup.RemoteStatus = externalStatusPass
			cleanup.RemoteReason = "pending ComfyUI prompt was removed from the queue"
		case strings.Contains(err.Error(), "not pending"):
			cleanup.RemoteStatus = externalStatusBlocked
			cleanup.RemoteReason = "prompt was no longer pending; it may be running or already finished, so no global interrupt was attempted"
		case strings.Contains(err.Error(), "refusing global interrupt"):
			cleanup.RemoteStatus = externalStatusBlocked
			cleanup.RemoteReason = "prompt is running and ComfyUI cannot cancel one prompt without interrupting unrelated work"
		default:
			cleanup.RemoteStatus = externalStatusFail
			cleanup.RemoteReason = err.Error()
		}
	} else {
		cleanup.RemoteReason = "no ComfyUI prompt ID was returned; there is no remote entry to target for cleanup"
	}
	if runDir != "" {
		if err := os.RemoveAll(runDir); err != nil {
			cleanup.LocalStatus = externalStatusFail
			cleanup.LocalReason = "remove partial acceptance workspace: " + err.Error()
		} else {
			cleanup.LocalStatus = externalStatusPass
			cleanup.LocalReason = "partial acceptance workspace removed; no unverified output is retained"
		}
	} else {
		cleanup.LocalStatus = externalStatusNotRun
	}
	report.Cleanup = cleanup
}

func addComfyAcceptCheck(report *comfyAcceptReport, id, status, reason string, start time.Time) {
	check := comfyAcceptCheck{ID: id, Status: status, Reason: reason}
	if !start.IsZero() {
		check.DurationMS = time.Since(start).Milliseconds()
	}
	report.Checks = append(report.Checks, check)
}

func comfyAcceptServerFromStats(stats providers.ComfyUISystemStats) *comfyAcceptServer {
	server := &comfyAcceptServer{
		ComfyUIVersion: stats.System.ComfyUIVersion,
		PythonVersion:  stats.System.PythonVersion,
		PyTorchVersion: stats.System.PyTorchVersion,
		OS:             stats.System.OS,
		RAMTotalBytes:  stats.System.RAMTotal,
		RAMFreeBytes:   stats.System.RAMFree,
	}
	for _, device := range stats.Devices {
		server.Devices = append(server.Devices, comfyAcceptDevice{
			Name: device.Name, Type: device.Type, Index: device.Index,
			VRAMTotalBytes: device.VRAMTotal, VRAMFreeBytes: device.VRAMFree,
		})
	}
	return server
}

func comfyAcceptSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func finalizeComfyAcceptReport(report *comfyAcceptReport) {
	statuses := make([]string, 0, len(report.Checks))
	for _, check := range report.Checks {
		statuses = append(statuses, check.Status)
	}
	report.Outcome = externalOutcome(statuses)
	if report.EvidenceClass == "loopback" {
		report.Limitations = append(report.Limitations,
			"The configured target is a loopback endpoint. A PASS is real model inference only if that endpoint is a real provisioned ComfyUI instance running a compatible text-to-video workflow; loopback fixture tests are harness evidence only.")
	}
	if report.Outcome == externalStatusBlocked {
		report.Limitations = append(report.Limitations,
			"Actual ComfyUI model inference was BLOCKED by missing provisioning, configuration, or verification tooling.")
	}
	if report.Outcome == externalStatusNotRun || !report.InferenceExecuted {
		report.Limitations = append(report.Limitations,
			"Actual ComfyUI model inference was NOT_RUN. No fixture output in this report may be described as real video inference.")
	}
}

func writeComfyAcceptReport(path string, report comfyAcceptReport) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".comfyui-accept-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func printComfyAcceptSummary(w io.Writer, report comfyAcceptReport, outPath string) {
	fmt.Fprintf(w, "ComfyUI acceptance: outcome=%s inference_executed=%t evidence=%s\n", report.Outcome, report.InferenceExecuted, report.EvidenceClass)
	if report.Server != nil {
		fmt.Fprintf(w, "  server: version=%q python=%q pytorch=%q devices=%d\n",
			report.Server.ComfyUIVersion, report.Server.PythonVersion, report.Server.PyTorchVersion, len(report.Server.Devices))
	}
	if report.Job != nil {
		fmt.Fprintf(w, "  job: prompt=%s state=%s polls=%d\n", report.Job.PromptID, report.Job.State, report.Job.Polls)
	}
	for _, check := range report.Checks {
		fmt.Fprintf(w, "  %-7s %s", check.Status, check.ID)
		if check.Reason != "" {
			fmt.Fprintf(w, " — %s", check.Reason)
		}
		fmt.Fprintln(w)
	}
	if report.Cleanup != nil {
		fmt.Fprintf(w, "  cleanup: remote=%s local=%s\n", report.Cleanup.RemoteStatus, report.Cleanup.LocalStatus)
	}
	fmt.Fprintf(w, "report: %s\n", outPath)
}
