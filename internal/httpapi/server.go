package httpapi

import (
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

//go:embed web/*
var webFS embed.FS

type Server struct {
	applyMu           sync.Mutex
	runtimeMu         sync.RWMutex
	cfg               config.Config
	configPath        string
	reg               *providers.Registry
	rt                *router.Router
	hm                *health.Manager
	bus               *events.Bus
	probe             *probe.Engine
	log               *log.Logger
	usage             *usage.Tracker
	requestSeq        atomic.Uint64
	requestTotal      atomic.Uint64
	inflight          atomic.Int64
	overloadRejects   atomic.Uint64
	clientAuthRejects atomic.Uint64
	admissionWaits    atomic.Uint64
	saturatedSpills   atomic.Uint64
	admissionWake     chan struct{}
	retryBudget       *retryBudget
}

type clientKeyNameKey struct{}

// clientKeyName returns the authenticated virtual client key name for a
// request, or "local" when client auth is disabled.
func clientKeyName(r *http.Request) string {
	if v, ok := r.Context().Value(clientKeyNameKey{}).(string); ok && v != "" {
		return v
	}
	return "local"
}

func New(cfg config.Config, configPath string, reg *providers.Registry, rt *router.Router, hm *health.Manager, bus *events.Bus, pe *probe.Engine, l *log.Logger) *Server {
	return &Server{cfg: cfg, configPath: configPath, reg: reg, rt: rt, hm: hm, bus: bus, probe: pe, log: l, usage: usage.New(), retryBudget: newRetryBudget(cfg.Routing.RetryBudgetRatio), admissionWake: make(chan struct{}, 1)}
}

func (s *Server) currentConfig() config.Config {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return cloneConfig(s.cfg)
}

func (s *Server) runtimeSettingsSnapshot() (config.RoutingConfig, config.ProbeConfig) {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.cfg.Routing, s.cfg.Probe
}

func (s *Server) adminConfigSnapshot() config.AdminConfig {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.cfg.Admin
}

func cloneConfig(in config.Config) config.Config {
	out := in
	out.Providers = append([]config.ProviderConfig(nil), in.Providers...)
	for i := range out.Providers {
		if in.Providers[i].Headers != nil {
			out.Providers[i].Headers = map[string]string{}
			for k, v := range in.Providers[i].Headers {
				out.Providers[i].Headers[k] = v
			}
		}
		out.Providers[i].Models = append([]config.ModelConfig(nil), in.Providers[i].Models...)
		out.Providers[i].Credentials = append([]config.CredentialConfig(nil), in.Providers[i].Credentials...)
		if in.Providers[i].ForwardHeaders != nil {
			out.Providers[i].ForwardHeaders = append([]string{}, in.Providers[i].ForwardHeaders...)
		} else {
			out.Providers[i].ForwardHeaders = nil
		}
		for j := range out.Providers[i].Models {
			out.Providers[i].Models[j].Aliases = append([]string(nil), in.Providers[i].Models[j].Aliases...)
		}
	}
	// Deep-copy the newer reference-typed sections so a rejected mutation can
	// never leak into the live config through shared backing arrays.
	out.ClientAuth.Keys = append([]config.ClientKey(nil), in.ClientAuth.Keys...)
	if in.Pricing != nil {
		out.Pricing = make(map[string]config.PriceConfig, len(in.Pricing))
		for k, v := range in.Pricing {
			out.Pricing[k] = v
		}
	}
	out.Guardrails.BlockedPatterns = append([]string(nil), in.Guardrails.BlockedPatterns...)
	return out
}

func providerProbeIdentityEqual(a, b config.ProviderConfig) bool {
	return a.Type == b.Type &&
		a.BaseURL == b.BaseURL &&
		a.APIKey == b.APIKey &&
		a.APIKeyEnv == b.APIKeyEnv &&
		reflect.DeepEqual(a.Credentials, b.Credentials) &&
		a.AuthMode == b.AuthMode &&
		reflect.DeepEqual(a.Headers, b.Headers) &&
		reflect.DeepEqual(a.ForwardHeaders, b.ForwardHeaders) &&
		a.ProxyURL == b.ProxyURL &&
		a.ChatPath == b.ChatPath &&
		a.MessagesPath == b.MessagesPath &&
		a.ModelsPath == b.ModelsPath &&
		a.CountTokensPath == b.CountTokensPath &&
		a.Enabled == b.Enabled
}

func providerAdapterIdentityEqual(a, b config.ProviderConfig) bool {
	return providerProbeIdentityEqual(a, b) &&
		a.MaxConcurrency == b.MaxConcurrency &&
		a.StreamIdleTimeoutSeconds == b.StreamIdleTimeoutSeconds
}

func changedProviderAdapterIDs(oldCfg, newCfg config.Config) map[string]struct{} {
	changed := map[string]struct{}{}
	oldProviders := make(map[string]config.ProviderConfig, len(oldCfg.Providers))
	for _, p := range oldCfg.Providers {
		oldProviders[p.ID] = p
	}
	adapterPolicyChanged := oldCfg.Routing.RequestTimeoutMS != newCfg.Routing.RequestTimeoutMS ||
		oldCfg.Routing.MaxRetryAfterSeconds != newCfg.Routing.MaxRetryAfterSeconds
	for _, np := range newCfg.Providers {
		if !np.Enabled {
			continue
		}
		op, ok := oldProviders[np.ID]
		if adapterPolicyChanged || !ok || !op.Enabled || !providerAdapterIdentityEqual(op, np) {
			changed[np.ID] = struct{}{}
		}
	}
	return changed
}

func changedDeploymentIDs(oldCfg, newCfg config.Config) map[string]struct{} {
	changed := map[string]struct{}{}
	oldProviders := map[string]config.ProviderConfig{}
	for _, p := range oldCfg.Providers {
		oldProviders[p.ID] = p
	}
	for _, np := range newCfg.Providers {
		if !np.Enabled {
			continue
		}
		op, ok := oldProviders[np.ID]
		providerChanged := !ok || !providerProbeIdentityEqual(op, np)
		oldModels := map[string]config.ModelConfig{}
		if ok {
			for _, m := range op.Models {
				oldModels[m.ID] = m
			}
		}
		for _, nm := range np.Models {
			if !nm.Enabled {
				continue
			}
			om, existed := oldModels[nm.ID]
			if providerChanged || !existed || !om.Enabled || om.Model != nm.Model || !reflect.DeepEqual(om.Capabilities, nm.Capabilities) {
				changed[np.ID+"/"+nm.ID] = struct{}{}
			}
		}
	}
	return changed
}

// applyConfig validates and persists first, then swaps the in-memory provider
// registry/router under one short lock. Existing Adapter pointers already taken
// by in-flight requests remain valid after the registry map is replaced.
func (s *Server) applyConfigLocked(cfg config.Config) error {
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	oldCfg := s.currentConfig()
	rebuild := changedProviderAdapterIDs(oldCfg, cfg)
	rotatedCredentialProviders := map[string]struct{}{}
	for _, p := range cfg.Providers {
		if p.Enabled && !s.reg.CredentialsMatchProvider(p) {
			rebuild[p.ID] = struct{}{}
			rotatedCredentialProviders[p.ID] = struct{}{}
		}
	}

	nextEnabled := make(map[string]struct{}, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p.Enabled {
			nextEnabled[p.ID] = struct{}{}
		}
	}
	staleAdapters := make([]providers.Adapter, 0, len(rebuild))
	for _, oldProvider := range oldCfg.Providers {
		if !oldProvider.Enabled {
			continue
		}
		_, removedOrDisabled := nextEnabled[oldProvider.ID]
		_, rebuilt := rebuild[oldProvider.ID]
		if removedOrDisabled && !rebuilt {
			continue
		}
		if a, ok := s.reg.Get(oldProvider.ID); ok {
			staleAdapters = append(staleAdapters, a)
		}
	}

	// Prepare the complete next registry before touching disk. Unchanged
	// providers reuse their live adapters, preserving HTTP connection pools and
	// credential cooldown/load state across routing-only or probe-only edits.
	nextReg, err := s.reg.Prepare(cfg, rebuild)
	if err != nil {
		return err
	}
	if err := config.SaveAtomic(s.configPath, cfg); err != nil {
		return err
	}

	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	s.reg.Replace(nextReg)
	s.rt.Reload(cfg)
	s.hm.ConfigureAdvanced(
		cfg.Routing.FailureThreshold,
		cfg.Cooldown(),
		cfg.Routing.CapabilityFailureThreshold,
		time.Duration(cfg.Routing.CapabilityCooldownSeconds)*time.Second,
	)

	valid := map[string]struct{}{}
	for _, d := range s.rt.All() {
		valid[d.ID] = struct{}{}
	}
	s.hm.Retain(valid)
	changedHealth := changedDeploymentIDs(oldCfg, cfg)
	for _, d := range s.rt.All() {
		if _, rotated := rotatedCredentialProviders[d.ProviderID]; rotated {
			changedHealth[d.ID] = struct{}{}
		}
	}
	for id := range changedHealth {
		s.hm.Invalidate(id)
	}

	s.cfg = cfg
	s.retryBudget.setRatio(cfg.Routing.RetryBudgetRatio)
	s.probe.Reload(cfg)
	for _, a := range staleAdapters {
		providers.CloseIdleConnections(a)
	}
	return nil
}

func (s *Server) applyConfig(cfg config.Config) error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	return s.applyConfigLocked(cfg)
}

func (s *Server) mutateConfig(fn func(*config.Config) error) (config.Config, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	cfg := s.currentConfig()
	if err := fn(&cfg); err != nil {
		return config.Config{}, err
	}
	if err := s.applyConfigLocked(cfg); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func (s *Server) routeSnapshot(req router.Requirement) (config.Config, []router.Scored) {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.cfg, s.rt.Candidates(req)
}

func (s *Server) currentRouteCandidate(id string, req router.Requirement) (router.Scored, providers.Adapter, bool) {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	candidate, ok := s.rt.Eligible(id, req)
	if !ok {
		return router.Scored{}, nil, false
	}
	adapter, ok := s.reg.Get(candidate.Deployment.ProviderID)
	if !ok {
		return router.Scored{}, nil, false
	}
	return candidate, adapter, true
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/hello", s.hello)
	mux.HandleFunc("/version", s.hello)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/readyz", s.ready)
	mux.HandleFunc("/metrics", s.metrics)
	mux.HandleFunc("/v1/models", s.models)
	mux.HandleFunc("/v1/messages", s.anthropicMessages)
	mux.HandleFunc("/v1/messages/count_tokens", s.countTokens)
	mux.HandleFunc("/v1/chat/completions", s.openAIChat)

	mux.HandleFunc("/admin/api/snapshot", s.adminSnapshot)
	mux.HandleFunc("/admin/api/probe", s.adminProbe)
	mux.HandleFunc("/admin/api/providers", s.adminProviders)
	mux.HandleFunc("/admin/api/providers/", s.adminProviderByID)
	mux.HandleFunc("/admin/api/provider-presets", s.adminProviderPresets)
	mux.HandleFunc("/admin/api/provider-check", s.adminProviderCheck)
	mux.HandleFunc("/admin/api/provider-test", s.adminProviderTest)
	mux.HandleFunc("/admin/api/provider-discover", s.adminProviderDiscover)
	mux.HandleFunc("/admin/api/settings", s.adminSettings)
	mux.HandleFunc("/admin/api/client-keys", s.adminClientKeys)
	mux.HandleFunc("/admin/api/client-keys/", s.adminClientKeys)
	mux.HandleFunc("/admin/api/client-auth-required", s.adminClientAuthRequired)
	mux.HandleFunc("/admin/api/guardrails/test", s.adminGuardrailsTest)
	mux.HandleFunc("/admin/api/usage", s.adminUsage)
	mux.HandleFunc("/admin/api/requests", s.adminRequests)
	mux.HandleFunc("/admin/api/route-preview", s.adminRoutePreview)
	mux.HandleFunc("/admin/api/usage/reset", s.adminUsage)

	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return s.middleware(mux)
}

func isDataPlaneRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions":
		return true
	default:
		return false
	}
}

func (s *Server) admissionLimit() int64 {
	s.runtimeMu.RLock()
	limit := s.cfg.Routing.MaxInflightRequests
	s.runtimeMu.RUnlock()
	if limit < 1 {
		limit = 128
	}
	return int64(limit)
}

func (s *Server) admissionQueueTimeout() time.Duration {
	s.runtimeMu.RLock()
	timeout := s.cfg.Routing.AdmissionQueueTimeoutMS
	s.runtimeMu.RUnlock()
	if timeout < 0 {
		timeout = 0
	}
	return time.Duration(timeout) * time.Millisecond
}

// acquireDataPlane admits a data-plane request into the bounded global
// inflight set. When the gateway is momentarily at capacity the request
// waits for a release instead of being rejected outright, absorbing agent
// bursts (dozens of parallel subagent calls); the wait is bounded by the
// admission queue timeout and by client cancellation.
func (s *Server) acquireDataPlane(ctx context.Context) bool {
	limit := s.admissionLimit()
	if s.tryInflight(limit) {
		return true
	}
	timeout := s.admissionQueueTimeout()
	if timeout <= 0 {
		return false
	}
	s.admissionWaits.Add(1)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-s.admissionWake:
		}
		if s.tryInflight(limit) {
			return true
		}
	}
}

func (s *Server) tryInflight(limit int64) bool {
	for {
		current := s.inflight.Load()
		if current >= limit {
			return false
		}
		if s.inflight.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (s *Server) releaseDataPlane() {
	if n := s.inflight.Add(-1); n < 0 {
		s.inflight.Store(0)
	}
	select {
	case s.admissionWake <- struct{}{}:
	default:
	}
}

func (s *Server) rejectOverloaded(w http.ResponseWriter, r *http.Request, requestID string) {
	s.overloadRejects.Add(1)
	s.bus.Add(events.Event{
		RequestID:  requestID,
		Kind:       "gateway_overloaded",
		Message:    "global data-plane admission limit reached",
		ErrorType:  "gateway_overloaded",
		StatusCode: http.StatusServiceUnavailable,
	})
	w.Header().Set("Retry-After", "1")
	if r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/messages/count_tokens" {
		anthropicErrorJSON(w, http.StatusServiceUnavailable, "gateway is at capacity; retry shortly")
		return
	}
	errorJSON(w, http.StatusServiceUnavailable, "gateway is at capacity; retry shortly")
}

func (s *Server) accessLoggingConfig() config.LoggingConfig {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.cfg.Logging
}

func (s *Server) shouldLogRequest(status int, duration time.Duration, streaming bool, requestNumber uint64) bool {
	cfg := s.accessLoggingConfig()
	if cfg.AccessMode == "off" {
		return false
	}
	if status >= http.StatusBadRequest {
		return true
	}
	if !streaming && cfg.SlowRequestMS > 0 && duration >= time.Duration(cfg.SlowRequestMS)*time.Millisecond {
		return true
	}
	switch cfg.AccessMode {
	case "all":
		return true
	case "sampled":
		every := cfg.SuccessSampleEvery
		if every < 1 {
			every = 1000
		}
		return requestNumber%uint64(every) == 0
	default:
		return false
	}
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestNumber := s.requestTotal.Add(1)
		rid := normalizeRequestID(r.Header.Get("x-request-id"))
		if rid == "" {
			rid = fmt.Sprintf("nexaroute-%x-%x", time.Now().UnixNano(), s.requestSeq.Add(1))
			r.Header.Set("x-request-id", rid)
		}
		w.Header().Set("x-request-id", rid)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}
				s.bus.Add(events.Event{RequestID: rid, Kind: "internal_panic", Message: "handler panic recovered", ErrorType: "internal_panic"})
				s.log.Printf("request_id=%s handler_panic type=%T stack=%q", rid, recovered, debug.Stack())
				if !sw.wroteHeader {
					errorJSON(sw, http.StatusInternalServerError, "internal gateway error")
				}
			}
			duration := time.Since(start)
			streaming := strings.Contains(strings.ToLower(sw.Header().Get("Content-Type")), "text/event-stream")
			if s.shouldLogRequest(sw.status, duration, streaming, requestNumber) {
				s.log.Printf("request_id=%s method=%s path=%s status=%d duration=%s", rid, r.Method, r.URL.Path, sw.status, duration)
			}
			if isDataPlaneRequest(r) {
				errMsg := ""
				if sw.status >= 400 {
					errMsg = http.StatusText(sw.status)
				}
				s.finalizeTelemetry(requestTelemetry(r), sw.status, duration, errMsg)
			}
		}()
		if strings.HasPrefix(r.URL.Path, "/admin/api/") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Pragma", "no-cache")
			if !s.adminAuthorized(r) {
				errorJSON(sw, http.StatusUnauthorized, "admin authorization required")
				return
			}
		}
		if isDataPlaneRequest(r) {
			r = markRequestTelemetry(r, rid)
			if s.clientAuthRequired() {
				name, ok := s.authorizeClientKey(r)
				if !ok {
					s.clientAuthRejects.Add(1)
					s.bus.Add(events.Event{RequestID: rid, Kind: "client_auth_rejected", Message: "missing or invalid client key", ErrorType: "unauthorized"})
					s.rejectUnauthorized(sw, r)
					return
				}
				r = r.WithContext(context.WithValue(r.Context(), clientKeyNameKey{}, name))
				if tm := requestTelemetry(r); tm != nil {
					tm.mu.Lock()
					tm.keyName = name
					tm.mu.Unlock()
				}
			}
			if !s.acquireDataPlane(r.Context()) {
				if clientRequestGone(r.Context()) {
					return
				}
				s.rejectOverloaded(sw, r, rid)
				return
			}
			s.retryBudget.deposit()
			defer s.releaseDataPlane()
		}
		next.ServeHTTP(sw, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *statusWriter) Committed() bool             { return w.wroteHeader }

func responseCommitted(w http.ResponseWriter) bool {
	if state, ok := w.(interface{ Committed() bool }); ok {
		return state.Committed()
	}
	return false
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Write keeps wroteHeader in sync with the implicit 200 header that
// net/http emits on the first body write, so a later guarded WriteHeader
// call cannot reach the underlying writer twice.
func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}
func (w *statusWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

func (s *Server) adminAuthorized(r *http.Request) bool {
	cfg := s.adminConfigSnapshot()
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	isLoopback := ip != nil && ip.IsLoopback()
	if cfg.BindLocalOnly && !isLoopback {
		return false
	}
	if cfg.APIKey != "" {
		got := r.Header.Get("x-admin-key")
		if got == "" {
			if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
				got = strings.TrimPrefix(v, "Bearer ")
			}
		}
		return len(got) == len(cfg.APIKey) && subtle.ConstantTimeCompare([]byte(got), []byte(cfg.APIKey)) == 1
	}
	return isLoopback
}
