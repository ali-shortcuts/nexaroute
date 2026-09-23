package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// usageFromEnvelope extracts (input, output) token counts from either
// protocol's response envelope. Unknown or missing fields yield 0.
func usageFromEnvelope(protocol string, b []byte) (int64, int64) {
	var root struct {
		Usage struct {
			// Anthropic style
			InputTokens  *int64 `json:"input_tokens"`
			OutputTokens *int64 `json:"output_tokens"`
			// OpenAI style
			PromptTokens     *int64 `json:"prompt_tokens"`
			CompletionTokens *int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &root); err != nil {
		return 0, 0
	}
	var in, out int64
	if root.Usage.InputTokens != nil {
		in = *root.Usage.InputTokens
	}
	if root.Usage.OutputTokens != nil {
		out = *root.Usage.OutputTokens
	}
	if root.Usage.PromptTokens != nil {
		in = *root.Usage.PromptTokens
	}
	if root.Usage.CompletionTokens != nil {
		out = *root.Usage.CompletionTokens
	}
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	return in, out
}

// priceFor resolves the USD-per-1M-token rate for a deployment. Lookup order:
// exact deployment id ("provider/model"), exact model id, then the "*"
// fallback. Unpriced deployments estimate zero cost.
func (s *Server) priceFor(deploymentID string) config.PriceConfig {
	cfg := s.currentConfig()
	if len(cfg.Pricing) == 0 {
		return config.PriceConfig{}
	}
	if pc, ok := cfg.Pricing[deploymentID]; ok {
		return pc
	}
	if slash := len(deploymentID) - 1; slash > 0 {
		for i := slash; i >= 0; i-- {
			if deploymentID[i] == '/' {
				if pc, ok := cfg.Pricing[deploymentID[i+1:]]; ok {
					return pc
				}
				break
			}
		}
	}
	return cfg.Pricing["*"]
}

// usageRecorder returns a closure that attributes token usage and estimated
// cost to the deployment and authenticated client key of one request.
func (s *Server) usageRecorder(r *http.Request, deploymentID string) func(in, out int64) {
	key := clientKeyName(r)
	return func(in, out int64) {
		price := s.priceFor(deploymentID)
		cost := float64(in)/1e6*price.InputPerM + float64(out)/1e6*price.OutputPerM
		s.usage.RecordRequest(deploymentID, key, in, out, cost)
	}
}

// proxyValidatedJSONWithUsage streams a validated JSON upstream response to
// the client and feeds the raw body to the usage callback after validation
// passes.
func (s *Server) proxyValidatedJSONWithUsage(w http.ResponseWriter, resp *http.Response, validate jsonEnvelopeValidator, onBody func([]byte)) error {
	defer resp.Body.Close()
	b, err := readJSONLimited(resp.Body)
	if err != nil {
		return err
	}
	if validate != nil {
		if err := validate(b); err != nil {
			return err
		}
	}
	if onBody != nil {
		onBody(b)
	}
	copyUpstreamResponseHeaders(w, resp, false)
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(b)
	return err
}
