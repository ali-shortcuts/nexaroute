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

// Gemini GenerateContent ingress (Gemini CLI via the @google/genai SDK).
//
// Routes:
//
//	POST /v1beta/models/{model}:generateContent
//	POST /v1beta/models/{model}:streamGenerateContent (alt=sse)
//	GET  /v1beta/models
//
// The model id lives in the URL path, as in the Gemini API; the client
// key arrives as Authorization Bearer, x-api-key, x-goog-api-key, or the
// ?key= query parameter (the forms the SDK sends). The key is never
// logged, and the ?key= form is stripped from the request URL before
// any downstream handling so it cannot leak into request logs.
//
// Execution reuses the canonical path (the same pipeline as
// /v1/responses): the Gemini request is decoded into the canonical IR,
// routed, encoded per upstream dialect, and the canonical response is
// re-encoded into GenerateContent shape. Opencode upstreams fail closed
// for this ingress (finishCanonicalAttempt has no Gemini path mapping
// for them), exactly as for unknown ingress kinds. After the first
// client byte is committed, an error is a stream truncation, never a
// silent restart or failover to another deployment.

// geminiStatusName maps an HTTP status onto the Gemini error status string.
func geminiStatusName(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	case http.StatusGatewayTimeout:
		return "DEADLINE_EXCEEDED"
	default:
		return "INTERNAL"
	}
}

// geminiErrorJSON writes the Gemini error envelope.
func geminiErrorJSON(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": status, "message": message, "status": geminiStatusName(status)},
	})
}

// geminiModels serves GET /v1beta/models: the same client-facing model
// identities as /v1/models, rendered in Gemini shape. A virtual
// endpoint's public model is advertised as "models/<public-model>".
func (s *Server) geminiModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		geminiErrorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r = geminiAuthRequest(r)
	if !s.clientAuthAllowedGemini(w, r, "", false) {
		return
	}
	models := make([]map[string]any, 0)
	for _, e := range s.clientModelEntries() {
		id, _ := e["id"].(string)
		if id == "" {
			continue
		}
		models = append(models, map[string]any{
			"name":                       "models/" + id,
			"displayName":                id,
			"supportedGenerationMethods": []string{"generateContent"},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// geminiAuthRequest returns a request copy whose client key is normalized
// into the x-api-key header from the Gemini forms (x-goog-api-key header
// or ?key= query). The ?key= form is ALWAYS stripped from the copied URL,
// even when a header key is present and wins authentication: a query key
// that stays in r.URL could otherwise leak into any log that records the
// request URL. The key value itself is never logged here.
func geminiAuthRequest(r *http.Request) *http.Request {
	hasHeaderAuth := r.Header.Get("x-api-key") != "" || r.Header.Get("Authorization") != ""
	query := r.URL.Query()
	_, hasQueryKey := query["key"]
	if hasHeaderAuth && !hasQueryKey {
		return r
	}
	key := strings.TrimSpace(r.Header.Get("x-goog-api-key"))
	if key == "" {
		key = strings.TrimSpace(query.Get("key"))
	}
	r2 := r.Clone(r.Context())
	if !hasHeaderAuth && key != "" {
		r2.Header.Set("x-api-key", key)
	}
	if hasQueryKey {
		u2 := *r.URL
		q := u2.Query()
		q.Del("key")
		u2.RawQuery = q.Encode()
		r2.URL = &u2
	}
	return r2
}

// parseGeminiModelPath splits a /v1beta/models/ remainder into its model
// and streaming flag. It returns ok=false for anything that is not a
// generateContent / streamGenerateContent action.
func parseGeminiModelPath(rest string) (model string, stream bool, ok bool) {
	if strings.HasSuffix(rest, ":streamGenerateContent") {
		model = strings.TrimSuffix(rest, ":streamGenerateContent")
		return model, true, model != ""
	}
	if strings.HasSuffix(rest, ":generateContent") {
		model = strings.TrimSuffix(rest, ":generateContent")
		return model, false, model != ""
	}
	return "", false, false
}

// geminiGenerateContent serves POST /v1beta/models/{model}:generateContent
// and :streamGenerateContent. It follows the /v1/responses handler's
// canonical-path structure with Gemini envelopes.
func (s *Server) geminiGenerateContent(w http.ResponseWriter, r *http.Request) {
	defer s.clearAttemptSeq(r.Header.Get("x-request-id"))
	if r.Method != http.MethodPost {
		geminiErrorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
	model, pathStream, ok := parseGeminiModelPath(rest)
	if !ok {
		geminiErrorJSON(w, http.StatusNotFound, "unknown Gemini action; expected {model}:generateContent or {model}:streamGenerateContent")
		return
	}
	// The SDK model id is bare ("gemini-2.5-flash"); tolerate a
	// "models/"-prefixed id, but reject deeper paths (tuned-model
	// paths are not route identities).
	model = strings.TrimPrefix(model, "models/")
	if strings.Contains(model, "/") || strings.TrimSpace(model) == "" {
		geminiErrorJSON(w, http.StatusBadRequest, "model path is not supported")
		return
	}
	r = geminiAuthRequest(r)
	if !s.clientAuthAllowedGemini(w, r, model, true) {
		return
	}
	var in canonical.GeminiRequest
	raw, err := readJSON(r, &in)
	if err != nil {
		if _, ok := err.(*requestTooLargeError); ok {
			geminiErrorJSON(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		geminiErrorJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(in.Contents) == 0 {
		geminiErrorJSON(w, http.StatusBadRequest, "model and contents are required")
		return
	}
	canReq, err := canonical.DecodeGeminiIngressRequest(in, model)
	if err != nil {
		geminiErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	streaming := pathStream || strings.EqualFold(r.URL.Query().Get("alt"), "sse")
	canReq.Stream = streaming
	reqReqs := canReq.DetectRequirements()
	hasSystem := in.SystemInstruction != nil && len(in.SystemInstruction.Parts) > 0
	var toolChoiceRequired *bool
	if canReq.ToolChoice != nil {
		req := canReq.ToolChoice.Mode == "required" || canReq.ToolChoice.Mode == "tool"
		toolChoiceRequired = &req
	}
	ti := extractFeaturesAndClassify(raw, feature.ExtractOptions{
		Protocol:               feature.ProtocolGemini,
		Model:                  model,
		Streaming:              streaming,
		VisionType:             "image",
		ContentFields:          []string{"contents"},
		MaxOutputTokens:        canReq.MaxOutputTokens,
		UserAgent:              r.Header.Get("User-Agent"),
		ToolCountHint:          len(canReq.Tools),
		ToolChoiceRequiredHint: toolChoiceRequired,
		HasSystemPromptHint:    &hasSystem,
	})
	if ti.Features.TooComplex {
		geminiErrorJSON(w, http.StatusBadRequest, "request JSON structure is too complex")
		return
	}
	s.emitRequestAccepted(r, "gemini", model, streaming)
	hasVision := false
	for _, m := range canReq.Messages {
		for _, p := range m.Parts {
			if p.Type == canonical.PartImage {
				hasVision = true
			}
		}
	}
	req := router.Requirement{
		Model: model, Tools: ti.Features.HasTools || reqReqs.Tools, Vision: hasVision || reqReqs.Vision,
		Streaming: streaming, Reasoning: ti.Features.HasReasoning || reqReqs.Reasoning,
		StructuredOutput: reqReqs.StructuredOutput,
	}
	req.EstimatedInputTokens = ti.Features.EstimatedPromptTokens
	req.MaxOutputTokens = canReq.MaxOutputTokens
	req.MinContextWindow = req.EstimatedInputTokens + req.MaxOutputTokens
	req = s.prepareRequirement(req, r, ti.Features.BodySessionKey)
	req = s.applyPrivacyRequirement(req, r)
	cfg, candidates, resolvedRoute, resolveErr := s.candidatesForRequirement(req, "gemini")
	if resolveErr != nil {
		s.budgetRelease(r)
		if strings.Contains(resolveErr.Error(), "disabled") {
			geminiErrorJSON(w, http.StatusNotFound, resolveErr.Error())
		} else {
			geminiErrorJSON(w, http.StatusBadRequest, resolveErr.Error())
		}
		return
	}
	taskRes, candidates, resolvedRoute := s.applyTaskIntel(r, req, cfg, candidates, resolvedRoute, ti)
	s.emitTaskClassified(r.Header.Get("x-request-id"), ti, resolvedRoute, taskRes)
	s.emitRouteResolved(r, "gemini", resolvedRoute, candidates)
	if len(candidates) == 0 {
		if s.isPrivacyUnavailable(req, "gemini", resolvedRoute, r.Header.Get("x-request-id")) {
			s.budgetRelease(r)
			geminiErrorJSON(w, http.StatusServiceUnavailable, privacyUnavailableMessage)
			return
		}
		s.budgetRelease(r)
		s.autopilotObserveNoEligible(cfg, resolvedRoute, autopilotRequestKeyFromContext(r.Context(), r.Header.Get("x-request-id")), autopilotCallerStratum(clientIdentityFromRequest(r)))
		geminiErrorJSON(w, http.StatusServiceUnavailable, "no compatible healthy deployment")
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
	routeCtx, routeCancel := routeContext(r.Context(), streaming, cfg.RequestTimeout())
	routeCtx = providers.WithQuotaEstimate(routeCtx, req.EstimatedInputTokens, req.MaxOutputTokens)
	routeCtx = providers.WithSessionKey(routeCtx, req.SessionKey)
	routeCtx = providers.WithClientScope(routeCtx, clientScopeForSession(r))
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
		bundle, ok := s.buildCanonicalAttempt(c, reqEligible, canReq, model, profile, "gemini")
		if !ok {
			lastErr = "candidate could not serve this request"
			continue
		}
		start := time.Now()
		attemptKind := attemptKindPrimary
		if attempts > 1 {
			attemptKind = attemptKindRetry
		}
		s.autopilotNoteDispatched(autopilotRequestKeyFromContext(r.Context(), r.Header.Get("x-request-id")))
		s.emitRouteAttempt(r.Header.Get("x-request-id"), "gemini", model, attempts, c, attemptKind)
		resp, sentPayload, repair, derr := s.doUpstreamWithRepair(
			routeCtx, r.Header.Get("x-request-id"), bundle.a, deployment,
			bundle.payload, bundle.path, streaming, forward, dialect, profile,
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
			s.observeTaskFeedback(string(ti.Profile.Type), deployment.ID, false, time.Since(start))
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
			s.emitAmbiguousOutcome(r.Header.Get("x-request-id"), deployment.ID, derr, time.Since(start))
			if gatewayDeadlineExceeded(routeCtx, r.Context()) {
				geminiErrorJSON(w, http.StatusGatewayTimeout, "gateway request timeout")
				return
			}
			if attempts < max && i+1 < len(candidates) {
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: deployment.ID, Message: "transport failure; trying next eligible candidate", FailoverClass: "transient_transport"})
				s.emitProductionEvent(r.Header.Get("x-request-id"), ProductionEventRequestFailover, deployment.ID, "transport failure; trying next eligible candidate", events.Event{})
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attempts-1, max)
				continue
			}
			geminiErrorJSON(w, http.StatusBadGateway, "all candidate deployments failed: "+lastErr)
			return
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			_ = resp.Body.Close()
			b = bundle.a.RedactBody(b)
			lastStatus := resp.StatusCode
			cls, policy := classifyFailure(resp.StatusCode, b)
			safeMessage := cls.Message
			if applyOpencodeEntitlementOverride(deployment.ProviderType, resp.StatusCode, &cls, &policy) {
				safeMessage = opencodeEntitlementDiagnostic
			}
			switch cls.Class {
			case compat.ClassModelRetired:
				safeMessage = string(cls.Class)
				policy.Failover = true
				policy.QuarantineDeployment = false
				policy.HardCooldown = false
				policy.SignalProvider = false
				s.hm.Retire(deployment.ID, "upstream model retired", string(cls.Class))
				s.bus.Add(events.Event{Kind: "model_retired", Deployment: deployment.ID, Message: "deployment retired by upstream model lifecycle", ErrorType: string(cls.Class), StatusCode: resp.StatusCode})
			case compat.ClassModelTemporarilyUnavailable:
				safeMessage = string(cls.Class)
				policy.Failover = true
				policy.SignalProvider = false
				s.bus.Add(events.Event{Kind: "model_unavailable", Deployment: deployment.ID, Message: "upstream model temporarily unavailable", ErrorType: string(cls.Class), StatusCode: resp.StatusCode})
			}
			s.recordProviderFailure(deployment.ProviderID, deployment.ID, safeMessage, policy, string(ti.Profile.Type))
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
				s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "failover", Deployment: deployment.ID, Message: fmt.Sprintf("HTTP %d; trying next eligible candidate", lastStatus), StatusCode: lastStatus, FailoverClass: policy.ErrorType})
				s.emitProductionEvent(r.Header.Get("x-request-id"), ProductionEventRequestFailover, deployment.ID, fmt.Sprintf("HTTP %d; trying next eligible candidate", lastStatus), events.Event{StatusCode: lastStatus})
				s.retryPause(routeCtx, r.Header.Get("x-request-id"), cfg, attempts-1, max)
				continue
			}
			if cls.Class == compat.ClassModelRetired || cls.Class == compat.ClassModelTemporarilyUnavailable {
				geminiErrorJSON(w, http.StatusServiceUnavailable, "all eligible upstream deployments failed")
				return
			}
			if isNativeFamily(deployment.ProviderType, "gemini") {
				writeRawUpstreamError(w, lastStatus, resp.Header.Get("Content-Type"), b)
				return
			}
			geminiErrorJSON(w, cls.HTTPStatus(), cls.Message)
			return
		}
		if streaming {
			resp.Body = observeFirstByte(resp.Body, start, func(d time.Duration) {
				s.hm.RecordTTFT(deployment.ID, d)
				s.observeTelemetryTTFT(deployment.ID, d)
				noteSpanTTFT(r.Context(), d)
				s.emitStreamCommitted(r.Header.Get("x-request-id"), deployment.ID, deployment.ProviderID, "gemini", d)
			})
		}
		if v := s.attemptsHeaderValue(r.Header.Get("x-request-id")); v != "" {
			w.Header().Set("X-NexaRoute-Attempts", v)
		}
		deploy := deployment
		sent := sentPayload
		var inputTokens, outputTokens int
		var usageErr error
		if canReq.Stream {
			usageErr = s.canonicalStreamPump(w, resp, bundle.canonicalKind, "gemini", model, r.Header.Get("x-request-id"), bundle.toolDefs,
				func(u canonical.Usage) {
					inputTokens, outputTokens = u.InputTokens, u.OutputTokens
					s.recordUsageCanonical(r, deploy.ID, bundle.canonicalKind, u)
					s.rt.ObserveSessionCache(req, deploy.ID, u.InputTokens, u.CacheReadTokens)
				})
		} else {
			usageErr = s.handleCanonicalResponse(w, resp, bundle.canonicalKind, "gemini", model, r.Header.Get("x-request-id"), false, bundle.toolDefs,
				func(u canonical.Usage) {
					inputTokens, outputTokens = u.InputTokens, u.OutputTokens
					s.recordUsageCanonical(r, deploy.ID, bundle.canonicalKind, u)
					s.rt.ObserveSessionCache(req, deploy.ID, u.InputTokens, u.CacheReadTokens)
				})
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
				geminiErrorJSON(w, http.StatusBadGateway, "upstream returned an invalid response")
				return
			}
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "stream_fail", Deployment: deploy.ID,
				Message: lastErr, ErrorType: string(cls.Class), LatencyMS: total.Milliseconds()})
			return
		}
		prevStatus := s.hm.Get(deploy.ID).Status
		s.recordRouteSuccess(req, deploy.ID, deploy.ProviderID, time.Since(start), string(ti.Profile.Type))
		s.learnFromSuccess(deploy.ID, deploy.ProviderID, deploy, sent, &canReq)
		ev := events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "route_ok", Deployment: deploy.ID,
			Message: "request completed", LatencyMS: total.Milliseconds()}
		if resolvedRoute != nil {
			ev.VirtualEndpoint = resolvedRoute.VirtualEndpointID
			ev.PublicModel = resolvedRoute.PublicModel
			ev.RouteProfile = resolvedRoute.RouteProfileID
			ev.Pool = resolvedRoute.PrimaryPoolID
		}
		if inputTokens > 0 || outputTokens > 0 {
			ev.InputTokens = inputTokens
			ev.OutputTokens = outputTokens
			ev.CostBasis = "actual"
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
		geminiErrorJSON(w, http.StatusGatewayTimeout, "gateway request timeout")
		return
	}
	if lastErr == "" {
		lastErr = "no usable deployment"
	}
	geminiErrorJSON(w, http.StatusBadGateway, "all candidate deployments failed: "+lastErr)
}
