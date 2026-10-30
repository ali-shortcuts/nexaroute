package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/httpapi"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

// Production NexaFlow wiring (issue #765): the gateway mounts the
// canonical Task 8 service only behind the explicit opt-in flag
// nexaflow.enabled (default off). Disabled means no AttachNexaflow
// call and no store opened, so gateway behaviour is byte-identical to
// a gateway without this section.
//
// Enabled construction, in ownership order:
//   - store: file-backed (durable, single-process file lock) or, when
//     store_backend is "memory", a volatile in-memory store. A second
//     process on the same file path fails at open time on the lock;
//     a corrupt file fails at open time. Neither falls back silently.
//   - engine: canonical registry, no backend (Backend nil). Nodes that
//     need a backend (LLM, MCP, video, gateway HTTP) fail closed per
//     node as backend_unavailable, never as a fake success. Wiring a
//     loopback GatewayClient is a separate security decision and is
//     deliberately not done here.
//   - service: no BudgetGuard (no production implementation exists;
//     budget_check stays report-only, hard-budget enforcement stays
//     unsupported).
//   - startup recovery: Service.Recover runs once before serving. It
//     reconciles persisted state only (provably-safe attempts back to
//     Pending, dispatched effectful attempts parked as ambiguous, live
//     leases honored) and never auto-executes a run.
//   - shutdown: the caller defers the returned closer after the HTTP
//     graceful shutdown, mirroring the video runtime's Close. Close
//     releases the file lock and never deletes data.
//
// Deliberately NOT wired here: scheduler daemon (Scheduler.Tick has
// no loop), webhook listener (no secret source is configured), the
// PostgreSQL store (needs control-plane DB plumbing), workflow.*
// permissions (admin routes keep the interim config.read/write
// mapping), and any tenant dimension (single-tenant admin deployment;
// runs are filtered by principal subject only).

// maybeAttachNexaflow attaches the NexaFlow service to api when the
// opt-in flag is set and returns a closer for the store (nil for the
// memory backend). When disabled it returns (nil, nil) and touches
// nothing. Call it before api.Handler() is captured: the mux mounts
// NexaFlow routes only while a service is attached.
func maybeAttachNexaflow(api *httpapi.Server, cfg config.NexaflowConfig, logger *log.Logger) (io.Closer, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	svc, closer, err := newNexaflowService(cfg)
	if err != nil {
		return nil, err
	}
	if err := svc.Recover(context.Background()); err != nil {
		if closer != nil {
			_ = closer.Close()
		}
		return nil, fmt.Errorf("nexaflow: startup recovery failed: %w", err)
	}
	api.AttachNexaflow(svc)
	backend := strings.TrimSpace(cfg.StoreBackend)
	if backend == "" {
		backend = "file"
	}
	logger.Printf("nexaflow enabled backend=%s", backend)
	return closer, nil
}

// newNexaflowService builds the production service for cfg. It is
// factored out of maybeAttachNexaflow so the flag gating, store
// selection, and failure modes are directly testable without serving.
func newNexaflowService(cfg config.NexaflowConfig) (*nexaflow.Service, io.Closer, error) {
	var store nexaflow.Store
	var closer io.Closer
	switch strings.TrimSpace(cfg.StoreBackend) {
	case "", "file":
		path := strings.TrimSpace(cfg.StorePath)
		if path == "" {
			return nil, nil, fmt.Errorf("nexaflow: store_path is required for the file backend")
		}
		fs, err := nexaflow.OpenFileStore(path)
		if err != nil {
			return nil, nil, err
		}
		store, closer = fs, fs
	case "memory":
		store = nexaflow.NewMemoryStore()
	default:
		return nil, nil, fmt.Errorf("nexaflow: unknown store_backend %q", cfg.StoreBackend)
	}
	reg := nexaflow.DefaultRegistry()
	engine, err := nexaflow.NewEngine(nexaflow.EngineOptions{Store: store, Registry: reg, Backend: nil})
	if err != nil {
		if closer != nil {
			_ = closer.Close()
		}
		return nil, nil, err
	}
	svc, err := nexaflow.NewService(store, engine, reg, nil)
	if err != nil {
		if closer != nil {
			_ = closer.Close()
		}
		return nil, nil, err
	}
	return svc, closer, nil
}
