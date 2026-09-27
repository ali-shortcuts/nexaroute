package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// This file implements the canonical-IR data path: requests whose upstream
// protocol family is served through internal/protocol/canonical (Gemini,
// OpenAI Responses, and uniform handling for the /v1/responses ingress).
//
// Pipeline per attempt:
//
//      canonical.Request -> provider payload encoder -> upstream
//      upstream -> provider response/stream decoder -> canonical
//      canonical -> client protocol encoder -> client

// upstreamKindFor maps a provider config onto a canonical upstream kind.
func upstreamKindFor(pType string) string {
	switch pType {
	case "anthropic_compatible":
		return "anthropic"
	case "gemini":
		return "gemini"
	case "openai_responses":
		return "openai_responses"
	default:
		return "openai_chat"
	}
}

// buildCanonicalAttempt encodes the canonical request for one candidate and
// returns the dispatch bundle. It returns false when the candidate became
// ineligible or the payload could not be built.
func (s *Server) buildCanonicalAttempt(cand router.Scored, req router.Requirement, canReq canonical.Request, clientModel string, profile compat.RequirementProfile) (hedgeAttemptBundle, bool) {
	fresh, a, ok := s.currentRouteCandidate(cand.Deployment.ID, req)
	if !ok {
		return hedgeAttemptBundle{}, false
	}
	if _, ineligible := s.capabilityIneligible("", fresh.Deployment.ID, profile); ineligible {
		return hedgeAttemptBundle{}, false
	}
	bundle := hedgeAttemptBundle{c: fresh, a: a}
	return s.finishCanonicalAttempt(bundle, canReq)
}

// canonicalStreamPump decodes an upstream SSE stream into canonical events and
// re-encodes them into the client's dialect. It returns the transport error,
// if any, after emitting a terminal client-side error frame.
func containsCanonicalEvent(events []canonical.StreamEvent, typ string) bool {
	for _, ev := range events {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

func canonicalAnthropicMessageStop(eventName, data string) bool {
	if strings.TrimSpace(eventName) == "message_stop" {
		return true
	}
	var env struct {
		Type string `json:"type"`
	}
	return json.Unmarshal([]byte(data), &env) == nil && env.Type == "message_stop"
}

func canonicalInitialFrameValid(kind, eventName, data string, evs []canonical.StreamEvent, terminal bool) (bool, error) {
	data = strings.TrimSpace(data)
	if data == "" {
		return false, nil
	}
	for _, ev := range evs {
		if ev.Type == canonical.StreamError {
			return false, fmt.Errorf("upstream stream reported an error before the first valid event")
		}
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &env); err != nil || env == nil {
		return false, fmt.Errorf("invalid initial upstream stream event")
	}
	if raw := env["error"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, fmt.Errorf("upstream stream reported an error before the first valid event")
	}
	var typ string
	if raw := env["type"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &typ)
	}
	switch kind {
	case "anthropic":
		if typ == "ping" || eventName == "ping" {
			return false, nil
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
		if json.Unmarshal([]byte(data), &initial) != nil || initial.Type != "message_start" || initial.Message.ID == "" ||
			initial.Message.Type != "message" || initial.Message.Role != "assistant" {
			return false, fmt.Errorf("Anthropic stream did not begin with a valid message_start")
		}
		var content []json.RawMessage
		if json.Unmarshal(initial.Message.Content, &content) != nil || content == nil || !containsCanonicalEvent(evs, canonical.StreamStart) {
			return false, fmt.Errorf("Anthropic stream did not begin with a valid message_start")
		}
		return true, nil
	case "gemini":
		var candidates []json.RawMessage
		if raw := env["candidates"]; len(raw) > 0 {
			if json.Unmarshal(raw, &candidates) != nil {
				return false, fmt.Errorf("invalid initial Gemini candidates")
			}
			for _, rawCandidate := range candidates {
				var candidate struct {
					Content      json.RawMessage `json:"content"`
					FinishReason string          `json:"finishReason"`
				}
				if json.Unmarshal(rawCandidate, &candidate) != nil || (len(candidate.Content) == 0 && candidate.FinishReason == "") {
					return false, fmt.Errorf("invalid initial Gemini candidate")
				}
			}
			if len(candidates) > 0 {
				return true, nil
			}
		}
		if raw := env["promptFeedback"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return true, nil
		}
		return false, fmt.Errorf("Gemini stream did not begin with a valid candidate")
	case "openai_responses":
		if typ == "" {
			typ = eventName
		}
		switch typ {
		case "response.created", "response.in_progress", "response.output_text.delta", "response.reasoning_summary_text.delta",
			"response.reasoning_text.delta", "response.output_item.added", "response.function_call_arguments.delta",
			"response.output_item.done", "response.incomplete", "response.completed":
			return true, nil
		default:
			if terminal || len(evs) > 0 {
				return true, nil
			}
			return false, fmt.Errorf("Responses stream did not begin with a recognized event")
		}
	default: // OpenAI Chat Completions
		if err := validateOpenAIStreamChunk(data, false); err != nil {
			return false, err
		}
		return true, nil
	}
}

func (s *Server) canonicalStreamPump(
	w http.ResponseWriter,
	resp *http.Response,
	kind, clientProtocol, requestedModel, requestID string,
	usageHook func(input, output int),
) error {
	// The adapter holds provider capacity, credential load and any local quota
	// reservation until the response body reaches EOF or is explicitly closed.
	defer resp.Body.Close()

	reader := canonical.NewSSEReader(resp.Body)
	type decodedFrame struct {
		events   []canonical.StreamEvent
		terminal bool
	}
	var initial decodedFrame
	for {
		name, data, done, err := reader.Next()
		if err != nil {
			return fmt.Errorf("failed reading initial upstream stream event")
		}
		if done {
			return io.ErrUnexpectedEOF
		}
		var evs []canonical.StreamEvent
		var terminal bool
		switch kind {
		case "anthropic":
			evs, err = canonical.DecodeAnthropicStreamEvent(name, data)
		case "gemini":
			evs, terminal, err = canonical.DecodeGeminiStreamChunk(data)
		case "openai_responses":
			evs, terminal, err = canonical.DecodeResponsesStreamEvent(name, data)
		default:
			evs, terminal, err = canonical.DecodeOpenAIStreamChunk(data)
		}
		if err != nil {
			return fmt.Errorf("invalid initial upstream stream event")
		}
		valid, err := canonicalInitialFrameValid(kind, name, data, evs, terminal)
		if err != nil {
			return err
		}
		if !valid {
			continue // e.g. Anthropic ping before message_start
		}
		initial = decodedFrame{events: evs, terminal: terminal}
		break
	}
	if clientProtocol == "openai_chat" && !containsCanonicalEvent(initial.events, canonical.StreamStart) {
		initial.events = append([]canonical.StreamEvent{{Type: canonical.StreamStart}}, initial.events...)
	}

	// Constructing the emitter can commit protocol frames (Anthropic and
	// Responses do this eagerly), so it happens only after a valid upstream
	// initial event has been decoded above.
	emitter := canonical.NewStreamEmitter(clientProtocol, w, requestedModel, requestID)
	terminal := false       // semantic stop reason observed
	sourceComplete := false // upstream protocol's terminal framing event observed
	var streamErr error
	inputTokens, outputTokens := 0, 0
	usageSeen := false
	anthropicToolBlocks := map[int]bool{}

	processEvents := func(evs []canonical.StreamEvent, sourceTerminal bool) error {
		for _, ev := range evs {
			if kind == "anthropic" && ev.Type == canonical.StreamEnd && terminal {
				// message_stop follows message_delta but must not overwrite its
				// semantic stop reason.
				continue
			}
			if kind == "anthropic" {
				switch ev.Type {
				case canonical.StreamToolStart:
					anthropicToolBlocks[ev.ToolIndex] = true
				case canonical.StreamToolEnd:
					if !anthropicToolBlocks[ev.ToolIndex] {
						continue
					}
					delete(anthropicToolBlocks, ev.ToolIndex)
				}
			}
			if ev.Usage != nil {
				if ev.Usage.InputTokens > inputTokens {
					inputTokens = ev.Usage.InputTokens
				}
				if ev.Usage.OutputTokens > outputTokens {
					outputTokens = ev.Usage.OutputTokens
				}
				if ev.Usage.InputTokens > 0 || ev.Usage.OutputTokens > 0 {
					usageSeen = true
				}
			}
			if ev.Type == canonical.StreamError {
				streamErr = fmt.Errorf("upstream stream error")
				ev.ErrorMsg = "upstream stream failed"
			}
			if ev.Type == canonical.StreamEnd {
				terminal = true
			}
			if emitErr := emitter.Emit(ev); emitErr != nil {
				return emitErr
			}
		}
		if sourceTerminal {
			sourceComplete = true
		}
		return nil
	}

	if err := processEvents(initial.events, initial.terminal); err != nil {
		return err
	}
	if streamErr != nil {
		return streamErr
	}
	for !sourceComplete {
		name, data, done, err := reader.Next()
		if err != nil {
			streamErr = err
			if responseCommitted(w) {
				_ = emitter.Emit(canonical.StreamEvent{Type: canonical.StreamError, ErrorMsg: "upstream stream terminated unexpectedly"})
			}
			return streamErr
		}
		if done {
			break
		}
		var evs []canonical.StreamEvent
		var frameTerminal bool
		switch kind {
		case "anthropic":
			evs, err = canonical.DecodeAnthropicStreamEvent(name, data)
			frameTerminal = canonicalAnthropicMessageStop(name, data)
		case "gemini":
			evs, frameTerminal, err = canonical.DecodeGeminiStreamChunk(data)
		case "openai_responses":
			evs, frameTerminal, err = canonical.DecodeResponsesStreamEvent(name, data)
		default:
			evs, frameTerminal, err = canonical.DecodeOpenAIStreamChunk(data)
		}
		if err != nil {
			streamErr = fmt.Errorf("upstream stream protocol violation")
			if responseCommitted(w) {
				_ = emitter.Emit(canonical.StreamEvent{Type: canonical.StreamError, ErrorMsg: "upstream stream protocol violation"})
			}
			return streamErr
		}
		if err := processEvents(evs, frameTerminal); err != nil {
			return err
		}
		if streamErr != nil {
			return streamErr
		}
	}
	if !sourceComplete {
		streamErr = io.ErrUnexpectedEOF
		if responseCommitted(w) {
			_ = emitter.Emit(canonical.StreamEvent{Type: canonical.StreamError, ErrorMsg: "upstream stream ended before completion"})
		}
		return streamErr
	}
	if finErr := emitter.Finish(); finErr != nil {
		return finErr
	}
	if usageHook != nil && usageSeen {
		usageHook(inputTokens, outputTokens)
	}
	return nil
}

// handleCanonicalResponse processes a successful upstream response received
// through the canonical path: stream pumping or non-stream decode/encode.
func (s *Server) handleCanonicalResponse(
	w http.ResponseWriter,
	resp *http.Response,
	kind, clientProtocol, requestedModel, requestID string,
	stream bool,
	// usageHook receives canonical usage once per response.
	usageHook func(input, output int),
) error {
	if stream {
		return s.canonicalStreamPump(w, resp, kind, clientProtocol, requestedModel, requestID, usageHook)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("upstream response read failed: %w", err)
	}
	var canResp canonical.Response
	switch kind {
	case "anthropic":
		canResp, err = canonical.DecodeAnthropicResponse(body)
	case "gemini":
		canResp, err = canonical.DecodeGeminiResponse(body)
	case "openai_responses":
		canResp, err = canonical.DecodeResponsesResponse(body)
	default:
		canResp, err = canonical.DecodeOpenAIChatResponse(body)
	}
	if err != nil {
		return fmt.Errorf("upstream response decode failed: %w", err)
	}
	if usageHook != nil {
		usageHook(canResp.Usage.InputTokens, canResp.Usage.OutputTokens)
	}
	switch clientProtocol {
	case "anthropic":
		an := canonical.EncodeAnthropicResponse(canResp, requestedModel)
		writeJSON(w, http.StatusOK, an)
	case "openai_responses":
		r := canonical.EncodeResponsesResponse(canResp, requestedModel)
		writeJSON(w, http.StatusOK, r)
	default:
		o := canonical.EncodeOpenAIChatResponse(canResp, requestedModel)
		writeJSON(w, http.StatusOK, o)
	}
	return nil
}

// canonicalErrorJSON writes a protocol-appropriate error envelope.
func canonicalErrorJSON(w http.ResponseWriter, clientProtocol string, status int, errType, message string) {
	switch clientProtocol {
	case "anthropic":
		anthropicErrorJSON(w, status, message)
	case "openai_responses":
		writeJSON(w, status, map[string]any{
			"error": map[string]any{"message": message, "type": errType, "code": errType},
		})
	default:
		writeJSON(w, status, map[string]any{
			"error": map[string]any{"message": message, "type": errType, "param": nil, "code": errType},
		})
	}
}

// openAIResponses serves POST /v1/responses (OpenAI Responses API ingress).
// Every upstream family is reached through the canonical IR, which makes the
// Responses API a first-class citizen on par with /v1/messages and
// /v1/chat/completions.
func (s *Server) openAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		canonicalErrorJSON(w, "openai_responses", http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !s.clientAuthAllowed(w, r, false) {
		return
	}
	var in canonical.ResponsesRequest
	raw, err := readJSON(r, &in)
	if err != nil {
		if _, ok := err.(*requestTooLargeError); ok {
			canonicalErrorJSON(w, "openai_responses", http.StatusRequestEntityTooLarge, "invalid_request_error", err.Error())
			return
		}
		canonicalErrorJSON(w, "openai_responses", http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Model) == "" || len(in.Input) == 0 {
		canonicalErrorJSON(w, "openai_responses", http.StatusBadRequest, "invalid_request_error", "model and input are required")
		return
	}
	canReq, err := canonical.DecodeResponsesRequest(in)
	if err != nil {
		canonicalErrorJSON(w, "openai_responses", http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	reqReqs := canReq.DetectRequirements()
	// Phase C: feature extraction + task classification (observational)
	hasSystem := len(in.Instructions) > 0
	var toolChoiceRequired *bool
	if canReq.ToolChoice != nil {
		// In canonical IR, NeedsTool indicates required
		req := reqReqs.NeedsTool
		toolChoiceRequired = &req
	}
	ti := extractFeaturesAndClassify(raw, feature.ExtractOptions{
		Protocol:               feature.ProtocolResponses,
		Model:                  in.Model,
		Streaming:              reqReqs.Streaming,
		VisionType:             "input_image",
		ReasoningKeys:          []string{"reasoning"},
		ContentFields:          []string{"input", "instructions"},
		MaxOutputTokens:        in.MaxOutputTokens,
		ToolCountHint:          len(in.Tools),
		ToolChoiceRequiredHint: toolChoiceRequired,
		HasSystemPromptHint:    &hasSystem,
	})
	if ti.Features.TooComplex {
		canonicalErrorJSON(w, "openai_responses", http.StatusBadRequest, "invalid_request_error", "request JSON structure is too complex")
		return
	}
	req := router.Requirement{
		Model: in.Model, Tools: ti.Features.HasTools || reqReqs.Tools, Vision: ti.Features.HasVision || reqReqs.Vision,
		Streaming: reqReqs.Streaming, Reasoning: ti.Features.HasReasoning || reqReqs.Reasoning,
	}
	req.EstimatedInputTokens = ti.Features.EstimatedPromptTokens
	req.MaxOutputTokens = in.MaxOutputTokens
	req.MinContextWindow = req.EstimatedInputTokens + req.MaxOutputTokens
	req = s.prepareRequirement(req, r, ti.Features.BodySessionKey)
	cfg, candidates, resolvedRoute, resolveErr := s.candidatesForRequirement(req, "openai_responses")
	if resolveErr != nil {
		if strings.Contains(resolveErr.Error(), "disabled") {
			canonicalErrorJSON(w, "openai_responses", http.StatusNotFound, "invalid_request_error", resolveErr.Error())
		} else {
			canonicalErrorJSON(w, "openai_responses", http.StatusBadRequest, "invalid_request_error", resolveErr.Error())
		}
		return
	}
	// Emit task_classified event (privacy-safe) after final resolution
	s.emitTaskClassified(r.Header.Get("x-request-id"), ti, resolvedRoute)
	if len(candidates) == 0 {
		canonicalErrorJSON(w, "openai_responses", http.StatusServiceUnavailable, "server_error", "no compatible healthy deployment")
		return
	}
	// Phase D/E: Decision plane — rank within eligible set only, fail-open, policy-aware
	candidates = s.applyDecisionPlane(r.Context(), candidates, ti, resolvedRoute, r.Header.Get("x-request-id"), req)
	if resolvedRoute != nil {
		w.Header().Set("X-Gateway-Virtual-Endpoint", resolvedRoute.VirtualEndpointID)
		w.Header().Set("X-Gateway-Public-Model", resolvedRoute.PublicModel)
		w.Header().Set("X-Gateway-Route-Profile", resolvedRoute.RouteProfileID)
	}
	// For virtual endpoints, ignore virtual model for eligibility.
	reqEligible := req
	if resolvedRoute != nil {
		reqEligible.Model = ""
	}
	max := cfg.Routing.MaxAttempts
	if max > len(candidates) {
		max = len(candidates)
	}
	routeCtx, routeCancel := routeContext(r.Context(), req.Streaming, cfg.RequestTimeout())
	routeCtx = providers.WithQuotaEstimate(routeCtx, req.EstimatedInputTokens, req.MaxOutputTokens)
	defer routeCancel()
	forward := copySelectedRequestHeaders(r)
	profile := profileFromRequirement(req, &canReq)
	dialects := map[string]compat.DialectProfile{}

	var lastErr string
	var lastClass compat.Classified
	var lastRetryAfter string
	var gatewayTimedOut bool
	attempts := 0
	for i := 0; i < len(candidates) && attempts < max; i++ {
		c := candidates[i]
		attempts++
		deployment := c.Deployment
		dialect, cached := dialects[deployment.ProviderID]
		if !cached {
			dialect = s.dialectFor(deployment.ProviderID, deployment.ProviderType, "")
			dialects[deployment.ProviderID] = dialect
		}
		bundle, ok := s.buildCanonicalAttempt(c, reqEligible, canReq, in.Model, profile)
		if !ok {
			lastErr = "candidate could not serve this request"
			continue
		}
		start := time.Now()
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_attempt", Deployment: deployment.ID,
			Message: fmt.Sprintf("attempt=%d kind=%s score=%.2f", attempts, bundle.canonicalKind, c.Score)})
		resp, sentPayload, repair, derr := s.doUpstreamWithRepair(
			routeCtx, r.Header.Get("x-request-id"), bundle.a, deployment,
			bundle.payload, req.Streaming, forward, dialect, profile,
			cfg.Routing.MaxRepairAttempts, &canReq,
		)
		if derr != nil {
			if repair.Repaired {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "compat_repair", Deployment: deployment.ID, Message: repair.Description})
			}
			if clientRequestGone(r.Context()) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "client_disconnect", Deployment: deployment.ID, Message: "client cancelled request", ErrorType: "caller_cancelled"})
				return
			}
			if gatewayDeadlineError(routeCtx, r.Context(), derr) {
				gatewayTimedOut = true
				lastRetryAfter = ""
				lastClass = compat.Classified{Class: compat.ClassTimeout, StatusCode: http.StatusGatewayTimeout}
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_timeout", Deployment: deployment.ID, Message: "gateway request deadline exceeded", ErrorType: "gateway_timeout", FailureClass: string(compat.ClassTimeout)})
				break
			}
			cls := classifyUpstreamTransportError(derr)
			lastClass = cls
			lastErr = safeFailureReason(cls)
			policy := s.applyClassifiedFailure(deployment, cls, time.Since(start), providers.RetryAfterValue(derr), derr)
			if cls.Class == compat.ClassRateLimit || cls.StatusCode == http.StatusTooManyRequests {
				lastRetryAfter = retryAfterErrorValue(derr, time.Duration(cfg.Routing.MaxRetryAfterSeconds)*time.Second)
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: deployment.ID,
				Message: "upstream transport failed", ErrorType: policy.ErrorType, FailureClass: string(cls.Class), LatencyMS: time.Since(start).Milliseconds(), StatusCode: cls.StatusCode})
			if policy.Failover && attempts < max && i+1 < len(candidates) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: deployment.ID, Message: "trying next eligible candidate", FailureClass: string(cls.Class), StatusCode: cls.StatusCode})
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attempts-1, max)
				continue
			}
			if !policy.Failover {
				writeClassifiedTerminalError(w, "openai_responses", cls, lastRetryAfter)
				return
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
			b = bundle.a.RedactBody(b)
			cls, policy := classifyFailure(resp.StatusCode, b)
			lastClass = cls
			lastErr = safeFailureReason(cls)
			if cls.Class == compat.ClassRateLimit || cls.StatusCode == http.StatusTooManyRequests {
				lastRetryAfter = retryAfterResponseValue(resp.Header, time.Duration(cfg.Routing.MaxRetryAfterSeconds)*time.Second)
			} else {
				lastRetryAfter = ""
			}
			s.applyClassifiedFailure(deployment, cls, time.Since(start), resp.Header.Get("Retry-After"), nil)
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: deployment.ID,
				Message: safeTerminalMessage(cls), ErrorType: policy.ErrorType, FailureClass: string(cls.Class), LatencyMS: time.Since(start).Milliseconds(), StatusCode: resp.StatusCode})
			if policy.Failover && attempts < max && i+1 < len(candidates) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: deployment.ID, Message: "trying next eligible candidate", FailureClass: string(cls.Class), StatusCode: resp.StatusCode})
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attempts-1, max)
				continue
			}
			writeClassifiedTerminalError(w, "openai_responses", cls, lastRetryAfter)
			return
		}

		if req.Streaming {
			resp.Body = observeFirstByte(resp.Body, start, func(d time.Duration) { s.hm.RecordTTFTForIdentity(deployment.ID, deployment.Identity, d) })
		}
		deploy := deployment
		sent := sentPayload
		var usageErr error
		if canReq.Stream {
			usageErr = s.canonicalStreamPump(w, resp, bundle.canonicalKind, "openai_responses", in.Model, r.Header.Get("x-request-id"),
				func(input, output int) { s.usage.Record(deploy.ID, int64(input), int64(output)) })
		} else {
			usageErr = s.handleCanonicalResponse(w, resp, bundle.canonicalKind, "openai_responses", in.Model, r.Header.Get("x-request-id"), false,
				func(input, output int) { s.usage.Record(deploy.ID, int64(input), int64(output)) })
		}
		total := time.Since(start)
		if usageErr != nil {
			if clientRequestGone(r.Context()) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "client_disconnect", Deployment: deploy.ID, Message: "client cancelled request", ErrorType: "caller_cancelled", LatencyMS: total.Milliseconds()})
				return
			}
			if gatewayDeadlineError(routeCtx, r.Context(), usageErr) {
				gatewayTimedOut = true
				lastRetryAfter = ""
				lastClass = compat.Classified{Class: compat.ClassTimeout, StatusCode: http.StatusGatewayTimeout}
				committed := responseCommitted(w)
				phase := "precommit"
				if committed {
					phase = "postcommit"
				}
				if req.Streaming {
					kind := "stream_fail_precommit"
					if committed {
						kind = "stream_fail_postcommit"
					}
					s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: kind, Deployment: deploy.ID,
						Message: "gateway request deadline exceeded during stream", ErrorType: "gateway_timeout", FailureClass: string(compat.ClassTimeout), StreamPhase: phase, LatencyMS: total.Milliseconds(), StatusCode: resp.StatusCode})
				}
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_timeout", Deployment: deploy.ID,
					Message: "gateway request deadline exceeded", ErrorType: "gateway_timeout", FailureClass: string(compat.ClassTimeout), LatencyMS: total.Milliseconds(), StatusCode: resp.StatusCode})
				if committed {
					return
				}
				break
			}
			cls := compat.ClassifyMalformedResponse(usageErr.Error())
			if req.Streaming {
				cls = compat.ClassifyStreamProtocolError(usageErr.Error())
			}
			lastClass = cls
			lastErr = safeFailureReason(cls)
			committed := responseCommitted(w)
			if !committed {
				s.applyClassifiedFailure(deploy, cls, total, "", nil)
				kind := "response_decode_fail"
				if req.Streaming {
					kind = "stream_fail_precommit"
				}
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: kind, Deployment: deploy.ID,
					Message: "upstream response failed validation before client commit", ErrorType: string(cls.Class), FailureClass: string(cls.Class), StreamPhase: "precommit", LatencyMS: total.Milliseconds(), StatusCode: resp.StatusCode})
				if attempts < max && i+1 < len(candidates) {
					s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attempts-1, max)
					continue
				}
				writeClassifiedTerminalError(w, "openai_responses", cls, "")
				return
			}
			s.applyClassifiedFailure(deploy, cls, total, "", nil)
			kind := "response_decode_fail"
			if req.Streaming {
				kind = "stream_fail_postcommit"
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: kind, Deployment: deploy.ID,
				Message: "upstream response failed validation after client commit", ErrorType: string(cls.Class), FailureClass: string(cls.Class), StreamPhase: "postcommit", LatencyMS: total.Milliseconds(), StatusCode: resp.StatusCode})
			return
		}

		s.recordRouteSuccess(req, deploy, time.Since(start))
		s.learnFromSuccess(deploy.ID, deploy.ProviderID, deploy, sent, &canReq)
		ev := events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_ok", Deployment: deploy.ID,
			Message: "request completed", LatencyMS: total.Milliseconds()}
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
		writeClassifiedTerminalError(w, "openai_responses", compat.Classified{Class: compat.ClassTimeout, StatusCode: http.StatusGatewayTimeout}, "")
		return
	}
	if clientRequestGone(r.Context()) {
		return
	}
	if lastClass.Class != "" {
		writeClassifiedTerminalError(w, "openai_responses", lastClass, lastRetryAfter)
		return
	}
	if lastErr == "" {
		lastClass = compat.Classified{Class: compat.ClassUnknown, StatusCode: http.StatusBadGateway}
	}
	writeClassifiedTerminalError(w, "openai_responses", lastClass, "")
}
