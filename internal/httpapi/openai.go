package httpapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	inspection := inspectRequestJSON(raw, "image_url", []string{"reasoning_effort", "reasoning"})
	if inspection.TooComplex {
		errorJSON(w, http.StatusBadRequest, "request JSON structure is too complex")
		return
	}
	req := router.Requirement{Model: in.Model, Tools: len(in.Tools) > 0, Vision: inspection.Vision, Streaming: in.Stream, Reasoning: inspection.Reasoning}
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
	var lastErr string
	var lastStatus int
	var lastBody []byte
	var lastContentType string
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
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_attempt", Deployment: c.Deployment.ID, Message: fmt.Sprintf("attempt=%d score=%.2f health=%s pressure=%.3f", attempts, c.Score, c.Health.Status, c.CapacityPressure)})
		start := time.Now()
		resp, e := a.Do(routeCtx, payload, in.Stream, forward)
		headerLatency := time.Since(start)
		if e != nil {
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
			b = redactProviderSecrets(cfg, c.Deployment.ProviderID, b)
			lastStatus = resp.StatusCode
			lastBody = b
			lastContentType = resp.Header.Get("Content-Type")
			lastErr = upstreamError(resp.StatusCode, b)
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
			e = proxyResponse(w, resp)
		} else if in.Stream {
			e = streamAnthropicToOpenAI(w, resp, in.Model, r.Header.Get("x-request-id"))
		} else {
			defer resp.Body.Close()
			var an core.AnthResponse
			if e = decodeJSONLimited(resp.Body, &an); e == nil {
				writeJSON(w, 200, translate.AnthropicResponseToOpenAI(an, in.Model))
			}
		}
		total := time.Since(start)
		if e != nil {
			lastErr = e.Error()
			if clientRequestGone(r.Context()) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "client_disconnect", Deployment: c.Deployment.ID, Message: r.Context().Err().Error(), ErrorType: "caller_cancelled", LatencyMS: total.Milliseconds(), StatusCode: resp.StatusCode})
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
		writeRawUpstreamError(w, lastStatus, lastContentType, lastBody)
		return
	}
	if lastErr == "" {
		lastErr = "no usable deployment"
	}
	errorJSON(w, 502, "all candidate deployments failed: "+lastErr)
}

func streamAnthropicToOpenAI(w http.ResponseWriter, resp *http.Response, model string, requestID ...string) error {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	finish := "stop"
	terminal := false
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
		if json.Unmarshal([]byte(d), &env) != nil {
			continue
		}
		typ, _ := env["type"].(string)
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
			return fmt.Errorf("anthropic stream error")
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !terminal {
		return io.ErrUnexpectedEOF
	}
	emit(map[string]any{"id": completionID, "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
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
