package httpapi

import (
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
func (s *Server) canonicalStreamPump(
	w http.ResponseWriter,
	resp *http.Response,
	kind, clientProtocol, requestedModel, requestID string,
	usageHook func(input, output int),
) error {
	// The adapter holds provider capacity, credential load and any local quota
	// reservation until the response body reaches EOF or is explicitly closed.
	// Early decoder/client-write failures therefore must close the body here.
	defer resp.Body.Close()

	fl, _ := w.(http.Flusher)
	emitter := canonical.NewStreamEmitter(clientProtocol, w, requestedModel, requestID)
	reader := canonical.NewSSEReader(resp.Body)
	terminal := false
	var streamErr error
	inputTokens, outputTokens := 0, 0
	usageSeen := false
	anthropicToolBlocks := map[int]bool{}
	for {
		name, data, done, err := reader.Next()
		if err != nil {
			if !terminal {
				_ = emitter.Emit(canonical.StreamEvent{Type: canonical.StreamError, ErrorMsg: "upstream stream read failed: " + err.Error()})
			}
			streamErr = err
			break
		}
		if done {
			break
		}
		var evs []canonical.StreamEvent
		switch kind {
		case "anthropic":
			evs, err = canonical.DecodeAnthropicStreamEvent(name, data)
		case "gemini":
			evs, _, err = canonical.DecodeGeminiStreamChunk(data)
		case "openai_responses":
			evs, terminal, err = canonical.DecodeResponsesStreamEvent(name, data)
		default:
			evs, terminal, err = canonical.DecodeOpenAIStreamChunk(data)
		}
		if err != nil {
			_ = emitter.Emit(canonical.StreamEvent{Type: canonical.StreamError, ErrorMsg: "upstream stream protocol violation: " + err.Error()})
			streamErr = err
			break
		}
		for _, ev := range evs {
			if kind == "anthropic" && ev.Type == canonical.StreamEnd && terminal {
				// Anthropic normally reports the semantic stop reason in
				// message_delta and then follows with message_stop. The latter
				// must terminate framing without overwriting tool_use,
				// max_tokens, stop_sequence, refusal, etc. with end_turn.
				continue
			}
			if kind == "anthropic" {
				switch ev.Type {
				case canonical.StreamToolStart:
					anthropicToolBlocks[ev.ToolIndex] = true
				case canonical.StreamToolEnd:
					if !anthropicToolBlocks[ev.ToolIndex] {
						// Anthropic emits content_block_stop for text, thinking
						// and tool blocks alike. Only a block that previously
						// emitted ToolStart may become a canonical ToolEnd.
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
				streamErr = fmt.Errorf("upstream stream error: %s", ev.ErrorMsg)
			}
			if ev.Type == canonical.StreamEnd {
				terminal = true
			}
			if emitErr := emitter.Emit(ev); emitErr != nil {
				// Client went away; stop reading upstream.
				return emitErr
			}
		}
	}
	if !terminal && streamErr == nil {
		streamErr = io.ErrUnexpectedEOF
		_ = emitter.Emit(canonical.StreamEvent{Type: canonical.StreamError, ErrorMsg: "upstream stream ended before completion"})
	}
	// A stream error is terminal by itself. Calling Finish after emitting an
	// error would append a success tail (for example response.completed or
	// [DONE]) and give clients contradictory terminal states.
	if streamErr == nil && terminal {
		if finErr := emitter.Finish(); finErr != nil {
			streamErr = finErr
		}
	}
	if streamErr == nil && terminal && usageHook != nil && usageSeen {
		usageHook(inputTokens, outputTokens)
	}
	_ = fl
	return streamErr
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
	// Phase D: Decision plane — rank within eligible set only, fail-open
	candidates = s.applyDecisionPlane(r.Context(), candidates, ti, resolvedRoute, r.Header.Get("x-request-id"))
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
			lastErr = derr.Error()
			if repair.Repaired {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "compat_repair", Deployment: deployment.ID, Message: repair.Description})
			}
			if clientRequestGone(r.Context()) {
				return
			}
			cls := compat.ClassifyTransportError(derr)
			policy := cls.Policy()
			s.hm.RecordProviderFailure(deployment.ProviderID, deployment.ID, lastErr)
			if router.IsReadyStrategy(cfg.Routing.Strategy) {
				s.hm.Quarantine(deployment.ID, lastErr, time.Since(start))
				s.probe.Recover(deployment.ID)
			} else if policy.HardCooldown {
				s.hm.ForceCooldown(deployment.ID, lastErr, cfg.Cooldown())
			}
			if attempts < max && i+1 < len(candidates) {
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attempts-1, max)
				continue
			}
			canonicalErrorJSON(w, "openai_responses", http.StatusBadGateway, "api_error", "all candidate deployments failed: "+lastErr)
			return
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
			b = bundle.a.RedactBody(b)
			lastStatus := resp.StatusCode
			lastCT := resp.Header.Get("Content-Type")
			cls, policy := classifyFailure(resp.StatusCode, b)
			// Capability failures never touch deployment health; the router
			// may still fail over to a deployment that supports the feature.
			s.recordProviderFailure(deployment.ProviderID, deployment.ID, cls.Message, policy)
			if !cls.CapabilityFailure {
				if router.IsReadyStrategy(cfg.Routing.Strategy) {
					if policy.QuarantineDeployment {
						s.hm.Quarantine(deployment.ID, cls.Message, time.Since(start))
						s.probe.Recover(deployment.ID)
					}
				} else if policy.HardCooldown {
					d := cfg.Cooldown()
					if resp.StatusCode == http.StatusTooManyRequests {
						d = retryAfterDuration(resp.Header, time.Duration(cfg.Routing.MaxRetryAfterSeconds)*time.Second)
					}
					s.hm.ForceCooldown(deployment.ID, cls.Message, d)
				} else if policy.QuarantineDeployment {
					s.hm.RecordFailure(deployment.ID, cls.Message, time.Since(start))
				}
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: deployment.ID,
				Message: cls.CapabilityLabel() + ": " + cls.Message, ErrorType: policy.ErrorType, LatencyMS: time.Since(start).Milliseconds(), StatusCode: lastStatus})
			if (policy.Failover || cls.CapabilityFailure) && attempts < max && i+1 < len(candidates) {
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attempts-1, max)
				continue
			}
			if isNativeFamily(deployment.ProviderType, "openai_responses") {
				writeRawUpstreamError(w, lastStatus, lastCT, b)
				return
			}
			canonicalErrorJSON(w, "openai_responses", cls.HTTPStatus(), policy.ErrorType, cls.Message)
			return
		}
		if req.Streaming {
			resp.Body = observeFirstByte(resp.Body, start, func(d time.Duration) { s.hm.RecordTTFT(deployment.ID, d) })
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
			lastErr = usageErr.Error()
			if clientRequestGone(r.Context()) {
				return
			}
			cls := compat.ClassifyMalformedResponse(lastErr)
			if !responseCommitted(w) {
				if router.IsReadyStrategy(cfg.Routing.Strategy) {
					s.hm.Quarantine(deploy.ID, lastErr, total)
					s.probe.Recover(deploy.ID)
				} else {
					s.hm.RecordFailure(deploy.ID, lastErr, total)
				}
				if attempts < max && i+1 < len(candidates) {
					s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attempts-1, max)
					continue
				}
				canonicalErrorJSON(w, "openai_responses", http.StatusBadGateway, "api_error", "upstream returned an invalid response")
				return
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "stream_fail", Deployment: deploy.ID,
				Message: lastErr, ErrorType: string(cls.Class), LatencyMS: total.Milliseconds()})
			return
		}
		s.recordRouteSuccess(req, deploy.ID, deploy.ProviderID, time.Since(start))
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
	if lastErr == "" {
		lastErr = "no usable deployment"
	}
	canonicalErrorJSON(w, "openai_responses", http.StatusBadGateway, "api_error", "all candidate deployments failed: "+lastErr)
}

// isNativeFamily reports whether the provider type natively speaks the
// protocol family, in which case upstream errors are passed through raw.
func isNativeFamily(providerType, family string) bool {
	return upstreamKindFor(providerType) == family
}
