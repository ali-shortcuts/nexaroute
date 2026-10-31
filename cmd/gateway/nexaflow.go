package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/httpapi"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

// Combined production NexaFlow wiring (issue #765). This is the single
// landing wiring that supersedes the three candidates #781, #779, and
// #771, taking each one's proven part:
//
//   - Store semantics from #781/#779: file-backed by default (durable,
//     single-process file lock), memory only when explicitly chosen
//     (volatile). StorePath is required for the file backend; there is
//     no implicit default path and no silent fallback to memory. A
//     second process on the same file path fails at open time on the
//     lock; a corrupt file fails at open time.
//   - Backend execution from #771: a loopback GatewayClient (chat,
//     video, MCP, HTTP via this gateway itself) composed with a direct
//     health/compat backend, plus EnvSecretResolver for secret
//     references. GatewayTokenRef names the environment variable
//     holding a client-auth token; without it, llm.chat nodes fail
//     closed at client authentication. The base URL derives from the
//     configured listener on 127.0.0.1 unless explicitly configured.
//   - Startup recovery from #781/#771: Service.Recover runs once
//     before serving (it delegates to Engine.Recover, so the two
//     candidates' recovery calls are the same operation). It
//     reconciles persisted state only and never auto-executes a run;
//     a recovery failure closes the store and fails the attach, so
//     the gateway does not serve with unrecovered state.
//   - Worker identity: the configured worker_id, else the fixed worker
//     "gateway". This deliberately differs from #771's
//     gateway-<pid> default: a pid changes every restart, so leases
//     would lose attribution across restarts; the file store is
//     single-process by design, so a fixed id is both stable and
//     attributable. Scope note: this fixed default is correct ONLY
//     for the single-process file/memory stores wired here. A future
//     multi-process store (e.g. PostgreSQL) must require explicit
//     distinct worker ids, or two gateways would treat each other's
//     leases as their own.
//
// The gateway mounts the canonical Task 8 service only behind the
// explicit opt-in flag nexaflow.enabled (default off). Disabled means
// no AttachNexaflow call and no store opened, so gateway behaviour is
// byte-identical to a gateway without this section.
//
// Deliberately NOT wired here: scheduler daemon (Scheduler.Tick has
// no loop), webhook listener (no secret source is configured), the
// PostgreSQL store (needs control-plane DB plumbing), workflow.*
// permissions (admin routes keep the interim config.read/write
// mapping), any tenant dimension (single-tenant admin deployment;
// runs are filtered by principal subject only), and any BudgetGuard
// (no production implementation exists; budget_check stays
// report-only, hard-budget enforcement stays unsupported).
//
// Shutdown: the caller defers the returned closer after the HTTP
// graceful shutdown, mirroring the video runtime's Close. Close
// releases the file lock and never deletes data.

// maybeAttachNexaflow attaches the NexaFlow service to api when the
// opt-in flag is set and returns a closer for the store (nil for the
// memory backend). When disabled it returns (nil, nil) and touches
// nothing. Call it before api.Handler() is captured: the mux mounts
// Nexaflow routes only while a service is attached.
func maybeAttachNexaflow(api *httpapi.Server, cfg config.Config, hm *health.Manager, logger *log.Logger) (io.Closer, error) {
	if !cfg.Nexaflow.Enabled {
		return nil, nil
	}
	baseURL, err := loopbackBaseURL(cfg)
	if err != nil {
		return nil, err
	}
	svc, closer, err := newNexaflowService(cfg.Nexaflow, baseURL, hm)
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
	if logger != nil {
		logger.Printf("nexaflow enabled backend=%s worker=%s", nexaflowBackendLabel(cfg.Nexaflow), resolveWorkerID(cfg.Nexaflow))
	}
	return closer, nil
}

// loopbackBaseURL resolves the gateway address backend nodes call: the
// explicit gateway_base_url when configured, else the configured
// listener's port on 127.0.0.1 (https when TLS is enabled). A listener
// without a port fails the attach instead of producing a bad address.
func loopbackBaseURL(cfg config.Config) (string, error) {
	if base := strings.TrimSpace(cfg.Nexaflow.GatewayBaseURL); base != "" {
		return base, nil
	}
	_, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return "", fmt.Errorf("derive loopback address from listen %q: %w", cfg.Listen, err)
	}
	scheme := "http"
	if cfg.TLS.Enabled {
		scheme = "https"
	}
	return scheme + "://127.0.0.1:" + port, nil
}

// resolveWorkerID returns the configured worker id, else the fixed
// worker "gateway".
func resolveWorkerID(cfg config.NexaflowConfig) string {
	if id := strings.TrimSpace(cfg.WorkerID); id != "" {
		return id
	}
	return "gateway"
}

// nexaflowBackendLabel renders the configured store backend for logs
// (empty means the file default).
func nexaflowBackendLabel(cfg config.NexaflowConfig) string {
	if backend := strings.TrimSpace(cfg.StoreBackend); backend != "" {
		return backend
	}
	return "file"
}

// newNexaflowService builds the production service for cfg. It is
// factored out of maybeAttachNexaflow so the flag gating, store
// selection, and failure modes are directly testable without serving.
func newNexaflowService(cfg config.NexaflowConfig, baseURL string, hm *health.Manager) (*nexaflow.Service, io.Closer, error) {
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
			return nil, nil, fmt.Errorf("open store: %w", err)
		}
		store, closer = fs, fs
	case "memory":
		store = nexaflow.NewMemoryStore()
	default:
		return nil, nil, fmt.Errorf("nexaflow: unknown store_backend %q", cfg.StoreBackend)
	}
	reg := nexaflow.DefaultRegistry()
	backend := &nexaflow.CompositeBackend{
		Gateway: &nexaflow.GatewayClient{
			BaseURL:        baseURL,
			AuthTokenRef:   strings.TrimSpace(cfg.GatewayTokenRef),
			SecretResolver: nexaflow.EnvSecretResolver,
		},
		HealthCompat: &nexaflow.DirectHealthCompatBackend{Health: hm},
	}
	var svc *nexaflow.Service
	engine, err := nexaflow.NewEngine(nexaflow.EngineOptions{
		Store:    store,
		Registry: reg,
		Backend:  backend,
		WorkerID: resolveWorkerID(cfg),
		Audit: func(ev nexaflow.AuditEvent) {
			if svc != nil {
				svc.HandleEngineEvent(ev)
			}
		},
	})
	if err != nil {
		if closer != nil {
			_ = closer.Close()
		}
		return nil, nil, err
	}
	svc, err = nexaflow.NewService(store, engine, reg, nil)
	if err != nil {
		if closer != nil {
			_ = closer.Close()
		}
		return nil, nil, err
	}
	return svc, closer, nil
}
