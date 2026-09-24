package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
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
	if v := s.guardrailProblem(raw); v != nil {
		s.rejectGuardrail(w, r, r.Header.Get("x-request-id"), v)
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
	s.telemetryModel(r, in.Model, in.Stream)
	cfg, candidates := s.routeSnapshot(req)
	if len(candidates) == 0 {
		anthropicErrorJSON(w, 503, "no compatible healthy deployment")
		return
	}
	max := cfg.Routing.MaxAttempts
	if max > len(candidates) {
		max = len(candidates)
	}
	routeCtx, routeCancel := routeContext(r.Context(), in.Stream, cfg.RequestTimeout(), cfg.StreamMaxDuration())
	defer routeCancel()
	var lastErr string
	var lastStatus int
	var lastBody []byte
	var lastContentType string
	lastRetryAfter := 0
	forward := copySelectedRequestHeaders(r)
	buildPayload := func(sc router.Scored) ([]byte, error) {
		if sc.Deployment.ProviderType == "anthropic_compatible" {
			return patchJSONModel(raw, sc.Deployment.Model)
		}
		o, err := translate.AnthropicToOpenAI(in, sc.Deployment.Model)
		if err != nil {
			return nil, err
		}
		return json.Marshal(o)
	}

	attempts := 0
	for i := 0; i < len(candidates) && attempts < max; i++ {
		c := candidates[i]
		fresh, a, ok := s.currentRouteCandidate(c.Deployment.ID, req)
		if !ok {
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_skip", Deployment: c.Deployment.ID, Message: "candidate is no longer eligible or provider changed"})
			continue
		}
		c = fresh
		payload, buildErr := buildPayload(c)
		if buildErr != nil {
			lastErr = buildErr.Error()
			continue
		}

		attempts++
		attemptIndex := attempts - 1
		recordUsage := s.usageRecorder(r, c.Deployment.ID)
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_attempt", Deployment: c.Deployment.ID, Message: fmt.Sprintf("attempt=%d score=%.2f health=%s pressure=%.3f", attempts, c.Score, c.Health.Status, c.CapacityPressure)})
		start := time.Now()
		hr := s.hedgedDo(hedgeCall{
			routeCtx: routeCtx, streaming: in.Stream, attemptTimeout: cfg.AttemptTimeout(),
			candidates: candidates, from: i, primary: c, primaryAdapter: a, primaryPayload: payload,
			req: req, buildPayload: buildPayload, forward: forward, requestID: r.Header.Get("x-request-id"),
			strategy: cfg.Routing.Strategy, delay: cfg.HedgeDelay(), maxAttempts: max, attempts: attempts,
		})
		if hr.hedged && hr.candidate.Deployment.ID != c.Deployment.ID {
			c = hr.candidate
			a = hr.adapter
			recordUsage = s.usageRecorder(r, c.Deployment.ID)
		}
		attemptCancel := hr.cancel
		attempts += hr.extraAttempts
		resp, e := hr.resp, hr.err
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
				if !s.consumeFailoverBudget(r.Header.Get("x-request-id"), c.Deployment.ID, "transport failure") {
					w.Header().Set("Retry-After", "1")
					break
				}
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
			errType := errorTypeForStatus(resp.StatusCode)
			if uerr := providers.ClassifyUpstreamResponse(resp.StatusCode, b); uerr != nil {
				errType = errorTypeForUpstreamClass(uerr.Class)
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: errType, LatencyMS: headerLatency.Milliseconds(), StatusCode: resp.StatusCode})
			if failoverEligible(resp.StatusCode) && attempts < max && i+1 < len(candidates) {
				if !s.consumeFailoverBudget(r.Header.Get("x-request-id"), c.Deployment.ID, "http "+strconv.Itoa(resp.StatusCode)) {
					w.Header().Set("Retry-After", "1")
					break
				}
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: c.Deployment.ID, Message: fmt.Sprintf("HTTP %d; trying next eligible candidate", resp.StatusCode), StatusCode: resp.StatusCode})
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
				continue
			}
			if lastStatus == http.StatusTooManyRequests && lastRetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(lastRetryAfter))
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
			if in.Stream {
				e = proxyNativeSSE(w, resp, "anthropic", recordUsage)
			} else {
				e = s.proxyValidatedJSONWithUsage(w, resp, validateAnthropicResponseJSON, func(b []byte) {
					inTok, outTok := usageFromEnvelope("anthropic", b)
					recordUsage(inTok, outTok)
				})
			}
		} else if in.Stream {
			e = streamOpenAIToAnthropic(w, resp, in.Model, recordUsage, r.Header.Get("x-request-id"))
		} else {
			var o core.OpenAIResponse
			e = decodeValidatedJSONLimited(resp.Body, &o, validateOpenAIResponseJSON)
			resp.Body.Close()
			if e == nil {
				var translated core.AnthResponse
				translated, e = translate.OpenAIResponseToAnthropic(o, in.Model)
				if e == nil {
					writeJSON(w, 200, translated)
					recordUsage(int64(o.Usage.PromptTokens), int64(o.Usage.CompletionTokens))
				}
			}
		}
		if attemptCancel != nil {
			attemptCancel() // body fully consumed; release the per-attempt timer
		}
		totalLatency := time.Since(start)
		if e != nil {
			lastErr = e.Error()
			decodeErrType := "provider_invalid_response"
			streamErrType := "provider_stream_error"
			if uerr, ok := asUpstreamLogicalError(e); ok {
				decodeErrType = errorTypeForUpstreamClass(uerr.Class)
				streamErrType = decodeErrType
			}
			if streamErrType == "provider_stream_error" && gatewayDeadlineExceeded(routeCtx, r.Context()) {
				streamErrType = "provider_timeout"
			}
			lastErr = string(redactProviderBody(providerConfigFor(cfg, c.Deployment.ProviderID), []byte(lastErr)))
			if AsClientWriteError(e) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "client_stalled", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: "client_write_timeout", LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
				return
			}
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
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: kind, Deployment: c.Deployment.ID, Message: lastErr, ErrorType: decodeErrType, LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
				if attempts < max && i+1 < len(candidates) {
					if !s.consumeFailoverBudget(r.Header.Get("x-request-id"), c.Deployment.ID, kind) {
						w.Header().Set("Retry-After", "1")
						break
					}
					s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
					continue
				}
				anthropicErrorJSON(w, http.StatusBadGateway, "upstream returned an invalid response: "+lastErr)
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
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "stream_fail", Deployment: c.Deployment.ID, Message: lastErr, ErrorType: streamErrType, LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
			// Once a successful upstream response has begun, do not attempt fake mid-stream failover.
			return
		}
		s.recordRouteSuccess(req, c.Deployment.ID, headerLatency)
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
		if lastStatus == http.StatusTooManyRequests && lastRetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(lastRetryAfter))
		}
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
	_ = writeOnce(w, b)
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
	err = writeOnce(w, b)
	return err
}

// proxyResponse performs an unvalidated streaming passthrough of an upstream
// response. It copies safe response headers, strips sensitive and hop-by-hop
// headers, and flushes incrementally when the payload is an SSE stream.
func proxyResponse(w http.ResponseWriter, resp *http.Response) error {
	defer resp.Body.Close()
	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	copyUpstreamResponseHeaders(w, resp, isSSE)
	w.WriteHeader(resp.StatusCode)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if werr := writeFlushed(w, buf[:n]); werr != nil {
				return werr
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

type nativeSSETracker struct {
	protocol string
	line     []byte
	terminal bool
	inTok    int64
	outTok   int64
	sniff    providers.StreamContentSniffer
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
	// Canonical in-band error detection: error chunks, Anthropic error
	// events, and terminal error finish/stop reasons. finish_reason
	// "error" in particular must fail, not pass as a normal terminal.
	if uerr := providers.ClassifySSEData(t.protocol, data); uerr != nil {
		return uerr
	}

	switch t.protocol {
	case "openai":
		var choices []struct {
			FinishReason json.RawMessage `json:"finish_reason"`
			Delta        struct {
				Content json.RawMessage `json:"content"`
			} `json:"delta"`
		}
		if raw := env["choices"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &choices); err != nil {
				return fmt.Errorf("invalid OpenAI SSE choices: %w", err)
			}
		}
		if raw := env["usage"]; len(raw) > 0 {
			var u struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			}
			_ = json.Unmarshal(raw, &u)
			if u.PromptTokens > 0 {
				t.inTok = u.PromptTokens
			}
			if u.CompletionTokens > 0 {
				t.outTok = u.CompletionTokens
			}
		}
		for _, choice := range choices {
			if len(choice.Delta.Content) > 0 {
				var text string
				if err := json.Unmarshal(choice.Delta.Content, &text); err == nil {
					t.sniff.Add(text)
				}
			}
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
		if typ == "content_block_delta" {
			var delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if raw := env["delta"]; len(raw) > 0 {
				if err := json.Unmarshal(raw, &delta); err == nil && delta.Type == "text_delta" {
					t.sniff.Add(delta.Text)
				}
			}
		}
		if typ == "message_start" {
			var start struct {
				Message struct {
					Usage struct {
						InputTokens int64 `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if raw := env["message"]; len(raw) > 0 {
				_ = json.Unmarshal(raw, &start.Message)
				t.inTok = start.Message.Usage.InputTokens
			}
		}
		if typ == "message_stop" {
			t.terminal = true
		}
		if typ == "message_delta" {
			var delta struct {
				StopReason *string `json:"stop_reason"`
				Usage      struct {
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			}
			if raw := env["delta"]; len(raw) > 0 {
				if err := json.Unmarshal(raw, &delta); err != nil {
					return fmt.Errorf("invalid Anthropic SSE message_delta: %w", err)
				}
				if delta.StopReason != nil && *delta.StopReason != "" {
					t.terminal = true
				}
			}
			if raw := env["usage"]; len(raw) > 0 {
				var u struct {
					OutputTokens int64 `json:"output_tokens"`
				}
				_ = json.Unmarshal(raw, &u)
				if u.OutputTokens > 0 {
					t.outTok = u.OutputTokens
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
	// A paywall message delivered as stream deltas is still a failure: it
	// must not record success or pin the session. Bytes are already
	// committed, so detection only corrects accounting, not delivery.
	if class := t.sniff.Sniff(); class != providers.UpstreamOK {
		return providers.ContentSniffError(class, true)
	}
	if !t.terminal {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func proxyNativeSSE(w http.ResponseWriter, resp *http.Response, protocol string, recordUsage func(in, out int64)) error {
	defer resp.Body.Close()
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return fmt.Errorf("expected text/event-stream from %s upstream", protocol)
	}
	copyUpstreamResponseHeaders(w, resp, true)
	w.WriteHeader(resp.StatusCode)
	tracker := &nativeSSETracker{protocol: protocol}
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if terr := tracker.consume(buf[:n]); terr != nil {
				return terr
			}
			if werr := writeFlushed(w, buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				if ferr := tracker.finish(); ferr != nil {
					return ferr
				}
				if recordUsage != nil {
					recordUsage(tracker.inTok, tracker.outTok)
				}
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

func streamOpenAIToAnthropic(w http.ResponseWriter, resp *http.Response, model string, recordUsage func(in, out int64), requestID ...string) error {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
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
		writeErr = writeFlushed(w, []byte("event: "+name+"\ndata: "+string(b)+"\n\n"))
	}
	messageID := uniqueStreamID("msg", requestID...)
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": messageID, "type": "message", "role": "assistant", "content": []any{}, "model": model, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}})
	if writeErr != nil {
		return writeErr
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	var usageIn, usageOut int64
	var sniff providers.StreamContentSniffer
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
		if err := json.Unmarshal([]byte(d), &raw); err != nil {
			return fmt.Errorf("invalid OpenAI SSE JSON: %w", err)
		}
		if u, ok := raw["usage"]; ok {
			if ub, err := json.Marshal(u); err == nil {
				ui, uo := usageFromEnvelope("openai", ub)
				if ui > 0 {
					usageIn = ui
				}
				if uo > 0 {
					usageOut = uo
				}
			}
		}
		if uerr := providers.ClassifySSEData("openai", []byte(d)); uerr != nil {
			emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": uerr.Message}})
			if writeErr != nil {
				return writeErr
			}
			return uerr
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
		if err := json.Unmarshal([]byte(d), &obj); err != nil {
			return fmt.Errorf("invalid OpenAI SSE chunk: %w", err)
		}
		if len(obj.Choices) == 0 {
			continue
		}
		ch := obj.Choices[0]
		switch v := ch.Delta.Content.(type) {
		case string:
			if v != "" {
				sniff.Add(v)
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
		if writeErr != nil {
			return writeErr
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
	if class := sniff.Sniff(); class != providers.UpstreamOK {
		uerr := providers.ContentSniffError(class, true)
		emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": uerr.Message}})
		return uerr
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
	if writeErr != nil {
		return writeErr
	}
	if recordUsage != nil {
		recordUsage(usageIn, usageOut)
	}
	return nil
}
