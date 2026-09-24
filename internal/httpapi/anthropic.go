package httpapi

import (
	"bytes"
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

	inspection := inspectRequestJSON(raw, "image", []string{"thinking", "reasoning"})
	if inspection.TooComplex {
		anthropicErrorJSON(w, http.StatusBadRequest, "request JSON structure is too complex")
		return
	}
	req := router.Requirement{Model: in.Model, Tools: len(in.Tools) > 0, Vision: inspection.Vision, Streaming: in.Stream, Reasoning: inspection.Reasoning}
	if req.Reasoning {
		req.ProviderType = "anthropic_compatible"
	}
	req = s.prepareRequirement(req, r, inspection.BodySessionKey)
	cfg, candidates := s.routeSnapshot(req)
	if len(candidates) == 0 && req.ProviderType != "" {
		// A reasoning request with no Anthropic-compatible deployment still
		// deserves a chance against reasoning-capable OpenAI-compatible
		// deployments; capability gating stays fully enforced.
		relaxed := req
		relaxed.ProviderType = ""
		if c2, cand2 := s.routeSnapshot(relaxed); len(cand2) > 0 {
			req = relaxed
			cfg, candidates = c2, cand2
		}
	}
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
		var nm *translate.NameMap
		streamOptionsInjected := false
		if c.Deployment.ProviderType == "anthropic_compatible" {
			payload, err = patchJSONModel(raw, c.Deployment.Model)
		} else {
			var o core.OpenAIRequest
			o, nm, err = translate.AnthropicToOpenAI(in, c.Deployment.Model)
			if err == nil && in.Stream {
				// Request real token accounting from OpenAI-compatible
				// upstreams so Anthropic clients receive usage; providers that
				// reject the option are retried once without it.
				o.StreamOptions = json.RawMessage(`{"include_usage":true}`)
				streamOptionsInjected = true
			}
			if err == nil {
				payload, err = json.Marshal(o)
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
		if e == nil && streamOptionsInjected && resp.StatusCode == http.StatusBadRequest {
			probe, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if bytes.Contains(bytes.ToLower(probe), []byte("stream_options")) {
				if stripped, ok := stripStreamOptions(payload); ok {
					payload = stripped
					streamOptionsInjected = false
					resp, e = a.Do(routeCtx, payload, in.Stream, forward)
				} else {
					resp.Body = io.NopCloser(bytes.NewReader(probe))
				}
			} else {
				resp.Body = io.NopCloser(bytes.NewReader(probe))
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
			policy := policyForStatus(resp.StatusCode)
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
			if policy.Failover && attempts < max && i+1 < len(candidates) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: c.Deployment.ID, Message: fmt.Sprintf("HTTP %d; trying next eligible candidate", resp.StatusCode), StatusCode: resp.StatusCode})
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
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
		if in.Stream {
			deploymentID := c.Deployment.ID
			resp.Body = observeFirstByte(resp.Body, start, func(d time.Duration) { s.hm.RecordTTFT(deploymentID, d) })
		}
		if c.Deployment.ProviderType == "anthropic_compatible" {
			if in.Stream {
				e = proxyNativeSSE(w, resp, "anthropic")
			} else {
				e = proxyValidatedJSONResponse(w, resp, validateAnthropicResponseJSON)
			}
		} else if in.Stream {
			e = streamOpenAIToAnthropic(w, resp, in.Model, nm, r.Header.Get("x-request-id"))
		} else {
			var o core.OpenAIResponse
			e = decodeValidatedJSONLimited(resp.Body, &o, validateOpenAIResponseJSON)
			resp.Body.Close()
			if e == nil {
				var translated core.AnthResponse
				translated, e = translate.OpenAIResponseToAnthropic(o, in.Model, nm)
				if e == nil {
					writeJSON(w, 200, translated)
				}
			}
		}
		totalLatency := time.Since(start)
		if e != nil {
			lastErr = e.Error()
			if clientRequestGone(r.Context()) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "client_disconnect", Deployment: c.Deployment.ID, Message: r.Context().Err().Error(), ErrorType: "caller_cancelled", LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
				return
			}
			if !responseCommitted(w) {
				lastStatus = 0
				lastBody = nil
				if cfg.Routing.Strategy == "ready_mesh" && req.Streaming {
					s.hm.RecordScopeFailure(c.Deployment.ID, []string{"streaming"}, lastErr)
				} else if router.IsReadyStrategy(cfg.Routing.Strategy) {
					s.hm.Quarantine(c.Deployment.ID, lastErr, totalLatency)
					s.probe.Recover(c.Deployment.ID)
				} else {
					s.hm.RecordFailure(c.Deployment.ID, lastErr, totalLatency)
				}
				kind := "response_decode_fail"
				if req.Streaming {
					kind = "stream_fail_precommit"
				}
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: kind, Deployment: c.Deployment.ID, Message: lastErr, ErrorType: "provider_invalid_response", LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
				if attempts < max && i+1 < len(candidates) {
					s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
					continue
				}
				anthropicErrorJSON(w, http.StatusBadGateway, "upstream returned an invalid response")
				return
			}
			if cfg.Routing.Strategy == "ready_mesh" && req.Streaming {
				s.hm.RecordScopeFailure(c.Deployment.ID, []string{"streaming"}, lastErr)
			} else if router.IsReadyStrategy(cfg.Routing.Strategy) {
				s.hm.Quarantine(c.Deployment.ID, lastErr, totalLatency)
				s.probe.Recover(c.Deployment.ID)
			} else {
				s.hm.RecordFailure(c.Deployment.ID, lastErr, totalLatency)
			}
			s.hm.RecordProviderFailure(c.Deployment.ProviderID, c.Deployment.ID, lastErr)
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "stream_fail", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: "provider_stream_error", LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
			// Once a successful upstream response has begun, do not attempt fake mid-stream failover.
			return
		}
		s.recordRouteSuccess(req, c.Deployment.ID, c.Deployment.ProviderID, headerLatency)
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

func copyUpstreamResponseHeaders(w http.ResponseWriter, resp *http.Response, isSSE bool) {
	connectionScoped := map[string]struct{}{}
	for _, value := range resp.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.ToLower(strings.TrimSpace(token)); token != "" {
				connectionScoped[token] = struct{}{}
			}
		}
	}
	for k, vs := range resp.Header {
		lk := strings.ToLower(strings.TrimSpace(k))
		if _, blocked := connectionScoped[lk]; blocked {
			continue
		}
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
}

func proxyValidatedJSONResponse(w http.ResponseWriter, resp *http.Response, validate jsonEnvelopeValidator) error {
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
	copyUpstreamResponseHeaders(w, resp, false)
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(b)
	return err
}

type nativeSSETracker struct {
	protocol string
	line     []byte
	terminal bool
}

const maxNativeSSELineBytes = 8 << 20

func (t *nativeSSETracker) consume(p []byte) error {
	for len(p) > 0 {
		n := bytes.IndexByte(p, '\n')
		if n < 0 {
			if len(t.line)+len(p) > maxNativeSSELineBytes {
				return fmt.Errorf("native SSE line exceeds %d bytes", maxNativeSSELineBytes)
			}
			t.line = append(t.line, p...)
			return nil
		}
		if len(t.line)+n > maxNativeSSELineBytes {
			return fmt.Errorf("native SSE line exceeds %d bytes", maxNativeSSELineBytes)
		}
		t.line = append(t.line, p[:n]...)
		if err := t.processLine(); err != nil {
			return err
		}
		t.line = t.line[:0]
		p = p[n+1:]
	}
	return nil
}

func (t *nativeSSETracker) processLine() error {
	line := bytes.TrimSpace(t.line)
	if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
		return nil
	}
	data := bytes.TrimSpace(line[len("data:"):])
	if len(data) == 0 {
		return nil
	}
	if t.protocol == "openai" && bytes.Equal(data, []byte("[DONE]")) {
		t.terminal = true
		return nil
	}

	var env map[string]json.RawMessage
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("invalid %s SSE JSON: %w", t.protocol, err)
	}
	if raw := env["error"]; len(raw) > 0 && string(raw) != "null" {
		return fmt.Errorf("%s SSE error event", t.protocol)
	}

	switch t.protocol {
	case "openai":
		var choices []struct {
			FinishReason json.RawMessage `json:"finish_reason"`
		}
		if raw := env["choices"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &choices); err != nil {
				return fmt.Errorf("invalid OpenAI SSE choices: %w", err)
			}
		}
		for _, choice := range choices {
			if len(choice.FinishReason) == 0 || string(choice.FinishReason) == "null" {
				continue
			}
			var reason string
			if err := json.Unmarshal(choice.FinishReason, &reason); err != nil {
				return fmt.Errorf("invalid OpenAI SSE finish_reason: %w", err)
			}
			if reason != "" {
				t.terminal = true
			}
		}
	case "anthropic":
		var typ string
		if raw := env["type"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &typ); err != nil {
				return fmt.Errorf("invalid Anthropic SSE type: %w", err)
			}
		}
		if typ == "error" {
			return fmt.Errorf("anthropic SSE error event")
		}
		if typ == "message_stop" {
			t.terminal = true
		}
		if typ == "message_delta" {
			var delta struct {
				StopReason *string `json:"stop_reason"`
			}
			if raw := env["delta"]; len(raw) > 0 {
				if err := json.Unmarshal(raw, &delta); err != nil {
					return fmt.Errorf("invalid Anthropic SSE message_delta: %w", err)
				}
				if delta.StopReason != nil && *delta.StopReason != "" {
					t.terminal = true
				}
			}
		}
	default:
		return fmt.Errorf("unknown native SSE protocol %q", t.protocol)
	}
	return nil
}

func (t *nativeSSETracker) finish() error {
	if len(t.line) > 0 {
		if err := t.processLine(); err != nil {
			return err
		}
		t.line = nil
	}
	if !t.terminal {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func proxyNativeSSE(w http.ResponseWriter, resp *http.Response, protocol string) error {
	defer resp.Body.Close()
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return fmt.Errorf("expected text/event-stream from %s upstream", protocol)
	}
	copyUpstreamResponseHeaders(w, resp, true)
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	tracker := &nativeSSETracker{protocol: protocol}
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if terr := tracker.consume(buf[:n]); terr != nil {
				return terr
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return tracker.finish()
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

// streamOpenAIToAnthropic translates an OpenAI Chat Completions SSE stream
// into Anthropic Messages SSE events. It propagates real token usage captured
// from the stream_options include_usage tail chunk, tolerates content-part
// array deltas, maps reasoning-style finish reasons, and restores original
// client-facing tool names through the request's NameMap.
func streamOpenAIToAnthropic(w http.ResponseWriter, resp *http.Response, model string, nm *translate.NameMap, requestID ...string) error {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	var writeErr error
	emit := func(name string, v any) {
		if writeErr != nil {
			return
		}
		b, err := json.Marshal(v)
		if err != nil {
			writeErr = err
			return
		}
		_, writeErr = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		if writeErr == nil && fl != nil {
			fl.Flush()
		}
	}
	messageID := uniqueStreamID("msg", requestID...)
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": messageID, "type": "message", "role": "assistant", "content": []any{}, "model": model, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}}})
	if writeErr != nil {
		return writeErr
	}
	// Real Anthropic streams interleave periodic ping frames; emitting one
	// keeps defensive clients that wait for known event names happy.
	emit("ping", map[string]any{"type": "ping"})
	if writeErr != nil {
		return writeErr
	}

	reader := newSSEReader(resp.Body)
	nextIndex := 0
	textIndex := -1
	textStarted := false
	finish := "end_turn"
	terminal := false
	tools := map[int]*openAIToolStreamState{}
	inputTokens, outputTokens := 0, 0
	cacheReadTokens, cacheCreationTokens := 0, 0
	usageSeen := false

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
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": st.anthIndex, "content_block": map[string]any{"type": "tool_use", "id": st.id, "name": nm.Reverse(st.name), "input": map[string]any{}}})
		if st.pending.Len() > 0 {
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": st.anthIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": st.pending.String()}})
			st.pending.Reset()
		}
	}
	emitText := func(txt string) {
		startText()
		if !textStarted {
			return
		}
		emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": textIndex, "delta": map[string]any{"type": "text_delta", "text": txt}})
	}

	for {
		ev, done, err := reader.Next()
		if err != nil {
			emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}})
			return err
		}
		if done {
			break
		}
		d := strings.TrimSpace(ev.data)
		if d == "" || d == "[DONE]" {
			if d == "[DONE]" {
				terminal = true
				break
			}
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(d), &raw); err != nil {
			// The response is already committed (message_start was sent), so
			// the Anthropic-protocol client must receive a terminal error
			// frame instead of a silently truncated stream.
			emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "upstream stream sent an invalid event"}})
			return fmt.Errorf("invalid OpenAI SSE JSON: %w", err)
		}
		if er, ok := raw["error"]; ok {
			emit("error", map[string]any{"type": "error", "error": er})
			if writeErr != nil {
				return writeErr
			}
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
			Usage *core.OpenAIUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(d), &obj); err != nil {
			emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "upstream stream sent an invalid chunk"}})
			return fmt.Errorf("invalid OpenAI SSE chunk: %w", err)
		}
		if obj.Usage != nil {
			usageSeen = true
			inputTokens = obj.Usage.PromptTokens
			outputTokens = obj.Usage.CompletionTokens
			if obj.Usage.PromptTokensDetails != nil {
				cacheReadTokens = obj.Usage.PromptTokensDetails.CachedTokens
			}
		}
		if len(obj.Choices) == 0 {
			continue
		}
		ch := obj.Choices[0]
		switch v := ch.Delta.Content.(type) {
		case string:
			if v != "" {
				emitText(v)
			}
		case []any:
			for _, part := range v {
				m, ok := part.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := m["type"].(string)
				if typ == "text" || typ == "output_text" {
					if txt, _ := m["text"].(string); txt != "" {
						emitText(txt)
					}
				}
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
		if writeErr != nil {
			return writeErr
		}
		if ch.FinishReason != nil {
			terminal = true
			switch *ch.FinishReason {
			case "tool_calls", "function_call":
				finish = "tool_use"
			case "length":
				finish = "max_tokens"
			case "content_filter":
				finish = "refusal"
			default:
				finish = "end_turn"
			}
		}
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
	usage := map[string]int{"output_tokens": outputTokens}
	if usageSeen {
		usage["input_tokens"] = inputTokens
		if cacheReadTokens > 0 {
			usage["cache_read_input_tokens"] = cacheReadTokens
		}
		if cacheCreationTokens > 0 {
			usage["cache_creation_input_tokens"] = cacheCreationTokens
		}
	}
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": finish, "stop_sequence": nil}, "usage": usage})
	emit("message_stop", map[string]any{"type": "message_stop"})
	if writeErr != nil {
		return writeErr
	}
	return nil
}
