package httpapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/universal-llm-gateway/internal/core"
	"github.com/ali-shortcuts/universal-llm-gateway/internal/events"
	"github.com/ali-shortcuts/universal-llm-gateway/internal/router"
	"github.com/ali-shortcuts/universal-llm-gateway/internal/translate"
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
	req := router.Requirement{Model: in.Model, Tools: len(in.Tools) > 0, Vision: hasVisionOpenAI(raw), Streaming: in.Stream, Reasoning: hasReasoningOpenAI(raw)}
	cfg, candidates, adapters := s.routeSnapshot(req)
	if len(candidates) == 0 {
		errorJSON(w, 503, "no compatible healthy deployment")
		return
	}
	max := cfg.Routing.MaxAttempts
	if max > len(candidates) {
		max = len(candidates)
	}
	var lastErr string
	var lastStatus int
	var lastBody []byte
	var lastContentType string
	forward := copySelectedRequestHeaders(r)
	for i := 0; i < max; i++ {
		c := candidates[i]
		a, ok := adapters[c.Deployment.ProviderID]
		if !ok {
			continue
		}
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
		start := time.Now()
		resp, e := a.Do(r.Context(), payload, in.Stream, forward)
		headerLatency := time.Since(start)
		if e != nil {
			lastErr = e.Error()
			s.hm.RecordFailure(c.Deployment.ID, lastErr, headerLatency)
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: c.Deployment.ID, Message: lastErr, LatencyMS: headerLatency.Milliseconds()})
			s.retryPause(r, cfg, i, max)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
			lastStatus = resp.StatusCode
			lastBody = b
			lastContentType = resp.Header.Get("Content-Type")
			lastErr = upstreamError(resp.StatusCode, b)
			if hardCooldownStatus(resp.StatusCode) {
				d := cfg.Cooldown()
				if resp.StatusCode == 429 {
					d = retryAfterDuration(resp.Header, time.Duration(cfg.Routing.MaxRetryAfterSeconds)*time.Second)
				}
				s.hm.ForceCooldown(c.Deployment.ID, lastErr, d)
			} else {
				s.hm.RecordFailure(c.Deployment.ID, lastErr, headerLatency)
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: c.Deployment.ID, Message: lastErr, LatencyMS: headerLatency.Milliseconds(), StatusCode: resp.StatusCode})
			if failoverEligible(resp.StatusCode) && i+1 < max {
				s.retryPause(r, cfg, i, max)
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
			if e = json.NewDecoder(resp.Body).Decode(&an); e == nil {
				writeJSON(w, 200, translate.AnthropicResponseToOpenAI(an, in.Model))
			}
		}
		total := time.Since(start)
		if e != nil {
			lastErr = e.Error()
			s.hm.RecordFailure(c.Deployment.ID, lastErr, total)
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "stream_fail", Deployment: c.Deployment.ID, Message: lastErr, LatencyMS: total.Milliseconds(), StatusCode: resp.StatusCode})
			return
		}
		s.hm.RecordSuccess(c.Deployment.ID, headerLatency)
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_ok", Deployment: c.Deployment.ID, Message: "request completed", LatencyMS: total.Milliseconds(), StatusCode: resp.StatusCode})
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
			if sr == "tool_use" {
				finish = "tool_calls"
			} else if sr == "max_tokens" {
				finish = "length"
			} else if sr != "" {
				finish = "stop"
			}
		case "error":
			emit(map[string]any{"error": env["error"]})
			return fmt.Errorf("anthropic stream error")
		}
	}
	if err := scanner.Err(); err != nil {
		return err
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
