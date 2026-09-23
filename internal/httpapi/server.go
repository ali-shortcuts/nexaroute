package httpapi

import (
	"crypto/subtle"
	"embed"
	"fmt"
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
)

//go:embed web/*
var webFS embed.FS

type Server struct {
	applyMu         sync.Mutex
	runtimeMu       sync.RWMutex
	cfg             config.Config
	configPath      string
	reg             *providers.Registry
	rt              *router.Router
	hm              *health.Manager
	bus             *events.Bus
	probe           *probe.Engine
	log             *log.Logger
	requestSeq      atomic.Uint64
	requestTotal    atomic.Uint64
	inflight        atomic.Int64
	overloadRejects atomic.Uint64
}

func New(cfg config.Config, configPath string, reg *providers.Registry, rt *router.Router, hm *health.Manager, bus *events.Bus, pe *probe.Engine, l *log.Logger) *Server {
	return &Server{cfg: cfg, configPath: configPath, reg: reg, rt: rt, hm: hm, bus: bus, probe: pe, log: l}
}

func (s *Server) currentConfig() config.Config {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return cloneConfig(s.cfg)
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
		out.Providers[i].ForwardHeaders = append([]string(nil), in.Providers[i].ForwardHeaders...)
		for j := range out.Providers[i].Models {
			out.Providers[i].Models[j].Aliases = append([]string(nil), in.Providers[i].Models[j].Aliases...)
		}
	}
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
	timeoutChanged := oldCfg.Routing.RequestTimeoutMS != newCfg.Routing.RequestTimeoutMS
	for _, np := range newCfg.Providers {
		if !np.Enabled {
			continue
		}
		op, ok := oldProviders[np.ID]
		if timeoutChanged || !ok || !op.Enabled || !providerAdapterIdentityEqual(op, np) {
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
			if providerChanged || !existed || !om.Enabled || om.Model != nm.Model {
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
	for id := range changedDeploymentIDs(oldCfg, cfg) {
		s.hm.Invalidate(id)
	}

	s.cfg = cfg
	s.probe.Reload(cfg)
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

func (s *Server) tryAcquireDataPlane() bool {
	limit := s.admissionLimit()
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

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		s.requestTotal.Add(1)
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
			s.log.Printf("request_id=%s method=%s path=%s status=%d duration=%s", rid, r.Method, r.URL.Path, sw.status, time.Since(start))
		}()
		if strings.HasPrefix(r.URL.Path, "/admin/api/") && !s.adminAuthorized(r) {
			errorJSON(sw, http.StatusUnauthorized, "admin authorization required")
			return
		}
		if isDataPlaneRequest(r) {
			if !s.tryAcquireDataPlane() {
				s.rejectOverloaded(sw, r, rid)
				return
			}
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

func (w *statusWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *statusWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) adminAuthorized(r *http.Request) bool {
	cfg := s.currentConfig()
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	isLoopback := ip != nil && ip.IsLoopback()
	if cfg.Admin.BindLocalOnly && !isLoopback {
		return false
	}
	if cfg.Admin.APIKey != "" {
		got := r.Header.Get("x-admin-key")
		if got == "" {
			if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
				got = strings.TrimPrefix(v, "Bearer ")
			}
		}
		return len(got) == len(cfg.Admin.APIKey) && subtle.ConstantTimeCompare([]byte(got), []byte(cfg.Admin.APIKey)) == 1
	}
	return isLoopback
}
