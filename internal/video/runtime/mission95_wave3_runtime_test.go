package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// Wave-3 runtime coverage: postgres refusal for both store and
// ledger, New propagation and provider branches (ComfyUI relative
// workflow, aggregator success/failure, asset-store failure, Piper
// default binary, code-render models/duration/work-root/retention),
// Start nil/double/recover-failure, pruneOnce retention, and Close
// nil/ledger paths. Postgres success, the New ledger-error branch
// (unreachable without a live database: the store pings first on
// the same DSN), the hostname-error branch and the hourly ticker
// body are deliberately not faked.

func wave3BaseConfig(t *testing.T) (video.Config, string) {
	t.Helper()
	baseDir := t.TempDir()
	t.Setenv("WAVE3_VIDEO_TOKEN", "test-token")
	return video.Config{Enabled: true, AuthTokenEnv: "WAVE3_VIDEO_TOKEN"}, baseDir
}

func wave3RefusedPostgresConfig(t *testing.T) (video.Config, string) {
	t.Helper()
	cfg, baseDir := wave3BaseConfig(t)
	cfg.JobStoreBackend = "postgres"
	cfg.JobStoreURL = "postgres://user:pass@127.0.0.1:1/db?sslmode=disable"
	return cfg, baseDir
}

func TestWave3OpenJobStorePostgresRefused(t *testing.T) {
	cfg, baseDir := wave3RefusedPostgresConfig(t)
	if _, err := openJobStore(cfg, baseDir); err == nil || !strings.Contains(err.Error(), "postgres is unreachable") {
		t.Fatalf("refused postgres store error = %v", err)
	}
}

func TestWave3OpenCostLedgerPostgresRefused(t *testing.T) {
	cfg, _ := wave3RefusedPostgresConfig(t)
	if _, _, err := openCostLedger(cfg); err == nil || !strings.Contains(err.Error(), "video cost ledger: postgres is unreachable") {
		t.Fatalf("refused postgres ledger error = %v", err)
	}
}

func TestWave3NewJobStorePathUnderFile(t *testing.T) {
	cfg, baseDir := wave3BaseConfig(t)
	blocker := filepath.Join(baseDir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.StorePath = blocker
	cfg.DevelopmentFakeProvider = true
	if _, err := New(cfg, baseDir); err == nil || !strings.Contains(err.Error(), "video job store:") {
		t.Fatalf("store under a file error = %v", err)
	}
}

func TestWave3NewComfyUIRelativeWorkflowJoin(t *testing.T) {
	cfg, baseDir := wave3BaseConfig(t)
	workflow := `{"1": {"class_type": "CLIPTextEncode", "inputs": {"text": "prompt"}},
		"2": {"class_type": "EmptyImage", "inputs": {"width": 512, "height": 512}},
		"3": {"class_type": "VideoSampler", "inputs": {"length": 16}}}`
	if err := os.WriteFile(filepath.Join(baseDir, "workflow.json"), []byte(workflow), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.ComfyUIEnabled = true
	cfg.ComfyUIBaseURL = "http://127.0.0.1:9"
	cfg.ComfyUIWorkflowPath = "workflow.json"
	cfg.ComfyUIPromptNodeID = "1"
	cfg.ComfyUIWidthNodeID = "2"
	cfg.ComfyUIHeightNodeID = "2"
	cfg.ComfyUIFramesNodeID = "3"
	// Success proves the relative workflow path was joined under
	// baseDir: a bare relative read from the test working directory
	// would not find the file.
	rt, err := New(cfg, baseDir)
	if err != nil {
		t.Fatalf("ComfyUI relative workflow must load: %v", err)
	}
	if rt == nil || rt.Orchestrator == nil {
		t.Fatal("ComfyUI runtime must be constructed")
	}
	rt.Close()
}

func TestWave3NewAggregatorBadURL(t *testing.T) {
	cfg, baseDir := wave3BaseConfig(t)
	cfg.AggregatorEnabled = true
	cfg.AggregatorBaseURL = "not-a-url"
	cfg.AggregatorModel = "agg-model"
	if _, err := New(cfg, baseDir); err == nil || !strings.Contains(err.Error(), "initialize aggregator provider") {
		t.Fatalf("aggregator bad URL error = %v", err)
	}
}

func TestWave3NewAggregatorSuccess(t *testing.T) {
	cfg, baseDir := wave3BaseConfig(t)
	t.Setenv("WAVE3_AGG_KEY", "test-key")
	cfg.AggregatorEnabled = true
	cfg.AggregatorBaseURL = "http://127.0.0.1:9"
	cfg.AggregatorModel = "agg-model"
	cfg.AggregatorAPIKeyEnv = "WAVE3_AGG_KEY"
	rt, err := New(cfg, baseDir)
	if err != nil {
		t.Fatalf("aggregator must initialize: %v", err)
	}
	if rt == nil {
		t.Fatal("aggregator runtime must be constructed")
	}
	rt.Close()
}

func TestWave3NewAssetStoreUnderFile(t *testing.T) {
	cfg, baseDir := wave3BaseConfig(t)
	blocker := filepath.Join(baseDir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.StorageRoot = blocker
	cfg.DevelopmentFakeProvider = true
	if _, err := New(cfg, baseDir); err == nil || !strings.Contains(err.Error(), "video asset store:") {
		t.Fatalf("asset store under a file error = %v", err)
	}
}

func TestWave3NewPiperDefaultBinaryMissing(t *testing.T) {
	cfg, baseDir := wave3BaseConfig(t)
	modelPath := filepath.Join(baseDir, "model.onnx")
	if err := os.WriteFile(modelPath, []byte("fake-model"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelPath+".json", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.DevelopmentFakeProvider = true
	cfg.PiperEnabled = true
	cfg.PiperModelPath = modelPath
	cfg.PiperBinary = ""
	if _, err := New(cfg, baseDir); err == nil || !strings.Contains(err.Error(), "initialize local Piper narrator") {
		t.Fatalf("missing piper binary error = %v", err)
	}
}

func wave3CodeRenderConfig(t *testing.T) (video.Config, string) {
	t.Helper()
	cfg, baseDir := wave3BaseConfig(t)
	cfg.CodeRenderEnabled = true
	cfg.CodeRenderPlannerBaseURL = "http://127.0.0.1:9"
	return cfg, baseDir
}

func TestWave3NewCodeRenderPlannerBaseURLRequired(t *testing.T) {
	cfg, baseDir := wave3BaseConfig(t)
	cfg.CodeRenderEnabled = true
	cfg.CodeRenderPlannerBaseURL = ""
	cfg.CodeRenderPlannerModel = "m1"
	if _, err := New(cfg, baseDir); err == nil || !strings.Contains(err.Error(), "initialize code-render planner") {
		t.Fatalf("empty planner URL error = %v", err)
	}
}

func TestWave3NewCodeRenderModelAlreadyListed(t *testing.T) {
	cfg, baseDir := wave3CodeRenderConfig(t)
	cfg.CodeRenderPlannerModel = "m1"
	cfg.CodeRenderPlannerModels = []string{"m1", "m2"}
	rt, err := New(cfg, baseDir)
	if err != nil {
		t.Fatalf("code-render must initialize: %v", err)
	}
	defer rt.Close()
	if rt.codeRender == nil {
		t.Fatal("code-render provider must be registered")
	}
	got := rt.codeRender.Models
	if len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Fatalf("models = %v, want [m1 m2] without a duplicate prepend", got)
	}
}

func TestWave3NewCodeRenderMaxDurationDefault(t *testing.T) {
	cfg, baseDir := wave3CodeRenderConfig(t)
	cfg.CodeRenderPlannerModel = "m1"
	cfg.CodeRenderMaxDurationSeconds = 0
	rt, err := New(cfg, baseDir)
	if err != nil {
		t.Fatalf("code-render must initialize: %v", err)
	}
	defer rt.Close()
	if rt.codeRender == nil || rt.codeRender.MaxDurationSeconds != 120 {
		t.Fatalf("max duration = %v, want 120", rt.codeRender)
	}
}

func TestWave3NewCodeRenderRelativeWorkRoot(t *testing.T) {
	cfg, baseDir := wave3CodeRenderConfig(t)
	cfg.CodeRenderPlannerModel = "m1"
	cfg.CodeRenderWorkRoot = "cr-work"
	rt, err := New(cfg, baseDir)
	if err != nil {
		t.Fatalf("code-render must initialize: %v", err)
	}
	defer rt.Close()
	want := filepath.Join(baseDir, "cr-work")
	if rt.codeRender == nil || rt.codeRender.WorkRoot != want {
		t.Fatalf("work root = %v, want %s", rt.codeRender, want)
	}
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Fatalf("relative work root must be created under baseDir: %v", err)
	}
}

func TestWave3NewCodeRenderWorkRootUnderFile(t *testing.T) {
	cfg, baseDir := wave3CodeRenderConfig(t)
	blocker := filepath.Join(baseDir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.CodeRenderPlannerModel = "m1"
	cfg.CodeRenderWorkRoot = blocker
	if _, err := New(cfg, baseDir); err == nil || !strings.Contains(err.Error(), "create code-render work root") {
		t.Fatalf("work root under a file error = %v", err)
	}
}

func TestWave3NewCodeRenderRetentionBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hours int
		want  time.Duration
	}{
		{"negative keeps forever", -1, 0},
		{"positive converts to hours", 5, 5 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, baseDir := wave3CodeRenderConfig(t)
			cfg.CodeRenderPlannerModel = "m1"
			cfg.CodeRenderRetentionHours = tc.hours
			rt, err := New(cfg, baseDir)
			if err != nil {
				t.Fatalf("code-render must initialize: %v", err)
			}
			defer rt.Close()
			if rt.codeRenderRetention != tc.want {
				t.Fatalf("retention = %v, want %v", rt.codeRenderRetention, tc.want)
			}
		})
	}
}

func TestWave3StartNilReceiver(t *testing.T) {
	var rt *Runtime
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("nil runtime Start must return nil, got %v", err)
	}
}

func TestWave3StartTwiceFails(t *testing.T) {
	cfg, baseDir := wave3BaseConfig(t)
	cfg.DevelopmentFakeProvider = true
	rt, err := New(cfg, baseDir)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("first Start must succeed: %v", err)
	}
	if err := rt.Start(ctx); err == nil || !strings.Contains(err.Error(), "already been started") {
		t.Fatalf("second Start error = %v", err)
	}
}

func TestWave3StartRecoverFailure(t *testing.T) {
	q := queue.New(8)
	orch := &orchestrator.Orchestrator{Store: nil, Queue: q}
	pool := queue.NewWorkerPool(q, 1, orch.RunJob)
	rt := &Runtime{Queue: q, Workers: pool, Orchestrator: orch}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := rt.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "recover persisted video jobs") {
		t.Fatalf("recover failure error = %v", err)
	}
	// Close after a failed start must not hang or panic: the queue
	// was already closed on the recover path (sync.Once-guarded).
	rt.Close()
}

func TestWave3PruneOnceRetentionWindow(t *testing.T) {
	dir := t.TempDir()
	store, err := queue.NewFileStore(filepath.Join(dir, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	q := queue.New(8)
	orch := &orchestrator.Orchestrator{Store: store, Queue: q}
	now := time.Now()
	oldCompleted := now.Add(-48 * time.Hour)
	recentCompleted := now.Add(-1 * time.Hour)
	oldJob := video.VideoJob{JobID: "old", State: video.StateCompleted, CreatedAt: now.Add(-72 * time.Hour), CompletedAt: &oldCompleted}
	recentJob := video.VideoJob{JobID: "recent", State: video.StateCompleted, CreatedAt: now.Add(-2 * time.Hour), CompletedAt: &recentCompleted}
	if err := store.Create(context.Background(), oldJob); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), recentJob); err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{Orchestrator: orch, jobRetention: 24 * time.Hour}
	rt.pruneOnce(context.Background(), now)
	if _, err := store.Get(context.Background(), "old"); !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("old completed job must be pruned, get error = %v", err)
	}
	if _, err := store.Get(context.Background(), "recent"); err != nil {
		t.Fatalf("recent completed job must be kept: %v", err)
	}
}

func TestWave3CloseNilReceiver(t *testing.T) {
	var rt *Runtime
	rt.Close() // must not panic
}

type wave3FakeLedgerCloser struct{ closed bool }

func (c *wave3FakeLedgerCloser) Close() error { c.closed = true; return nil }

func TestWave3CloseClosesLedger(t *testing.T) {
	closer := &wave3FakeLedgerCloser{}
	rt := &Runtime{ledgerCloser: closer}
	rt.Close()
	if !closer.closed {
		t.Fatal("Close must close the ledger closer")
	}
}
