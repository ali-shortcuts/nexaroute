package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/httpapi"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

// attachNexaflow is the production gateway caller of
// httpapi.Server.AttachNexaflow. It runs only when the operator enables the
// nexaflow config section, so a disabled gateway opens no workflow store and
// mounts no workflow route.
//
// The canonical Task 8 service is built from the same constructors the
// service tests use: a single-host file store (the durable authority for
// this topology), the shipped node registry, and a composite backend whose
// gateway client talks to this process over loopback. Health/compat nodes
// use the in-process health manager; compat, cost, and eval ports are not
// wired here and those nodes fail closed with backend_unavailable rather
// than returning invented data.
//
// Startup recovery runs synchronously before the listener serves, matching
// the canonical runtime rule that interrupted runs are re-queued or parked
// (never blindly re-executed). Scheduler ticking and signed-webhook
// attachment remain separate wiring decisions: canonical schedules are
// caller-supplied and webhook secrets need an operator-owned source, so
// neither is invented here.
func attachNexaflow(cfg config.Config, configDir string, api *httpapi.Server, hm *health.Manager, logger *log.Logger) (func(), error) {
	if !cfg.Nexaflow.Enabled {
		return func() {}, nil
	}
	storePath := strings.TrimSpace(cfg.Nexaflow.StorePath)
	if storePath == "" {
		storePath = filepath.Join("nexaflow", "nexaflow-store")
	}
	if !filepath.IsAbs(storePath) {
		storePath = filepath.Join(configDir, storePath)
	}
	store, err := nexaflow.OpenFileStore(storePath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	cleanup := func() { _ = store.Close() }

	baseURL := strings.TrimSpace(cfg.Nexaflow.GatewayBaseURL)
	if baseURL == "" {
		_, port, splitErr := net.SplitHostPort(cfg.Listen)
		if splitErr != nil {
			cleanup()
			return nil, fmt.Errorf("derive loopback address from listen %q: %w", cfg.Listen, splitErr)
		}
		scheme := "http"
		if cfg.TLS.Enabled {
			scheme = "https"
		}
		baseURL = scheme + "://127.0.0.1:" + port
	}

	registry := nexaflow.DefaultRegistry()
	backend := &nexaflow.CompositeBackend{
		Gateway: &nexaflow.GatewayClient{
			BaseURL:        baseURL,
			AuthTokenRef:   strings.TrimSpace(cfg.Nexaflow.GatewayTokenRef),
			SecretResolver: nexaflow.EnvSecretResolver,
		},
		HealthCompat: &nexaflow.DirectHealthCompatBackend{Health: hm},
	}

	workerID := strings.TrimSpace(cfg.Nexaflow.WorkerID)
	if workerID == "" {
		workerID = fmt.Sprintf("gateway-%d", os.Getpid())
	}
	var svc *nexaflow.Service
	engine, err := nexaflow.NewEngine(nexaflow.EngineOptions{
		Store:    store,
		Registry: registry,
		Backend:  backend,
		WorkerID: workerID,
		Audit: func(ev nexaflow.AuditEvent) {
			if svc != nil {
				svc.HandleEngineEvent(ev)
			}
		},
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("build engine: %w", err)
	}
	svc, err = nexaflow.NewService(store, engine, registry, nil)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("build service: %w", err)
	}
	if err := engine.Recover(context.Background()); err != nil {
		cleanup()
		return nil, fmt.Errorf("startup recovery: %w", err)
	}
	api.AttachNexaflow(svc)
	if logger != nil {
		logger.Printf("nexaflow enabled store=%s worker=%s", storePath, workerID)
	}
	return cleanup, nil
}
