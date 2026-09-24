package httpapi

import (
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

	"github.com/ali-shortcuts/nexaroute/internal/cache"
	"github.com/ali-shortcuts/nexaroute/internal/compat"
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
	adminRL         sync.Mutex
	adminBuckets    map[string]*adminBucket
	clientRL        sync.Mutex
	clientBuckets   map[string]*clientBucket
	respCache       *cache.Cache
	usage           *usage.Tracker
	capStore        *compat.Store
}

// adminBucket is a compact token bucket keyed by remote address. Capacity 90
// with a 1.5 token/s refill keeps the dashboard's serialized polling far
// below the ceiling while capping online key-guessing traffic.
const (
	adminBucketCapacity = 90
	adminBucketRefill   = 1.5
	adminAuthPenalty    = 10.0
	adminBucketIdle     = 10 * time.Minute
)

type adminBucket struct {
	tokens float64
	last   time.Time
}

func (s *Server) adminAllow(ip string, cost float64) bool {
	now := time.Now()
	s.adminRL.Lock()
	defer s.adminRL.Unlock()
	if s.adminBuckets == nil {
		s.adminBuckets = map[string]*adminBucket{}
	}
	if len(s.adminBuckets) > 4096 {
		for k, b := range s.adminBuckets {
			if now.Sub(b.last) > adminBucketIdle {
				delete(s.adminBuckets, k)
			}
		}
	}
	b, ok := s.adminBuckets[ip]
	if !ok || now.Sub(b.last) > adminBucketIdle {
		b = &adminBucket{tokens: adminBucketCapacity, last: now}
		s.adminBuckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * adminBucketRefill
	if b.tokens > adminBucketCapacity {
		b.tokens = adminBucketCapacity
	}
	b.last = now
	if b.tokens < cost {
		return false
	}
	b.tokens -= cost
	return true
}

func adminRemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host
}

// adminHostAllowed rejects DNS-rebinding requests that present a Host header
// pointing at a public name. It applies to the keyless loopback trust mode,
// where a rebounded browser origin could otherwise read admin data (including
// resolved provider API keys via reveal=1) same-origin without any CORS
// preflight. When an explicit admin key is configured the check is skipped:
// cross-origin reads already fail the key requirement.
func adminHostAllowed(r *http.Request) bool {
	h := r.Host
	if h == "" {
		return false
	}
	if hOnly, _, err := net.SplitHostPort(h); err == nil {
		h = hOnly
	} else {
		h = strings.Trim(h, "[]")
	}
	if h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "[::1]" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func New(cfg config.Config, configPath string, reg *providers.Registry, rt *router.Router, hm *health.Manager, bus *events.Bus, pe *probe.Engine, l *log.Logger) *Server {
	hm.ConfigureAdvanced(
		cfg.Routing.FailureThreshold,
		cfg.Cooldown(),
		cfg.Routing.CapabilityFailureThreshold,
		time.Duration(cfg.Routing.CapabilityCooldownSeconds)*time.Second,
	)
	hm.ConfigureProviderIncidents(
		cfg.Routing.ProviderFailureThreshold,
		cfg.ProviderFailureWindow(),
		cfg.ProviderCooldown(),
	)
	return &Server{
		cfg: cfg, configPath: configPath, reg: reg, rt: rt, hm: hm, bus: bus, probe: pe, log: l,
		respCache: cache.New(cfg.CacheTTL(), cfg.Cache.MaxEntries, int64(cfg.Cache.MaxBodyBytes)),
		usage:     usage.New(),
		capStore:  compat.NewStore(),
	}
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
		a.ResponsesPath == b.ResponsesPath &&
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
	s.hm.ConfigureProviderIncidents(
		cfg.Routing.ProviderFailureThreshold,
		cfg.ProviderFailureWindow(),
		cfg.ProviderCooldown(),
	)

	valid := map[string]struct{}{}
	for _, d := range s.rt.All() {
		valid[d.ID] = struct{}{}
	}
	s.hm.Retain(valid)
	validProviders := map[string]struct{}{}
	for _, p := range cfg.Providers {
		if p.Enabled {
			validProviders[p.ID] = struct{}{}
		}
	}
	s.hm.RetainProviders(validProviders)
	s.usage.Retain(valid)
	// Cached responses must never outlive the topology that produced them:
	// any config swap invalidates the exact-match cache wholesale.
	s.respCache.Invalidate()

	changedHealth := changedDeploymentIDs(oldCfg, cfg)
	oldProvidersByID := make(map[string]config.ProviderConfig, len(oldCfg.Providers))
	for _, p := range oldCfg.Providers {
		oldProvidersByID[p.ID] = p
	}
	changedProviderHealth := map[string]struct{}{}
	for _, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		old, existed := oldProvidersByID[p.ID]
		if !existed || !old.Enabled || !providerProbeIdentityEqual(old, p) {
			changedProviderHealth[p.ID] = struct{}{}
		}
		if _, rotated := rotatedCredentialProviders[p.ID]; rotated {
			changedProviderHealth[p.ID] = struct{}{}
		}
	}
	for _, d := range s.rt.All() {
		if _, rotated := rotatedCredentialProviders[d.ProviderID]; rotated {
			changedHealth[d.ID] = struct{}{}
		}
	}
	for id := range changedHealth {
		s.hm.Invalidate(id)
	}
	for id := range changedProviderHealth {
		s.hm.InvalidateProvider(id)
	}

	s.cfg = cfg
	s.probe.Reload(cfg)
	s.syncCapabilityContracts(cfg)
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

// routeSnapshot returns the request-path view of the runtime config. The
// map/slice-bearing fields (Providers) are stripped: unlike currentConfig()
// this runs on the hot path of every data-plane request, so instead of a
// deep copy the caller gets a value copy whose only shared state is the
// scalar-only Routing struct. A handler that needs Providers must use
// currentConfig().
func (s *Server) routeSnapshot(req router.Requirement) (config.Config, []router.Scored) {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	out := s.cfg
	out.Providers = nil
	return out, s.rt.Candidates(req)
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
	mux.HandleFunc("/v1/responses", s.openAIResponses)

	mux.HandleFunc("/admin/api/snapshot", s.adminSnapshot)
	mux.HandleFunc("/admin/api/probe", s.adminProbe)
	mux.HandleFunc("/admin/api/providers", s.adminProviders)
	mux.HandleFunc("/admin/api/providers/", s.adminProviderByID)
	mux.HandleFunc("/admin/api/provider-presets", s.adminProviderPresets)
	mux.HandleFunc("/admin/api/provider-check", s.adminProviderCheck)
	mux.HandleFunc("/admin/api/provider-test", s.adminProviderTest)
	mux.HandleFunc("/admin/api/compat", s.adminCompatMatrix)
	mux.HandleFunc("/admin/api/compat/reset", s.adminCompatReset)
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
	case "/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/responses":
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
	if r.URL.Path == "/v1/responses" {
		canonicalErrorJSON(w, "openai_responses", http.StatusServiceUnavailable, "server_error", "gateway is at capacity; retry shortly")
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
		}()
		if strings.HasPrefix(r.URL.Path, "/admin/api/") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Pragma", "no-cache")
			cost := 1.0
			if !s.adminAuthorized(r) {
				// Failed attempts burn a chunk of the bucket so an online
				// key brute force collapses to a handful of guesses per IP
				// while the dashboard's own polling stays unaffected.
				cost = adminBucketCapacity
			}
			if !s.adminAllow(adminRemoteIP(r), cost) {
				errorJSON(sw, http.StatusTooManyRequests, "admin rate limit exceeded")
				return
			}
			if !s.adminAuthorized(r) {
				errorJSON(sw, http.StatusUnauthorized, "admin authorization required")
				return
			}
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

// Write commits the wrapper state so responseCommitted() observes implicit
// 200s from handlers that stream bytes without an explicit WriteHeader.
// Without it a committed stream could be mistaken for pre-commit state and
// concatenated onto a failover provider's output.
func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
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
	// Keyless mode trusts the loopback; make sure the request was actually
	// addressed to a loopback name so a DNS-rebound browser origin cannot
	// silently read admin data (including reveal=1 resolved keys).
	return isLoopback && adminHostAllowed(r)
}
