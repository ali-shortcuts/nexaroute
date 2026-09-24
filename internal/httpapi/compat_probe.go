package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat/capabilities"
	"github.com/ali-shortcuts/nexaroute/internal/compat/capprobe"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// adminCompatProbe runs Quick / Full / Claude Code Agent compatibility tests.
// See spec section 24. Request:
//
//	{"deployment": "provider/model", "mode": "quick|full|agent"}
//
// Quick: basic generation only. Full: Level A + Level B matrix.
// Agent: simulated Claude Code tool loop (tool definition -> invocation ->
// tool_result continuation).
func (s *Server) adminCompatProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, 405, "method not allowed")
		return
	}
	var in struct {
		Deployment string `json:"deployment"`
		Mode       string `json:"mode"`
	}
	if _, err := readJSON(r, &in); err != nil {
		errorJSON(w, 400, "invalid JSON: "+err.Error())
		return
	}
	in.Deployment = strings.TrimSpace(in.Deployment)
	in.Mode = strings.ToLower(strings.TrimSpace(in.Mode))
	if in.Deployment == "" {
		errorJSON(w, 400, "deployment is required")
		return
	}
	if in.Mode == "" {
		in.Mode = "quick"
	}
	d, ok := s.rt.Deployment(in.Deployment)
	if !ok {
		errorJSON(w, 404, "deployment not found")
		return
	}
	a, ok := s.reg.Get(d.ProviderID)
	if !ok {
		errorJSON(w, 404, "provider adapter not found")
		return
	}
	cfg := s.currentConfig()
	providerCfg := providerConfigByID(cfg, d.ProviderID)
	doer := ccaprobe.ChatDoer(func(ctx context.Context, payload map[string]any, stream bool) (int, []byte, error) {
		return compatProbeCall(ctx, a, providerCfg, d, payload, stream)
	})
	runner := ccaprobe.Runner{Model: d.Model, Timeout: 15 * time.Second, Do: doer}
	switch in.Mode {
	case "quick":
		rep := runner.RunLevelA(r.Context())
		if rep.Outcome == ccaprobe.OutcomePass {
			s.compatStore().MarkVerified(in.Deployment, "text", capabilities.Supported, capabilities.SourceProbe, "quick test PASS")
		}
		writeJSON(w, 200, map[string]any{"mode": "quick", "deployment": in.Deployment, "result": rep})
	case "full":
		rep := runner.Run(r.Context())
		rep.Deployment = in.Deployment
		rep.ApplyToStore(s.compatStore(), in.Deployment)
		writeJSON(w, 200, map[string]any{"mode": "full", "deployment": in.Deployment, "report": rep})
	case "agent":
		rep := s.runAgentProbe(r.Context(), a, providerCfg, d)
		writeJSON(w, 200, map[string]any{"mode": "agent", "deployment": in.Deployment, "report": rep})
	default:
		errorJSON(w, 400, "mode must be quick, full, or agent")
	}
}

func providerConfigByID(cfg config.Config, id string) config.ProviderConfig {
	for _, p := range cfg.Providers {
		if p.ID == id {
			return p
		}
	}
	return config.ProviderConfig{}
}

// compatProbeCall sends one probe payload, translating to the provider's
// native protocol when needed.
func compatProbeCall(ctx context.Context, a providers.Adapter, p config.ProviderConfig, d router.Deployment, payload map[string]any, stream bool) (int, []byte, error) {
	_ = p
	_ = d
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	resp, err := a.Do(ctx, b, stream, nil)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// agentProbeReport is the Claude Code agent-loop simulation result.
type agentProbeReport struct {
	Deployment   string `json:"deployment"`
	ToolDefine   string `json:"tool_define"`
	ToolInvoke   string `json:"tool_invoke"`
	ToolContinue string `json:"tool_continue"`
	Parallel     string `json:"parallel"`
	Stream       string `json:"stream"`
	Ready        bool   `json:"ready"`
	Detail       string `json:"detail,omitempty"`
}

func (s *Server) runAgentProbe(ctx context.Context, a providers.Adapter, p config.ProviderConfig, d router.Deployment) agentProbeReport {
	_ = s
	_ = p
	model := d.Model
	rep := agentProbeReport{ToolDefine: "UNTESTED", ToolInvoke: "UNTESTED", ToolContinue: "UNTESTED", Parallel: "UNTESTED", Stream: "UNTESTED"}
	call := func(payload map[string]any) (int, []byte, error) {
		b, _ := json.Marshal(payload)
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		resp, err := a.Do(pctx, b, false, nil)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, body, nil
	}
	toolDef := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "Get weather for a city",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []string{"city"},
			},
		},
	}}
	// Step 1: tool definition + invocation.
	status, body, err := call(map[string]any{
		"model": model, "max_tokens": 64, "stream": false,
		"messages": []any{map[string]any{"role": "user", "content": "What is the weather in Paris? Use the tool."}},
		"tools":    toolDef, "tool_choice": "auto",
	})
	if err != nil || status < 200 || status >= 300 {
		rep.Detail = fmt.Sprintf("tool definition call failed: status=%d err=%v body=%s", status, err, truncateCompat(string(body), 200))
		return rep
	}
	rep.ToolDefine = "PASS"
	toolCallID, toolName, toolArgs := parseFirstToolCall(body)
	if toolCallID == "" || toolName == "" {
		rep.ToolInvoke = "FAIL"
		rep.Detail = "upstream did not emit a tool call: " + truncateCompat(string(body), 300)
		return rep
	}
	_ = toolArgs
	rep.ToolInvoke = "PASS"
	// Step 2: tool_result continuation.
	status, body, err = call(map[string]any{
		"model": model, "max_tokens": 64, "stream": false,
		"messages": []any{
			map[string]any{"role": "user", "content": "What is the weather in Paris? Use the tool."},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
				"id": toolCallID, "type": "function",
				"function": map[string]any{"name": toolName, "arguments": `{"city":"Paris"}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": toolCallID, "content": `{"temp_c": 21, "condition": "sunny"}`},
		},
		"tools": toolDef,
	})
	if err != nil || status < 200 || status >= 300 {
		rep.ToolContinue = "FAIL"
		rep.Detail = fmt.Sprintf("tool continuation failed: status=%d err=%v", status, err)
		return rep
	}
	if !bytes.Contains(body, []byte("choices")) && !bytes.Contains(body, []byte("content")) {
		rep.ToolContinue = "FAIL"
		rep.Detail = "continuation response has no completion envelope"
		return rep
	}
	rep.ToolContinue = "PASS"
	rep.Parallel = "UNKNOWN"
	rep.Stream = "UNKNOWN"
	rep.Ready = true
	return rep
}

func parseFirstToolCall(body []byte) (id, name, args string) {
	var openai struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &openai); err == nil && len(openai.Choices) > 0 && len(openai.Choices[0].Message.ToolCalls) > 0 {
		tc := openai.Choices[0].Message.ToolCalls[0]
		return tc.ID, tc.Function.Name, tc.Function.Arguments
	}
	var anth struct {
		Content []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &anth); err == nil {
		for _, b := range anth.Content {
			if b.Type == "tool_use" {
				arg, _ := json.Marshal(b.Input)
				return b.ID, b.Name, string(arg)
			}
		}
	}
	return "", "", ""
}
