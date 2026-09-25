package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/translate"
)

func (s *Server) openAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, 405, "method not allowed")
		return
	}
	if !s.clientAuthAllowed(w, r, false) {
		return
	}
	var in core.OpenAIRequest
	raw, err := readJSON(r, &in)
	if err != nil {
		if _, ok := err.(*requestTooLargeError); ok {
			errorJSON(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		errorJSON(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Model) == "" || len(in.Messages) == 0 {
		errorJSON(w, 400, "model and messages are required")
		return
	}
	// Phase C: feature extraction + task classification (observational, routing-neutral)
	maxOut := in.MaxCompletionTokens
	if maxOut == 0 {
		maxOut = in.MaxTokens
	}
	hasSystem := false
	for _, m := range in.Messages {
		if strings.EqualFold(m.Role, "system") || strings.EqualFold(m.Role, "developer") {
			hasSystem = true
			break
		}
	}
	ti := extractFeaturesAndClassify(raw, feature.ExtractOptions{
		Protocol:            feature.ProtocolOpenAI,
		Model:               in.Model,
		Streaming:           in.Stream,
		VisionType:          "image_url",
		ReasoningKeys:       []string{"reasoning_effort", "reasoning"},
		ContentFields:       []string{"messages"},
		MaxOutputTokens:     maxOut,
		ToolCountHint:       len(in.Tools),
		ToolChoiceHint:      in.ToolChoice != nil,
		HasSystemPromptHint: &hasSystem,
	})
	if ti.Features.TooComplex {
		errorJSON(w, http.StatusBadRequest, "request JSON structure is too complex")
		return
	}
	req := router.Requirement{Model: in.Model, Tools: ti.Features.HasTools, Vision: ti.Features.HasVision, Streaming: in.Stream, Reasoning: ti.Features.HasReasoning}
	if req.Reasoning {
		req.ProviderType = "openai_compatible"
	}
	// Context-window pre-routing: skip deployments advertising a window too
	// small for the estimated prompt plus requested output. The same estimate
	// feeds cost-aware ordering. If the caller omits an output ceiling,
	// cost-aware routing deliberately falls back to normal ordering.
	req.EstimatedInputTokens = ti.Features.EstimatedPromptTokens
	req.MaxOutputTokens = maxOut
	req.MinContextWindow = req.EstimatedInputTokens + req.MaxOutputTokens
	req = s.prepareRequirement(req, r, ti.Features.BodySessionKey)
	cfg, candidates, resolvedRoute, resolveErr := s.candidatesForRequirement(req, "openai")
	if resolveErr != nil {
		if strings.Contains(resolveErr.Error(), "disabled") {
			errorJSON(w, 404, resolveErr.Error())
		} else {
			errorJSON(w, 400, resolveErr.Error())
		}
		return
	}
	if len(candidates) == 0 && req.ProviderType != "" {
		relaxed := req
		relaxed.ProviderType = ""
		if c2, cand2, rr2, err2 := s.candidatesForRequirement(relaxed, "openai"); len(cand2) > 0 && err2 == nil {
			req = relaxed
			cfg, candidates = c2, cand2
			resolvedRoute = rr2
		}
	}
	// Emit task_classified event (privacy-safe, no raw prompt) after final route resolution
	s.emitTaskClassified(r.Header.Get("x-request-id"), ti, resolvedRoute)
	if len(candidates) == 0 {
		errorJSON(w, 503, "no compatible healthy deployment")
		return
	}
	if resolvedRoute != nil {
		w.Header().Set("X-Gateway-Virtual-Endpoint", resolvedRoute.VirtualEndpointID)
		w.Header().Set("X-Gateway-Public-Model", resolvedRoute.PublicModel)
		w.Header().Set("X-Gateway-Route-Profile", resolvedRoute.RouteProfileID)
	}
	// For virtual endpoints, eligibility should ignore the virtual public model
	// and use only pool + capability checks. The pool filtering already happened
	// in candidatesForRequirement, so we clear Model for eligibility.
	reqEligible := req
	if resolvedRoute != nil {
		reqEligible.Model = ""
	}
	// Exact-match response cache (opt-in). Only complete, non-streaming,
	// deterministic requests are ever considered; anything else bypasses.
	cacheKey, cacheable := s.cacheLookupFor(r.URL.Path, raw, in.Stream, in.Temperature, in.TopP)
	if s.cacheServe(w, r, cacheKey, cacheable) {
		return
	}
	max := cfg.Routing.MaxAttempts
	if max > len(candidates) {
		max = len(candidates)
	}
	profile := profileFromRequirement(req, nil)
	routeCtx, routeCancel := routeContext(r.Context(), in.Stream, cfg.RequestTimeout())
	routeCtx = providers.WithQuotaEstimate(routeCtx, req.EstimatedInputTokens, req.MaxOutputTokens)
	defer routeCancel()
	var lastErr string
	var lastStatus int
	var lastBody []byte
	var lastContentType string
	forward := copySelectedRequestHeaders(r)
	attempts := 0
	skip := map[int]bool{}
	for i := 0; i < len(candidates) && attempts < max; i++ {
		if skip[i] {
			continue
		}
		c := candidates[i]
		if _, ineligible := s.capabilityIneligible(r.Header.Get("x-request-id"), c.Deployment.ID, profile); ineligible {
			continue
		}
		fresh, a, ok := s.currentRouteCandidate(c.Deployment.ID, reqEligible)
		if !ok {
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_skip", Deployment: c.Deployment.ID, Message: "candidate is no longer eligible or provider changed"})
			continue
		}
		c = fresh
		primary, ok := s.buildOpenAIAttempt(c, reqEligible, raw, in)
		if !ok {
			lastErr = "attempt payload could not be built"
			continue
		}
		var nm *translate.NameMap
		c, a, nm = primary.c, primary.a, primary.nm
		payload := primary.payload
		kind := primary.canonicalKind
		attempts++
		attemptIndex := attempts - 1
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_attempt", Deployment: c.Deployment.ID, Message: fmt.Sprintf("attempt=%d score=%.2f health=%s pressure=%.3f", attempts, c.Score, c.Health.Status, c.CapacityPressure)})
		start := time.Now()
		out, winner, hedgeLaunched := s.doAttemptWithHedge(routeCtx, r.Header.Get("x-request-id"), cfg, candidates, i, attempts, max, primary,
			func(idx int) (hedgeAttemptBundle, bool) {
				return s.buildOpenAIAttempt(candidates[idx], reqEligible, raw, in)
			},
			in.Stream, forward)
		if hedgeLaunched {
			if i+1 < len(candidates) {
				skip[i+1] = true
			}
			// A launched hedge is a real upstream call even when the primary
			// wins the race. Count it against max_attempts so hedging cannot
			// silently exceed the operator's upstream-call budget.
			attempts++
			attemptIndex = attempts - 1
		}
		if out.secondaryWon {
			c, a, nm = winner.c, winner.a, winner.nm
			kind = winner.canonicalKind
			payload = winner.payload
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_attempt", Deployment: c.Deployment.ID, Message: fmt.Sprintf("attempt=%d score=%.2f health=%s pressure=%.3f (hedged winner)", attempts, c.Score, c.Health.Status, c.CapacityPressure)})
		}
		resp, e := out.resp, out.err
		start = out.start
		if e == nil && resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// Bounded deterministic repair on classified capability
			// rejections; never marks the deployment unhealthy.
			resp, payload, _ = s.maybeRepairUpstream(routeCtx, r.Header.Get("x-request-id"),
				hedgeAttemptBundle{c: c, a: a}, payload, resp, in.Stream, forward, cfg.Routing.MaxRepairAttempts, profile)
			if resp == nil {
				e = fmt.Errorf("repair retry transport failure")
			}
		}
		headerLatency := time.Since(start)
		if e != nil {
			lastErr = e.Error()
			if clientRequestGone(r.Context()) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "client_disconnect", Deployment: c.Deployment.ID, Message: r.Context().Err().Error(), ErrorType: "caller_cancelled", LatencyMS: headerLatency.Milliseconds()})
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
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_timeout", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: "provider_timeout", LatencyMS: headerLatency.Milliseconds()})
				break
			}
			s.hm.RecordProviderFailure(c.Deployment.ProviderID, c.Deployment.ID, lastErr)
			if router.IsReadyStrategy(cfg.Routing.Strategy) {
				s.hm.Quarantine(c.Deployment.ID, lastErr, headerLatency)
				s.probe.Recover(c.Deployment.ID)
			} else {
				s.hm.RecordFailure(c.Deployment.ID, lastErr, headerLatency)
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: "provider_connection_failed", LatencyMS: headerLatency.Milliseconds()})
			if attempts < max && i+1 < len(candidates) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: c.Deployment.ID, Message: "transport failure; trying next eligible candidate"})
			}
			s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
			b = a.RedactBody(b)
			lastStatus = resp.StatusCode
			lastBody = b
			lastContentType = resp.Header.Get("Content-Type")
			lastErr = upstreamError(resp.StatusCode, b)
			cls, policy := classifyFailure(resp.StatusCode, b)
			if cls.CapabilityFailure {
				// Capability failures are compatibility facts, not health
				// failures: the deployment stays in the ready mesh.
				lastErr = fmt.Sprintf("%s: %s", cls.CapabilityLabel(), cls.Message)
				policy.Failover = true
				policy.QuarantineDeployment = false
				policy.HardCooldown = false
				policy.SignalProvider = false
			}
			s.recordProviderFailure(c.Deployment.ProviderID, c.Deployment.ID, lastErr, policy)
			if router.IsReadyStrategy(cfg.Routing.Strategy) {
				if policy.QuarantineDeployment {
					s.hm.Quarantine(c.Deployment.ID, lastErr, headerLatency)
					s.probe.Recover(c.Deployment.ID)
				}
			} else if policy.HardCooldown {
				d := cfg.Cooldown()
				if resp.StatusCode == 429 {
					d = retryAfterDuration(resp.Header, time.Duration(cfg.Routing.MaxRetryAfterSeconds)*time.Second)
				}
				s.hm.ForceCooldown(c.Deployment.ID, lastErr, d)
			} else if policy.QuarantineDeployment {
				s.hm.RecordFailure(c.Deployment.ID, lastErr, headerLatency)
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: policy.ErrorType, LatencyMS: headerLatency.Milliseconds(), StatusCode: resp.StatusCode})
			if (policy.Failover || cls.CapabilityFailure) && attempts < max && i+1 < len(candidates) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: c.Deployment.ID, Message: fmt.Sprintf("HTTP %d; trying next eligible candidate", resp.StatusCode), StatusCode: resp.StatusCode})
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
				continue
			}
			if c.Deployment.ProviderType == "openai_compatible" {
				writeRawUpstreamError(w, lastStatus, lastContentType, lastBody)
				return
			}
			errorJSON(w, resp.StatusCode, lastErr)
			return
		}
		w.Header().Set("X-Gateway-Deployment", c.Deployment.ID)
		w.Header().Set("X-Gateway-Provider", c.Deployment.ProviderID)
		w.Header().Set("X-Gateway-Upstream-Model", c.Deployment.Model)
		if in.Stream {
			deploymentID := c.Deployment.ID
			resp.Body = observeFirstByte(resp.Body, start, func(d time.Duration) { s.hm.RecordTTFT(deploymentID, d) })
		}
		switch {
		case kind != "":
			// Canonical-IR upstream (Gemini / Responses-native) decoded through
			// the IR and re-encoded for the OpenAI client.
			if in.Stream {
				e = s.canonicalStreamPump(w, resp, kind, "openai_chat", in.Model, r.Header.Get("x-request-id"),
					func(input, output int) { s.usage.Record(c.Deployment.ID, int64(input), int64(output)) })
			} else {
				e = s.handleCanonicalResponse(w, resp, kind, "openai_chat", in.Model, r.Header.Get("x-request-id"), false,
					func(input, output int) { s.usage.Record(c.Deployment.ID, int64(input), int64(output)) })
			}
		case c.Deployment.ProviderType == "openai_compatible":
			if in.Stream {
				e = proxyNativeSSE(w, resp, "openai", func(prompt, completion int) {
					s.usage.Record(c.Deployment.ID, int64(prompt), int64(completion))
				})
			} else {
				e = s.proxyOpenAINativeJSON(w, resp, c.Deployment.ID, cacheKey, cacheable)
			}
		case in.Stream:
			e = streamAnthropicToOpenAIWithUsage(w, resp, in.Model, nm, func(prompt, completion int) {
				s.usage.Record(c.Deployment.ID, int64(prompt), int64(completion))
			}, r.Header.Get("x-request-id"))
		default:
			var an core.AnthResponse
			e = decodeValidatedJSONLimited(resp.Body, &an, validateAnthropicResponseJSON)
			resp.Body.Close()
			if e == nil {
				s.usage.Record(c.Deployment.ID, int64(an.Usage.InputTokens), int64(an.Usage.OutputTokens))
				translated := translate.AnthropicResponseToOpenAI(an, in.Model, nm)
				if b, merr := json.Marshal(translated); merr == nil {
					s.cacheStoreResponse(cacheKey, cacheable, c.Deployment.ID, 200, "application/json", b)
				}
				writeJSON(w, 200, translated)
			}
		}
		total := time.Since(start)
		if e != nil {
			lastErr = e.Error()
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
		s.learnFromSuccess(c.Deployment.ID, c.Deployment.ProviderID, c.Deployment, payload, nil)
		ev := events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_ok", Deployment: c.Deployment.ID, Message: "request completed", LatencyMS: total.Milliseconds(), StatusCode: resp.StatusCode}
		if resolvedRoute != nil {
			ev.VirtualEndpoint = resolvedRoute.VirtualEndpointID
			ev.PublicModel = resolvedRoute.PublicModel
			ev.RouteProfile = resolvedRoute.RouteProfileID
			ev.Pool = resolvedRoute.PrimaryPoolID
		}
		s.bus.Add(ev)
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
		writeRawUpstreamError(w, lastStatus, lastContentType, lastBody)
		return
	}
	if lastErr == "" {
		lastErr = "no usable deployment"
	}
	errorJSON(w, 502, "all candidate deployments failed: "+lastErr)
}

// anthropicStopToOpenAIFinish maps Anthropic stop_reason values onto the
// OpenAI finish_reason vocabulary, including refusal and pause_turn.
func anthropicStopToOpenAIFinish(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

// streamAnthropicToOpenAI translates an Anthropic Messages SSE stream into
// OpenAI Chat Completions chunks. It emits the role-first chunk OpenAI
// clients expect, surfaces thinking deltas through the widely-supported
// reasoning_content field, pads empty tool argument streams with "{}" so
// clients never observe an empty JSON parse, and forwards real usage in a
// final usage-only chunk before [DONE].
func streamAnthropicToOpenAI(w http.ResponseWriter, resp *http.Response, model string, nm *translate.NameMap, requestID ...string) error {
	return streamAnthropicToOpenAIWithUsage(w, resp, model, nm, nil, requestID...)
}

func streamAnthropicToOpenAIWithUsage(w http.ResponseWriter, resp *http.Response, model string, nm *translate.NameMap, usageSink func(prompt, completion int), requestID ...string) error {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	var writeErr error
	emit := func(v any) {
		if writeErr != nil {
			return
		}
		b, err := json.Marshal(v)
		if err != nil {
			writeErr = err
			return
		}
		_, writeErr = fmt.Fprintf(w, "data: %s\n\n", b)
		if writeErr == nil && fl != nil {
			fl.Flush()
		}
	}
	reader := newSSEReader(resp.Body)
	completionID := uniqueStreamID("chatcmpl", requestID...)
	chunk := func(delta map[string]any, finish *string) map[string]any {
		return map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	finish := "stop"
	terminal := false
	toolIndex := map[int]int{}
	toolArgsSeen := map[int]bool{}
	nextTool := 0
	inputTokens, outputTokens := 0, 0
	cacheReadTokens, cacheCreationTokens := 0, 0
	usageSeen := false

	// The OpenAI role chunk commits the client response. It is deferred
	// until the first valid upstream event proves the stream is alive, so
	// an upstream that answers 200 and then dies before streaming can
	// still fail over pre-commit to the next eligible candidate.
	roleEmitted := false
	emitRoleOnce := func() {
		if roleEmitted {
			return
		}
		roleEmitted = true
		emit(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	}
	// streamErrorChunk surfaces a terminal OpenAI-style error payload so
	// clients can detect a mid-stream truncation instead of seeing a
	// silent network cut. [DONE] is intentionally withheld afterwards.
	streamErrorChunk := func(message string) {
		if roleEmitted && writeErr == nil {
			emit(map[string]any{"error": map[string]any{"message": message, "type": "gateway_stream_error", "code": "provider_stream_error"}})
		}
	}

	for {
		ev, done, err := reader.Next()
		if err != nil {
			streamErrorChunk("upstream stream terminated before completion: " + err.Error())
			return err
		}
		if done {
			break
		}
		d := strings.TrimSpace(ev.data)
		if d == "" {
			continue
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(d), &env); err != nil {
			streamErrorChunk("upstream stream sent an invalid event")
			return fmt.Errorf("invalid Anthropic SSE JSON: %w", err)
		}
		emitRoleOnce()
		if writeErr != nil {
			return writeErr
		}
		typ, _ := env["type"].(string)
		switch typ {
		case "message_start":
			if msg, _ := env["message"].(map[string]any); msg != nil {
				if u, _ := msg["usage"].(map[string]any); u != nil {
					usageSeen = true
					if v, ok := numberInt(u["input_tokens"]); ok && v >= 0 {
						inputTokens = v
					}
					if v, ok := numberInt(u["cache_read_input_tokens"]); ok && v > 0 {
						cacheReadTokens = v
					}
					if v, ok := numberInt(u["cache_creation_input_tokens"]); ok && v > 0 {
						cacheCreationTokens = v
					}
					if v, ok := numberInt(u["output_tokens"]); ok && v > 0 {
						outputTokens = v
					}
				}
			}
		case "content_block_start":
			ai, _ := numberInt(env["index"])
			cb, _ := env["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				oi := nextTool
				nextTool++
				toolIndex[ai] = oi
				name, _ := cb["name"].(string)
				id, _ := cb["id"].(string)
				if id == "" {
					id = fmt.Sprintf("call_%s_%d", completionID, oi)
				}
				emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": oi, "id": id, "type": "function", "function": map[string]any{"name": nm.Reverse(name), "arguments": ""}}}}, "finish_reason": nil}}})
				if input, ok := cb["input"].(map[string]any); ok && len(input) > 0 {
					if b, merr := json.Marshal(input); merr == nil && len(b) > 0 {
						emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": oi, "function": map[string]any{"arguments": string(b)}}}}, "finish_reason": nil}}})
						toolArgsSeen[oi] = true
					}
				}
			}
		case "content_block_delta":
			ai, _ := numberInt(env["index"])
			delta, _ := env["delta"].(map[string]any)
			dt, _ := delta["type"].(string)
			switch dt {
			case "text_delta":
				txt, _ := delta["text"].(string)
				if txt != "" {
					emit(chunk(map[string]any{"content": txt}, nil))
				}
			case "input_json_delta":
				part, _ := delta["partial_json"].(string)
				if part != "" {
					if oi, ok := toolIndex[ai]; ok {
						emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": oi, "function": map[string]any{"arguments": part}}}}, "finish_reason": nil}}})
						toolArgsSeen[oi] = true
					}
				}
			case "thinking_delta":
				txt, _ := delta["thinking"].(string)
				if txt != "" {
					emit(chunk(map[string]any{"reasoning_content": txt}, nil))
				}
			}
		case "content_block_stop":
			ai, _ := numberInt(env["index"])
			if oi, ok := toolIndex[ai]; ok && !toolArgsSeen[oi] {
				// A tool block that never streamed arguments still needs a
				// valid JSON object; one-api/new-api apply the same padding.
				emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": oi, "function": map[string]any{"arguments": "{}"}}}}, "finish_reason": nil}}})
				toolArgsSeen[oi] = true
			}
		case "message_delta":
			del, _ := env["delta"].(map[string]any)
			sr, _ := del["stop_reason"].(string)
			if sr != "" {
				terminal = true
				finish = anthropicStopToOpenAIFinish(sr)
			}
			if u, _ := env["usage"].(map[string]any); u != nil {
				usageSeen = true
				if v, ok := numberInt(u["output_tokens"]); ok && v >= 0 {
					outputTokens = v
				}
				if v, ok := numberInt(u["input_tokens"]); ok && v >= 0 {
					inputTokens = v
				}
				if v, ok := numberInt(u["cache_read_input_tokens"]); ok && v > 0 {
					cacheReadTokens = v
				}
				if v, ok := numberInt(u["cache_creation_input_tokens"]); ok && v > 0 {
					cacheCreationTokens = v
				}
			}
		case "message_stop":
			terminal = true
		case "error":
			emit(map[string]any{"error": env["error"]})
			if writeErr != nil {
				return writeErr
			}
			return fmt.Errorf("anthropic stream error")
		}
		if writeErr != nil {
			return writeErr
		}
	}
	if !terminal {
		streamErrorChunk("upstream stream ended without a terminal Anthropic event")
		return io.ErrUnexpectedEOF
	}
	emitRoleOnce()
	if writeErr != nil {
		return writeErr
	}
	emit(chunk(map[string]any{}, &finish))
	if writeErr != nil {
		return writeErr
	}
	if usageSeen {
		usage := map[string]any{"prompt_tokens": inputTokens, "completion_tokens": outputTokens, "total_tokens": inputTokens + outputTokens}
		if cacheReadTokens+cacheCreationTokens > 0 {
			usage["prompt_tokens_details"] = map[string]any{"cached_tokens": cacheReadTokens}
		}
		emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{}, "usage": usage})
		if writeErr != nil {
			return writeErr
		}
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	if usageSink != nil && usageSeen {
		usageSink(inputTokens, outputTokens)
	}
	return nil
}

func numberInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case json.Number:
		n, e := x.Int64()
		return int(n), e == nil
	}
	return 0, false
}
