package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
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
}

type testResult struct {
	Model      string `json:"model"`
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code"`
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
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
	cfg := s.currentConfig()
	s.runtimeMu.RLock()
	deployments := s.rt.All()
	s.runtimeMu.RUnlock()
	writeJSON(w, 200, map[string]any{
		"deployments":    deployments,
		"health":         s.hm.Snapshot(),
		"events":         s.bus.Snapshot(),
		"provider_stats": s.reg.Stats(),
		"session_count":  s.rt.SessionCount(),
		"config": map[string]any{
			"probe":   cfg.Probe,
			"routing": cfg.Routing,
		},
	})
}

func (s *Server) adminProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, 405, "method not allowed")
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
	if in.Provider.Type != "openai_compatible" && in.Provider.Type != "anthropic_compatible" {
		errorJSON(w, 400, "unsupported provider type")
		return
	}
	a, err := providers.NewAdapter(in.Provider, 10*time.Second)
	if err != nil {
		errorJSON(w, 400, err.Error())
		return
	}
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
	for i, model := range models {
		wg.Add(1)
		go func(i int, model string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			lat, status, e := a.Probe(ctx, model, 1)
			tr := testResult{Model: model, OK: e == nil, StatusCode: status, LatencyMS: lat.Milliseconds()}
			if e != nil {
				tr.Error = e.Error()
			}
			results[i] = tr
		}(i, model)
	}
	wg.Wait()
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
