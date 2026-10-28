package httpapi

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

// AttachNexaflow wires the workflow engine into the gateway. It is called
// once during startup, before Handler is served, when the operator enables
// NexaFlow in configuration. The store is operator-owned (FileStore in the
// single-host topology). The audit sink is passed separately because the
// Store interface does not statically include Record; in practice the
// operator passes the same FileStore value for both, so workflow audit
// events persist alongside runs. GatewayClient talks to this gateway over
// loopback, which is the Phase 1 model-transport decision: there is no
// in-process chat API, only POST /v1/chat/completions.
//
// Attach runs Recover synchronously so runs interrupted by a previous
// process lifetime are parked (never blindly re-run) before serving.
//
// Loopback chat needs a client-auth token: gatewayTokenRef is the
// environment variable name holding one, resolved per request. Without it,
// llm.chat nodes fail closed at the gateway's client-auth check.
func (s *Server) AttachNexaflow(store nexaflow.Store, audit nexaflow.AuditSink, gatewayBaseURL, gatewayTokenRef string) error {
	if s.nexaflowEngine != nil {
		return fmt.Errorf("nexaflow already attached")
	}
	if store == nil {
		return fmt.Errorf("nexaflow store is required")
	}
	if gatewayBaseURL == "" {
		return fmt.Errorf("nexaflow gateway base URL is required")
	}
	gateway := &nexaflow.GatewayClient{
		BaseURL:        gatewayBaseURL,
		AuthTokenRef:   gatewayTokenRef,
		SecretResolver: nexaflow.EnvSecretResolver,
	}
	var runner *eval.Runner
	if s.evaluation != nil {
		runner = s.evaluation.runner
	}
	backend := &nexaflow.RuntimeBackend{
		Gateway: gateway,
		Health: &nexaflow.DirectHealthCompatBackend{
			Health:      s.hm,
			CompatStore: s.capStore,
		},
		Cost: &nexaflow.DirectCostBackend{
			Tracker: s.usage,
			Ledger:  s.budgets,
		},
		Eval:                &nexaflow.ReplayEvalBackend{Runner: runner},
		ResolveSelectorFunc: s.nexaflowResolveSelector,
		Secrets:             nexaflow.EnvSecretResolver,
	}
	if audit == nil {
		audit = &nexaflow.MemoryAuditSink{}
	}
	engine := nexaflow.NewEngine(nil, store, backend, audit, nexaflow.EngineOptions{})
	s.nexaflowEngine = engine
	s.nexaflowStore = store
	s.nexaflowScheduler = nexaflow.NewScheduler(engine, store)
	if _, err := engine.Recover(context.Background()); err != nil {
		s.nexaflowEngine = nil
		s.nexaflowStore = nil
		s.nexaflowScheduler = nil
		return fmt.Errorf("nexaflow recovery failed: %w", err)
	}
	return nil
}

// StartNexaflowScheduler ticks the schedule dispatcher in the background.
// The returned stop function ends the loop; the caller owns shutdown.
// An interval <= 0 disables background ticking (operators tick manually
// through POST /admin/api/nexaflow/scheduler/tick).
func (s *Server) StartNexaflowScheduler(interval time.Duration) func() {
	if s.nexaflowScheduler == nil || interval <= 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = s.nexaflowScheduler.Tick(ctx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// nexaflowResolveSelector proves a node selector exists before any billable
// call. Resolution order: virtual-endpoint public model, virtual endpoint
// by id, deployment id, deployment alias. The gateway still performs the
// actual routing; this only validates the selector.
func (s *Server) nexaflowResolveSelector(ctx context.Context, selector string) (nexaflow.RouteInfo, error) {
	_ = ctx
	if s.routeResolver != nil {
		if resolved, ok := s.routeResolver.Resolve(selector); ok {
			ids := make([]string, 0, len(resolved.AllowedDeployments))
			for id := range resolved.AllowedDeployments {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			if len(ids) == 0 {
				ids = resolved.OrderedPoolIDs
			}
			return nexaflow.RouteInfo{Kind: "route", ResolvedModel: selector, DeploymentIDs: ids}, nil
		}
		if _, ok := s.routeResolver.ResolveByID(selector); ok {
			return nexaflow.RouteInfo{Kind: "route", ResolvedModel: selector}, nil
		}
	}
	if s.rt != nil {
		for _, d := range s.rt.All() {
			if d.ID == selector {
				return nexaflow.RouteInfo{Kind: "deployment", ResolvedModel: d.Model, DeploymentIDs: []string{d.ID}}, nil
			}
			for _, alias := range d.Aliases {
				if alias == selector {
					return nexaflow.RouteInfo{Kind: "alias", ResolvedModel: d.Model, DeploymentIDs: []string{d.ID}}, nil
				}
			}
		}
	}
	return nexaflow.RouteInfo{}, fmt.Errorf("nexaflow: selector %q did not resolve", selector)
}
