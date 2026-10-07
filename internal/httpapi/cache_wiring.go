package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/cache"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// Exact-match response cache (opt-in, Cloudflare-AI-Gateway-inspired)
//
// The cache is deliberately conservative. A request is only cacheable when
// the operator enabled caching AND the request is complete (non-streaming)
// AND its sampling parameters are deterministic (temperature absent or 0,
// top_p absent or 1) AND the body fits the configured byte budget. Every hit
// is observable via the X-NexaRoute-Cache response header, a cache_hit event,
// and hit/miss counters in metrics and the dashboard.
//
// The whole table is invalidated on every successful config swap, so a cached
// response can never outlive the routing topology that produced it. Clients
// can force a fresh evaluation per request with the x-nexaroute-no-cache
// header.

func (s *Server) cacheLookupFor(r *http.Request, path string, body []byte, stream bool, temperature, topP *float64, candidates []router.Scored, generation uint64) (string, bool) {
	cfg := s.currentConfig()
	if !cfg.Cache.Enabled || stream {
		return "", false
	}
	if temperature != nil && *temperature != 0 {
		return "", false
	}
	if topP != nil && *topP != 1 {
		return "", false
	}
	if len(body) > cfg.Cache.MaxBodyBytes {
		return "", false
	}
	providerIDs := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		providerIDs[candidate.Deployment.ProviderID] = struct{}{}
	}
	forwardNames := map[string]struct{}{}
	for _, name := range []string{"x-claude-code-session-id", "x-litellm-session-id", "x-litellm-trace-id", "x-session-id"} {
		forwardNames[http.CanonicalHeaderKey(name)] = struct{}{}
	}
	for _, provider := range cfg.Providers {
		if _, eligible := providerIDs[provider.ID]; !eligible {
			continue
		}
		for _, name := range provider.ForwardHeaders {
			if name != "" {
				forwardNames[http.CanonicalHeaderKey(name)] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(forwardNames))
	for name := range forwardNames {
		names = append(names, name)
	}
	sort.Strings(names)
	forwarded := make(map[string][]string, len(names))
	for _, name := range names {
		forwarded[name] = append([]string(nil), r.Header.Values(name)...)
	}
	clientKeyDigest := ""
	if cfg.ClientAuth.Enabled {
		clientKeyDigest = keyDigest(extractClientKey(r))
	}
	contextBytes, err := json.Marshal(struct {
		Generation       uint64              `json:"generation"`
		ClientKeyDigest  string              `json:"client_key_digest,omitempty"`
		ForwardedHeaders map[string][]string `json:"forwarded_headers,omitempty"`
	}{Generation: generation, ClientKeyDigest: clientKeyDigest, ForwardedHeaders: forwarded})
	if err != nil {
		return "", false
	}
	return cache.KeyWithContext(path, body, contextBytes), true
}

// cacheServe writes a cache hit and returns true when the request is complete.
func (s *Server) cacheServe(w http.ResponseWriter, r *http.Request, key string, cacheable bool) bool {
	if !cacheable {
		return false
	}
	if r.Header.Get("x-nexaroute-no-cache") != "" {
		s.respCache.Bypass()
		return false
	}
	entry, ok := s.respCache.Lookup(key)
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", entry.ContentType)
	w.Header().Set("X-NexaRoute-Cache", "HIT")
	w.Header().Set("X-Gateway-Deployment", entry.Deployment)
	w.WriteHeader(entry.Status)
	_, _ = w.Write(entry.Body)
	s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "cache_hit", Deployment: entry.Deployment, Message: "served from exact-match response cache"})
	return true
}

// cacheStoreResponse stores a complete client-facing response body.
func (s *Server) cacheStoreResponse(key string, generation uint64, cacheable bool, deploymentID string, status int, contentType string, body []byte) {
	if !cacheable || status != http.StatusOK || len(body) == 0 {
		return
	}
	cfg, currentGeneration := s.cacheConfigSnapshot()
	if !cfg.Cache.Enabled || generation != currentGeneration {
		return
	}
	s.respCache.Store(key, cache.Entry{
		Body:        body,
		ContentType: contentType,
		Status:      status,
		CreatedAt:   time.Now(),
		Deployment:  deploymentID,
	})
}

// proxyOpenAINativeJSON performs the validated non-stream passthrough while
// observing real usage and feeding the response cache.
func (s *Server) proxyOpenAINativeJSON(w http.ResponseWriter, r *http.Request, resp *http.Response, deploymentID, cacheKey string, generation uint64, cacheable bool) error {
	defer resp.Body.Close()
	b, err := readJSONLimited(resp.Body)
	if err != nil {
		return err
	}
	if err := validateOpenAIResponseJSON(b); err != nil {
		return err
	}
	if p, c, ok := extractOpenAIUsage(b); ok {
		s.recordUsage(r, deploymentID, int64(p), int64(c))
	}
	s.cacheStoreResponse(cacheKey, generation, cacheable, deploymentID, resp.StatusCode, "application/json", b)
	copyUpstreamResponseHeaders(w, resp, false)
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(b)
	return err
}

// extractOpenAIUsage pulls prompt/completion token counts from a complete
// OpenAI Chat Completions response body.
func extractOpenAIUsage(b []byte) (int, int, bool) {
	var env struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &env); err != nil || env.Usage == nil {
		return 0, 0, false
	}
	p, c := env.Usage.PromptTokens, env.Usage.CompletionTokens
	if p <= 0 && c <= 0 {
		return 0, 0, false
	}
	return p, c, true
}

// extractAnthropicUsage pulls input/output token counts from a complete
// Anthropic Messages response body.
func extractAnthropicUsage(b []byte) (int, int, bool) {
	var env struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &env); err != nil || env.Usage == nil {
		return 0, 0, false
	}
	p, c := env.Usage.InputTokens, env.Usage.OutputTokens
	if p <= 0 && c <= 0 {
		return 0, 0, false
	}
	return p, c, true
}

// usageSnapshotWithPrices builds the cumulative usage snapshot with estimated
// cost derived from the live model pricing configuration. Deployments without
// configured pricing report tokens with zero cost rather than a guess.
func (s *Server) usageSnapshotWithPrices(cfg config.Config) usage.Snapshot {
	cfg = s.currentConfig()
	prices := make(map[string]usage.Price, len(cfg.Providers)*4)
	for _, p := range cfg.Providers {
		for _, m := range p.Models {
			prices[p.ID+"/"+m.ID] = usage.Price{InputPerMTok: m.InputCostPerMTok, OutputPerMTok: m.OutputCostPerMTok}
		}
	}
	return s.usage.Snapshot(prices)
}
