package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/translate"
)

func (s *Server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		anthropicErrorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in core.AnthropicRequest
	raw, err := readJSON(r, &in)
	if err != nil {
		if _, ok := err.(*requestTooLargeError); ok {
			anthropicErrorJSON(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		anthropicErrorJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Model) == "" || len(in.Messages) == 0 {
		anthropicErrorJSON(w, 400, "model and messages are required")
		return
	}

	req := router.Requirement{Model: in.Model, Tools: len(in.Tools) > 0, Vision: hasVisionAnth(raw), Streaming: in.Stream, Reasoning: hasReasoningAnth(raw)}
	cfg, candidates, adapters := s.routeSnapshot(req)
	if len(candidates) == 0 {
		anthropicErrorJSON(w, 503, "no compatible healthy deployment")
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

	for i := 0; i < max; i++ {
		c := candidates[i]
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_attempt", Deployment: c.Deployment.ID, Message: fmt.Sprintf("attempt=%d score=%.2f health=%s", i+1, c.Score, c.Health.Status)})
		a, ok := adapters[c.Deployment.ProviderID]
		if !ok {
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_skip", Deployment: c.Deployment.ID, Message: "provider adapter unavailable"})
			continue
		}
		var payload []byte
		if c.Deployment.ProviderType == "anthropic_compatible" {
			payload, err = patchJSONModel(raw, c.Deployment.Model)
		} else {
			var o core.OpenAIRequest
			o, err = translate.AnthropicToOpenAI(in, c.Deployment.Model)
			if err == nil {
				payload, err = json.Marshal(o)
			}
		}
		if err != nil {
			lastErr = err.Error()
			continue
		}

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
				if cfg.Routing.Strategy == "ready_queue" {
					s.hm.Quarantine(c.Deployment.ID, lastErr, headerLatency)
					s.probe.Recover(c.Deployment.ID)
				} else {
					s.hm.RecordFailure(c.Deployment.ID, lastErr, headerLatency)
				}
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_timeout", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: "provider_timeout", LatencyMS: headerLatency.Milliseconds()})
				break
			}
			if cfg.Routing.Strategy == "ready_queue" {
				s.hm.Quarantine(c.Deployment.ID, lastErr, headerLatency)
				s.probe.Recover(c.Deployment.ID)
			} else {
				s.hm.RecordFailure(c.Deployment.ID, lastErr, headerLatency)
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: "provider_connection_failed", LatencyMS: headerLatency.Milliseconds()})
			if i+1 < max {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: c.Deployment.ID, Message: "transport failure; trying " + candidates[i+1].Deployment.ID})
			}
			s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, i, max)
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
			if cfg.Routing.Strategy == "ready_queue" {
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
			if failoverEligible(resp.StatusCode) && i+1 < max {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: c.Deployment.ID, Message: fmt.Sprintf("HTTP %d; trying %s", resp.StatusCode, candidates[i+1].Deployment.ID), StatusCode: resp.StatusCode})
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, i, max)
				continue
			}
			if c.Deployment.ProviderType == "anthropic_compatible" {
				writeRawUpstreamError(w, lastStatus, lastContentType, lastBody)
				return
			}
			anthropicErrorJSON(w, resp.StatusCode, lastErr)
			return
		}

		w.Header().Set("X-Gateway-Deployment", c.Deployment.ID)
		w.Header().Set("X-Gateway-Provider", c.Deployment.ProviderID)
		w.Header().Set("X-Gateway-Upstream-Model", c.Deployment.Model)
		if c.Deployment.ProviderType == "anthropic_compatible" {
			e = proxyResponse(w, resp)
		} else if in.Stream {
			e = streamOpenAIToAnthropic(w, resp, in.Model, r.Header.Get("x-request-id"))
		} else {
			defer resp.Body.Close()
			var o core.OpenAIResponse
			if e = json.NewDecoder(resp.Body).Decode(&o); e == nil {
				writeJSON(w, 200, translate.OpenAIResponseToAnthropic(o, in.Model))
			}
		}
		totalLatency := time.Since(start)
		if e != nil {
			lastErr = e.Error()
			if clientRequestGone(r.Context()) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "client_disconnect", Deployment: c.Deployment.ID, Message: r.Context().Err().Error(), ErrorType: "caller_cancelled", LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
				return
			}
			if cfg.Routing.Strategy == "ready_queue" {
				s.hm.Quarantine(c.Deployment.ID, lastErr, totalLatency)
				s.probe.Recover(c.Deployment.ID)
			} else {
				s.hm.RecordFailure(c.Deployment.ID, lastErr, totalLatency)
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "stream_fail", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: "provider_stream_error", LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
			// Once a successful upstream response has begun, do not attempt fake mid-stream failover.
			return
		}
		s.hm.RecordSuccess(c.Deployment.ID, headerLatency)
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_ok", Deployment: c.Deployment.ID, Message: "request completed", LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
		return
	}
	if gatewayDeadlineExceeded(routeCtx, r.Context()) {
		anthropicErrorJSON(w, http.StatusGatewayTimeout, "gateway request timeout")
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
	anthropicErrorJSON(w, 502, "all candidate deployments failed: "+lastErr)
}

func (s *Server) retryPause(ctx context.Context, requestID string, cfg interface{ RetryBackoff() time.Duration }, i, max int) {
	if i+1 >= max {
		return
	}
	d := jitteredRetryBackoff(cfg.RetryBackoff(), i, requestID)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

func writeRawUpstreamError(w http.ResponseWriter, status int, contentType string, b []byte) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func proxyResponse(w http.ResponseWriter, resp *http.Response) error {
	defer resp.Body.Close()
	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	for k, vs := range resp.Header {
		lk := strings.ToLower(strings.TrimSpace(k))
		switch lk {
		case "connection", "proxy-connection", "keep-alive", "transfer-encoding", "te", "trailer", "upgrade",
			"proxy-authenticate", "proxy-authorization", "authorization", "x-api-key", "x-admin-key", "set-cookie":
			continue
		}
		if isSSE && lk == "content-length" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if isSSE {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)
	if !isSSE {
		_, err := io.Copy(w, resp.Body)
		return err
	}
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

type openAIToolStreamState struct {
	anthIndex int
	id, name  string
	started   bool
	pending   strings.Builder
}

func streamOpenAIToAnthropic(w http.ResponseWriter, resp *http.Response, model string, requestID ...string) error {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	emit := func(name string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		if fl != nil {
			fl.Flush()
		}
	}
	messageID := uniqueStreamID("msg", requestID...)
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": messageID, "type": "message", "role": "assistant", "content": []any{}, "model": model, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}})

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	nextIndex := 0
	textIndex := -1
	textStarted := false
	finish := "end_turn"
	terminal := false
	tools := map[int]*openAIToolStreamState{}
	startText := func() {
		if textStarted {
			return
		}
		textIndex = nextIndex
		nextIndex++
		textStarted = true
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": textIndex, "content_block": map[string]any{"type": "text", "text": ""}})
	}
	startTool := func(st *openAIToolStreamState) {
		if st.started {
			return
		}
		if st.name == "" {
			return
		}
		st.anthIndex = nextIndex
		nextIndex++
		st.started = true
		if st.id == "" {
			st.id = fmt.Sprintf("tool_%d", st.anthIndex)
		}
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": st.anthIndex, "content_block": map[string]any{"type": "tool_use", "id": st.id, "name": st.name, "input": map[string]any{}}})
		if st.pending.Len() > 0 {
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": st.anthIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": st.pending.String()}})
			st.pending.Reset()
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if d == "" {
			continue
		}
		if d == "[DONE]" {
			terminal = true
			break
		}
		var raw map[string]any
		if json.Unmarshal([]byte(d), &raw) != nil {
			continue
		}
		if er, ok := raw["error"]; ok {
			emit("error", map[string]any{"type": "error", "error": er})
			return fmt.Errorf("openai stream error")
		}
		var obj struct {
			Choices []struct {
				Delta struct {
					Content   any                   `json:"content"`
					ToolCalls []core.OpenAIToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(d), &obj) != nil || len(obj.Choices) == 0 {
			continue
		}
		ch := obj.Choices[0]
		switch v := ch.Delta.Content.(type) {
		case string:
			if v != "" {
				startText()
				emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": textIndex, "delta": map[string]any{"type": "text_delta", "text": v}})
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			st := tools[tc.Index]
			if st == nil {
				st = &openAIToolStreamState{anthIndex: -1}
				tools[tc.Index] = st
			}
			if tc.ID != "" {
				st.id = tc.ID
			}
			if tc.Function.Name != "" {
				st.name += tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				if st.started {
					emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": st.anthIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments}})
				} else {
					st.pending.WriteString(tc.Function.Arguments)
				}
			}
			startTool(st)
		}
		if ch.FinishReason != nil {
			terminal = true
			switch *ch.FinishReason {
			case "tool_calls":
				finish = "tool_use"
			case "length":
				finish = "max_tokens"
			default:
				finish = "end_turn"
			}
		}
	}
	if err := scanner.Err(); err != nil {
		emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}})
		return err
	}
	if !terminal {
		err := io.ErrUnexpectedEOF
		emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "upstream stream ended before completion"}})
		return err
	}
	if textStarted {
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": textIndex})
	}
	orderedTools := make([]*openAIToolStreamState, 0, len(tools))
	for _, st := range tools {
		if !st.started {
			if st.name == "" {
				st.name = "tool"
			}
			startTool(st)
		}
		if st.started {
			orderedTools = append(orderedTools, st)
		}
	}
	sort.Slice(orderedTools, func(i, j int) bool { return orderedTools[i].anthIndex < orderedTools[j].anthIndex })
	for _, st := range orderedTools {
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": st.anthIndex})
	}
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": finish, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 0}})
	emit("message_stop", map[string]any{"type": "message_stop"})
	return nil
}
