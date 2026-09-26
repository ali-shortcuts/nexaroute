package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

var (
	errAdminProviderNotFound = errors.New("provider not found")
	errAdminProviderExists   = errors.New("provider id already exists")
)

type providerForm struct {
	Provider       config.ProviderConfig `json:"provider"`
	PreserveSecret bool                  `json:"preserve_secret"`
	TestModels     []string              `json:"test_models,omitempty"`
	// Mode selects the probe depth: quick (default, availability),
	// full (Level B capability suite) or claude_code (agent-loop
	// simulation). See docs/COMPATIBILITY.md.
	Mode string `json:"mode,omitempty"`
}

type testResult struct {
	Model      string `json:"model"`
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code"`
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
	// CapabilityReport is present for mode=full.
	CapabilityReport *compat.ProbeReport `json:"capability_report,omitempty"`
	// AgentReport is present for mode=claude_code.
	AgentReport *compat.ProbeReport `json:"agent_report,omitempty"`
}

func (s *Server) adminProviderPresets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, map[string]any{"presets": providers.Presets()})
}

func (s *Server) adminProviderCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, 405, "method not allowed")
		return
	}
	var in providerForm
	if _, err := readJSON(r, &in); err != nil {
		errorJSON(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if in.PreserveSecret {
		mergeExistingSecret(s.currentConfig(), &in.Provider)
	}
	normalizeProvider(&in.Provider)
	if in.Provider.BaseURL == "" {
		errorJSON(w, 400, "base_url is required")
		return
	}
	a, err := providers.NewAdapter(in.Provider, 8*time.Second)
	if err != nil {
		errorJSON(w, 400, err.Error())
		return
	}
	defer providers.CloseIdleConnections(a)
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := a.DoPath(ctx, http.MethodGet, in.Provider.ModelsPath, nil, false, nil)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"ok": false, "reachable": false, "auth_ok": false,
			"latency_ms": latency, "error": err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	authOK := resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden
	reachable := true
	ok := authOK && resp.StatusCode < 500
	writeJSON(w, 200, map[string]any{
		"ok": ok, "reachable": reachable, "auth_ok": authOK,
		"status_code": resp.StatusCode, "latency_ms": latency,
	})
}

func (s *Server) adminSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, 405, "method not allowed")
		return
	}
	parseLimit := func(name string, max int) int {
		v := strings.TrimSpace(r.URL.Query().Get(name))
		if v == "" {
			return 0
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0
		}
		if n > max {
			return max
		}
		return n
	}
	routingCfg, probeCfg := s.runtimeSettingsSnapshot()
	limit := parseLimit("limit", 5000)
	deployments, totalDeployments := s.rt.AllLimit(limit)
	healthAll := s.hm.Snapshot()
	healthCounts := map[health.Status]int{}
	for _, st := range healthAll {
		healthCounts[st.Status]++
	}
	truncated := limit > 0 && totalDeployments > len(deployments)
	if truncated {
		keep := make(map[string]struct{}, len(deployments))
		for _, d := range deployments {
			keep[d.ID] = struct{}{}
		}
		filtered := make([]health.State, 0, len(deployments))
		for _, st := range healthAll {
			if _, ok := keep[st.Deployment]; ok {
				filtered = append(filtered, st)
			}
		}
		healthAll = filtered
	}
	eventLimit := parseLimit("events", 500)
	cfgFull := s.currentConfig()
	usageSnap := s.usageSnapshotWithPrices(cfgFull)
	// Virtual endpoint observability: include expanded counts
	s.runtimeMu.RLock()
	resolver := s.routeResolver
	s.runtimeMu.RUnlock()
	var veList any = cfgFull.VirtualEndpoints
	var rpList any = cfgFull.RouteProfiles
	var cpList any = cfgFull.CandidatePools
	var fcList any = cfgFull.FallbackChains
	if resolver != nil {
		// Enrich VE list with pool membership counts (configured, not runtime eligibility)
		ves := []map[string]any{}
		for _, ve := range cfgFull.VirtualEndpoints {
			poolID := ""
			for _, rp := range cfgFull.RouteProfiles {
				if rp.ID == ve.RouteProfile {
					poolID = rp.CandidatePool
					break
				}
			}
			poolMemberCount := 0
			if poolID != "" {
				if set, ok := resolver.GetExpanded(poolID); ok {
					poolMemberCount = len(set)
				}
			}
			ves = append(ves, map[string]any{
				"id": ve.ID, "name": ve.Name, "enabled": ve.IsEnabled(),
				"public_model": ve.PublicModel, "route_profile": ve.RouteProfile,
				"protocols":                  ve.Protocols,
				"pool_member_count":          poolMemberCount,
				"configured_candidate_count": poolMemberCount,
			})
		}
		veList = ves
		cps := []map[string]any{}
		for _, cp := range cfgFull.CandidatePools {
			item := map[string]any{"id": cp.ID, "name": cp.Name, "mode": cp.Mode, "deployments": cp.Deployments}
			if set, ok := resolver.GetExpanded(cp.ID); ok {
				item["expanded_count"] = len(set)
			}
			cps = append(cps, item)
		}
		cpList = cps
	}
	// Phase D/F: decision plane snapshot
	s.runtimeMu.RLock()
	decisionCfg := cfgFull.Decision
	decisionMetrics := map[string]int64{}
	decisionProviders := map[string]any{}
	if s.decisionOrchestrator != nil {
		decisionMetrics = s.decisionOrchestrator.MetricsSnapshot()
	}
	if s.decisionRegistry != nil {
		decisionProviders = map[string]any{"providers": s.decisionRegistry.Snapshot()}
	}
	// Phase F: external decision providers safe status
	externalProviders := []map[string]any{}
	for _, extCfg := range cfgFull.DecisionProviders {
		// Resolve key configured (no secret) — check actual env var
		keyConfigured := false
		if extCfg.APIKey != "" {
			keyConfigured = true
		} else if extCfg.APIKeyEnv != "" {
			if v := os.Getenv(extCfg.APIKeyEnv); v != "" {
				keyConfigured = true
			}
		}
		// Get health from registry
		healthStatus := "unknown"
		healthMsg := ""
		if s.decisionRegistry != nil {
			if p, ok := s.decisionRegistry.Get(extCfg.ID); ok {
				h := p.Health()
				healthStatus = h.Status
				healthMsg = h.Message
				// If health says api key not configured, then keyConfigured false
				if h.Message == "api key not configured" {
					keyConfigured = false
				}
			}
		}
		item := map[string]any{
			"id":             extCfg.ID,
			"type":           extCfg.Type,
			"enabled":        extCfg.IsEnabled(),
			"health":         healthStatus,
			"key_configured": keyConfigured,
			"privacy_mode":   extCfg.PrivacyMode,
		}
		// Do not expose API key, base URL with secrets, etc.
		if healthMsg != "" && healthMsg != "api key not configured" && healthMsg != "disabled" {
			item["health_message"] = healthMsg
		}
		externalProviders = append(externalProviders, item)
	}
	// Phase G: chain state and provider health (bounded, safe)
	chainsSnapshot := []map[string]any{}
	for _, ch := range cfgFull.DecisionChains {
		stepInfos := []map[string]any{}
		for _, step := range ch.Steps {
			// Determine type for display
			pType := "jev"
			if step.Provider == "local" {
				pType = "local"
			} else if step.Provider == "policy" {
				pType = "policy"
			} else {
				// lookup external type
				for _, ext := range cfgFull.DecisionProviders {
					if ext.ID == step.Provider {
						pType = ext.Type
						break
					}
				}
			}
			item := map[string]any{
				"provider_id": step.Provider,
				"type":        pType,
			}
			if step.TimeoutMS > 0 {
				item["timeout_ms"] = step.TimeoutMS
			}
			// Add runtime health if available
			if s.decisionOrchestrator != nil && s.decisionOrchestrator.ProviderState() != nil {
				if st, ok := s.decisionOrchestrator.ProviderState().SnapshotOne(step.Provider); ok {
					item["state"] = st.Status
					if !st.CooldownUntil.IsZero() {
						item["cooldown_until"] = st.CooldownUntil.Format(time.RFC3339)
						item["cooldown_active"] = !time.Now().Before(st.CooldownUntil) == false && s.decisionOrchestrator.ProviderState().IsCooldown(step.Provider)
						// Use IsCooldown check
						item["cooldown_active"] = s.decisionOrchestrator.ProviderState().IsCooldown(step.Provider)
					} else {
						item["cooldown_active"] = false
					}
					item["consecutive_failures"] = st.ConsecutiveFailures
					if !st.LastFailure.IsZero() {
						item["last_failure"] = st.LastFailure.Format(time.RFC3339)
					}
					if !st.LastSuccess.IsZero() {
						item["last_success"] = st.LastSuccess.Format(time.RFC3339)
					}
				} else {
					item["state"] = "healthy"
					item["cooldown_active"] = false
					item["consecutive_failures"] = 0
				}
			}
			stepInfos = append(stepInfos, item)
		}
		chainsSnapshot = append(chainsSnapshot, map[string]any{
			"id":         ch.ID,
			"step_count": len(ch.Steps),
			"steps":      stepInfos,
		})
	}
	// Provider state snapshot for all decision providers (bounded)
	providerStateSnapshot := map[string]any{}
	if s.decisionOrchestrator != nil && s.decisionOrchestrator.ProviderState() != nil {
		snap := s.decisionOrchestrator.ProviderState().Snapshot()
		safeSnap := map[string]any{}
		for id, st := range snap {
			entry := map[string]any{
				"status":               st.Status,
				"consecutive_failures": st.ConsecutiveFailures,
				"failures_in_window":   st.FailuresInWindow,
				"successes":            st.Successes,
				"cooldown_active":      s.decisionOrchestrator.ProviderState().IsCooldown(id),
			}
			if !st.CooldownUntil.IsZero() {
				entry["cooldown_until"] = st.CooldownUntil.Format(time.RFC3339)
			}
			if !st.LastFailure.IsZero() {
				entry["last_failure"] = st.LastFailure.Format(time.RFC3339)
			}
			if !st.LastSuccess.IsZero() {
				entry["last_success"] = st.LastSuccess.Format(time.RFC3339)
			}
			safeSnap[id] = entry
		}
		providerStateSnapshot = safeSnap
	}
	s.runtimeMu.RUnlock()

	// Phase H: scorecards + evaluation plane. Bounded and privacy-safe: rows carry
	// ids, scores, provenance and sample counts, never model outputs.
	evalPlane := s.evaluationSnapshot()
	scorecardsSection := map[string]any{
		"rows":  []map[string]any{},
		"total": 0,
	}
	evaluationSection := map[string]any{"enabled": false, "suites": []any{}}
	if evalPlane != nil {
		scorecardsSection = map[string]any{
			"rows":             evalPlane.scorecardRows(50),
			"total":            evalPlane.Registry().Len(),
			"by_provenance":    evalPlane.provenanceCounts(),
			"values":           evalPlane.Registry().Stats().Values,
			"quality_coverage": evalPlane.Registry().Stats().QualityCoverage,
		}
		st := evalPlane.Stats()
		evaluationSection = map[string]any{
			"enabled":                   st.Enabled,
			"judge_registered":          st.JudgeRegistered,
			"suites":                    evalPlane.SuiteCatalog(),
			"evaluators":                st.Evaluators,
			"runs_stored":               st.Runs,
			"runs_total":                st.RunsTotal,
			"runs_insufficient_samples": st.RunsInsufficient,
			"runs_rejected":             st.RunsRejected,
			"scorecards_written":        st.ScorecardsWritten,
			"imported_scorecards":       st.Imported,
			"import_error":              st.ImportError,
			"state_path":                st.StatePath,
			"state_writes_failed":       st.StateWritesFailed,
			"verdict_counts":            evalPlane.verdictCounts(),
			"health":                    evalPlane.evaluationHealthRows(50),
			"note":                      "Phase H supports offline replay and opt-in live physical-deployment evaluation; scorecards never change routing in Phase H",
		}
	}

	writeJSON(w, 200, map[string]any{
		"deployments":        deployments,
		"deployment_total":   totalDeployments,
		"snapshot_truncated": truncated,
		"health":             healthAll,
		"health_counts":      healthCounts,
		"provider_health":    s.hm.ProviderSnapshot(),
		"events":             s.bus.SnapshotLimit(eventLimit),
		"provider_stats":     s.reg.Stats(),
		"provider_pressure":  providerPressure(s.reg.Stats()),
		"scope_health":       scopeHealthRows(healthAll),
		"session_count":      s.rt.SessionCount(),
		"probe_stats":        s.probe.Stats(),
		"request_total":      s.requestTotal.Load(),
		"version":            gatewayVersion,
		"usage":              usageSnap,
		"cache":              s.respCache.Stats(),
		"client_auth": map[string]any{
			"enabled": cfgFull.ClientAuth.Enabled,
			"keys":    len(cfgFull.ClientAuth.Keys),
			"rpm":     cfgFull.ClientAuth.RPM,
		},
		"virtual_endpoints": veList,
		"route_profiles":    rpList,
		"candidate_pools":   cpList,
		"fallback_chains":   fcList,
		"decision": map[string]any{
			"config":                      decisionCfg,
			"metrics":                     decisionMetrics,
			"providers":                   decisionProviders,
			"external_decision_providers": externalProviders,
			"chains":                      chainsSnapshot,
			"provider_state":              providerStateSnapshot,
		},
		"decision_chains":          cfgFull.DecisionChains,
		"decision_provider_health": cfgFull.DecisionProviderHealth,
		"scorecards":               scorecardsSection,
		"evaluation":               evaluationSection,
		"config": map[string]any{
			"probe":    probeCfg,
			"routing":  routingCfg,
			"decision": decisionCfg,
		},
	})
}

// providerPressure maps registry adapter stats onto the dashboard's
// provider-pressure table shape.
func providerPressure(stats []providers.ProviderStats) []map[string]any {
	out := make([]map[string]any, 0, len(stats))
	for _, st := range stats {
		out = append(out, map[string]any{
			"provider":                     st.ID,
			"provider_id":                  st.ID,
			"active":                       st.ActiveRequests,
			"waiting":                      st.WaitingRequests,
			"capacity":                     st.MaxConcurrency,
			"credentials":                  st.Credentials,
			"cooling":                      st.CredentialsCooling,
			"request_limit":                st.RequestLimit,
			"remaining_requests":           st.RemainingRequests,
			"reserved_requests":            st.ReservedRequests,
			"effective_remaining_requests": st.EffectiveRemainingRequests,
			"token_limit":                  st.TokenLimit,
			"remaining_tokens":             st.RemainingTokens,
			"reserved_tokens":              st.ReservedTokens,
			"effective_remaining_tokens":   st.EffectiveRemainingTokens,
			"request_reset":                st.RequestResetUnix,
			"token_reset":                  st.TokenResetUnix,
			"rate_limit_reset":             st.RateLimitResetUnix,
		})
	}
	return out
}

var scopeSeverity = map[health.Status]int{
	health.Healthy:  0,
	health.Unknown:  1,
	health.HalfOpen: 2,
	health.Degraded: 3,
	health.Cooldown: 4,
}

// scopeHealthRows condenses per-scope circuit states into one row per
// deployment whose scopes are not all healthy, matching the dashboard's
// capability-evidence list.
func scopeHealthRows(states []health.State) []map[string]any {
	out := []map[string]any{}
	for _, st := range states {
		if len(st.Scopes) == 0 {
			continue
		}
		names := make([]string, 0, len(st.Scopes))
		worst := health.Healthy
		worstFails := 0
		anyUnhealthy := false
		for name, sc := range st.Scopes {
			names = append(names, name)
			if scopeSeverity[sc.Status] > scopeSeverity[worst] {
				worst = sc.Status
			}
			if sc.Status != health.Healthy {
				anyUnhealthy = true
				if sc.ConsecutiveFailures > worstFails {
					worstFails = sc.ConsecutiveFailures
				}
			}
		}
		if !anyUnhealthy {
			continue
		}
		sort.Strings(names)
		out = append(out, map[string]any{
			"deployment":           st.Deployment,
			"scopes":               names,
			"status":               worst,
			"consecutive_failures": worstFails,
		})
	}
	return out
}

func (s *Server) adminProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, 405, "method not allowed")
		return
	}
	// Reading the JSON body enforces the application/json content-type gate.
	// Without it a cross-site simple POST (text/plain, no CORS preflight)
	// could repeatedly trigger full probe sweeps against every upstream.
	var body struct {
		Reason string `json:"reason"`
	}
	if _, err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	wait := r.URL.Query().Get("wait") == "1" || strings.EqualFold(r.URL.Query().Get("wait"), "true")
	if wait {
		result := s.probe.RunOnce(r.Context())
		writeJSON(w, 200, map[string]any{"completed": true, "result": result})
		return
	}
	s.probe.Trigger()
	writeJSON(w, 202, map[string]any{"accepted": true})
}

func (s *Server) adminProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		out := make([]map[string]any, 0, len(cfg.Providers))
		for _, p := range cfg.Providers {
			out = append(out, providerSummary(p))
		}
		writeJSON(w, 200, map[string]any{"providers": out})
	case http.MethodPost:
		var in providerForm
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		normalizeProvider(&in.Provider)
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			if cfg.ProviderIndex(in.Provider.ID) >= 0 {
				return errAdminProviderExists
			}
			cfg.Providers = append(cfg.Providers, in.Provider)
			return nil
		}); err != nil {
			if errors.Is(err, errAdminProviderExists) {
				errorJSON(w, 409, err.Error())
			} else {
				errorJSON(w, 400, err.Error())
			}
			return
		}
		s.probe.Trigger()
		writeJSON(w, 201, map[string]any{"saved": true, "provider": providerSummary(in.Provider)})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}

func (s *Server) adminProviderByID(w http.ResponseWriter, r *http.Request) {
	id, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/admin/api/providers/"))
	if err != nil || strings.TrimSpace(id) == "" {
		errorJSON(w, 400, "provider id required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		idx := cfg.ProviderIndex(id)
		if idx < 0 {
			errorJSON(w, 404, "provider not found")
			return
		}
		p := cfg.Providers[idx]
		reveal := r.URL.Query().Get("reveal") == "1" || r.URL.Query().Get("reveal") == "true"
		payload := map[string]any{
			"provider":      p,
			"secret_source": secretSource(p),
			"has_secret":    len(p.ResolvedCredentials()) > 0,
		}
		if reveal {
			payload["resolved_api_key"] = p.ResolvedAPIKey()
		} else {
			p.APIKey = ""
			for i := range p.Credentials {
				p.Credentials[i].APIKey = ""
			}
			payload["provider"] = p
		}
		writeJSON(w, 200, payload)

	case http.MethodPut:
		var in providerForm
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		var saved config.ProviderConfig
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			idx := cfg.ProviderIndex(id)
			if idx < 0 {
				return errAdminProviderNotFound
			}
			old := cfg.Providers[idx]
			if in.Provider.ID == "" {
				in.Provider.ID = old.ID
			}
			if in.Provider.ID != old.ID && cfg.ProviderIndex(in.Provider.ID) >= 0 {
				return errAdminProviderExists
			}
			if in.PreserveSecret {
				in.Provider.APIKey = old.APIKey
				in.Provider.APIKeyEnv = old.APIKeyEnv
				in.Provider.Credentials = old.Credentials
			}
			dropEnvResolvedLiteral(&in.Provider, old)
			normalizeProvider(&in.Provider)
			cfg.Providers[idx] = in.Provider
			saved = in.Provider
			return nil
		}); err != nil {
			switch {
			case errors.Is(err, errAdminProviderNotFound):
				errorJSON(w, 404, err.Error())
			case errors.Is(err, errAdminProviderExists):
				errorJSON(w, 409, err.Error())
			default:
				errorJSON(w, 400, err.Error())
			}
			return
		}
		s.probe.Trigger()
		writeJSON(w, 200, map[string]any{"saved": true, "provider": providerSummary(saved)})

	case http.MethodDelete:
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			idx := cfg.ProviderIndex(id)
			if idx < 0 {
				return errAdminProviderNotFound
			}
			cfg.Providers = append(cfg.Providers[:idx], cfg.Providers[idx+1:]...)
			return nil
		}); err != nil {
			if errors.Is(err, errAdminProviderNotFound) {
				errorJSON(w, 404, err.Error())
			} else {
				errorJSON(w, 400, err.Error())
			}
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": true, "id": id})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}

func (s *Server) adminProviderTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, 405, "method not allowed")
		return
	}
	var in providerForm
	if _, err := readJSON(r, &in); err != nil {
		errorJSON(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if in.PreserveSecret {
		mergeExistingSecret(s.currentConfig(), &in.Provider)
	}
	normalizeProvider(&in.Provider)
	if in.Provider.ID == "" || in.Provider.BaseURL == "" {
		errorJSON(w, 400, "provider id and base_url are required")
		return
	}
	if in.Provider.Type != "openai_compatible" && in.Provider.Type != "anthropic_compatible" && in.Provider.Type != "gemini" && in.Provider.Type != "openai_responses" {
		errorJSON(w, 400, "unsupported provider type")
		return
	}
	a, err := providers.NewAdapter(in.Provider, 10*time.Second)
	if err != nil {
		errorJSON(w, 400, err.Error())
		return
	}
	defer providers.CloseIdleConnections(a)
	models := uniqueStrings(in.TestModels)
	if len(models) == 0 {
		for _, m := range in.Provider.Models {
			if m.Enabled {
				models = append(models, m.Model)
			}
		}
	}
	models = uniqueStrings(models)
	if len(models) == 0 {
		errorJSON(w, 400, "select or enter at least one model to test")
		return
	}
	if len(models) > 100 {
		models = models[:100]
	}
	for _, model := range models {
		if len(model) > 1024 {
			errorJSON(w, 400, "model id exceeds safe limit 1024 bytes")
			return
		}
	}

	results := make([]testResult, len(models))
	var wg sync.WaitGroup
	limit := in.Provider.MaxConcurrency
	if limit < 1 || limit > 32 {
		limit = 16
	}
	sem := make(chan struct{}, limit)
	parentCtx := r.Context()
	for i, model := range models {
		wg.Add(1)
		go func(i int, model string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-parentCtx.Done():
				results[i] = testResult{Model: model, Error: parentCtx.Err().Error()}
				return
			}
			switch in.Mode {
			case "full":
				ctx, cancel := context.WithTimeout(parentCtx, 120*time.Second)
				defer cancel()
				tr := testResult{Model: model}
				var report compat.ProbeReport
				switch in.Provider.Type {
				case "anthropic_compatible":
					report = compat.RunCapabilitySuiteAnthropic(ctx, adapterTransport{a: a}, in.Provider.ID, model)
				case "openai_responses":
					report = compat.RunCapabilitySuiteResponses(ctx, adapterTransport{a: a}, in.Provider.ID, model)
				default:
					report = compat.RunCapabilitySuite(ctx, adapterTransport{a: a}, in.Provider.ID, model, in.Provider.Dialect)
				}
				tr.CapabilityReport = &report
				tr.OK = report.OK && report.TransportFail == 0
				results[i] = tr
			case "claude_code":
				ctx, cancel := context.WithTimeout(parentCtx, 120*time.Second)
				defer cancel()
				tr := testResult{Model: model}
				var report compat.ProbeReport
				if in.Provider.Type == "openai_responses" {
					report = compat.RunAgentLoopSimulationResponses(ctx, adapterTransport{a: a}, in.Provider.ID, model)
				} else {
					report = compat.RunAgentLoopSimulation(ctx, adapterTransport{a: a}, in.Provider.ID, model)
				}
				tr.AgentReport = &report
				tr.OK = report.OK
				results[i] = tr
			default:
				ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
				defer cancel()
				lat, status, e := a.Probe(ctx, model, 1)
				tr := testResult{Model: model, OK: e == nil, StatusCode: status, LatencyMS: lat.Milliseconds()}
				if e != nil {
					tr.Error = e.Error()
				}
				results[i] = tr
			}
		}(i, model)
	}
	wg.Wait()
	if parentCtx.Err() != nil {
		return
	}
	passed := 0
	for _, x := range results {
		if x.OK {
			passed++
		}
	}
	writeJSON(w, 200, map[string]any{"ok": passed == len(results), "passed": passed, "total": len(results), "results": results})
}

func (s *Server) adminProviderDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, 405, "method not allowed")
		return
	}
	var in providerForm
	if _, err := readJSON(r, &in); err != nil {
		errorJSON(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if in.PreserveSecret {
		mergeExistingSecret(s.currentConfig(), &in.Provider)
	}
	normalizeProvider(&in.Provider)
	if in.Provider.BaseURL == "" {
		errorJSON(w, 400, "base_url is required")
		return
	}
	models, status, err := discoverModels(r.Context(), in.Provider)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "status_code": status, "models": []string{}, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "status_code": status, "models": models})
}

func providerSummary(p config.ProviderConfig) map[string]any {
	models := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		models = append(models, m.Model)
	}
	return map[string]any{
		"id":               p.ID,
		"name":             p.Name,
		"type":             p.Type,
		"base_url":         p.BaseURL,
		"auth_mode":        p.AuthMode,
		"enabled":          p.Enabled,
		"models":           models,
		"model_count":      len(models),
		"has_secret":       len(p.ResolvedCredentials()) > 0,
		"api_key_env":      p.APIKeyEnv,
		"credential_count": len(p.ResolvedCredentials()),
		"proxy_url":        p.ProxyURL,
		"max_concurrency":  p.MaxConcurrency,
	}
}

func secretSource(p config.ProviderConfig) string {
	if len(p.Credentials) > 0 {
		return "pool"
	}
	if p.APIKeyEnv != "" {
		return "env"
	}
	if p.APIKey != "" {
		return "literal"
	}
	return "none"
}

// dropEnvResolvedLiteral prevents an environment-provided secret from being
// persisted as a file literal. A dashboard round-trip echoes the resolved key
// back; when the submitted literal equals the current env value the literal
// is dropped and the env reference kept.
func dropEnvResolvedLiteral(in *config.ProviderConfig, old config.ProviderConfig) {
	if in.APIKeyEnv == "" || in.APIKey == "" {
		return
	}
	resolved := os.Getenv(in.APIKeyEnv)
	if resolved == "" && old.APIKeyEnv == in.APIKeyEnv && old.APIKey == in.APIKey {
		resolved = old.APIKey
	}
	if resolved != "" && subtle.ConstantTimeCompare([]byte(in.APIKey), []byte(resolved)) == 1 {
		in.APIKey = ""
	}
}

func mergeExistingSecret(cfg config.Config, p *config.ProviderConfig) {
	if i := cfg.ProviderIndex(p.ID); i >= 0 {
		p.APIKey = cfg.Providers[i].APIKey
		p.APIKeyEnv = cfg.Providers[i].APIKeyEnv
		p.Credentials = cfg.Providers[i].Credentials
	}
}

func normalizeProvider(p *config.ProviderConfig) {
	p.ApplyDefaults()
	for i := range p.Models {
		p.Models[i].Model = strings.TrimSpace(p.Models[i].Model)
		if p.Models[i].ID == "" {
			p.Models[i].ID = slug(p.Models[i].Model)
		}
		if p.Models[i].Weight == 0 {
			p.Models[i].Weight = 1
		}
	}
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else if r == '/' || r == ' ' || r == ':' {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		out = "model"
	}
	return out
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func discoverModels(ctx context.Context, p config.ProviderConfig) ([]string, int, error) {
	p.ApplyDefaults()
	a, err := providers.NewAdapter(p, 8*time.Second)
	if err != nil {
		return nil, 0, err
	}
	defer providers.CloseIdleConnections(a)
	paths := []string{p.ModelsPath}
	if p.ModelsPath == "/v1/models" {
		paths = append(paths, "/models")
	}
	var lastErr error
	lastStatus := 0
	for _, path := range uniqueStrings(paths) {
		resp, err := a.DoPath(ctx, http.MethodGet, path, nil, false, nil)
		if err != nil {
			lastErr = err
			continue
		}
		lastStatus = resp.StatusCode
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("model discovery read: %w", readErr)
			continue
		}
		if len(b) > 4<<20 {
			lastErr = fmt.Errorf("model discovery response exceeds 4194304 bytes")
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b = redactProviderBody(p, b)
			const maxDiscoveryErrorBytes = 2048
			if len(b) > maxDiscoveryErrorBytes {
				b = append(append([]byte(nil), b[:maxDiscoveryErrorBytes]...), []byte("…")...)
			}
			lastErr = fmt.Errorf("model discovery HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
			continue
		}
		models := parseModelList(b)
		if len(models) == 0 {
			lastErr = fmt.Errorf("model endpoint returned no model ids")
			continue
		}
		return models, resp.StatusCode, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("model discovery failed")
	}
	return nil, lastStatus, lastErr
}

func parseModelList(b []byte) []string {
	const maxModels = 10000
	const maxModelIDBytes = 1024
	var root any
	if json.Unmarshal(b, &root) != nil {
		return nil
	}
	out := []string{}
	add := func(v any) {
		if len(out) >= maxModels {
			return
		}
		switch x := v.(type) {
		case string:
			x = strings.TrimSpace(x)
			if strings.HasPrefix(x, "models/") {
				x = strings.TrimPrefix(x, "models/")
			}
			if len(x) <= maxModelIDBytes {
				out = append(out, x)
			}
		case map[string]any:
			for _, key := range []string{"id", "model", "name"} {
				if id, _ := x[key].(string); strings.TrimSpace(id) != "" {
					if strings.HasPrefix(id, "models/") {
						id = strings.TrimPrefix(id, "models/")
					}
					if len(id) <= maxModelIDBytes {
						out = append(out, id)
					}
					return
				}
			}
		}
	}
	switch obj := root.(type) {
	case []any:
		for _, item := range obj {
			add(item)
		}
	case map[string]any:
		for _, key := range []string{"data", "models", "items"} {
			if arr, ok := obj[key].([]any); ok {
				for _, item := range arr {
					add(item)
				}
			}
		}
	}
	return uniqueStrings(out)
}

func applyProviderHeaders(req *http.Request, p config.ProviderConfig) {
	// Custom headers first. Explicit configured auth then wins, preventing stale
	// Authorization/x-api-key headers from silently replacing the real key.
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	key := p.ResolvedAPIKey()
	if key != "" && p.AuthMode != "none" {
		mode := p.AuthMode
		if mode == "" {
			if p.Type == "anthropic_compatible" {
				mode = "x-api-key"
			} else {
				mode = "bearer"
			}
		}
		if mode == "x-api-key" {
			req.Header.Set("x-api-key", key)
		} else {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	if p.Type == "anthropic_compatible" && req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
}
