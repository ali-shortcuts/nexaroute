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
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
)

const issue662TokenEnv = "NEXAROUTE_RUNTIME_ISSUE662_TOKEN"

func issue662Config() video.Config {
	return video.Config{
		Enabled: true, AuthTokenEnv: issue662TokenEnv,
		DevelopmentFakeProvider: true,
	}
}

func issue662New(t *testing.T, cfg video.Config) *Runtime {
	t.Helper()
	t.Setenv(issue662TokenEnv, "issue662-token")
	base := t.TempDir()
	r, err := New(cfg, base)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r == nil {
		t.Fatal("New returned a nil runtime")
	}
	return r
}

func TestIssue662NewPropagatesJobStoreFailure(t *testing.T) {
	t.Setenv(issue662TokenEnv, "issue662-token")
	base := t.TempDir()
	cfg := issue662Config()
	cfg.StorePath = "."
	if _, err := New(cfg, base); err == nil || !strings.Contains(err.Error(), "video job store") {
		t.Fatalf("New(store path is a directory) error = %v", err)
	}
}

func TestIssue662NewPropagatesAggregatorInitFailure(t *testing.T) {
	t.Setenv(issue662TokenEnv, "issue662-token")
	cfg := issue662Config()
	cfg.AggregatorEnabled = true
	cfg.AggregatorBaseURL = "http://127.0.0.1:9"
	if _, err := New(cfg, t.TempDir()); err == nil || !strings.Contains(err.Error(), "initialize aggregator provider") || !strings.Contains(err.Error(), "aggregator_model is required") {
		t.Fatalf("New(aggregator without model) error = %v", err)
	}
}

func TestIssue662NewPropagatesAssetStoreFailure(t *testing.T) {
	t.Setenv(issue662TokenEnv, "issue662-token")
	base := t.TempDir()
	filePath := filepath.Join(base, "assets")
	if err := os.WriteFile(filePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := issue662Config()
	cfg.StorageRoot = "assets"
	if _, err := New(cfg, base); err == nil || !strings.Contains(err.Error(), "video asset store") {
		t.Fatalf("New(storage root is a file) error = %v", err)
	}
}

func TestIssue662NewCodeRenderPlannerFailures(t *testing.T) {
	t.Setenv(issue662TokenEnv, "issue662-token")
	base := t.TempDir()

	invalidURL := issue662Config()
	invalidURL.CodeRenderEnabled = true
	invalidURL.CodeRenderPlannerBaseURL = "://bad"
	invalidURL.CodeRenderPlannerModel = "m"
	if _, err := New(invalidURL, base); err == nil || !strings.Contains(err.Error(), "initialize code-render planner") {
		t.Fatalf("New(invalid planner URL) error = %v", err)
	}

	missingKey := issue662Config()
	missingKey.CodeRenderEnabled = true
	missingKey.CodeRenderPlannerBaseURL = "http://127.0.0.1:9"
	missingKey.CodeRenderPlannerAPIKeyEnv = "NEXAROUTE_ISSUE662_UNSET_PLANNER_KEY"
	missingKey.CodeRenderPlannerModel = "m"
	t.Setenv("NEXAROUTE_ISSUE662_UNSET_PLANNER_KEY", "")
	if _, err := New(missingKey, base); err == nil || !strings.Contains(err.Error(), "initialize code-render planner") {
		t.Fatalf("New(missing planner key) error = %v", err)
	}

	missingModels := issue662Config()
	missingModels.CodeRenderEnabled = true
	missingModels.CodeRenderPlannerBaseURL = "http://127.0.0.1:9"
	if _, err := New(missingModels, base); err == nil || !strings.Contains(err.Error(), "requires at least one planning model") {
		t.Fatalf("New(no planner models) error = %v", err)
	}
}

func TestIssue662NewCodeRenderWorkRootFailure(t *testing.T) {
	t.Setenv(issue662TokenEnv, "issue662-token")
	base := t.TempDir()
	blocked := filepath.Join(base, "work-root")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := issue662Config()
	cfg.CodeRenderEnabled = true
	cfg.CodeRenderPlannerBaseURL = "http://127.0.0.1:9"
	cfg.CodeRenderPlannerModel = "m"
	cfg.CodeRenderWorkRoot = blocked
	if _, err := New(cfg, base); err == nil || !strings.Contains(err.Error(), "create code-render work root") {
		t.Fatalf("New(blocked work root) error = %v", err)
	}
}

func TestIssue662NewCodeRenderConfiguration(t *testing.T) {
	t.Setenv("NEXAROUTE_FFMPEG", "  /opt/issue662/ffmpeg  ")
	cfg := issue662Config()
	cfg.CodeRenderEnabled = true
	cfg.CodeRenderPlannerBaseURL = "http://127.0.0.1:9"
	cfg.CodeRenderPlannerModels = []string{"a", "b"}
	cfg.CodeRenderPlannerModel = "a"
	cfg.CodeRenderMaxDurationSeconds = 0
	cfg.CodeRenderRetentionHours = 2
	cfg.CodeRenderWorkRoot = "relative-work"
	r := issue662New(t, cfg)
	if r.codeRender == nil {
		t.Fatal("code-render provider was not registered")
	}
	if got := strings.Join(r.codeRender.Models, ","); got != "a,b" {
		t.Fatalf("planner models = %q, want deduplicated a,b", got)
	}
	if r.codeRender.MaxDurationSeconds != 120 {
		t.Fatalf("default code-render max duration = %v, want 120", r.codeRender.MaxDurationSeconds)
	}
	if r.codeRenderRetention != 2*time.Hour || r.codeRender.Retention != 2*time.Hour {
		t.Fatalf("retention = %v / %v, want 2h", r.codeRenderRetention, r.codeRender.Retention)
	}
	if !filepath.IsAbs(r.codeRender.WorkRoot) || !strings.HasSuffix(r.codeRender.WorkRoot, "relative-work") {
		t.Fatalf("relative work root not resolved under base dir: %q", r.codeRender.WorkRoot)
	}
	if r.codeRender.Renderer == nil || r.codeRender.Renderer.FFmpeg != "/opt/issue662/ffmpeg" {
		t.Fatalf("ffmpeg binary not taken from NEXAROUTE_FFMPEG: %+v", r.codeRender.Renderer)
	}

	prepend := issue662Config()
	prepend.CodeRenderEnabled = true
	prepend.CodeRenderPlannerBaseURL = "http://127.0.0.1:9"
	prepend.CodeRenderPlannerModels = []string{"b", "c"}
	prepend.CodeRenderPlannerModel = "a"
	prepend.CodeRenderRetentionHours = -1
	r = issue662New(t, prepend)
	if got := strings.Join(r.codeRender.Models, ","); got != "a,b,c" {
		t.Fatalf("planner models = %q, want configured model prepended", got)
	}
	if r.codeRenderRetention != 0 {
		t.Fatalf("negative retention hours = %v, want keep-forever 0", r.codeRenderRetention)
	}

	defaultRetention := issue662Config()
	defaultRetention.CodeRenderEnabled = true
	defaultRetention.CodeRenderPlannerBaseURL = "http://127.0.0.1:9"
	defaultRetention.CodeRenderPlannerModel = "m"
	defaultRetention.StorageRoot = "assets"
	r = issue662New(t, defaultRetention)
	if r.codeRenderRetention != providers.DefaultCodeRenderRetention {
		t.Fatalf("default retention = %v, want %v", r.codeRenderRetention, providers.DefaultCodeRenderRetention)
	}
	if !filepath.IsAbs(r.codeRender.WorkRoot) || !strings.Contains(r.codeRender.WorkRoot, filepath.Join("assets", "code-render-work")) {
		t.Fatalf("default work root = %q, want under storage root", r.codeRender.WorkRoot)
	}
}

func TestIssue662StartRecoverFailureIsReportedAndCleansUp(t *testing.T) {
	q := queue.New(2)
	store := issue662FailListStore{MemoryStore: queue.NewMemoryStore()}
	pool := queue.NewWorkerPool(q, 1, func(context.Context, video.VideoJob) error { return nil })
	r := &Runtime{
		Queue: q, Workers: pool, Store: store,
		Orchestrator: &orchestrator.Orchestrator{Store: store, Queue: q},
	}
	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "recover persisted video jobs") || !strings.Contains(err.Error(), "list unavailable") {
		t.Fatalf("Start(recover failure) error = %v", err)
	}
	r.Close()
}

func TestIssue662StartDoubleStartIsRejected(t *testing.T) {
	r := issue662New(t, issue662Config())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Start(ctx); err == nil || !strings.Contains(err.Error(), "already been started") {
		t.Fatalf("second Start error = %v", err)
	}
}

func TestIssue662StartPrunesExpiredTerminalJobs(t *testing.T) {
	cfg := issue662Config()
	cfg.JobRetentionDays = 1
	r := issue662New(t, cfg)
	old := video.VideoJob{
		JobID: "issue662-old-terminal", State: video.StateCompleted,
		CreatedAt: time.Now().Add(-72 * time.Hour),
	}
	if err := r.Store.Create(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Store.Get(context.Background(), old.JobID); !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("expired terminal job still present: %v", err)
	}
}

func TestIssue662PruneOnceCoversCodeRenderBranch(t *testing.T) {
	root := t.TempDir()
	cr := providers.NewCodeRender(nil, &render.Renderer{}, nil, root, []string{"m"}, 60)
	store := queue.NewMemoryStore()
	r := &Runtime{
		Orchestrator:        &orchestrator.Orchestrator{Store: store},
		codeRender:          cr,
		codeRenderRetention: time.Hour,
	}
	r.pruneOnce(context.Background(), time.Now())

	r.jobRetention = time.Hour
	r.pruneOnce(context.Background(), time.Now())
}

func TestIssue662CloseCallsLedgerCloser(t *testing.T) {
	closer := &issue662Closer{}
	r := &Runtime{ledgerCloser: closer}
	r.Close()
	if !closer.closed {
		t.Fatal("Close did not release the durable ledger")
	}
}

func TestIssue662NilRuntimeStartAndCloseAreSafe(t *testing.T) {
	var r *Runtime
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("nil Start error = %v", err)
	}
	r.Close()
}

type issue662FailListStore struct {
	*queue.MemoryStore
}

func (issue662FailListStore) List(context.Context) ([]video.VideoJob, error) {
	return nil, errors.New("list unavailable")
}

type issue662Closer struct{ closed bool }

func (c *issue662Closer) Close() error {
	c.closed = true
	return nil
}
