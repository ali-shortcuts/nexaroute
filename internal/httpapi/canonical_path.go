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

// streamCommitWriter observes client-visible canonical emitter writes. It is
// deliberately independent of the outer middleware so direct handler tests and
// production statusWriter wrappers share the same pre-commit boundary.
type streamCommitWriter struct {
	http.ResponseWriter
	committed bool
}

func (w *streamCommitWriter) WriteHeader(status int) {
	w.committed = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *streamCommitWriter) Write(p []byte) (int, error) {
	w.committed = true
	return w.ResponseWriter.Write(p)
}

func (w *streamCommitWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// canonicalStreamPump decodes an upstream SSE stream into canonical events and
// re-encodes them into the client's dialect. It returns the transport error,
// if any, after emitting a terminal client-side error frame only when client
// bytes were already committed; otherwise the handler may fail over safely.
//
// Defense-in-depth: it assembles tool args per index and validates them on
// tool_end to catch string->unknown coercion, double-encoding, and malformed
// payloads before they reach the client. Validation failures are emitted as
// StreamError with structured diagnostics (tool= field= expected= actual= ...).
func (s *Server) canonicalStreamPump(
	w http.ResponseWriter,
	resp *http.Response,
	kind, clientProtocol, requestedModel, requestID string,
	toolDefs []canonical.ToolDef,
	usageHook func(input, output int),
) error {
	defer resp.Body.Close()
	streamWriter := &streamCommitWriter{ResponseWriter: w}
	var emitter canonical.StreamEmitter
	ensureEmitter := func() canonical.StreamEmitter {
		if emitter == nil {
			emitter = canonical.NewStreamEmitter(clientProtocol, streamWriter, requestedModel, requestID)
		}
		return emitter
	}
	committed := func() bool { return streamWriter.committed || responseCommitted(w) }
	_, precommitFailoverAware := w.(interface{ Committed() bool })
	emitTerminalError := func(ev canonical.StreamEvent) {
		if emitter != nil && committed() {
			_ = emitter.Emit(ev)
			return
		}
		// Direct unit calls have no surrounding candidate loop. Retain their
		// terminal diagnostic while production handlers preserve failover before
		// their first client-visible write.
		if !precommitFailoverAware {
			_ = ensureEmitter().Emit(ev)
		}
	}
	reader := canonical.NewSSEReader(resp.Body)
	terminal := false
	var streamErr error
	inputTokens, outputTokens := 0, 0
	usageSeen := false
	anthropicToolBlocks := map[int]bool{}
	assembledArgs := map[int]*strings.Builder{}
	toolNames := map[int]string{}
	pendingToolStarts := map[int]canonical.StreamEvent{}
	pendingToolOrder := make([]int, 0, 4)
	validateTool := func(toolIndex int) error {
		args := ""
		if b, ok := assembledArgs[toolIndex]; ok {
			args = b.String()
		}
		meta := canonical.ValidationMeta{
			Stage:     kind + "_to_canonical",
			Protocol:  kind,
			Streaming: true,
			Provider:  kind,
		}
		block := canonical.Block{Type: canonical.PartToolCall, ToolCall: &canonical.ToolCall{
			Name: toolNames[toolIndex], Arguments: args,
		}}
		if err := canonical.ValidateResponseBlocks([]canonical.Block{block}, toolDefs, meta); err != nil {
			return err
		}
		return nil
	}
	ensurePendingTool := func(ev canonical.StreamEvent) {
		if _, ok := pendingToolStarts[ev.ToolIndex]; !ok {
			pendingToolStarts[ev.ToolIndex] = canonical.StreamEvent{Type: canonical.StreamToolStart, ToolIndex: ev.ToolIndex, ToolID: ev.ToolID, ToolName: ev.ToolName}
			pendingToolOrder = append(pendingToolOrder, ev.ToolIndex)
		}
		if _, ok := assembledArgs[ev.ToolIndex]; !ok {
			assembledArgs[ev.ToolIndex] = &strings.Builder{}
		}
		if ev.ToolName != "" {
			toolNames[ev.ToolIndex] = ev.ToolName
		}
	}
	emitValidatedTool := func(toolIndex int) error {
		start, ok := pendingToolStarts[toolIndex]
		if !ok {
			return nil
		}
		start.ToolName = toolNames[toolIndex]
		active := ensureEmitter()
		if err := active.Emit(start); err != nil {
			return err
		}
		args := assembledArgs[toolIndex].String()
		if args != "" {
			if err := active.Emit(canonical.StreamEvent{Type: canonical.StreamToolDelta, ToolIndex: toolIndex, ToolName: start.ToolName, ArgsDelta: args}); err != nil {
				return err
			}
		}
		if err := active.Emit(canonical.StreamEvent{Type: canonical.StreamToolEnd, ToolIndex: toolIndex, ToolName: start.ToolName}); err != nil {
			return err
		}
		delete(pendingToolStarts, toolIndex)
		delete(assembledArgs, toolIndex)
		delete(toolNames, toolIndex)
		return nil
	}
	for {
		name, data, done, err := reader.Next()
		if err != nil {
			if !terminal {
				emitTerminalError(canonical.StreamEvent{Type: canonical.StreamError, ErrorMsg: "upstream stream read failed: " + err.Error()})
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
			// No client-visible frame has been emitted yet, so preserve the
			// pre-commit failover opportunity rather than committing a terminal
			// error from this candidate.
			emitTerminalError(canonical.StreamEvent{Type: canonical.StreamError, ErrorMsg: "upstream stream protocol violation: " + err.Error()})
			streamErr = err
			break
		}
		for _, ev := range evs {
			if ev.Type == canonical.StreamError {
				streamErr = fmt.Errorf("upstream stream error: %s", ev.ErrorMsg)
				emitTerminalError(ev)
				break
			}
			deferClientEvent := false
			if kind == "anthropic" && ev.Type == canonical.StreamEnd && terminal {
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
			switch ev.Type {
			case canonical.StreamToolStart:
				ensurePendingTool(ev)
				deferClientEvent = true
			case canonical.StreamToolDelta:
				ensurePendingTool(ev)
				if assembledArgs[ev.ToolIndex].Len()+len(ev.ArgsDelta) > 1<<20 {
					msg := fmt.Sprintf("tool=%s field=arguments expected=object actual=too_large stage=%s protocol=%s streaming=true provider=%s: args exceed 1MB limit", toolNames[ev.ToolIndex], kind+"_to_canonical", kind, kind)
					emitTerminalError(canonical.StreamEvent{Type: canonical.StreamError, ErrorCode: "invalid_request_error", ErrorMsg: msg})
					streamErr = fmt.Errorf("%s", msg)
					break
				}
				assembledArgs[ev.ToolIndex].WriteString(ev.ArgsDelta)
				deferClientEvent = true
			case canonical.StreamToolEnd:
				if err := validateTool(ev.ToolIndex); err != nil {
					emitTerminalError(canonical.StreamEvent{Type: canonical.StreamError, ErrorCode: "invalid_request_error", ErrorMsg: err.Error()})
					streamErr = err
					break
				}
				if err := emitValidatedTool(ev.ToolIndex); err != nil {
					return err
				}
				deferClientEvent = true
			case canonical.StreamEnd:
				// OpenAI-compatible streams signal only a finish reason and do
				// not emit a distinct per-tool completion frame. Validate every
				// still-open tool before allowing a successful terminal frame.
				for _, toolIndex := range pendingToolOrder {
					if _, pending := pendingToolStarts[toolIndex]; !pending {
						continue
					}
					if err := validateTool(toolIndex); err != nil {
						emitTerminalError(canonical.StreamEvent{Type: canonical.StreamError, ErrorCode: "invalid_request_error", ErrorMsg: err.Error()})
						streamErr = err
						break
					}
					if err := emitValidatedTool(toolIndex); err != nil {
						return err
					}
				}
			}
			if streamErr != nil {
				break
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
			if ev.Type == canonical.StreamEnd {
				terminal = true
			}
			if !deferClientEvent {
				if emitErr := ensureEmitter().Emit(ev); emitErr != nil {
					return emitErr
				}
			}
		}
		if streamErr != nil {
			break
		}
	}
	if !terminal && streamErr == nil {
		streamErr = io.ErrUnexpectedEOF
		emitTerminalError(canonical.StreamEvent{Type: canonical.StreamError, ErrorMsg: "upstream stream ended before completion"})
	}
	if streamErr == nil && terminal {
		if finErr := ensureEmitter().Finish(); finErr != nil {
			streamErr = finErr
		}
	}
	if streamErr == nil && terminal && usageHook != nil && usageSeen {
		usageHook(inputTokens, outputTokens)
	}
	return streamErr
}

// handleCanonicalResponse processes a successful upstream response received
// through the canonical path: stream pumping or non-stream decode/encode.
func (s *Server) handleCanonicalResponse(
	w http.ResponseWriter,
	resp *http.Response,
	kind, clientProtocol, requestedModel, requestID string,
	stream bool,
	toolDefs []canonical.ToolDef,
	usageHook func(input, output int),
) error {
	if stream {
		return s.canonicalStreamPump(w, resp, kind, clientProtocol, requestedModel, requestID, toolDefs, usageHook)
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
	if len(canResp.Blocks) > 0 {
		meta := canonical.ValidationMeta{
			Stage:     kind + "_to_" + clientProtocol,
			Protocol:  kind,
			Streaming: false,
			Provider:  kind,
		}
		if err := canonical.ValidateResponseBlocks(canResp.Blocks, toolDefs, meta); err != nil {
			return fmt.Errorf("tool call validation failed: %w", err)
		}
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
	hasSystem := len(in.Instructions) > 0
	var toolChoiceRequired *bool
	if canReq.ToolChoice != nil {
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
	s.emitTaskClassified(r.Header.Get("x-request-id"), ti, resolvedRoute)
	if len(candidates) == 0 {
		canonicalErrorJSON(w, "openai_responses", http.StatusServiceUnavailable, "server_error", "no compatible healthy deployment")
		return
	}
	candidates = s.applyDecisionPlane(r.Context(), candidates, ti, resolvedRoute, r.Header.Get("x-request-id"), req)
	if resolvedRoute != nil {
		w.Header().Set("X-Gateway-Virtual-Endpoint", resolvedRoute.VirtualEndpointID)
		w.Header().Set("X-Gateway-Public-Model", resolvedRoute.PublicModel)
		w.Header().Set("X-Gateway-Route-Profile", resolvedRoute.RouteProfileID)
	}
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
		// Noisy per-request route_attempt log removed; production observability
		// uses request_failover/route_changed/model_* lifecycle events only.
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
			} else {
				s.hm.RecordFailure(deployment.ID, lastErr, time.Since(start))
				if policy.HardCooldown {
					s.hm.ForceCooldown(deployment.ID, lastErr, cfg.Cooldown())
					s.emitProductionEvent(r.Header.Get("x-request-id"), ProductionEventModelCooldown, deployment.ID, lastErr, events.Event{ErrorType: policy.ErrorType})
				}
			}
			s.emitProductionEvent(r.Header.Get("x-request-id"), ProductionEventModelFailed, deployment.ID, lastErr, events.Event{ErrorType: policy.ErrorType, LatencyMS: time.Since(start).Milliseconds()})
			if gatewayDeadlineExceeded(routeCtx, r.Context()) {
				canonicalErrorJSON(w, "openai_responses", http.StatusGatewayTimeout, "timeout", "gateway request timeout")
				return
			}
			if attempts < max && i+1 < len(candidates) {
				s.emitProductionEvent(r.Header.Get("x-request-id"), ProductionEventRequestFailover, deployment.ID, "transport failure; trying next eligible candidate", events.Event{})
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
			safeMessage := cls.Message
			if cls.Class == compat.ClassModelRetired {
				safeMessage = string(cls.Class)
				policy.Failover = true
				policy.QuarantineDeployment = false
				policy.HardCooldown = false
				policy.SignalProvider = false
				s.hm.Retire(deployment.ID, "upstream model retired", string(cls.Class))
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "model_retired", Deployment: deployment.ID, Message: "deployment retired by upstream model lifecycle", ErrorType: string(cls.Class), StatusCode: resp.StatusCode})
			} else if cls.Class == compat.ClassModelTemporarilyUnavailable {
				safeMessage = string(cls.Class)
				policy.Failover = true
				policy.SignalProvider = false
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "model_unavailable", Deployment: deployment.ID, Message: "upstream model temporarily unavailable", ErrorType: string(cls.Class), StatusCode: resp.StatusCode})
			}
			s.recordProviderFailure(deployment.ProviderID, deployment.ID, safeMessage, policy)
			cooldownEntered := false
			if !cls.CapabilityFailure && cls.Class != compat.ClassModelRetired {
				if router.IsReadyStrategy(cfg.Routing.Strategy) {
					if policy.QuarantineDeployment {
						s.hm.Quarantine(deployment.ID, safeMessage, time.Since(start))
						s.probe.Recover(deployment.ID)
					}
				} else if policy.HardCooldown {
					d := cfg.Cooldown()
					if resp.StatusCode == http.StatusTooManyRequests {
						d = retryAfterDuration(resp.Header, time.Duration(cfg.Routing.MaxRetryAfterSeconds)*time.Second)
					}
					s.hm.ForceCooldown(deployment.ID, safeMessage, d)
					cooldownEntered = true
				} else if policy.QuarantineDeployment {
					s.hm.RecordFailure(deployment.ID, safeMessage, time.Since(start))
				}
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_fail", Deployment: deployment.ID,
				Message: cls.CapabilityLabel() + ": " + safeMessage, ErrorType: policy.ErrorType, LatencyMS: time.Since(start).Milliseconds(), StatusCode: lastStatus})
			s.emitProductionEvent(r.Header.Get("x-request-id"), ProductionEventModelFailed, deployment.ID, safeMessage, events.Event{ErrorType: policy.ErrorType, LatencyMS: time.Since(start).Milliseconds(), StatusCode: lastStatus})
			if lastStatus == http.StatusTooManyRequests {
				s.emitProductionEvent(r.Header.Get("x-request-id"), ProductionEventProviderRateLimited, deployment.ID, safeMessage, events.Event{ErrorType: policy.ErrorType, StatusCode: lastStatus})
			}
			if cooldownEntered {
				s.emitProductionEvent(r.Header.Get("x-request-id"), ProductionEventModelCooldown, deployment.ID, safeMessage, events.Event{ErrorType: policy.ErrorType, StatusCode: lastStatus})
			}
			if (policy.Failover || cls.CapabilityFailure) && attempts < max && i+1 < len(candidates) {
				s.emitProductionEvent(r.Header.Get("x-request-id"), ProductionEventRequestFailover, deployment.ID, fmt.Sprintf("HTTP %d; trying next eligible candidate", lastStatus), events.Event{StatusCode: lastStatus})
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attempts-1, max)
				continue
			}
			if cls.Class == compat.ClassModelRetired || cls.Class == compat.ClassModelTemporarilyUnavailable {
				canonicalErrorJSON(w, "openai_responses", http.StatusServiceUnavailable, "candidate_exhausted", "all eligible upstream deployments failed")
				return
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
			usageErr = s.canonicalStreamPump(w, resp, bundle.canonicalKind, "openai_responses", in.Model, r.Header.Get("x-request-id"), bundle.toolDefs,
				func(input, output int) { s.usage.Record(deploy.ID, int64(input), int64(output)) })
		} else {
			usageErr = s.handleCanonicalResponse(w, resp, bundle.canonicalKind, "openai_responses", in.Model, r.Header.Get("x-request-id"), false, bundle.toolDefs,
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
		prevStatus := s.hm.Get(deploy.ID).Status
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
		if prevStatus != "healthy" {
			s.emitProductionEvent(r.Header.Get("x-request-id"), ProductionEventModelHealthy, deploy.ID, "deployment healthy", events.Event{LatencyMS: total.Milliseconds()})
		}
		if attempts > 1 {
			firstID := ""
			if len(candidates) > 0 {
				firstID = candidates[0].Deployment.ID
			}
			if firstID != "" && firstID != deploy.ID {
				s.emitProductionEvent(r.Header.Get("x-request-id"), ProductionEventRouteChanged, deploy.ID, "route changed to "+deploy.ID, events.Event{LatencyMS: total.Milliseconds()})
			}
		}
		return
	}
	if gatewayDeadlineExceeded(routeCtx, r.Context()) {
		canonicalErrorJSON(w, "openai_responses", http.StatusGatewayTimeout, "timeout", "gateway request timeout")
		return
	}
	if lastErr == "" {
		lastErr = "no usable deployment"
	}
	canonicalErrorJSON(w, "openai_responses", http.StatusBadGateway, "api_error", "all candidate deployments failed: "+lastErr)
}

func isNativeFamily(providerType, family string) bool {
	return upstreamKindFor(providerType) == family
}
