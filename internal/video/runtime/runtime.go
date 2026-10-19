package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/agent"
	"github.com/ali-shortcuts/nexaroute/internal/video/api"
	"github.com/ali-shortcuts/nexaroute/internal/video/composer"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
	"github.com/ali-shortcuts/nexaroute/internal/video/tts"
)

const defaultMaxAssetBytes int64 = 512 << 20

type Runtime struct {
	Handler      *api.Handler
	Queue        *queue.Queue
	Workers      *queue.WorkerPool
	Store        queue.JobStore
	Orchestrator *orchestrator.Orchestrator
	startMu      sync.Mutex
	started      bool
	workerCancel context.CancelFunc
	workerDone   chan struct{}
	jobRetention time.Duration
	pruneDone    chan struct{}
	// codeRender is the registered code-render provider (nil when
	// disabled); codeRenderRetention is its terminal-job/work-dir
	// retention window, pruned by the same hourly sweeper as jobs.
	codeRender          *providers.CodeRender
	codeRenderRetention time.Duration
}

func New(cfg video.Config, baseDir string) (*Runtime, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if strings.TrimSpace(cfg.AuthTokenEnv) == "" {
		return nil, errors.New("video.auth_token_env is required when video.enabled is true")
	}
	token := strings.TrimSpace(os.Getenv(cfg.AuthTokenEnv))
	if token == "" {
		return nil, fmt.Errorf("video authentication token environment variable %q is missing or empty", cfg.AuthTokenEnv)
	}
	if cfg.QueueSize < 1 {
		cfg.QueueSize = 32
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	storePath := cfg.StorePath
	if storePath == "" {
		storePath = filepath.Join(baseDir, "video-jobs.json")
	} else if !filepath.IsAbs(storePath) {
		storePath = filepath.Join(baseDir, storePath)
	}
	store, err := queue.NewFileStore(storePath)
	if err != nil {
		return nil, fmt.Errorf("video job store: %w", err)
	}
	q := queue.New(cfg.QueueSize)
	reg := orchestrator.Registry{}
	if cfg.DevelopmentFakeProvider {
		reg["fake"] = providers.NewFake(2)
	}
	if cfg.ComfyUIEnabled {
		if strings.TrimSpace(cfg.ComfyUIWorkflowPath) != "" && !filepath.IsAbs(cfg.ComfyUIWorkflowPath) {
			cfg.ComfyUIWorkflowPath = filepath.Join(baseDir, cfg.ComfyUIWorkflowPath)
		}
		comfy, err := providers.NewComfyUI(cfg, cfg.MaxAssetBytes)
		if err != nil {
			return nil, fmt.Errorf("initialize local ComfyUI provider: %w", err)
		}
		reg[comfy.ID()] = comfy
	}
	if cfg.AggregatorEnabled {
		agg, err := providers.NewAggregator(cfg, cfg.MaxAssetBytes)
		if err != nil {
			return nil, fmt.Errorf("initialize aggregator provider: %w", err)
		}
		reg[agg.ID()] = agg
	}
	storageRoot := cfg.StorageRoot
	if storageRoot == "" {
		storageRoot = filepath.Join(baseDir, "video-assets")
	} else if !filepath.IsAbs(storageRoot) {
		storageRoot = filepath.Join(baseDir, storageRoot)
	}
	maxBytes := cfg.MaxAssetBytes
	if maxBytes == 0 {
		maxBytes = defaultMaxAssetBytes
	}
	if maxBytes < 1 {
		return nil, errors.New("video.max_asset_bytes must be greater than zero")
	}
	assetSink, err := storage.NewLocal(storageRoot, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("video asset store: %w", err)
	}
	var narrator orchestrator.Narrator
	if cfg.PiperEnabled {
		modelPath := strings.TrimSpace(cfg.PiperModelPath)
		if modelPath == "" {
			return nil, errors.New("video.piper_model_path is required when video.piper_enabled is true")
		}
		if !filepath.IsAbs(modelPath) {
			modelPath = filepath.Join(baseDir, modelPath)
		}
		binary := strings.TrimSpace(cfg.PiperBinary)
		if binary == "" {
			binary = "piper"
		}
		piper, err := tts.NewPiper(binary, modelPath)
		if err != nil {
			return nil, fmt.Errorf("initialize local Piper narrator: %w", err)
		}
		narrator = piper
	}
	// Universal AI-to-Video (code-render) provider registration. The
	// planner talks to an OpenAI-compatible chat-completions endpoint —
	// normally this gateway's own data plane — so planning reuses the
	// configured providers, routing, and credentials.
	var universalAgent *agent.Agent
	var codeRenderMaxDuration float64
	var codeRender *providers.CodeRender
	var codeRenderRetention time.Duration
	if cfg.CodeRenderEnabled {
		planner, err := agent.NewOpenAIPlanner(cfg.CodeRenderPlannerBaseURL, cfg.CodeRenderPlannerAPIKeyEnv, cfg.CodeRenderPlannerJSONMode)
		if err != nil {
			return nil, fmt.Errorf("initialize code-render planner: %w", err)
		}
		universalAgent = &agent.Agent{Planner: planner, MaxAttempts: 3}
		models := append([]string(nil), cfg.CodeRenderPlannerModels...)
		if m := strings.TrimSpace(cfg.CodeRenderPlannerModel); m != "" {
			found := false
			for _, x := range models {
				if x == m {
					found = true
				}
			}
			if !found {
				models = append([]string{m}, models...)
			}
		}
		if len(models) == 0 {
			return nil, errors.New("video.code_render_enabled requires at least one planning model (video.code_render_planner_model or video.code_render_planner_models)")
		}
		codeRenderMaxDuration = cfg.CodeRenderMaxDurationSeconds
		if codeRenderMaxDuration <= 0 {
			codeRenderMaxDuration = 120
		}
		workRoot := cfg.CodeRenderWorkRoot
		if workRoot == "" {
			workRoot = filepath.Join(storageRoot, "code-render-work")
		} else if !filepath.IsAbs(workRoot) {
			workRoot = filepath.Join(baseDir, workRoot)
		}
		if err := os.MkdirAll(workRoot, 0o700); err != nil {
			return nil, fmt.Errorf("create code-render work root: %w", err)
		}
		ffmpegBinary := strings.TrimSpace(os.Getenv("NEXAROUTE_FFMPEG"))
		if ffmpegBinary == "" {
			ffmpegBinary = "ffmpeg"
		}
		cr := providers.NewCodeRender(universalAgent, &render.Renderer{FFmpeg: ffmpegBinary}, narrator, workRoot, models, codeRenderMaxDuration)
		cr.MaxAssetBytes = maxBytes
		reg[cr.ID()] = cr
		codeRender = cr
		switch {
		case cfg.CodeRenderRetentionHours < 0:
			codeRenderRetention = 0 // keep terminal jobs and work dirs forever
		case cfg.CodeRenderRetentionHours > 0:
			codeRenderRetention = time.Duration(cfg.CodeRenderRetentionHours) * time.Hour
		default:
			codeRenderRetention = providers.DefaultCodeRenderRetention
		}
		cr.Retention = codeRenderRetention
	}
	if len(reg) == 0 {
		return nil, errors.New("video gateway is enabled but no provider is configured; configure the local ComfyUI workflow, enable the code-render provider, or enable development_fake_provider only for tests")
	}
	ledger := &cost.Ledger{}
	ffmpegBinary := strings.TrimSpace(os.Getenv("NEXAROUTE_FFMPEG"))
	if ffmpegBinary == "" {
		ffmpegBinary = "ffmpeg"
	}
	orch := &orchestrator.Orchestrator{
		Store: store, Queue: q, Providers: reg, Ledger: ledger, Assets: assetSink,
		Composer: composer.New(ffmpegBinary, ""), Narrator: narrator, MaxAssetBytes: maxBytes,
		InstanceID: instanceID(),
	}
	pool := queue.NewWorkerPool(q, cfg.Workers, orch.RunJob)
	h := &api.Handler{Orch: orch, Store: store, Providers: reg, Assets: assetSink, RequireAuth: true, BearerToken: token, Universal: universalAgent, UniversalMaxDuration: codeRenderMaxDuration}
	return &Runtime{Handler: h, Queue: q, Workers: pool, Store: store, Orchestrator: orch, jobRetention: time.Duration(cfg.JobRetentionDays) * 24 * time.Hour, codeRender: codeRender, codeRenderRetention: codeRenderRetention}, nil
}

// instanceID names this process as a video job lease owner:
// hostname + pid, so an operator reading a job's lease fields
// can tell which replica holds it.
func instanceID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

func (r *Runtime) Start(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.startMu.Lock()
	if r.started {
		r.startMu.Unlock()
		return errors.New("video runtime has already been started")
	}
	r.started = true
	workerCtx, workerCancel := context.WithCancel(ctx)
	r.workerCancel = workerCancel
	workerDone := make(chan struct{})
	r.workerDone = workerDone
	r.startMu.Unlock()
	go func() {
		defer close(workerDone)
		r.Workers.Run(workerCtx)
	}()
	select {
	case <-r.Workers.Ready():
	case <-workerCtx.Done():
		workerCancel()
		<-workerDone
		return workerCtx.Err()
	}
	if err := r.Orchestrator.Recover(workerCtx); err != nil {
		workerCancel()
		r.Queue.Close()
		<-workerDone
		return fmt.Errorf("recover persisted video jobs: %w", err)
	}
	// Retention (#420 item 5; #461 code-render): when configured,
	// prune terminal jobs past the retention window once at startup
	// and then hourly for the life of the runtime, including
	// code-render terminal jobs and their now-unneeded work
	// directories. Best-effort: a prune failure never blocks startup
	// or stops the ticker.
	if r.jobRetention > 0 || r.codeRenderRetention > 0 {
		r.pruneOnce(workerCtx, time.Now())
		pruneDone := make(chan struct{})
		r.pruneDone = pruneDone
		go func() {
			defer close(pruneDone)
			t := time.NewTicker(time.Hour)
			defer t.Stop()
			for {
				select {
				case <-workerCtx.Done():
					return
				case now := <-t.C:
					r.pruneOnce(workerCtx, now)
				}
			}
		}()
	}
	return nil
}

// pruneOnce runs one best-effort retention sweep for terminal video
// jobs and, when the code-render provider is registered, its terminal
// provider jobs and work directories.
func (r *Runtime) pruneOnce(ctx context.Context, now time.Time) {
	if r.jobRetention > 0 {
		_, _ = r.Orchestrator.PruneTerminalJobs(ctx, now, r.jobRetention)
	}
	if r.codeRender != nil && r.codeRenderRetention > 0 {
		r.codeRender.PruneTerminal(now)
	}
}

func (r *Runtime) Close() {
	if r == nil {
		return
	}
	r.startMu.Lock()
	cancel := r.workerCancel
	done := r.workerDone
	pruneDone := r.pruneDone
	r.startMu.Unlock()
	// Cancel the worker context before closing the queue so buffered queued jobs
	// cannot start new provider submissions during shutdown.
	if cancel != nil {
		cancel()
	}
	if r.Queue != nil {
		r.Queue.Close()
	}
	if done != nil {
		<-done
	}
	if pruneDone != nil {
		<-pruneDone
	}
}
