package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/agent"
	"github.com/ali-shortcuts/nexaroute/internal/video/composer"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
	"github.com/ali-shortcuts/nexaroute/internal/video/tts"
)

// CodeRenderID is the provider ID of the Universal AI-to-Video engine.
const CodeRenderID = "code-render"

// CodeRender is the Tier-A "code-render" video provider (issue #461): a
// configured text/coding model authors a validated declarative plan and the
// deterministic local renderer turns it into a real MP4. The provider is
// honest about its capabilities: it produces programmatic animation,
// explainer, data-viz, and kinetic-type videos. It is NOT a native
// generative (photoreal) video model and never claims to be one.
type CodeRender struct {
	Agent    *agent.Agent // may be nil when only explicit plans are accepted
	Renderer *render.Renderer
	Narrator Narrator // optional Piper-backed narration; nil = no narration
	WorkRoot string   // per-job working directories live under here
	Models   []string // selectable planning models (gateway-configured models/aliases)
	// HandlesNarrationFlag is kept unexported; see HandlesNarration.
	MaxDurationSeconds float64
	MaxAssetBytes      int64

	mu   sync.Mutex
	jobs map[string]*codeRenderJob
	seq  int
}

// Narrator synthesizes narration audio to a local WAV path (Piper).
type Narrator interface {
	Synthesize(context.Context, string, string) error
}

type codeRenderJob struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	state    video.JobState
	progress float64
	err      string
	output   string // final MP4 path, set only after QC passes
	plan     video.Plan
	usage    map[string]string
}

// NewCodeRender builds the provider. workRoot is created lazily per job.
func NewCodeRender(a *agent.Agent, r *render.Renderer, narrator Narrator, workRoot string, models []string, maxDuration float64) *CodeRender {
	if maxDuration <= 0 {
		maxDuration = 120
	}
	return &CodeRender{
		Agent: a, Renderer: r, Narrator: narrator, WorkRoot: workRoot,
		Models: append([]string(nil), models...), MaxDurationSeconds: maxDuration,
		jobs: map[string]*codeRenderJob{},
	}
}

func (p *CodeRender) ID() string { return CodeRenderID }

// HandlesNarration reports that this provider performs narration and
// subtitle composition itself (via the injected local Piper narrator), so
// the orchestrator's multi-shot narration gate does not apply to its jobs.
func (p *CodeRender) HandlesNarration() bool { return p.Narrator != nil }

func (p *CodeRender) Capabilities(context.Context) (video.Capabilities, error) {
	models := append([]string(nil), p.Models...)
	return video.Capabilities{
		Modes:              []video.Mode{video.ModeTextToVideo},
		Models:             models,
		MaxDurationSeconds: p.MaxDurationSeconds,
		Resolutions:        []string{"1280x720", "720x1280", "960x960"},
		AspectRatios:       []video.AspectRatio{video.Aspect16x9, video.Aspect9x16, video.Aspect1x1},
		SupportsAudio:      p.Narrator != nil,
		SupportsWebhooks:   false,
		SupportsCancel:     true,
	}, nil
}

// ValidateRequest enforces the honest capability boundary. Unsupported
// inputs are rejected with a reason, never silently ignored or downgraded.
func (p *CodeRender) ValidateRequest(r video.VideoRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	switch r.CreationMode {
	case "", "auto", "programmatic", "hybrid":
	case "native":
		return errors.New("creation_mode \"native\" requires a native generative video provider (for example the local ComfyUI adapter); code-render creates programmatic video planned by a text/coding model")
	default:
		return fmt.Errorf("unsupported creation_mode %q", r.CreationMode)
	}
	if r.Mode != video.ModeTextToVideo {
		return fmt.Errorf("code-render supports text_to_video only, not %q", r.Mode)
	}
	if len(r.ShotPlan) > 0 {
		return errors.New("code-render does not accept shot_plan; supply a natural-language prompt or a video_plan (schema video-plan.v1)")
	}
	if len(r.ReferenceAssets) > 0 || r.FirstFrameAsset != nil || r.LastFrameAsset != nil ||
		len(r.CharacterReferences) > 0 || len(r.LocationReferences) > 0 || len(r.StyleReferences) > 0 {
		return errors.New("code-render does not accept reference/first/last-frame assets in the baseline implementation")
	}
	if r.DialogueRequested {
		return errors.New("code-render does not generate dialogue; narration is supported when a local Piper narrator is configured")
	}
	if r.AudioRequested && p.Narrator == nil {
		return errors.New("code-render audio is narration-only and requires a configured local Piper narrator (video.piper_enabled)")
	}
	if strings.TrimSpace(r.NarrationText) != "" && p.Narrator == nil {
		return errors.New("narration_text requires a configured local Piper narrator (video.piper_enabled) for code-render jobs")
	}
	if r.CallbackURL != "" {
		return errors.New("code-render does not support provider callbacks/webhooks")
	}
	if r.VideoPlan != nil {
		if err := r.VideoPlan.Validate(p.MaxDurationSeconds); err != nil {
			return fmt.Errorf("video_plan is invalid: %w", err)
		}
	}
	return nil
}

// EstimateCost reports a zero provider charge honestly: rendering is local
// and the planning model's token usage is metered by the gateway's normal
// request accounting when the planner call passes through it, not by the
// video ledger. Local rendering still consumes compute/electricity.
func (p *CodeRender) EstimateCost(r video.VideoRequest) (video.CostEstimate, error) {
	if err := p.ValidateRequest(r); err != nil {
		return video.CostEstimate{}, err
	}
	return video.CostEstimate{
		Provider: p.ID(), Model: r.ModelPreference, Currency: "USD",
		EstimatedUSD: 0, UpperBoundUSD: 0, PriceKnown: true, BillableSeconds: r.DurationSeconds,
	}, nil
}

// CreateJob resolves the plan (explicit or via the planning agent) and
// starts asynchronous rendering. The returned provider job ID is durable
// only for this process; orchestrator restart recovery treats a missing
// in-memory job as unknown rather than resubmitting billable work — the
// planning call is the only billable step and it happens before CreateJob
// returns, so a restart never silently repeats it.
func (p *CodeRender) CreateJob(ctx context.Context, r video.VideoRequest) (video.ProviderJob, error) {
	if err := p.ValidateRequest(r); err != nil {
		return video.ProviderJob{}, err
	}
	if p.Renderer == nil {
		return video.ProviderJob{}, errors.New("code-render renderer is not configured")
	}
	if err := p.Renderer.Preflight(ctx); err != nil {
		return video.ProviderJob{}, fmt.Errorf("code-render preflight: %w", err)
	}
	plan, attempts, err := p.resolvePlan(ctx, r)
	if err != nil {
		return video.ProviderJob{}, err
	}
	p.mu.Lock()
	p.seq++
	id := fmt.Sprintf("coderender-%d", p.seq)
	p.mu.Unlock()
	jobCtx, cancel := context.WithCancel(context.Background())
	job := &codeRenderJob{
		ctx: jobCtx, cancel: cancel, done: make(chan struct{}),
		state: video.StateProcessing, plan: plan,
		usage: map[string]string{
			"creation_mode":   modeOrDefault(r.CreationMode),
			"planning_model":  r.ModelPreference,
			"plan_schema":     video.PlanSchemaVersion,
			"plan_attempts":   fmt.Sprint(attempts),
			"scenes":          fmt.Sprint(len(plan.Scenes)),
			"renderer":        "nexaroute-tier-a-ffmpeg",
			"tools_used":      "llm-planner,ffmpeg-renderer,ffprobe-qc",
			"native_video":    "false",
			"programmatic":    "true",
			"planner_billing": "metered by the gateway data plane, not the video ledger",
		},
	}
	p.mu.Lock()
	p.jobs[id] = job
	p.mu.Unlock()
	go p.run(jobCtx, job, r)
	return video.ProviderJob{ProviderJobID: id, Provider: p.ID(), Model: r.ModelPreference, Accepted: true}, nil
}

func modeOrDefault(m string) string {
	if strings.TrimSpace(m) == "" {
		return "auto"
	}
	return m
}

// resolvePlan returns the explicit plan when supplied (validated again) or
// asks the Universal Video Agent to author one from the prompt.
func (p *CodeRender) resolvePlan(ctx context.Context, r video.VideoRequest) (video.Plan, int, error) {
	if r.VideoPlan != nil {
		return r.VideoPlan.Normalized(), 0, nil
	}
	if p.Agent == nil {
		return video.Plan{}, 0, errors.New("code-render has no planning model configured and the request supplied no video_plan")
	}
	width, height := requestDimensions(r)
	res, err := p.Agent.GeneratePlan(ctx, agent.Request{
		Brief:           r.Prompt,
		Model:           r.ModelPreference,
		DurationSeconds: r.DurationSeconds,
		Width:           width,
		Height:          height,
		FPS:             r.FPS,
		AspectRatio:     string(r.AspectRatio),
		Language:        planLanguage(r),
		MaxDuration:     p.MaxDurationSeconds,
	})
	if err != nil {
		return video.Plan{}, 0, err
	}
	return res.Plan, res.Attempts, nil
}

func planLanguage(r video.VideoRequest) string {
	if strings.TrimSpace(r.SubtitleLanguage) != "" {
		return r.SubtitleLanguage
	}
	return ""
}

// requestDimensions resolves the request's width/height from explicit
// fields, its resolution string, or its aspect ratio.
func requestDimensions(r video.VideoRequest) (int, int) {
	if r.Width > 0 && r.Height > 0 {
		return r.Width, r.Height
	}
	switch r.AspectRatio {
	case video.Aspect9x16:
		return 720, 1280
	case video.Aspect1x1:
		return 960, 960
	default:
		return 1280, 720
	}
}

func (p *CodeRender) run(ctx context.Context, job *codeRenderJob, r video.VideoRequest) {
	defer close(job.done)
	defer job.cancel()
	setErr := func(format string, args ...any) {
		job.mu.Lock()
		job.state = video.StateFailed
		job.err = fmt.Sprintf(format, args...)
		job.mu.Unlock()
	}
	if strings.TrimSpace(p.WorkRoot) == "" {
		setErr("code-render work root is not configured")
		return
	}
	dir, err := os.MkdirTemp(p.WorkRoot, "job-")
	if err != nil {
		// WorkRoot itself may not exist yet.
		if mkErr := os.MkdirAll(p.WorkRoot, 0o700); mkErr != nil {
			setErr("create code-render work root: %v", mkErr)
			return
		}
		dir, err = os.MkdirTemp(p.WorkRoot, "job-")
		if err != nil {
			setErr("create code-render job directory: %v", err)
			return
		}
	}
	plan := job.plan
	// Persist the exact plan that is executed, for audit/reproducibility.
	if data, mErr := json.MarshalIndent(plan, "", "  "); mErr == nil {
		_ = os.WriteFile(filepath.Join(dir, "plan.json"), data, 0o600)
	}
	job.mu.Lock()
	job.progress = 0.2
	job.mu.Unlock()
	result, err := p.Renderer.RenderPlan(ctx, plan, dir)
	if err != nil {
		if ctx.Err() != nil {
			job.mu.Lock()
			job.state = video.StateCancelled
			job.mu.Unlock()
			return
		}
		setErr("render plan: %v", err)
		return
	}
	job.mu.Lock()
	job.progress = 0.7
	job.mu.Unlock()

	finalPath := result.OutputPath
	narration := strings.TrimSpace(r.NarrationText)
	if narration == "" {
		narration = plan.NarrationScript()
	}
	wantSubtitles := r.SubtitleLanguage != "" || plan.Subtitles
	if narration != "" || wantSubtitles {
		if p.Narrator == nil {
			setErr("plan or request requires narration/subtitles but no local Piper narrator is configured")
			return
		}
		if narration == "" {
			setErr("subtitles were requested but the plan contains no narration or caption text")
			return
		}
		wavPath := filepath.Join(dir, "narration.wav")
		if err := p.Narrator.Synthesize(ctx, narration, wavPath); err != nil {
			setErr("synthesize narration: %v", err)
			return
		}
		dur, err := tts.WAVDuration(wavPath)
		if err != nil {
			setErr("validate narration audio: %v", err)
			return
		}
		if total := plan.TotalDuration(); dur > time.Duration(total*float64(time.Second))+2*time.Second {
			setErr("narration audio (%.1fs) exceeds the video duration (%.1fs); shorten the narration or lengthen the scenes", dur.Seconds(), total)
			return
		}
		spec := composer.EpisodeSpec{
			Shots: []string{result.OutputPath}, OutputPath: filepath.Join(dir, "final.mp4"),
			Width: plan.Width, Height: plan.Height, FPS: plan.FPS, NarrationPath: wavPath,
		}
		if wantSubtitles {
			srt := tts.BuildSRT(firstNonEmpty(plan.CaptionText(), narration), time.Duration(plan.TotalDuration()*float64(time.Second)))
			if len(srt) == 0 {
				setErr("could not build subtitles from the narration/caption text")
				return
			}
			srtPath := filepath.Join(dir, "subtitles.srt")
			if err := os.WriteFile(srtPath, srt, 0o600); err != nil {
				setErr("write subtitles: %v", err)
				return
			}
			spec.SubtitlePath = srtPath
		}
		comp := composer.New(ffmpegBinary(), dir)
		if _, err := comp.ComposeEpisode(ctx, spec); err != nil {
			setErr("compose narration/subtitles: %v", err)
			return
		}
		finalPath = spec.OutputPath
		job.mu.Lock()
		job.usage["tools_used"] = job.usage["tools_used"] + ",narration-tts"
		if wantSubtitles {
			job.usage["tools_used"] += ",srt-subtitles"
		}
		job.mu.Unlock()
	}

	// Deterministic QC gate: a job completes only with a real, playable,
	// non-blank MP4 that matches the plan. Failure publishes no asset.
	if err := p.Renderer.ValidateOutput(ctx, plan, finalPath); err != nil {
		setErr("output quality check failed: %v", err)
		return
	}
	if p.MaxAssetBytes > 0 {
		if info, err := os.Stat(finalPath); err != nil || info.Size() > p.MaxAssetBytes {
			setErr("rendered output exceeds the configured asset size limit")
			return
		}
	}
	job.mu.Lock()
	job.output = finalPath
	job.state = video.StateCompleted
	job.progress = 1
	job.mu.Unlock()
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func ffmpegBinary() string {
	if v := strings.TrimSpace(os.Getenv("NEXAROUTE_FFMPEG")); v != "" {
		return v
	}
	return "ffmpeg"
}

func (p *CodeRender) GetJob(_ context.Context, id string) (video.ProviderJobStatus, error) {
	p.mu.Lock()
	job, ok := p.jobs[id]
	p.mu.Unlock()
	if !ok {
		return video.ProviderJobStatus{}, fmt.Errorf("unknown code-render job %q", id)
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	st := video.ProviderJobStatus{
		ProviderJobID: id, State: job.state, Progress: job.progress,
		Error: job.err, Usage: copyUsage(job.usage),
	}
	if job.state == video.StateCompleted {
		st.Outputs = []video.Asset{{ID: id + "-output", Kind: "video", Filename: "episode.mp4", ContentType: "video/mp4"}}
	}
	return st, nil
}

func copyUsage(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (p *CodeRender) CancelJob(_ context.Context, id string) error {
	p.mu.Lock()
	job, ok := p.jobs[id]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown code-render job %q", id)
	}
	job.cancel()
	<-job.done
	job.mu.Lock()
	if job.state != video.StateCompleted {
		job.state = video.StateCancelled
	}
	job.mu.Unlock()
	return nil
}

// DownloadOutputs persists the QC-validated MP4 through the asset sink.
// It is only reachable for completed jobs; the orchestrator additionally
// verifies URI, content type, size, and SHA-256 before completing a job.
func (p *CodeRender) DownloadOutputs(ctx context.Context, status video.ProviderJobStatus, sink video.AssetSink) ([]video.Asset, error) {
	if sink == nil {
		return nil, errors.New("asset sink is not configured")
	}
	p.mu.Lock()
	job, ok := p.jobs[status.ProviderJobID]
	p.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown code-render job %q", status.ProviderJobID)
	}
	job.mu.Lock()
	output := job.output
	job.mu.Unlock()
	if output == "" {
		return nil, errors.New("code-render job has no validated output")
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return nil, fmt.Errorf("read rendered output: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("rendered output is empty")
	}
	if p.MaxAssetBytes > 0 && int64(len(data)) > p.MaxAssetBytes {
		return nil, errors.New("rendered output exceeds the configured asset size limit")
	}
	asset := video.Asset{ID: status.ProviderJobID + "-output", Kind: "video", Filename: "episode.mp4", ContentType: "video/mp4"}
	// The orchestrator stamps project/episode/job metadata onto status
	// outputs before download; the local sink requires it, so carry it
	// through verbatim.
	for _, out := range status.Outputs {
		if out.ID == asset.ID && len(out.Metadata) > 0 {
			asset.Metadata = out.Metadata
		}
	}
	saved, err := sink.Put(ctx, asset, data)
	if err != nil {
		return nil, err
	}
	return []video.Asset{saved}, nil
}

func (p *CodeRender) RegisterWebhook(context.Context, video.WebhookRegistration) error {
	return errors.New("code-render does not support webhooks")
}
func (p *CodeRender) VerifyWebhook(*http.Request) error {
	return errors.New("code-render does not support webhooks")
}
func (p *CodeRender) ParseWebhook(*http.Request) (video.ProviderJobStatus, error) {
	return video.ProviderJobStatus{}, errors.New("code-render does not support webhooks")
}

var _ video.VideoProvider = (*CodeRender)(nil)
