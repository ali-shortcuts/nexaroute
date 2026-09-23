package httpapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/translate"
)

func (s *Server) openAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, 405, "method not allowed")
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
	if v := s.guardrailProblem(raw); v != nil {
		s.rejectGuardrail(w, r, r.Header.Get("x-request-id"), v)
		return
	}
	inspection := inspectRequestJSON(raw, "image_url", []string{"reasoning_effort", "reasoning"})
	if inspection.TooComplex {
		errorJSON(w, http.StatusBadRequest, "request JSON structure is too complex")
		return
	}
	req := router.Requirement{Model: in.Model, Tools: len(in.Tools) > 0, Vision: inspection.Vision, Streaming: in.Stream, Reasoning: inspection.Reasoning}
	if req.Reasoning {
		req.ProviderType = "openai_compatible"
	}
	req = s.prepareRequirement(req, r, inspection.BodySessionKey)
	s.telemetryModel(r, in.Model, in.Stream)
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
	var lastErr string
	var lastStatus int
	var lastBody []byte
	var lastContentType string
	lastRetryAfter := 0
	forward := copySelectedRequestHeaders(r)
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
		if c.Deployment.ProviderType == "openai_compatible" {
			payload, err = patchJSONModel(raw, c.Deployment.Model)
		} else {
			var an core.AnthropicRequest
			an, err = translate.OpenAIToAnthropic(in, c.Deployment.Model)
			if err == nil {
				payload, err = json.Marshal(an)
			}
		}
		if err != nil {
			lastErr = err.Error()
			continue
		}
		attempts++
		attemptIndex := attempts - 1
		recordUsage := s.usageRecorder(r, c.Deployment.ID)
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_attempt", Deployment: c.Deployment.ID, Message: fmt.Sprintf("attempt=%d score=%.2f health=%s pressure=%.3f", attempts, c.Score, c.Health.Status, c.CapacityPressure)})
		attemptCtx, attemptCancel := attemptContext(routeCtx, in.Stream, cfg.AttemptTimeout())
		start := time.Now()
		resp, e := a.Do(attemptCtx, payload, in.Stream, forward)
		headerLatency := time.Since(start)
		if e != nil {
			if attemptCancel != nil {
				attemptCancel()
			}
			lastErr = e.Error()
			if clientRequestGone(r.Context()) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "client_disconnect", Deployment: c.Deployment.ID, Message: r.Context().Err().Error(), ErrorType: "caller_cancelled", LatencyMS: headerLatency.Milliseconds()})
				return
			}
			if gatewayDeadlineExceeded(routeCtx, r.Context()) {
				if router.IsReadyStrategy(cfg.Routing.Strategy) {
					s.hm.Quarantine(c.Deployment.ID, lastErr, headerLatency)
					s.probe.Recover(c.Deployment.ID)
				} else {
					s.hm.RecordFailure(c.Deployment.ID, lastErr, headerLatency)
				}
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_timeout", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: "provider_timeout", LatencyMS: headerLatency.Milliseconds()})
				break
			}
			if router.IsReadyStrategy(cfg.Routing.Strategy) {
				s.hm.Quarantine(c.Deployment.ID, lastErr, headerLatency)
				s.probe.Recover(c.Deployment.ID)
			} else {
				s.hm.RecordFailure(c.Deployment.ID, lastErr, headerLatency)
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: classifyTransportError(e), LatencyMS: headerLatency.Milliseconds()})
			if attempts < max && i+1 < len(candidates) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: c.Deployment.ID, Message: "transport failure; trying next eligible candidate"})
			}
			s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
			if attemptCancel != nil {
				attemptCancel()
			}
			b = a.RedactBody(b)
			lastStatus = resp.StatusCode
			lastBody = b
			lastContentType = resp.Header.Get("Content-Type")
			lastErr = upstreamError(resp.StatusCode, b)
			if resp.StatusCode == http.StatusTooManyRequests {
				lastRetryAfter = int(retryAfterDuration(resp.Header, time.Duration(cfg.Routing.MaxRetryAfterSeconds)*time.Second) / time.Second)
			}
			if router.IsReadyStrategy(cfg.Routing.Strategy) {
				if failoverEligible(resp.StatusCode) {
					s.hm.Quarantine(c.Deployment.ID, lastErr, headerLatency)
					s.probe.Recover(c.Deployment.ID)
				}
			} else if hardCooldownStatus(resp.StatusCode) {
				d := cfg.Cooldown()
				if resp.StatusCode == 429 {
					d = retryAfterDuration(resp.Header, time.Duration(cfg.Routing.MaxRetryAfterSeconds)*time.Second)
				}
				s.hm.ForceCooldown(c.Deployment.ID, lastErr, d)
			} else {
				s.hm.RecordFailure(c.Deployment.ID, lastErr, headerLatency)
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: errorTypeForStatus(resp.StatusCode), LatencyMS: headerLatency.Milliseconds(), StatusCode: resp.StatusCode})
			if failoverEligible(resp.StatusCode) && attempts < max && i+1 < len(candidates) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: c.Deployment.ID, Message: fmt.Sprintf("HTTP %d; trying next eligible candidate", resp.StatusCode), StatusCode: resp.StatusCode})
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
				continue
			}
			if lastStatus == http.StatusTooManyRequests && lastRetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(lastRetryAfter))
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
		if c.Deployment.ProviderType == "openai_compatible" {
			if in.Stream {
				e = proxyNativeSSE(w, resp, "openai", recordUsage)
			} else {
				e = s.proxyValidatedJSONWithUsage(w, resp, validateOpenAIResponseJSON, func(b []byte) {
					inTok, outTok := usageFromEnvelope("openai", b)
					recordUsage(inTok, outTok)
				})
			}
		} else if in.Stream {
			e = streamAnthropicToOpenAI(w, resp, in.Model, recordUsage, r.Header.Get("x-request-id"))
		} else {
			var an core.AnthResponse
			e = decodeValidatedJSONLimited(resp.Body, &an, validateAnthropicResponseJSON)
			resp.Body.Close()
			if e == nil {
				writeJSON(w, 200, translate.AnthropicResponseToOpenAI(an, in.Model))
			}
		}
		if attemptCancel != nil {
			attemptCancel() // body fully consumed; release the per-attempt timer
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
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "stream_fail", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: "provider_stream_error", LatencyMS: total.Milliseconds(), StatusCode: resp.StatusCode})
			return
		}
		s.recordRouteSuccess(req, c.Deployment.ID, headerLatency)
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_ok", Deployment: c.Deployment.ID, Message: "request completed", LatencyMS: total.Milliseconds(), StatusCode: resp.StatusCode})
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
		if lastStatus == http.StatusTooManyRequests && lastRetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(lastRetryAfter))
		}
		writeRawUpstreamError(w, lastStatus, lastContentType, lastBody)
		return
	}
	if lastErr == "" {
		lastErr = "no usable deployment"
	}
	errorJSON(w, 502, "all candidate deployments failed: "+lastErr)
}

func streamAnthropicToOpenAI(w http.ResponseWriter, resp *http.Response, model string, recordUsage func(in, out int64), requestID ...string) error {
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
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	finish := "stop"
	terminal := false
	var usageIn, usageOut int64
	completionID := uniqueStreamID("chatcmpl", requestID...)
	toolIndex := map[int]int{}
	nextTool := 0
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if d == "" {
			continue
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(d), &env); err != nil {
			return fmt.Errorf("invalid Anthropic SSE JSON: %w", err)
		}
		typ, _ := env["type"].(string)
		if typ == "message_start" || typ == "message_delta" {
			if ub, err := json.Marshal(env); err == nil {
				ui, uo := usageFromEnvelope("anthropic", ub)
				if ui > 0 {
					usageIn = ui
				}
				if uo > 0 {
					usageOut = uo
				}
			}
		}
		switch typ {
		case "content_block_start":
			ai, _ := numberInt(env["index"])
			cb, _ := env["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				oi := nextTool
				nextTool++
				toolIndex[ai] = oi
				emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": oi, "id": cb["id"], "type": "function", "function": map[string]any{"name": cb["name"], "arguments": ""}}}}, "finish_reason": nil}}})
			}
		case "content_block_delta":
			ai, _ := numberInt(env["index"])
			delta, _ := env["delta"].(map[string]any)
			dt, _ := delta["type"].(string)
			switch dt {
			case "text_delta":
				txt, _ := delta["text"].(string)
				emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": txt}, "finish_reason": nil}}})
			case "input_json_delta":
				part, _ := delta["partial_json"].(string)
				oi, ok := toolIndex[ai]
				if ok {
					emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": oi, "function": map[string]any{"arguments": part}}}}, "finish_reason": nil}}})
				}
			}
		case "message_delta":
			del, _ := env["delta"].(map[string]any)
			sr, _ := del["stop_reason"].(string)
			if sr != "" {
				terminal = true
			}
			if sr == "tool_use" {
				finish = "tool_calls"
			} else if sr == "max_tokens" {
				finish = "length"
			} else if sr != "" {
				finish = "stop"
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
	if err := scanner.Err(); err != nil {
		return err
	}
	if !terminal {
		return io.ErrUnexpectedEOF
	}
	emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}})
	if writeErr != nil {
		return writeErr
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	if recordUsage != nil {
		recordUsage(usageIn, usageOut)
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
