package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/api"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

const defaultMaxAssetBytes int64 = 512 << 20

type Runtime struct {
	Handler *api.Handler
	Queue *queue.Queue
	Workers *queue.WorkerPool
	Store queue.JobStore
	Orchestrator *orchestrator.Orchestrator
	startMu sync.Mutex
	started bool
	workerCancel context.CancelFunc
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
	if len(reg) == 0 {
		return nil, errors.New("video gateway is enabled but this build has no real video provider adapter; enable development_fake_provider only for local testing")
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
	ledger := &cost.Ledger{}
	orch := &orchestrator.Orchestrator{Store: store, Queue: q, Providers: reg, Ledger: ledger, Assets: assetSink}
	pool := queue.NewWorkerPool(q, cfg.Workers, orch.RunJob)
	h := &api.Handler{Orch: orch, Store: store, Providers: reg, RequireAuth: true, BearerToken: token}
	return &Runtime{Handler: h, Queue: q, Workers: pool, Store: store, Orchestrator: orch}, nil
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
	r.startMu.Unlock()
	go r.Workers.Run(workerCtx)
	select {
	case <-r.Workers.Ready():
	case <-workerCtx.Done():
		workerCancel()
		return workerCtx.Err()
	}
	if err := r.Orchestrator.Recover(workerCtx); err != nil {
		workerCancel()
		r.Queue.Close()
		return fmt.Errorf("recover persisted video jobs: %w", err)
	}
	return nil
}

func (r *Runtime) Close() {
	if r == nil {
		return
	}
	r.startMu.Lock()
	cancel := r.workerCancel
	r.startMu.Unlock()
	// Cancel the worker context before closing the queue so buffered queued jobs
	// cannot start new provider submissions during shutdown.
	if cancel != nil {
		cancel()
	}
	if r.Queue != nil {
		r.Queue.Close()
	}
}
