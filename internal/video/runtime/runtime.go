package runtime

import (
	"context"
	"fmt"
	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/api"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
	"os"
	"path/filepath"
)

type Runtime struct {
	Handler *api.Handler
	Queue   *queue.Queue
	Workers *queue.WorkerPool
	Store   queue.JobStore
}

func New(cfg video.Config, baseDir string) (*Runtime, error) {
	if !cfg.Enabled {
		return nil, nil
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
	orch := &orchestrator.Orchestrator{Store: store, Queue: q, Providers: reg, Ledger: &cost.Ledger{}}
	token := ""
	if cfg.AuthTokenEnv != "" {
		token = os.Getenv(cfg.AuthTokenEnv)
	}
	h := &api.Handler{Orch: orch, Store: store, Providers: reg, RequireAuth: token != "", BearerToken: token}
	pool := queue.NewWorkerPool(q, cfg.Workers, orch.RunJob)
	return &Runtime{Handler: h, Queue: q, Workers: pool, Store: store}, nil
}
func (r *Runtime) Start(ctx context.Context) {
	if r == nil {
		return
	}
	go r.Workers.Run(ctx)
}
func (r *Runtime) Close() {
	if r != nil && r.Queue != nil {
		r.Queue.Close()
	}
}
