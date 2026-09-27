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
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/translate"
)

func (s *Server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		anthropicErrorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.clientAuthAllowed(w, r, true) {
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

	// Phase C: feature extraction + task classification (observational, routing-neutral)
	hasSystem := false
	if in.System != nil {
		hasSystem = true
	}
	var toolChoiceRequired *bool
	if in.ToolChoice != nil {
		b, _ := json.Marshal(in.ToolChoice)
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			typ, _ := m["type"].(string)
			lowerTyp := strings.ToLower(typ)
			_, hasName := m["name"]
			req := hasName || lowerTyp == "tool" || lowerTyp == "any" || lowerTyp == "required"
			toolChoiceRequired = &req
		} else {
			// If string value
			var s string
			if json.Unmarshal(b, &s) == nil {
				lower := strings.ToLower(strings.TrimSpace(s))
				req := lower == "any" || lower == "tool" || lower == "required"
				toolChoiceRequired = &req
			}
		}
	}
	ti := extractFeaturesAndClassify(raw, feature.ExtractOptions{
		Protocol:               feature.ProtocolAnthropic,
		Model:                  in.Model,
		Streaming:              in.Stream,
		VisionType:             "image",
		ReasoningKeys:          []string{"thinking", "reasoning"},
		ContentFields:          []string{"messages"},
		MaxOutputTokens:        in.MaxTokens,
		ToolCountHint:          len(in.Tools),
		ToolChoiceHint:         in.ToolChoice != nil,
		ToolChoiceRequiredHint: toolChoiceRequired,
		HasSystemPromptHint:    &hasSystem,
	})
	if ti.Features.TooComplex {
		anthropicErrorJSON(w, http.StatusBadRequest, "request JSON structure is too complex")
		return
	}
	req := router.Requirement{Model: in.Model, Tools: ti.Features.HasTools, Vision: ti.Features.HasVision, Streaming: in.Stream, Reasoning: ti.Features.HasReasoning}
	if req.Reasoning {
		req.ProviderType = "anthropic_compatible"
	}
	// Context and cost pre-routing use the same conservative prompt estimate.
	// Anthropic requires an explicit max_tokens, so cost-aware ordering has a
	// complete output ceiling for this request.
	req.EstimatedInputTokens = ti.Features.EstimatedPromptTokens
	req.MaxOutputTokens = in.MaxTokens
	req.MinContextWindow = req.EstimatedInputTokens + req.MaxOutputTokens
	req = s.prepareRequirement(req, r, ti.Features.BodySessionKey)
	cfg, candidates, resolvedRoute, resolveErr := s.candidatesForRequirement(req, "anthropic")
	if resolveErr != nil {
		// Disabled endpoint or protocol not allowed
		if strings.Contains(resolveErr.Error(), "disabled") {
			anthropicErrorJSON(w, 404, resolveErr.Error())
		} else {
			anthropicErrorJSON(w, 400, resolveErr.Error())
		}
		return
	}
	if len(candidates) == 0 && req.ProviderType != "" {
		relaxed := req
		relaxed.ProviderType = ""
		if c2, cand2, rr2, err2 := s.candidatesForRequirement(relaxed, "anthropic"); len(cand2) > 0 && err2 == nil {
			req = relaxed
			cfg, candidates = c2, cand2
			resolvedRoute = rr2
		}
	}
	// Emit task_classified event (privacy-safe) after final resolution
	s.emitTaskClassified(r.Header.Get("x-request-id"), ti, resolvedRoute)
	if len(candidates) == 0 {
		anthropicErrorJSON(w, 503, "no compatible healthy deployment")
		return
	}
	// For virtual endpoints, ignore virtual public model for eligibility.
	reqEligible := req
	if resolvedRoute != nil {
		reqEligible.Model = ""
	}
	// Exact-match response cache (opt-in; see cache_wiring.go).
	// Phase F/G: Check cache BEFORE decision — on HIT, decision calls must be 0
	cacheKey, cacheable := s.cacheLookupFor(r.URL.Path, raw, in.Stream, in.Temperature, in.TopP)
	if s.cacheServe(w, r, cacheKey, cacheable) {
		return
	}
	// Phase D/E/G: Decision plane — rank within eligible set only, fail-open, chain-aware
	candidates = s.applyDecisionPlane(r.Context(), candidates, ti, resolvedRoute, r.Header.Get("x-request-id"), req)
	// For observability: if virtual endpoint, add headers
	if resolvedRoute != nil {
		w.Header().Set("X-Gateway-Virtual-Endpoint", resolvedRoute.VirtualEndpointID)
		w.Header().Set("X-Gateway-Public-Model", resolvedRoute.PublicModel)
		w.Header().Set("X-Gateway-Route-Profile", resolvedRoute.RouteProfileID)
	}
	max := cfg.Routing.MaxAttempts
	if max > len(candidates) {
		max = len(candidates)
	}
	profile := profileFromRequirement(req, nil)
	if toolChoiceRequired != nil {
		profile.NeedsTool = *toolChoiceRequired
	}
	routeCtx, routeCancel := routeContext(r.Context(), in.Stream, cfg.RequestTimeout())
	routeCtx = providers.WithQuotaEstimate(routeCtx, req.EstimatedInputTokens, req.MaxOutputTokens)
	defer routeCancel()
	var lastErr string
	var lastClass compat.Classified
	var lastRetryAfter string
	var gatewayTimedOut bool
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
		primary, ok := s.buildAnthropicAttempt(c, reqEligible, raw, in)
		if !ok {
			if primary.buildErr != nil {
				lastErr = primary.buildErr.Error()
			} else {
				lastErr = "attempt payload could not be built"
			}
			continue
		}
		var nm *translate.NameMap
		var streamOptionsInjected bool
		var payload []byte
		kind := primary.canonicalKind
		c, a, nm, streamOptionsInjected, payload = primary.c, primary.a, primary.nm, primary.injected, primary.payload
		attempts++
		attemptIndex := attempts - 1
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_attempt", Deployment: c.Deployment.ID, Message: fmt.Sprintf("attempt=%d score=%.2f health=%s pressure=%.3f", attempts, c.Score, c.Health.Status, c.CapacityPressure)})
		start := time.Now()
		out, winner, hedgeLaunched := s.doAttemptWithHedge(routeCtx, r.Header.Get("x-request-id"), cfg, candidates, i, attempts, max, primary,
			func(idx int) (hedgeAttemptBundle, bool) {
				return s.buildAnthropicAttempt(candidates[idx], reqEligible, raw, in)
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
			kind = winner.canonicalKind
			c, a, nm, streamOptionsInjected, payload = winner.c, winner.a, winner.nm, winner.injected, winner.payload
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_attempt", Deployment: c.Deployment.ID, Message: fmt.Sprintf("attempt=%d score=%.2f health=%s pressure=%.3f (hedged winner)", attempts, c.Score, c.Health.Status, c.CapacityPressure)})
		}
		resp, e := out.resp, out.err
		start = out.start
		if e == nil && resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// Bounded deterministic repair: a classified capability rejection
			// (e.g. "temperature is not supported") retries once with an
			// adapted payload instead of killing a healthy deployment.
			resp, payload, _ = s.maybeRepairUpstream(routeCtx, r.Header.Get("x-request-id"),
				hedgeAttemptBundle{c: c, a: a}, payload, resp, in.Stream, forward, cfg.Routing.MaxRepairAttempts, profile)
			if resp == nil {
				e = fmt.Errorf("repair retry transport failure")
			}
		}
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
			if clientRequestGone(r.Context()) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "client_disconnect", Deployment: c.Deployment.ID, Message: "client cancelled request", ErrorType: "caller_cancelled", LatencyMS: headerLatency.Milliseconds()})
				return
			}
			if gatewayDeadlineError(routeCtx, r.Context(), e) {
				gatewayTimedOut = true
				lastRetryAfter = ""
				lastClass = compat.Classified{Class: compat.ClassTimeout, StatusCode: http.StatusGatewayTimeout}
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_timeout", Deployment: c.Deployment.ID, Message: "gateway request deadline exceeded", ErrorType: "gateway_timeout", FailureClass: string(compat.ClassTimeout), LatencyMS: headerLatency.Milliseconds()})
				break
			}
			cls := classifyUpstreamTransportError(e)
			lastClass = cls
			lastErr = safeFailureReason(cls)
			policy := s.applyClassifiedFailure(c.Deployment, cls, headerLatency, providers.RetryAfterValue(e), e)
			if cls.Class == compat.ClassRateLimit || cls.StatusCode == http.StatusTooManyRequests {
				lastRetryAfter = retryAfterErrorValue(e, time.Duration(cfg.Routing.MaxRetryAfterSeconds)*time.Second)
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: c.Deployment.ID,
				Message: "upstream transport failed", ErrorType: policy.ErrorType, FailureClass: string(cls.Class), LatencyMS: headerLatency.Milliseconds(), StatusCode: cls.StatusCode})
			if policy.Failover && attempts < max && i+1 < len(candidates) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: c.Deployment.ID, Message: "trying next eligible candidate", FailureClass: string(cls.Class), StatusCode: cls.StatusCode})
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
				continue
			}
			if !policy.Failover {
				writeClassifiedTerminalError(w, "anthropic", cls, lastRetryAfter)
				return
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
			b = a.RedactBody(b)
			cls, policy := classifyFailure(resp.StatusCode, b)
			lastClass = cls
			lastErr = safeFailureReason(cls)
			if cls.Class == compat.ClassRateLimit || cls.StatusCode == http.StatusTooManyRequests {
				lastRetryAfter = retryAfterResponseValue(resp.Header, time.Duration(cfg.Routing.MaxRetryAfterSeconds)*time.Second)
			} else {
				lastRetryAfter = ""
			}
			s.applyClassifiedFailure(c.Deployment, cls, headerLatency, resp.Header.Get("Retry-After"), nil)
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: c.Deployment.ID,
				Message: safeTerminalMessage(cls), ErrorType: policy.ErrorType, FailureClass: string(cls.Class), LatencyMS: headerLatency.Milliseconds(), StatusCode: resp.StatusCode})
			if policy.Failover && attempts < max && i+1 < len(candidates) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: c.Deployment.ID, Message: "trying next eligible candidate", FailureClass: string(cls.Class), StatusCode: resp.StatusCode})
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
				continue
			}
			writeClassifiedTerminalError(w, "anthropic", cls, lastRetryAfter)
			return
		}

		w.Header().Set("X-Gateway-Deployment", c.Deployment.ID)
		w.Header().Set("X-Gateway-Provider", c.Deployment.ProviderID)
		w.Header().Set("X-Gateway-Upstream-Model", c.Deployment.Model)
		if in.Stream {
			deploymentID := c.Deployment.ID
			resp.Body = observeFirstByte(resp.Body, start, func(d time.Duration) { s.hm.RecordTTFTForIdentity(deploymentID, c.Deployment.Identity, d) })
		}
		switch {
		case kind != "":
			// Canonical-IR upstream (Gemini / Responses-native): decode into
			// canonical events/blocks and re-encode for the Anthropic client.
			if in.Stream {
				e = s.canonicalStreamPump(w, resp, kind, "anthropic", in.Model, r.Header.Get("x-request-id"),
					func(input, output int) { s.usage.Record(c.Deployment.ID, int64(input), int64(output)) })
			} else {
				e = s.handleCanonicalResponse(w, resp, kind, "anthropic", in.Model, r.Header.Get("x-request-id"), false,
					func(input, output int) { s.usage.Record(c.Deployment.ID, int64(input), int64(output)) })
			}
		case c.Deployment.ProviderType == "anthropic_compatible":
			if in.Stream {
				e = proxyNativeSSEWithModel(w, resp, "anthropic", in.Model, func(prompt, completion int) {
					s.usage.Record(c.Deployment.ID, int64(prompt), int64(completion))
				})
			} else {
				var b []byte
				b, e = readJSONLimited(resp.Body)
				resp.Body.Close()
				if e == nil {
					e = validateAnthropicResponseJSON(b)
				}
				if e == nil {
					b, e = rewriteAnthropicResponseModel(b, in.Model)
				}
				if e == nil {
					if p, ct, ok := extractAnthropicUsage(b); ok {
						s.usage.Record(c.Deployment.ID, int64(p), int64(ct))
					}
					s.cacheStoreResponse(cacheKey, cacheable, c.Deployment.ID, resp.StatusCode, "application/json", b)
					copyUpstreamResponseHeaders(w, resp, false)
					w.WriteHeader(resp.StatusCode)
					_, e = w.Write(b)
				}
			}
		case in.Stream:
			e = streamOpenAIToAnthropicWithUsage(w, resp, in.Model, nm, func(prompt, completion int) {
				s.usage.Record(c.Deployment.ID, int64(prompt), int64(completion))
			}, r.Header.Get("x-request-id"))
		default:
			var o core.OpenAIResponse
			e = decodeValidatedJSONLimited(resp.Body, &o, validateOpenAIResponseJSON)
			resp.Body.Close()
			if e == nil {
				s.usage.Record(c.Deployment.ID, int64(o.Usage.PromptTokens), int64(o.Usage.CompletionTokens))
				var translated core.AnthResponse
				translated, e = translate.OpenAIResponseToAnthropic(o, in.Model, nm)
				if e == nil {
					if b, merr := json.Marshal(translated); merr == nil {
						s.cacheStoreResponse(cacheKey, cacheable, c.Deployment.ID, 200, "application/json", b)
					}
					writeJSON(w, 200, translated)
				}
			}
		}
		totalLatency := time.Since(start)
		if e != nil {
			if clientRequestGone(r.Context()) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "client_disconnect", Deployment: c.Deployment.ID, Message: "client cancelled request", ErrorType: "caller_cancelled", LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
				return
			}
			if gatewayDeadlineError(routeCtx, r.Context(), e) {
				gatewayTimedOut = true
				lastRetryAfter = ""
				lastClass = compat.Classified{Class: compat.ClassTimeout, StatusCode: http.StatusGatewayTimeout}
				committed := responseCommitted(w)
				phase := "precommit"
				if committed {
					phase = "postcommit"
				}
				if in.Stream {
					kind := "stream_fail_precommit"
					if committed {
						kind = "stream_fail_postcommit"
					}
					s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: kind, Deployment: c.Deployment.ID,
						Message: "gateway request deadline exceeded during stream", ErrorType: "gateway_timeout", FailureClass: string(compat.ClassTimeout), StreamPhase: phase, LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
				}
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_timeout", Deployment: c.Deployment.ID,
					Message: "gateway request deadline exceeded", ErrorType: "gateway_timeout", FailureClass: string(compat.ClassTimeout), LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
				if committed {
					return
				}
				break
			}
			cls := compat.ClassifyMalformedResponse(e.Error())
			if in.Stream {
				cls = compat.ClassifyStreamProtocolError(e.Error())
			}
			lastClass = cls
			lastErr = safeFailureReason(cls)
			committed := responseCommitted(w)
			if !committed {
				s.applyClassifiedFailure(c.Deployment, cls, totalLatency, "", nil)
				kind := "response_decode_fail"
				if in.Stream {
					kind = "stream_fail_precommit"
				}
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: kind, Deployment: c.Deployment.ID,
					Message: "upstream response failed validation before client commit", ErrorType: string(cls.Class), FailureClass: string(cls.Class), StreamPhase: "precommit", LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
				if attempts < max && i+1 < len(candidates) {
					s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attemptIndex, max)
					continue
				}
				writeClassifiedTerminalError(w, "anthropic", cls, "")
				return
			}
			s.applyClassifiedFailure(c.Deployment, cls, totalLatency, "", nil)
			kind := "response_decode_fail"
			phase := "postcommit"
			if in.Stream {
				kind = "stream_fail_postcommit"
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: kind, Deployment: c.Deployment.ID,
				Message: "upstream response failed validation after client commit", ErrorType: string(cls.Class), FailureClass: string(cls.Class), StreamPhase: phase, LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode})
			// Once the client response has started, never fake continuation on a
			// different upstream. The stream ends with a protocol-valid error frame
			// only when the stream encoder itself can safely provide one.
			return
		}

		s.recordRouteSuccess(req, c.Deployment, headerLatency)
		s.learnFromSuccess(c.Deployment.ID, c.Deployment.ProviderID, c.Deployment, payload, nil)
		ev := events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_ok", Deployment: c.Deployment.ID, Message: "request completed", LatencyMS: totalLatency.Milliseconds(), StatusCode: resp.StatusCode}
		if resolvedRoute != nil {
			ev.VirtualEndpoint = resolvedRoute.VirtualEndpointID
			ev.PublicModel = resolvedRoute.PublicModel
			ev.RouteProfile = resolvedRoute.RouteProfileID
			ev.Pool = resolvedRoute.PrimaryPoolID
		}
		s.bus.Add(ev)
		return
	}
	if gatewayTimedOut || gatewayDeadlineExceeded(routeCtx, r.Context()) {
		anthropicErrorJSON(w, http.StatusGatewayTimeout, "gateway request timeout")
		return
	}
	if clientRequestGone(r.Context()) {
		return
	}
	if lastClass.Class != "" {
		writeClassifiedTerminalError(w, "anthropic", lastClass, lastRetryAfter)
		return
	}
	if lastErr == "" {
		lastClass = compat.Classified{Class: compat.ClassUnknown, StatusCode: http.StatusBadGateway}
	}
	writeClassifiedTerminalError(w, "anthropic", lastClass, "")
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
	protocol  string
	terminal  bool
	doneToken bool

	// Usage is emitted only after a protocol-valid terminal frame.
	usageHook    func(prompt, completion int)
	usageInput   int
	usageOutput  int
	usageSeen    bool
	usageEmitted bool
}

const maxNativeSSEFrameBytes = 8 << 20

func readNativeSSEFrame(reader *bufio.Reader) ([]byte, error) {
	frame := make([]byte, 0, 1024)
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(frame)+len(fragment) > maxNativeSSEFrameBytes {
			return nil, fmt.Errorf("native SSE frame exceeds %d bytes", maxNativeSSEFrameBytes)
		}
		frame = append(frame, fragment...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(frame) == 0 {
				return nil, io.EOF
			}
			if err == io.EOF {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if len(bytes.TrimSpace(fragment)) == 0 {
			return frame, nil
		}
	}
}

func nativeSSEFrameData(frame []byte) (string, string) {
	var event string
	var data []string
	for _, rawLine := range bytes.Split(frame, []byte("\n")) {
		line := bytes.TrimSuffix(rawLine, []byte("\r"))
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			event = strings.TrimSpace(string(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			value := strings.TrimSpace(string(line[len("data:"):]))
			data = append(data, value)
		}
	}
	return event, strings.Join(data, "\n")
}

func validateNativeSSEInitialFrame(protocol string, frame []byte) (bool, error) {
	event, data := nativeSSEFrameData(frame)
	if data == "" {
		return false, nil // comment/heartbeat frame; wait for protocol data
	}
	if event == "error" {
		return false, fmt.Errorf("%s initial SSE event reported an error", protocol)
	}
	if protocol == "openai" {
		if err := validateOpenAIStreamChunk(data, false); err != nil {
			return false, err
		}
		return true, nil
	}
	if protocol != "anthropic" {
		return false, fmt.Errorf("unsupported native SSE protocol %q", protocol)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &env); err != nil || env == nil {
		return false, fmt.Errorf("invalid Anthropic initial SSE event")
	}
	var initial struct {
		Type    string `json:"type"`
		Message struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(data), &initial); err != nil {
		return false, fmt.Errorf("invalid Anthropic initial SSE event")
	}
	if initial.Type == "ping" || event == "ping" {
		return false, nil
	}
	var content []json.RawMessage
	if initial.Type != "message_start" || initial.Message.ID == "" || initial.Message.Type != "message" ||
		initial.Message.Role != "assistant" || json.Unmarshal(initial.Message.Content, &content) != nil || content == nil {
		return false, fmt.Errorf("Anthropic stream did not begin with a valid message_start event")
	}
	return true, nil
}

func validateAnthropicUsage(raw json.RawMessage) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var usage map[string]json.RawMessage
	if json.Unmarshal(raw, &usage) != nil || usage == nil {
		return fmt.Errorf("invalid Anthropic stream usage")
	}
	for _, name := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		if value := usage[name]; len(value) > 0 {
			var n int
			if json.Unmarshal(value, &n) != nil || n < 0 {
				return fmt.Errorf("invalid Anthropic stream usage field")
			}
		}
	}
	return nil
}

func (t *nativeSSETracker) consume(frame []byte) error {
	event, data := nativeSSEFrameData(frame)
	data = strings.TrimSpace(data)
	if data == "" {
		return nil
	}
	if event == "error" {
		return fmt.Errorf("%s SSE error event", t.protocol)
	}
	if t.protocol == "openai" && data == "[DONE]" {
		t.terminal = true
		t.doneToken = true
		return nil
	}
	if t.protocol == "openai" {
		if err := validateOpenAIStreamChunk(data, true); err != nil {
			return err
		}
		var env map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &env); err != nil {
			return fmt.Errorf("invalid OpenAI SSE event")
		}
		if raw := env["choices"]; len(raw) > 0 {
			var choices []struct {
				FinishReason json.RawMessage `json:"finish_reason"`
			}
			if err := json.Unmarshal(raw, &choices); err != nil {
				return fmt.Errorf("invalid OpenAI SSE choices")
			}
			for _, choice := range choices {
				if len(choice.FinishReason) == 0 || bytes.Equal(bytes.TrimSpace(choice.FinishReason), []byte("null")) {
					continue
				}
				var reason string
				if json.Unmarshal(choice.FinishReason, &reason) != nil {
					return fmt.Errorf("invalid OpenAI SSE finish_reason")
				}
				if reason != "" {
					t.terminal = true
				}
			}
		}
		if raw := env["usage"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			var usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			}
			if json.Unmarshal(raw, &usage) != nil || usage.PromptTokens < 0 || usage.CompletionTokens < 0 {
				return fmt.Errorf("invalid OpenAI SSE usage")
			}
			t.usageInput, t.usageOutput, t.usageSeen = usage.PromptTokens, usage.CompletionTokens, true
		}
		return nil
	}
	if t.protocol != "anthropic" {
		return fmt.Errorf("unknown native SSE protocol %q", t.protocol)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &env); err != nil || env == nil {
		return fmt.Errorf("invalid Anthropic SSE JSON")
	}
	var typ string
	if raw := env["type"]; len(raw) == 0 || json.Unmarshal(raw, &typ) != nil || typ == "" {
		return fmt.Errorf("invalid Anthropic SSE event type")
	}
	if raw := env["error"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || typ == "error" {
		return fmt.Errorf("Anthropic SSE error event")
	}
	switch typ {
	case "ping", "message_stop":
		if typ == "message_stop" {
			t.terminal = true
		}
	case "message_start":
		var initial struct {
			Message struct {
				ID      string          `json:"id"`
				Type    string          `json:"type"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
				Usage   json.RawMessage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(data), &initial) != nil || initial.Message.ID == "" || initial.Message.Type != "message" || initial.Message.Role != "assistant" {
			return fmt.Errorf("invalid Anthropic message_start")
		}
		var content []json.RawMessage
		if json.Unmarshal(initial.Message.Content, &content) != nil || content == nil {
			return fmt.Errorf("invalid Anthropic message_start content")
		}
		if err := validateAnthropicUsage(initial.Message.Usage); err != nil {
			return err
		}
		t.observeAnthropicUsage(typ, env)
	case "content_block_start":
		var chunk struct {
			Index        *int            `json:"index"`
			ContentBlock json.RawMessage `json:"content_block"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || chunk.Index == nil || len(chunk.ContentBlock) == 0 {
			return fmt.Errorf("invalid Anthropic content_block_start")
		}
		var block map[string]json.RawMessage
		if json.Unmarshal(chunk.ContentBlock, &block) != nil || block == nil || len(block["type"]) == 0 {
			return fmt.Errorf("invalid Anthropic content block")
		}
	case "content_block_delta":
		var chunk struct {
			Index *int            `json:"index"`
			Delta json.RawMessage `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || chunk.Index == nil || len(chunk.Delta) == 0 {
			return fmt.Errorf("invalid Anthropic content_block_delta")
		}
		var delta map[string]json.RawMessage
		if json.Unmarshal(chunk.Delta, &delta) != nil || delta == nil || len(delta["type"]) == 0 {
			return fmt.Errorf("invalid Anthropic content block delta")
		}
	case "content_block_stop":
		var chunk struct {
			Index *int `json:"index"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || chunk.Index == nil {
			return fmt.Errorf("invalid Anthropic content_block_stop")
		}
	case "message_delta":
		var chunk struct {
			Delta json.RawMessage `json:"delta"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Delta) == 0 {
			return fmt.Errorf("invalid Anthropic message_delta")
		}
		var delta map[string]json.RawMessage
		if json.Unmarshal(chunk.Delta, &delta) != nil || delta == nil {
			return fmt.Errorf("invalid Anthropic message_delta payload")
		}
		if err := validateAnthropicUsage(chunk.Usage); err != nil {
			return err
		}
		t.observeAnthropicUsage(typ, env)
	default:
		// Future Anthropic event types remain pass-through compatible as long
		// as they are valid JSON objects with a type discriminator.
	}
	if t.usageHook != nil && typ == "message_stop" && (t.usageInput > 0 || t.usageOutput > 0) {
		t.usageSeen = true
	}
	return nil
}

func (t *nativeSSETracker) observeAnthropicUsage(typ string, env map[string]json.RawMessage) {
	switch typ {
	case "message_start":
		var start struct {
			Message struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if raw := env["message"]; len(raw) > 0 && json.Unmarshal(raw, &start) == nil {
			t.usageInput = start.Message.Usage.InputTokens
			if start.Message.Usage.OutputTokens > t.usageOutput {
				t.usageOutput = start.Message.Usage.OutputTokens
			}
		}
	case "message_delta":
		var usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		}
		if raw := env["usage"]; len(raw) > 0 && json.Unmarshal(raw, &usage) == nil {
			if usage.OutputTokens > t.usageOutput {
				t.usageOutput = usage.OutputTokens
			}
			if usage.InputTokens > t.usageInput {
				t.usageInput = usage.InputTokens
			}
		}
	}
}

func (t *nativeSSETracker) finish() error {
	if t.protocol == "openai" && !t.doneToken {
		return io.ErrUnexpectedEOF
	}
	if !t.terminal {
		return io.ErrUnexpectedEOF
	}
	if t.usageHook != nil && t.usageSeen && !t.usageEmitted {
		t.usageEmitted = true
		t.usageHook(t.usageInput, t.usageOutput)
	}
	return nil
}

func proxyNativeSSE(w http.ResponseWriter, resp *http.Response, protocol string, usageHooks ...func(prompt, completion int)) error {
	return proxyNativeSSEWithModel(w, resp, protocol, "", usageHooks...)
}

func writeNativeSSEError(w http.ResponseWriter, protocol string, fl http.Flusher) {
	if protocol == "anthropic" {
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream stream failed\"}}\n\n")
	} else {
		_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"upstream stream failed\",\"type\":\"gateway_stream_error\",\"code\":\"provider_stream_error\"}}\n\n")
	}
	if fl != nil {
		fl.Flush()
	}
}

func proxyNativeSSEWithModel(w http.ResponseWriter, resp *http.Response, protocol, publicModel string, usageHooks ...func(prompt, completion int)) error {
	defer resp.Body.Close()
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return fmt.Errorf("expected text/event-stream from %s upstream", protocol)
	}
	tracker := &nativeSSETracker{protocol: protocol}
	if len(usageHooks) > 0 {
		tracker.usageHook = usageHooks[0]
	}
	reader := bufio.NewReaderSize(resp.Body, 32<<10)
	initialFrames := make([][]byte, 0, 2)
	initialFrameBytes := 0
	for {
		frame, err := readNativeSSEFrame(reader)
		if err != nil {
			return err
		}
		if len(frame) > maxNativeSSEFrameBytes-initialFrameBytes {
			return fmt.Errorf("native SSE initial data exceeds %d bytes", maxNativeSSEFrameBytes)
		}
		initialFrameBytes += len(frame)
		if err := tracker.consume(frame); err != nil {
			return err
		}
		initialFrames = append(initialFrames, frame)
		valid, err := validateNativeSSEInitialFrame(protocol, frame)
		if err != nil {
			return err
		}
		if valid {
			break
		}
	}

	copyUpstreamResponseHeaders(w, resp, true)
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	writeFrame := func(frame []byte) error {
		if protocol == "anthropic" && publicModel != "" {
			frame = rewriteAnthropicSSEModelFrame(frame, publicModel)
		}
		if _, err := w.Write(frame); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	for _, frame := range initialFrames {
		if err := writeFrame(frame); err != nil {
			return err
		}
	}
	for {
		frame, err := readNativeSSEFrame(reader)
		if err == io.EOF {
			if err := tracker.finish(); err != nil {
				writeNativeSSEError(w, protocol, fl)
				return err
			}
			return nil
		}
		if err != nil {
			writeNativeSSEError(w, protocol, fl)
			return err
		}
		if err := tracker.consume(frame); err != nil {
			writeNativeSSEError(w, protocol, fl)
			return err
		}
		if err := writeFrame(frame); err != nil {
			return err
		}
		if (protocol == "anthropic" && tracker.terminal) || (protocol == "openai" && tracker.doneToken) {
			return tracker.finish()
		}
	}
}

func rewriteAnthropicResponseModel(body []byte, model string) ([]byte, error) {
	if strings.TrimSpace(model) == "" {
		return body, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if envelope == nil {
		return nil, fmt.Errorf("Anthropic response must be a JSON object")
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	envelope["model"] = encoded
	return json.Marshal(envelope)
}

func rewriteAnthropicSSEModelFrame(frame []byte, model string) []byte {
	if strings.TrimSpace(model) == "" {
		return frame
	}
	_, data := nativeSSEFrameData(frame)
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &envelope) != nil || envelope == nil {
		return frame
	}
	var typ string
	if json.Unmarshal(envelope["type"], &typ) != nil || typ != "message_start" {
		return frame
	}
	var message map[string]json.RawMessage
	if json.Unmarshal(envelope["message"], &message) != nil || message == nil {
		return frame
	}
	modelJSON, err := json.Marshal(model)
	if err != nil {
		return frame
	}
	message["model"] = modelJSON
	envelope["message"], err = json.Marshal(message)
	if err != nil {
		return frame
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return frame
	}

	var out bytes.Buffer
	replaced := false
	for _, rawLine := range bytes.SplitAfter(frame, []byte("\n")) {
		if len(rawLine) == 0 {
			continue
		}
		line := rawLine
		ending := []byte(nil)
		if bytes.HasSuffix(line, []byte("\n")) {
			ending = []byte("\n")
			line = line[:len(line)-1]
		}
		if bytes.HasSuffix(line, []byte("\r")) {
			ending = append([]byte("\r"), ending...)
			line = line[:len(line)-1]
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			if replaced {
				continue
			}
			prefixEnd := len("data:")
			if len(line) > prefixEnd && line[prefixEnd] == ' ' {
				prefixEnd++
			}
			_, _ = out.Write(line[:prefixEnd])
			_, _ = out.Write(encoded)
			_, _ = out.Write(ending)
			replaced = true
			continue
		}
		_, _ = out.Write(rawLine)
	}
	if !replaced {
		return frame
	}
	return out.Bytes()
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
	return streamOpenAIToAnthropicWithUsage(w, resp, model, nm, nil, requestID...)
}

func validateOpenAIStreamChunk(data string, allowUsageOnly bool) error {
	if strings.TrimSpace(data) == "[DONE]" {
		return fmt.Errorf("OpenAI stream ended before its first valid chunk")
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &env); err != nil || env == nil {
		return fmt.Errorf("invalid OpenAI stream event")
	}
	if raw := env["error"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("OpenAI stream reported an error")
	}
	choicesValid := false
	if raw, ok := env["choices"]; ok {
		var choices []json.RawMessage
		if json.Unmarshal(raw, &choices) != nil {
			return fmt.Errorf("invalid OpenAI stream choices")
		}
		if len(choices) > 0 {
			for _, rawChoice := range choices {
				var choice map[string]json.RawMessage
				if json.Unmarshal(rawChoice, &choice) != nil || choice == nil {
					return fmt.Errorf("invalid OpenAI stream choice")
				}
				if rawDelta := choice["delta"]; len(rawDelta) > 0 && !bytes.Equal(bytes.TrimSpace(rawDelta), []byte("null")) {
					var delta map[string]json.RawMessage
					if json.Unmarshal(rawDelta, &delta) != nil || delta == nil {
						return fmt.Errorf("invalid OpenAI stream delta")
					}
				} else if rawFinish := choice["finish_reason"]; len(rawFinish) > 0 && !bytes.Equal(bytes.TrimSpace(rawFinish), []byte("null")) {
					if !allowUsageOnly {
						return fmt.Errorf("OpenAI stream initial choice has no delta")
					}
					var finish string
					if json.Unmarshal(rawFinish, &finish) != nil {
						return fmt.Errorf("invalid OpenAI stream finish_reason")
					}
				} else {
					return fmt.Errorf("OpenAI stream choice has neither delta nor finish_reason")
				}
				if rawFinish := choice["finish_reason"]; len(rawFinish) > 0 && !bytes.Equal(bytes.TrimSpace(rawFinish), []byte("null")) {
					var finish string
					if json.Unmarshal(rawFinish, &finish) != nil {
						return fmt.Errorf("invalid OpenAI stream finish_reason")
					}
				}
			}
			choicesValid = true
		}
	}
	usageValid := false
	if raw := env["usage"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		}
		if json.Unmarshal(raw, &usage) != nil || usage.PromptTokens < 0 || usage.CompletionTokens < 0 {
			return fmt.Errorf("invalid OpenAI stream usage")
		}
		usageValid = true
	}
	if !choicesValid && !(allowUsageOnly && usageValid) {
		return fmt.Errorf("OpenAI stream did not contain a valid choice")
	}
	return nil
}

func validateOpenAIInitialStreamEvent(data string) error {
	return validateOpenAIStreamChunk(data, false)
}

func streamOpenAIToAnthropicWithUsage(w http.ResponseWriter, resp *http.Response, model string, nm *translate.NameMap, usageSink func(prompt, completion int), requestID ...string) error {
	defer resp.Body.Close()
	reader := newSSEReader(resp.Body)
	var initial sseEvent
	for {
		ev, done, err := reader.Next()
		if err != nil {
			return fmt.Errorf("failed reading initial OpenAI stream event")
		}
		if done {
			return io.ErrUnexpectedEOF
		}
		if ev.name == "error" {
			return fmt.Errorf("OpenAI stream reported an error before its first chunk")
		}
		d := strings.TrimSpace(ev.data)
		if d == "" {
			continue
		}
		if err := validateOpenAIInitialStreamEvent(d); err != nil {
			return err
		}
		initial = ev
		break
	}
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

	pendingInitial := true
	for {
		ev := initial
		done := false
		var err error
		if pendingInitial {
			pendingInitial = false
		} else {
			ev, done, err = reader.Next()
		}
		if err != nil {
			emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "upstream stream terminated unexpectedly"}})
			return err
		}
		if done {
			break
		}
		d := strings.TrimSpace(ev.data)
		if ev.name == "error" {
			emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "upstream stream failed"}})
			return fmt.Errorf("OpenAI stream error event")
		}
		if d == "" || d == "[DONE]" {
			if d == "[DONE]" {
				terminal = true
				break
			}
			continue
		}
		if err := validateOpenAIStreamChunk(d, true); err != nil {
			emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "upstream stream sent an invalid chunk"}})
			return err
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(d), &raw); err != nil {
			// The response is already committed (message_start was sent), so
			// the Anthropic-protocol client must receive a terminal error
			// frame instead of a silently truncated stream.
			emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "upstream stream sent an invalid event"}})
			return fmt.Errorf("invalid OpenAI SSE JSON: %w", err)
		}
		if er, ok := raw["error"]; ok && er != nil {
			emit("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "upstream stream failed"}})
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
	if usageSink != nil && usageSeen {
		usageSink(inputTokens, outputTokens)
	}
	return nil
}
