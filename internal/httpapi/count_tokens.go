package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// countTokens prefers a provider-native Anthropic-compatible token count when
// one is available. If the selected pool only contains OpenAI-compatible
// backends, it falls back to a conservative local estimate so Claude Code can
// still operate without creating a generation request.
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		anthropicErrorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.clientAuthAllowed(w, r, true) {
		return
	}
	var in core.AnthropicRequest
	raw, err := readJSON(r, &in)
	if err != nil {
		if _, ok := err.(*requestTooLargeError); ok {
			anthropicErrorJSON(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		anthropicErrorJSON(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Model) != "" {
		inspection := inspectRequestJSON(raw, "image", []string{"thinking", "reasoning"})
		if inspection.TooComplex {
			anthropicErrorJSON(w, http.StatusBadRequest, "request JSON structure is too complex")
			return
		}
		req := router.Requirement{Model: in.Model, Tools: len(in.Tools) > 0, Vision: inspection.Vision, Reasoning: inspection.Reasoning}
		if req.Reasoning {
			req.ProviderType = "anthropic_compatible"
		}
		req = s.prepareRequirement(req, r, inspection.BodySessionKey)
		_, candidates := s.routeSnapshot(req)
		forward := copySelectedRequestHeaders(r)
		for _, c := range candidates {
			if c.Deployment.ProviderType != "anthropic_compatible" {
				continue
			}
			fresh, a, ok := s.currentRouteCandidate(c.Deployment.ID, req)
			if !ok || fresh.Deployment.ProviderType != "anthropic_compatible" {
				continue
			}
			c = fresh
			payload, e := patchJSONModel(raw, c.Deployment.Model)
			if e != nil {
				continue
			}
			resp, e := a.CountTokens(r.Context(), payload, forward)
			if e != nil {
				continue
			}
			const maxNativeTokenCountBytes = 2 << 20
			b, readErr := io.ReadAll(io.LimitReader(resp.Body, maxNativeTokenCountBytes+1))
			resp.Body.Close()
			if readErr != nil || len(b) > maxNativeTokenCountBytes {
				continue
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var root map[string]json.RawMessage
				if json.Unmarshal(b, &root) != nil {
					continue
				}
				rawTokens, ok := root["input_tokens"]
				if !ok {
					continue
				}
				var inputTokens int64
				if json.Unmarshal(rawTokens, &inputTokens) != nil || inputTokens < 0 {
					continue
				}
				if ct := resp.Header.Get("Content-Type"); ct != "" {
					w.Header().Set("Content-Type", ct)
				}
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(b)
				return
			}
			if !failoverEligible(resp.StatusCode) {
				break
			}
		}
	}
	// Estimate from the full JSON body, including tool schemas and image/PDF
	// metadata. This is intentionally labelled estimated because tokenization
	// is model-specific.
	var compact bytes.Buffer
	if json.Compact(&compact, raw) == nil {
		raw = compact.Bytes()
	}
	chars := utf8.RuneCount(raw)
	tokens := (chars + 3) / 4
	if tokens < 1 {
		tokens = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens, "estimated": true})
}
