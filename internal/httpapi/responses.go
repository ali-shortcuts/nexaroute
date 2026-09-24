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

	"github.com/ali-shortcuts/nexaroute/internal/compat/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/compat/stream"
	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// OpenAI Responses ingress (first-class adapter, Phase 9, streaming Phase 10).
// The Responses wire shape is decoded into the Canonical IR, routed like any
// other request, and encoded back — per upstream protocol class. Streaming
// responses are translated from upstream SSE through the compat/stream
// decoders, so every supported upstream class can serve stream:true.

type responsesInputItem struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type responsesRequest struct {
	Model           string          `json:"model"`
	Input           any             `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	Tools           []responsesTool `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Metadata        map[string]any  `json:"metadata,omitempty"`
}

func responsesInputToMessages(input any, instructions string) ([]canonical.Message, error) {
	msgs := []canonical.Message{}
	if instructions != "" {
		// Instructions become the canonical system prompt at the Request level.
		_ = instructions
	}
	switch v := input.(type) {
	case nil:
		return nil, fmt.Errorf("input is required")
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("input is required")
		}
		msgs = append(msgs, canonical.Message{Role: "user", Parts: []canonical.ContentPart{{Kind: canonical.ContentText, Text: v}}})
	case []any:
		for _, item := range v {
			switch e := item.(type) {
			case string:
				msgs = append(msgs, canonical.Message{Role: "user", Parts: []canonical.ContentPart{{Kind: canonical.ContentText, Text: e}}})
			case map[string]any:
				typ, _ := e["type"].(string)
				role, _ := e["role"].(string)
				if role == "" {
					role = "user"
				}
				switch typ {
				case "message", "":
					msg := canonical.Message{Role: role}
					switch c := e["content"].(type) {
					case string:
						msg.Parts = append(msg.Parts, canonical.ContentPart{Kind: canonical.ContentText, Text: c})
					case []any:
						for _, p := range c {
							pm, ok := p.(map[string]any)
							if !ok {
								continue
							}
							pt, _ := pm["type"].(string)
							switch pt {
							case "input_text", "output_text", "text":
								if t, _ := pm["text"].(string); t != "" {
									msg.Parts = append(msg.Parts, canonical.ContentPart{Kind: canonical.ContentText, Text: t})
								}
							case "input_image", "image_url":
								url := ""
								if u, _ := pm["image_url"].(string); u != "" {
									url = u
								} else if iu, _ := pm["image_url"].(map[string]any); iu != nil {
									url, _ = iu["url"].(string)
								}
								if url != "" {
									msg.Parts = append(msg.Parts, canonical.ContentPart{Kind: canonical.ContentImage, ImageURL: url})
								}
							}
						}
					}
					if len(msg.Parts) > 0 {
						msgs = append(msgs, msg)
					}
				case "function_call":
					name, _ := e["name"].(string)
					callID, _ := e["call_id"].(string)
					args := map[string]any{}
					if a, _ := e["arguments"].(string); a != "" {
						_ = json.Unmarshal([]byte(a), &args)
					}
					msgs = append(msgs, canonical.Message{Role: "assistant", Parts: []canonical.ContentPart{{
						Kind: canonical.ContentToolCall, ToolCallID: callID, ToolName: name, ToolInput: args,
					}}})
				case "function_call_output":
					callID, _ := e["call_id"].(string)
					output := ""
					if o, _ := e["output"].(string); o != "" {
						output = o
					}
					msgs = append(msgs, canonical.Message{Role: "tool", Parts: []canonical.ContentPart{{
						Kind: canonical.ContentToolResult, ToolUseID: callID, ToolContent: output,
					}}})
				}
			}
		}
	default:
		return nil, fmt.Errorf("unsupported input shape")
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("input produced no messages")
	}
	return msgs, nil
}

func responsesToCanonical(in responsesRequest) (canonical.Request, error) {
	msgs, err := responsesInputToMessages(in.Input, in.Instructions)
	if err != nil {
		return canonical.Request{}, err
	}
	out := canonical.Request{
		Model: in.Model, System: in.Instructions, Messages: msgs,
		Temperature: in.Temperature, TopP: in.TopP,
		MaxTokens: in.MaxOutputTokens, Stream: in.Stream, Metadata: in.Metadata,
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		params := t.Parameters
		if params == nil {
			params = map[string]any{"type": "object"}
		}
		out.Tools = append(out.Tools, canonical.ToolDef{Name: t.Name, Description: t.Description, Parameters: params})
	}
	switch v := in.ToolChoice.(type) {
	case string:
		out.ToolChoice = v
	case map[string]any:
		if typ, _ := v["type"].(string); typ != "" {
			out.ToolChoice = typ
		}
	}
	return out, nil
}

func canonicalToResponsesObject(model string, resp canonical.Response, responseID string) map[string]any {
	output := []any{}
	if resp.Text != "" {
		output = append(output, map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": resp.Text}},
		})
	}
	for _, tc := range resp.ToolCalls {
		arg, _ := json.Marshal(tc.Arguments)
		if len(arg) == 0 {
			arg = []byte("{}")
		}
		output = append(output, map[string]any{
			"type": "function_call", "call_id": tc.ID, "name": tc.Name,
			"arguments": string(arg),
		})
	}
	return map[string]any{
		"id": responseID, "object": "response", "model": model,
		"output": output,
		"usage": map[string]any{
			"input_tokens": resp.InputTokens, "output_tokens": resp.OutputTokens,
			"total_tokens": resp.InputTokens + resp.OutputTokens,
		},
	}
}

func (s *Server) openAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, 405, "method not allowed")
		return
	}
	if !s.clientAuthAllowed(w, r, false) {
		return
	}
	var in responsesRequest
	raw, err := readJSON(r, &in)
	if err != nil {
		if _, ok := err.(*requestTooLargeError); ok {
			errorJSON(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		errorJSON(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Model) == "" {
		errorJSON(w, 400, "model and input are required")
		return
	}
	canon, err := responsesToCanonical(in)
	if err != nil {
		errorJSON(w, 400, err.Error())
		return
	}
	inspection := inspectRequestJSON(raw, "image_url", []string{"reasoning_effort", "reasoning"})
	if inspection.TooComplex {
		errorJSON(w, http.StatusBadRequest, "request JSON structure is too complex")
		return
	}
	// Canonical requirements drive routing; vision falls back to raw
	// inspection when the Responses input used image parts.
	req := router.Requirement{
		Model: in.Model, Tools: canon.HasTools(),
		Vision:    canon.HasImages() || inspection.Vision,
		Streaming: in.Stream, Reasoning: canon.HasReasoning() || inspection.Reasoning,
		MinContextWindow: inspection.EstimatedPromptTokens + in.MaxOutputTokens,
	}
	req = s.prepareRequirement(req, r, inspection.BodySessionKey)
	cfg, candidates := s.routeSnapshot(req)
	if len(candidates) == 0 {
		errorJSON(w, 503, "no compatible healthy deployment")
		return
	}
	max := cfg.Routing.MaxAttempts
	if max > len(candidates) {
		max = len(candidates)
	}
	routeCtx, routeCancel := routeContext(r.Context(), in.Stream, cfg.RequestTimeout())
	defer routeCancel()
	forward := copySelectedRequestHeaders(r)
	var lastErr string
	var lastStatus int
	var lastBody []byte
	attempts := 0
	for i := 0; i < len(candidates) && attempts < max; i++ {
		c := candidates[i]
		fresh, a, ok := s.currentRouteCandidate(c.Deployment.ID, req)
		if !ok {
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_skip", Deployment: c.Deployment.ID, Message: "candidate is no longer eligible or provider changed"})
			continue
		}
		c = fresh
		var payload []byte
		var streamOptsInjected bool
		if c.Deployment.ProviderType == "gemini" {
			gemReq := canon.ToGeminiRequest()
			var err error
			payload, err = json.Marshal(gemReq)
			if err != nil {
				lastErr = "attempt payload could not be built"
				continue
			}
		} else if c.Deployment.ProviderType == "anthropic_compatible" {
			anthReq := canon.ToAnthropicRequest(c.Deployment.Model)
			var err error
			payload, err = json.Marshal(anthReq)
			if err != nil {
				lastErr = "attempt payload could not be built"
				continue
			}
		} else {
			chatReq := canon.ToOpenAIRequest(c.Deployment.Model)
			if in.Stream {
				chatReq.StreamOptions = json.RawMessage(`{"include_usage":true}`)
				streamOptsInjected = true
			}
			var err error
			payload, err = json.Marshal(chatReq)
			if err != nil {
				lastErr = "attempt payload could not be built"
				continue
			}
		}
		attempts++
		attemptIndex := attempts - 1
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_attempt", Deployment: c.Deployment.ID, Message: fmt.Sprintf("attempt=%d responses ingress", attempts)})
		send := func(ctx context.Context, p []byte) (*http.Response, error) {
			return a.Do(ctx, p, in.Stream, forward)
		}
		if c.Deployment.ProviderType == "gemini" {
			send = geminiSendFor(a, c.Deployment.Model, in.Stream, forward)
		}
		start := time.Now()
		resp, err := send(routeCtx, payload)
		if err == nil && streamOptsInjected && resp.StatusCode == http.StatusBadRequest {
			probe, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if bytes.Contains(bytes.ToLower(probe), []byte("stream_options")) {
				if stripped, ok := stripStreamOptions(payload); ok {
					payload = stripped
					streamOptsInjected = false
					resp, err = send(routeCtx, payload)
				} else {
					resp.Body = io.NopCloser(bytes.NewReader(probe))
				}
			} else {
				resp.Body = io.NopCloser(bytes.NewReader(probe))
			}
		}
		headerLatency := time.Since(start)
		if err != nil {
			lastErr = err.Error()
			if clientRequestGone(r.Context()) {
				return
			}
			if gatewayDeadlineExceeded(routeCtx, r.Context()) {
				s.hm.RecordProviderFailure(c.Deployment.ProviderID, c.Deployment.ID, lastErr)
				if router.IsReadyStrategy(cfg.Routing.Strategy) {
					s.hm.Quarantine(c.Deployment.ID, lastErr, headerLatency)
					s.probe.Recover(c.Deployment.ID)
				} else {
					s.hm.RecordFailure(c.Deployment.ID, lastErr, headerLatency)
				}
				break
			}
			s.hm.RecordProviderFailure(c.Deployment.ProviderID, c.Deployment.ID, lastErr)
			if router.IsReadyStrategy(cfg.Routing.Strategy) {
				s.hm.Quarantine(c.Deployment.ID, lastErr, headerLatency)
				s.probe.Recover(c.Deployment.ID)
			} else {
				s.hm.RecordFailure(c.Deployment.ID, lastErr, headerLatency)
			}
			s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
			b = a.RedactBody(b)
			// Bounded repair before failing over.
			if repaired, _, _, good := s.maybeRepairUpstream(routeCtx, send, a.RedactBody, c.Deployment.ID, payload, resp.StatusCode, b, r.Header.Get("x-request-id")); good {
				resp = repaired
			} else {
				lastStatus = resp.StatusCode
				lastBody = b
				lastErr = upstreamError(resp.StatusCode, b)
				policy := policyForStatus(resp.StatusCode)
				policy, classified := classifyUpstreamFailureForPolicy(resp.StatusCode, b, policy)
				s.recordCompatObservation(c.Deployment.ID, classified)
				s.recordProviderFailure(c.Deployment.ProviderID, c.Deployment.ID, lastErr, policy)
				if router.IsReadyStrategy(cfg.Routing.Strategy) {
					if policy.QuarantineDeployment {
						s.hm.Quarantine(c.Deployment.ID, lastErr, headerLatency)
						s.probe.Recover(c.Deployment.ID)
					}
				} else if policy.HardCooldown {
					s.hm.ForceCooldown(c.Deployment.ID, lastErr, cfg.Cooldown())
				} else if policy.QuarantineDeployment {
					s.hm.RecordFailure(c.Deployment.ID, lastErr, headerLatency)
				}
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: policy.ErrorType, LatencyMS: headerLatency.Milliseconds(), StatusCode: resp.StatusCode})
				if policy.Failover && attempts < max && i+1 < len(candidates) {
					s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
					continue
				}
				if lastStatus > 0 {
					writeRawUpstreamError(w, lastStatus, "application/json", lastBody)
					return
				}
				errorJSON(w, 502, lastErr)
				return
			}
		}
		w.Header().Set("X-Gateway-Deployment", c.Deployment.ID)
		w.Header().Set("X-Gateway-Provider", c.Deployment.ProviderID)
		w.Header().Set("X-Gateway-Upstream-Model", c.Deployment.Model)
		if in.Stream {
			deploymentID := c.Deployment.ID
			resp.Body = observeFirstByte(resp.Body, start, func(d time.Duration) { s.hm.RecordTTFT(deploymentID, d) })
		}
		responseID := uniqueStreamID("resp", r.Header.Get("x-request-id"))
		var se error
		if in.Stream {
			var dec stream.Decoder = &stream.OpenAIDecoder{}
			if c.Deployment.ProviderType == "anthropic_compatible" {
				dec = &stream.AnthropicDecoder{}
			} else if c.Deployment.ProviderType == "gemini" {
				dec = &stream.GeminiDecoder{}
			}
			se = streamUpstreamToResponses(w, resp, dec, in.Model, responseID, func(prompt, completion int) {
				s.usage.Record(c.Deployment.ID, int64(prompt), int64(completion))
			}, r.Header.Get("x-request-id"))
		} else if c.Deployment.ProviderType == "anthropic_compatible" {
			var an core.AnthResponse
			se = decodeValidatedJSONLimited(resp.Body, &an, validateAnthropicResponseJSON)
			resp.Body.Close()
			if se == nil {
				s.usage.Record(c.Deployment.ID, int64(an.Usage.InputTokens), int64(an.Usage.OutputTokens))
				canonResp := canonical.FromAnthropicResponse(an)
				writeJSON(w, 200, canonicalToResponsesObject(in.Model, canonResp, responseID))
			}
		} else if c.Deployment.ProviderType == "gemini" {
			var g canonical.GeminiResponse
			se = decodeValidatedJSONLimited(resp.Body, &g, validateGeminiResponseJSON)
			resp.Body.Close()
			if se == nil {
				canonResp, cerr := canonical.FromGeminiResponse(g)
				if cerr != nil {
					se = cerr
				} else {
					s.usage.Record(c.Deployment.ID, int64(canonResp.InputTokens), int64(canonResp.OutputTokens))
					writeJSON(w, 200, canonicalToResponsesObject(in.Model, canonResp, responseID))
				}
			}
		} else {
			var o core.OpenAIResponse
			se = decodeValidatedJSONLimited(resp.Body, &o, validateOpenAIResponseJSON)
			resp.Body.Close()
			if se == nil {
				s.usage.Record(c.Deployment.ID, int64(o.Usage.PromptTokens), int64(o.Usage.CompletionTokens))
				canonResp := canonical.FromOpenAIResponse(o)
				writeJSON(w, 200, canonicalToResponsesObject(in.Model, canonResp, responseID))
			}
		}
		total := time.Since(start)
		if se != nil {
			lastErr = se.Error()
			if clientRequestGone(r.Context()) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "client_disconnect", Deployment: c.Deployment.ID, Message: r.Context().Err().Error(), ErrorType: "caller_cancelled", LatencyMS: total.Milliseconds(), StatusCode: resp.StatusCode})
				return
			}
			if !responseCommitted(w) {
				lastStatus = 0
				lastBody = nil
				if cfg.Routing.Strategy == "ready_mesh" && req.Streaming {
					s.hm.RecordScopeFailure(c.Deployment.ID, []string{"streaming"}, lastErr)
				} else if router.IsReadyStrategy(cfg.Routing.Strategy) {
					s.hm.Quarantine(c.Deployment.ID, lastErr, total)
					s.probe.Recover(c.Deployment.ID)
				} else {
					s.hm.RecordFailure(c.Deployment.ID, lastErr, total)
				}
				kind := "response_decode_fail"
				if req.Streaming {
					kind = "stream_fail_precommit"
				}
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: kind, Deployment: c.Deployment.ID, Message: lastErr, ErrorType: "provider_invalid_response", LatencyMS: total.Milliseconds(), StatusCode: resp.StatusCode})
				if attempts < max && i+1 < len(candidates) {
					s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
					continue
				}
				errorJSON(w, http.StatusBadGateway, "upstream returned an invalid response")
				return
			}
			if cfg.Routing.Strategy == "ready_mesh" && req.Streaming {
				s.hm.RecordScopeFailure(c.Deployment.ID, []string{"streaming"}, lastErr)
			} else if router.IsReadyStrategy(cfg.Routing.Strategy) {
				s.hm.Quarantine(c.Deployment.ID, lastErr, total)
				s.probe.Recover(c.Deployment.ID)
			} else {
				s.hm.RecordFailure(c.Deployment.ID, lastErr, total)
			}
			s.hm.RecordProviderFailure(c.Deployment.ProviderID, c.Deployment.ID, lastErr)
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "stream_fail", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: "provider_stream_error", LatencyMS: total.Milliseconds(), StatusCode: resp.StatusCode})
			return
		}
		s.recordRouteSuccess(req, c.Deployment.ID, c.Deployment.ProviderID, headerLatency)
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_ok", Deployment: c.Deployment.ID, Message: "responses request completed", LatencyMS: total.Milliseconds(), StatusCode: 200})
		return
	}
	if gatewayDeadlineExceeded(routeCtx, r.Context()) {
		errorJSON(w, http.StatusGatewayTimeout, "gateway request timeout")
		return
	}
	if clientRequestGone(r.Context()) {
		return
	}
	if lastStatus > 0 && len(lastBody) > 0 {
		writeRawUpstreamError(w, lastStatus, "application/json", lastBody)
		return
	}
	if lastErr == "" {
		lastErr = "no usable deployment"
	}
	errorJSON(w, 502, "all candidate deployments failed: "+lastErr)
}
