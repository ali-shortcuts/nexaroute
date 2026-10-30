package main

import (
	"fmt"
	"log"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/httpapi"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

// attachNexaflow wires the Task 8 NexaFlow service onto the production
// gateway server, but only when the operator explicitly opts in with
// nexaflow.enabled (issue #765). With the flag off it returns before
// constructing anything: no store is opened, no engine is built, and no
// route is mounted, so gateway behaviour is byte-identical to a gateway
// without this wiring. With the flag on it builds exactly the service
// the Task 8 HTTP tests exercise (memory or file store, canonical
// registry, no provider Backend), so backend nodes fail closed with
// backend_unavailable instead of reporting stubbed success.
//
// Deliberately NOT wired here, per the preserved-unsupported list: the
// webhook listener (AttachNexaflowWebhook is never called), the
// scheduler daemon (no tick, no startup recovery, no schedule state),
// the PostgreSQL store, tenant isolation, subworkflows, any-join,
// workflow.* permissions, live eval, hard-budget enforcement, video
// cancellation and resume support. The BudgetGuard is nil, matching the
// Task 8 probe precedent: budget_check stays report-only and admission
// reserve is not claimed.
//
// On any construction error attachNexaflow returns the error and
// attaches nothing, so main can fail startup rather than serve a
// half-wired service. The returned closer releases the file store when
// one was opened; it is nil for the memory store.
func attachNexaflow(cfg config.Config, api *httpapi.Server, logger *log.Logger) (func() error, error) {
	if !cfg.Nexaflow.Enabled {
		return nil, nil
	}
	storeKind := strings.TrimSpace(cfg.Nexaflow.Store)
	if storeKind == "" {
		storeKind = "memory"
	}
	var (
		store   nexaflow.Store
		closer  func() error
		openErr error
	)
	switch storeKind {
	case "memory":
		store = nexaflow.NewMemoryStore()
	case "file":
		fileStore, err := nexaflow.OpenFileStore(cfg.Nexaflow.StorePath)
		if err != nil {
			openErr = err
			break
		}
		store = fileStore
		closer = fileStore.Close
	default:
		return nil, fmt.Errorf("nexaflow.store must be memory or file, got %q", cfg.Nexaflow.Store)
	}
	if openErr != nil {
		return nil, fmt.Errorf("nexaflow: open store: %w", openErr)
	}
	reg := nexaflow.DefaultRegistry()
	engine, err := nexaflow.NewEngine(nexaflow.EngineOptions{Store: store, Registry: reg, Backend: nil, WorkerID: "gateway"})
	if err != nil {
		if closer != nil {
			_ = closer()
		}
		return nil, fmt.Errorf("nexaflow: build engine: %w", err)
	}
	svc, err := nexaflow.NewService(store, engine, reg, nil)
	if err != nil {
		if closer != nil {
			_ = closer()
		}
		return nil, fmt.Errorf("nexaflow: build service: %w", err)
	}
	api.AttachNexaflow(svc)
	if logger != nil {
		logger.Printf("nexaflow enabled store=%s", storeKind)
	}
	return closer, nil
}
