package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

// One-click connection test (#415 item 6): a BOUNDED probe of one
// saved deployment — a fixed tiny prompt with max_tokens=8,
// exactly one upstream request, 15s ceiling — reported with the
// facts an operator needs before pointing a coding client at the
// route: provider/model, endpoint, auth result, protocol match,
// and the expected vs actual quota/token use. The bound is stated
// up front in the report so the cost of testing is never a
// surprise, and the upstream credentials never leave the server.

const (
	connectionTestPrompt        = "Reply with OK."
	connectionTestMaxOutTokens  = 8
	connectionTestPromptEstToks = 5 // chars/4 estimate of the fixed prompt
)

type connectionTestReport struct {
	DeploymentID    string   `json:"deployment_id"`
	Provider        string   `json:"provider"`
	Model           string   `json:"model"`
	Endpoint        string   `json:"endpoint"`
	Dialect         string   `json:"dialect"`
	OK              bool     `json:"ok"`
	AuthOK          bool     `json:"auth_ok"`
	ProtocolMatch   bool     `json:"protocol_match"`
	StatusCode      int      `json:"status_code"`
	LatencyMS       int64    `json:"latency_ms"`
	ExpectedMaxToks int      `json:"expected_max_tokens"`
	InputTokens     *int     `json:"input_tokens,omitempty"`
	OutputTokens    *int     `json:"output_tokens,omitempty"`
	EstimatedCost   *float64 `json:"estimated_cost_usd,omitempty"`
	CostBasis       string   `json:"cost_basis"` // actual_usage | bound | unpriced
	Error           string   `json:"error,omitempty"`
}

func (s *Server) adminConnectionTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, 405, "method not allowed")
		return
	}
	var in struct {
		DeploymentID string `json:"deployment_id"`
		Dialect      string `json:"dialect"`
	}
	if _, err := readJSON(r, &in); err != nil {
		errorJSON(w, 400, "invalid JSON: "+err.Error())
		return
	}
	providerID, modelID, ok := strings.Cut(in.DeploymentID, "/")
	if !ok || providerID == "" || modelID == "" {
		errorJSON(w, 400, "deployment_id must be provider/model")
		return
	}
	cfg := s.currentConfig()
	var prov *config.ProviderConfig
	var model *config.ModelConfig
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == providerID {
			prov = &cfg.Providers[i]
			for j := range prov.Models {
				if prov.Models[j].ID == modelID {
					model = &prov.Models[j]
				}
			}
		}
	}
	if prov == nil || model == nil {
		errorJSON(w, 404, "deployment not found in the saved configuration")
		return
	}
	dialect := in.Dialect
	if dialect != "anthropic" && dialect != "openai" {
		if prov.Type == "anthropic_compatible" {
			dialect = "anthropic"
		} else {
			dialect = "openai"
		}
	}
	report := runConnectionTest(r.Context(), *prov, *model, in.DeploymentID, dialect)
	writeJSON(w, 200, report)
}

func runConnectionTest(ctx context.Context, prov config.ProviderConfig, model config.ModelConfig, deploymentID, dialect string) connectionTestReport {
	path := "/v1/chat/completions"
	if dialect == "anthropic" {
		path = "/v1/messages"
	}
	rep := connectionTestReport{
		DeploymentID:    deploymentID,
		Provider:        prov.ID,
		Model:           model.Model,
		Endpoint:        strings.TrimRight(prov.BaseURL, "/") + path,
		Dialect:         dialect,
		ExpectedMaxToks: connectionTestPromptEstToks + connectionTestMaxOutTokens,
		CostBasis:       "unpriced",
	}
	a, err := providers.NewAdapter(prov, 15*time.Second)
	if err != nil {
		rep.Error = "adapter: " + err.Error()
		return rep
	}
	defer providers.CloseIdleConnections(a)
	payload, _ := json.Marshal(map[string]any{
		"model":      model.Model,
		"max_tokens": connectionTestMaxOutTokens,
		"messages":   []map[string]string{{"role": "user", "content": connectionTestPrompt}},
	})
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := a.Do(cctx, payload, false, nil)
	rep.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	defer resp.Body.Close()
	rep.StatusCode = resp.StatusCode
	rep.AuthOK = resp.StatusCode != 401 && resp.StatusCode != 403
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	rep.ProtocolMatch, rep.InputTokens, rep.OutputTokens = parseConnectionTestResponse(dialect, body)
	rep.OK = resp.StatusCode >= 200 && resp.StatusCode < 300 && rep.ProtocolMatch
	if !rep.OK && rep.Error == "" {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			rep.Error = "upstream HTTP status " + http.StatusText(resp.StatusCode)
		} else {
			rep.Error = "response did not match the " + dialect + " protocol shape"
		}
	}
	// Cost: actual usage when the provider reported it, else the
	// stated bound; unpriced models say so instead of inventing 0.
	if model.InputCostPerMTok > 0 || model.OutputCostPerMTok > 0 {
		inToks, outToks := connectionTestPromptEstToks, connectionTestMaxOutTokens
		rep.CostBasis = "bound"
		if rep.InputTokens != nil && rep.OutputTokens != nil {
			inToks, outToks = *rep.InputTokens, *rep.OutputTokens
			rep.CostBasis = "actual_usage"
		}
		cost := float64(inToks)/1e6*model.InputCostPerMTok + float64(outToks)/1e6*model.OutputCostPerMTok
		rep.EstimatedCost = &cost
	}
	return rep
}

// parseConnectionTestResponse checks the dialect's response shape
// and extracts usage. A body that does not parse or lacks the
// dialect's defining field is NOT a protocol match.
func parseConnectionTestResponse(dialect string, body []byte) (bool, *int, *int) {
	if dialect == "anthropic" {
		var v struct {
			Type  string `json:"type"`
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &v) != nil || v.Type != "message" {
			return false, nil, nil
		}
		if v.Usage == nil {
			return true, nil, nil
		}
		return true, &v.Usage.InputTokens, &v.Usage.OutputTokens
	}
	var v struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &v) != nil || len(v.Choices) == 0 {
		return false, nil, nil
	}
	if v.Usage == nil {
		return true, nil, nil
	}
	return true, &v.Usage.PromptTokens, &v.Usage.CompletionTokens
}
