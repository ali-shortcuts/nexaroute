package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/cache"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Semantic response cache wiring (M5-4). A separately opt-in layer
// beside the exact-match cache: prompts are embedded through a
// configured deployment and matched by cosine similarity INSIDE an
// isolation scope (client key digest + ingress path + model +
// forwarded-header context — the same isolation ingredients as the
// exact cache). Entries carry privacy provenance; a no_training
// request is only ever served by an entry stored under no_training
// whose origin provider declares trains_on_data="no". Any embedder
// or eligibility failure bypasses silently: the semantic cache can
// only accelerate a request, never fail one.

type semanticContext struct {
	scope          string
	vector         []float32
	promptSHA256   string
	embeddingDep   string
	requireNoTrain bool
}

// semanticPrepare computes the request's semantic context, or nil
// when the request is ineligible (disabled, streaming,
// non-deterministic sampling, tools present, oversized, no
// extractable prompt text, or the embedder failed).
func (s *Server) semanticPrepare(r *http.Request, path string, raw []byte, stream bool, temperature, topP *float64, hasTools bool, model string, requireNoTraining bool, candidates []router.Scored) *semanticContext {
	cfg := s.currentConfig()
	sem := cfg.Cache.Semantic
	if !sem.Enabled || stream || hasTools {
		return nil
	}
	if r.Header.Get("x-nexaroute-no-cache") != "" {
		return nil
	}
	if temperature != nil && *temperature != 0 {
		return nil
	}
	if topP != nil && *topP != 1 {
		return nil
	}
	if len(raw) > cfg.Cache.MaxBodyBytes {
		return nil
	}
	text, ok := semanticPromptText(raw)
	if !ok {
		return nil
	}
	scope := s.semanticScope(r, path, model, candidates)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	vector, err := s.embedText(ctx, text)
	if err != nil || len(vector) == 0 {
		return nil
	}
	sum := sha256.Sum256([]byte(text))
	return &semanticContext{
		scope:          scope,
		vector:         vector,
		promptSHA256:   hex.EncodeToString(sum[:]),
		embeddingDep:   sem.EmbeddingDeployment,
		requireNoTrain: requireNoTraining,
	}
}

// semanticScope mirrors the exact cache's isolation context
// (client key digest + forwarded headers of eligible providers),
// extended with the ingress path and requested model — similarity
// is only ever computed inside one scope.
func (s *Server) semanticScope(r *http.Request, path, model string, candidates []router.Scored) string {
	cfg := s.currentConfig()
	providerIDs := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		providerIDs[c.Deployment.ProviderID] = struct{}{}
	}
	forwardNames := map[string]struct{}{}
	for _, name := range []string{"x-claude-code-session-id", "x-litellm-session-id", "x-litellm-trace-id", "x-session-id"} {
		forwardNames[http.CanonicalHeaderKey(name)] = struct{}{}
	}
	for _, p := range cfg.Providers {
		if _, eligible := providerIDs[p.ID]; !eligible {
			continue
		}
		for _, name := range p.ForwardHeaders {
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
		Path            string              `json:"path"`
		Model           string              `json:"model"`
		ClientKeyDigest string              `json:"client_key_digest,omitempty"`
		Forwarded       map[string][]string `json:"forwarded_headers,omitempty"`
	}{Path: path, Model: model, ClientKeyDigest: clientKeyDigest, Forwarded: forwarded})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(contextBytes)
	return hex.EncodeToString(sum[:])
}

// semanticServe writes a semantic hit and returns true when one
// was found. Provenance travels in a response header and an event
// so a served-from-cache answer is always attributable.
func (s *Server) semanticServe(w http.ResponseWriter, r *http.Request, sc *semanticContext) bool {
	if sc == nil || sc.scope == "" || s.semCache == nil {
		return false
	}
	entry, score, ok := s.semCache.Lookup(sc.scope, sc.vector, sc.requireNoTrain)
	if !ok {
		return false
	}
	// A semantic hit is a zero-upstream-spend serve, exactly like an
	// exact-match hit: the admission hold must be released, not left
	// to expire into a committed charge 15 minutes later.
	s.budgetRelease(r)
	w.Header().Set("Content-Type", entry.ContentType)
	w.Header().Set("X-NexaRoute-Cache", "SEMANTIC-HIT")
	w.Header().Set("X-Gateway-Deployment", entry.Deployment)
	w.Header().Set("X-Gateway-Cache-Provenance", fmt.Sprintf("deployment=%s;trains_on_data=%s;embedding=%s;score=%.4f", entry.Deployment, entry.TrainsOnData, entry.Embedding, score))
	w.WriteHeader(entry.Status)
	_, _ = w.Write(entry.Body)
	s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "semantic_cache_hit", Deployment: entry.Deployment, Message: fmt.Sprintf("served from semantic cache (score %.4f, origin trains_on_data=%s)", score, entry.TrainsOnData)})
	return true
}

// semanticStore records a complete upstream response in the
// semantic cache with its provenance, under the same generation
// guard as the exact cache.
func (s *Server) semanticStore(sc *semanticContext, generation uint64, deploymentID string, status int, body []byte) {
	if sc == nil || status != http.StatusOK || len(body) == 0 || s.semCache == nil {
		return
	}
	cfg, currentGeneration := s.cacheConfigSnapshot()
	if !cfg.Cache.Semantic.Enabled || generation != currentGeneration {
		return
	}
	providerID := deploymentID
	if i := strings.Index(deploymentID, "/"); i > 0 {
		providerID = deploymentID[:i]
	}
	trains := "unknown"
	sourceModel := ""
	if p, ok := s.providerConfigFor(providerID); ok {
		trains = p.DataHandling.TrainsOnData
		for _, m := range p.Models {
			if providerID+"/"+m.ID == deploymentID {
				sourceModel = m.Model
			}
		}
	}
	s.semCache.Store(cache.SemanticEntry{
		Body:         body,
		ContentType:  "application/json",
		Status:       status,
		CreatedAt:    time.Now(),
		Vector:       sc.vector,
		Scope:        sc.scope,
		Deployment:   deploymentID,
		TrainsOnData: trains,
		NoTraining:   sc.requireNoTrain,
		Embedding:    sc.embeddingDep,
		PromptSHA256: sc.promptSHA256,
		SourceModel:  sourceModel,
	})
}

// embedText embeds text through the configured embedding
// deployment's own provider adapter. The call goes straight to the
// adapter — never through the gateway's HTTP handlers — so the
// semantic cache cannot recurse into itself.
func (s *Server) embedText(ctx context.Context, text string) ([]float32, error) {
	cfg := s.currentConfig()
	sem := cfg.Cache.Semantic
	parts := strings.SplitN(sem.EmbeddingDeployment, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid embedding deployment")
	}
	p, ok := s.providerConfigFor(parts[0])
	if !ok {
		return nil, fmt.Errorf("embedding provider not found")
	}
	upstreamModel := ""
	for _, m := range p.Models {
		if m.ID == parts[1] {
			upstreamModel = m.Model
		}
	}
	if upstreamModel == "" {
		return nil, fmt.Errorf("embedding model not found")
	}
	a, ok := s.reg.Get(p.ID)
	if !ok {
		return nil, fmt.Errorf("embedding adapter not found")
	}
	payload, err := json.Marshal(map[string]any{"model": upstreamModel, "input": text})
	if err != nil {
		return nil, err
	}
	resp, err := a.DoPath(ctx, http.MethodPost, sem.EmbeddingPath, payload, false, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding upstream status %d", resp.StatusCode)
	}
	body, err := readJSONLimited(resp.Body)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embedding response has no vector")
	}
	return parsed.Data[0].Embedding, nil
}

// semanticPromptText flattens a chat/messages request body into
// the text that represents the prompt for embedding: role-tagged
// text parts in order. Requests whose text exceeds 32 KiB are
// ineligible rather than silently truncated.
func semanticPromptText(raw []byte) (string, bool) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || len(req.Messages) == 0 {
		return "", false
	}
	var sb strings.Builder
	for _, m := range req.Messages {
		var plain string
		if err := json.Unmarshal(m.Content, &plain); err == nil {
			if plain != "" {
				sb.WriteString(m.Role + ": " + plain + "\n")
			}
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			if (b.Type == "text" || b.Type == "input_text") && b.Text != "" {
				sb.WriteString(m.Role + ": " + b.Text + "\n")
			}
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" || len(text) > 32<<10 {
		return "", false
	}
	return text, true
}
