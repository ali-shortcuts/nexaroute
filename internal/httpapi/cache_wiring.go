package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/cache"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
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

type responseCacheKey struct {
	value      string
	generation uint64
}

type scopedHeader struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// cacheRequestScope includes every request input that can affect the upstream
// reply or route. Only a SHA-256 digest of this scope is kept in the cache;
// client credentials and forwarded header values are never stored in clear.
func cacheRequestScope(r *http.Request, cfg config.Config) []byte {
	names := map[string]struct{}{}
	for _, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		for _, h := range p.ForwardHeaders {
			if !blockedClientForwardHeader(h) {
				names[http.CanonicalHeaderKey(strings.TrimSpace(h))] = struct{}{}
			}
		}
	}
	// Session affinity affects deployment choice even if the provider does
	// not forward any of these headers.
	for _, h := range []string{"x-claude-code-session-id", "x-litellm-session-id", "x-litellm-trace-id", "x-session-id"} {
		names[http.CanonicalHeaderKey(h)] = struct{}{}
	}
	keys := make([]string, 0, len(names))
	for h := range names {
		keys = append(keys, h)
	}
	sort.Strings(keys)
	headers := make([]scopedHeader, 0, len(keys))
	for _, h := range keys {
		if values := r.Header.Values(h); len(values) > 0 {
			headers = append(headers, scopedHeader{Name: h, Values: values})
		}
	}
	scope, _ := json.Marshal(struct {
		ClientKey string         `json:"client_key"`
		Headers   []scopedHeader `json:"headers"`
	}{ClientKey: extractClientKey(r), Headers: headers})
	return scope
}

func (s *Server) cacheLookupFor(r *http.Request, body []byte, stream bool, temperature, topP *float64) (responseCacheKey, bool) {
	cfg := s.currentConfig()
	if !cfg.Cache.Enabled || stream {
		return responseCacheKey{}, false
	}
	if temperature != nil && *temperature != 0 {
		return responseCacheKey{}, false
	}
	if topP != nil && *topP != 1 {
		return responseCacheKey{}, false
	}
	if len(body) > cfg.Cache.MaxBodyBytes {
		return responseCacheKey{}, false
	}
	return responseCacheKey{
		value:      cache.KeyScoped(r.URL.Path, body, cacheRequestScope(r, cfg)),
		generation: s.respCache.Generation(),
	}, true
}

// cacheServe writes a cache hit and returns true when the request is complete.
func (s *Server) cacheServe(w http.ResponseWriter, r *http.Request, key responseCacheKey, cacheable bool) bool {
	if !cacheable {
		return false
	}
	if r.Header.Get("x-nexaroute-no-cache") != "" {
		s.respCache.Bypass()
		return false
	}
	entry, ok := s.respCache.LookupForGeneration(key.value, key.generation)
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
func (s *Server) cacheStoreResponse(key responseCacheKey, cacheable bool, deploymentID string, status int, contentType string, body []byte) {
	if !cacheable || status != http.StatusOK || len(body) == 0 {
		return
	}
	cfg := s.currentConfig()
	if !cfg.Cache.Enabled {
		return
	}
	s.respCache.StoreForGeneration(key.value, key.generation, cache.Entry{
		Body:        body,
		ContentType: contentType,
		Status:      status,
		CreatedAt:   time.Now(),
		Deployment:  deploymentID,
	})
}

// proxyOpenAINativeJSON performs the validated non-stream passthrough while
// observing real usage and feeding the response cache.
func (s *Server) proxyOpenAINativeJSON(w http.ResponseWriter, resp *http.Response, deploymentID string, cacheKey responseCacheKey, cacheable bool) error {
	defer resp.Body.Close()
	b, err := readJSONLimited(resp.Body)
	if err != nil {
		return err
	}
	if err := validateOpenAIResponseJSON(b); err != nil {
		return err
	}
	if p, c, ok := extractOpenAIUsage(b); ok {
		s.usage.Record(deploymentID, int64(p), int64(c))
	}
	s.cacheStoreResponse(cacheKey, cacheable, deploymentID, resp.StatusCode, "application/json", b)
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
