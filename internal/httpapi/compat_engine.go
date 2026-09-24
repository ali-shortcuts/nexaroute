package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/translate"
)

// This file wires the Universal Compatibility Engine into the data plane:
//
//   - per-deployment capability contracts (seeded from dialect + config,
//     refined by probes and real traffic, invalidated on identity changes)
//   - pre-attempt eligibility from verified capabilities
//   - the structured error classifier driving health policy
//   - the bounded deterministic repair engine (a 400 that says
//     "temperature is not supported" is a capability fact, not a dead model)
//
// Health and compatibility stay separate state machines end to end.

// SyncCapabilityContracts re-seeds/invalidates the capability store for the
// current topology. Called at startup and after every config swap.
func (s *Server) SyncCapabilityContracts() { s.syncCapabilityContracts(s.currentConfig()) }

// CapabilityStore exposes the shared capability contract store (wired into
// the probe engine and admin endpoints).
func (s *Server) CapabilityStore() *compat.Store { return s.capStore }

// dialectFor resolves the dialect profile for a deployment's provider.
func (s *Server) dialectFor(providerID, providerType, baseURL string) compat.DialectProfile {
	cfg := s.currentConfig()
	for _, p := range cfg.Providers {
		if p.ID == providerID {
			return compat.DetectDialect(p.ID, p.Type, p.BaseURL, p.Dialect)
		}
	}
	return compat.DetectDialect(providerID, providerType, baseURL, "")
}

// providerConfigFor returns the current provider config for an adapter.
func (s *Server) providerConfigFor(providerID string) (config.ProviderConfig, bool) {
	cfg := s.currentConfig()
	for _, p := range cfg.Providers {
		if p.ID == providerID {
			return p, true
		}
	}
	return config.ProviderConfig{}, false
}

// credentialScope fingerprints the provider's credential identity so a
// materially different key/scope invalidates stale contracts.
func credentialScope(p config.ProviderConfig) string {
	keys := p.ResolvedCredentials()
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

// staticCapsFromConfig converts the operator's 4-bool capability block into
// a static capability seed. Configured `false` values are respected as
// "operator says unsupported" (matching legacy routing semantics).
func staticCapsFromConfig(m config.ModelConfig) compat.ModelCapabilities {
	var out compat.ModelCapabilities
	if m.Capabilities.Streaming {
		out.Streaming = compat.Supported
	} else {
		out.Streaming = compat.Unsupported
	}
	if m.Capabilities.Tools {
		out.Tools = compat.Supported
	} else {
		out.Tools = compat.Unsupported
	}
	if m.Capabilities.Vision {
		out.Vision = compat.Supported
	} else {
		out.Vision = compat.Unsupported
	}
	if m.Capabilities.Reasoning {
		out.Reasoning = compat.Supported
	} else {
		out.Reasoning = compat.Unsupported
	}
	return out
}

// syncCapabilityContracts re-seeds the capability store after a config swap:
// contracts whose identity (base URL / dialect / model / credentials) changed
// are dropped, unchanged deployments keep their learned evidence, and fresh
// deployments get dialect+config seeds. It takes cfg explicitly because it
// runs while the runtime write lock is held.
func (s *Server) syncCapabilityContracts(cfg config.Config) {
	byProvider := map[string]config.ProviderConfig{}
	byDeployment := map[string]config.ModelConfig{}
	for _, p := range cfg.Providers {
		byProvider[p.ID] = p
		for _, m := range p.Models {
			byDeployment[p.ID+"/"+m.ID] = m
		}
	}
	valid := map[string]struct{}{}
	for _, d := range s.rt.All() {
		valid[d.ID] = struct{}{}
		p, ok := byProvider[d.ProviderID]
		if !ok {
			continue
		}
		// Deployment identity, not upstream model name, selects static caps:
		// multiple aliases may intentionally point to the same model with
		// different operator-declared capabilities/context windows.
		modelCfg, ok := byDeployment[d.ID]
		if !ok {
			continue
		}
		dialect := compat.DetectDialect(p.ID, p.Type, p.BaseURL, p.Dialect)
		key := compat.InvalidationKey(p.BaseURL, dialect.Name, d.Model, credentialScope(p))
		if !s.capStore.InvalidateIf(d.ID, key) {
			seed := compat.SeedFromDialect(dialect, staticCapsFromConfig(modelCfg))
			s.capStore.Seed(d.ID, seed, compat.SourceStatic, "dialect prior + config", key)
		}
		if modelCfg.ContextWindow > 0 {
			s.capStore.LearnNumeric(d.ID, modelCfg.ContextWindow, 0)
		}
	}
	// Drop contracts for deployments that no longer exist.
	for id := range s.capStore.Snapshot() {
		if _, ok := valid[id]; !ok {
			s.capStore.Drop(id)
		}
	}
}

// profileFromRequirement converts routing requirements plus the canonical
// request shape into the compatibility requirement profile.
func profileFromRequirement(req router.Requirement, canReq *canonical.Request) compat.RequirementProfile {
	profile := compat.RequirementProfile{
		Tools:     req.Tools,
		Vision:    req.Vision,
		Streaming: req.Streaming,
		Reasoning: req.Reasoning,
	}
	if canReq != nil {
		r := canReq.DetectRequirements()
		profile.Tools = profile.Tools || r.Tools
		profile.NeedsTool = r.NeedsTool
		profile.Vision = profile.Vision || r.Vision
		profile.Streaming = profile.Streaming || r.Streaming
		profile.Reasoning = profile.Reasoning || r.Reasoning
		profile.Stop = r.Stop
		profile.Seed = r.Seed
		profile.StructuredOutput = canReq.ResponseFormat != nil && canReq.ResponseFormat.Kind != canonical.FormatText
	}
	return profile
}

// capabilityIneligible reports whether a deployment must be skipped before
// dispatching because a required capability is verified-unsupported.
func (s *Server) capabilityIneligible(requestID, deploymentID string, profile compat.RequirementProfile) (string, bool) {
	capability, ineligible := s.capStore.IneligibleFor(deploymentID, profile)
	if ineligible {
		s.bus.Add(events.Event{
			RequestID: requestID, Kind: "capability_skip", Deployment: deploymentID,
			Message: "required capability verified-unsupported: " + capability, ErrorType: "capability_ineligible",
		})
	}
	return capability, ineligible
}

// learnFromSuccess records capability evidence from real successful traffic.
// Learning is conservative: only parameters that were actually present in
// the successful payload are marked SUPPORTED.
func (s *Server) learnFromSuccess(deploymentID, providerID string, d router.Deployment, a providers.Adapter, payload []byte, canReq *canonical.Request) {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	if !s.routeStillCurrent(d, a) {
		return
	}
	var p config.ProviderConfig
	for _, configured := range s.cfg.Providers {
		if configured.ID == providerID {
			p = configured
			break
		}
	}
	if p.ID == "" {
		return
	}
	dialect := compat.DetectDialect(p.ID, p.Type, p.BaseURL, p.Dialect)
	key := compat.InvalidationKey(p.BaseURL, dialect.Name, d.Model, credentialScope(p))
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err == nil {
		for param, capability := range map[string]string{
			"temperature":         compat.CapTemperature,
			"top_p":               compat.CapTopP,
			"top_k":               compat.CapTopP,
			"stop":                compat.CapStop,
			"stop_sequences":      compat.CapStop,
			"seed":                compat.CapSeed,
			"tools":               compat.CapTools,
			"tool_choice":         compat.CapToolChoiceAuto,
			"parallel_tool_calls": compat.CapParallelToolCalls,
			"response_format":     compat.CapStructuredOutput,
			"reasoning_effort":    compat.CapReasoningEffort,
			"stream_options":      compat.CapStreaming,
		} {
			if _, present := m[param]; present {
				// Never flip a verified UNSUPPORTED back to SUPPORTED: a
				// payload field that survived means the sanitizer allowed it,
				// not that the upstream accepts it. Only UNKNOWN upgrades.
				if s.capStore.Get(deploymentID).Capabilities.Get(capability) == compat.Unsupported {
					continue
				}
				s.capStore.LearnSuccess(deploymentID, capability, compat.SourceRuntime, "successful real traffic", key)
			}
		}
	}
	if canReq != nil {
		if canReq.Reasoning != nil && !canReq.Reasoning.Disabled {
			s.capStore.LearnSuccess(deploymentID, compat.CapReasoning, compat.SourceRuntime, "successful real traffic", key)
		}
		for _, msg := range canReq.Messages {
			for _, part := range msg.Parts {
				if part.Type == canonical.PartImage {
					s.capStore.LearnSuccess(deploymentID, compat.CapVision, compat.SourceRuntime, "successful real traffic", key)
				}
			}
		}
	}
}

// failureVerdict classifies an upstream failure and maps it onto the shared
// failure policy. This replaces status-only classification everywhere the
// compatibility engine is active, so capability failures can be separated
// from health failures.
func classifyFailure(status int, body []byte) (compat.Classified, upstreamFailurePolicy) {
	cls := compat.ClassifyUpstreamError(status, body)
	p := cls.Policy()
	return cls, upstreamFailurePolicy{
		ErrorType:            p.ErrorType,
		Failover:             p.Failover,
		QuarantineDeployment: p.QuarantineDeployment,
		SignalProvider:       p.SignalProvider,
		HardCooldown:         p.HardCooldown,
	}
}

// repairOutcome reports what the bounded repair engine did.
type repairOutcome struct {
	// Repaired is true when a repair rule applied and the request was retried.
	Repaired bool
	// Description names the applied rules (events + UI).
	Description string
	// GaveUp is true when repairs were attempted but the upstream kept
	// rejecting with a capability failure.
	GaveUp bool
	// FinalBody/StatusCode carry the last upstream error when giving up.
	FinalBody   []byte
	StatusCode  int
	ContentType string
}

// doUpstreamWithRepair performs one upstream dispatch with bounded,
// deterministic repair on classified capability failures (spec section 9).
//
//	200            -> response returned untouched
//	400 "unknown parameter: temperature" -> drop temperature, retry once
//	400 "maximum context length"   -> not repairable; returned as-is
//
// A repair attempt does NOT count as a routing attempt and NEVER marks the
// deployment unhealthy. Every repair outcome is written to the capability
// contract.
func (s *Server) doUpstreamWithRepair(
	ctx context.Context,
	requestID string,
	bundle hedgeAttemptBundle,
	stream bool,
	forward http.Header,
	dialect compat.DialectProfile,
	profile compat.RequirementProfile,
	maxRepairs int,
	canReq *canonical.Request,
) (*http.Response, []byte, repairOutcome, error) {
	a := bundle.a
	deployment := bundle.c.Deployment
	payload := bundle.payload
	out := repairOutcome{}
	attempts := maxRepairs
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 2 {
		attempts = 2
	}
	p, _ := s.providerConfigFor(deployment.ProviderID)
	key := compat.InvalidationKey(p.BaseURL, dialect.Name, deployment.Model, credentialScope(p))
	// Proactive sanitizer: skip the round trip entirely when the contract
	// already knows the payload contains unsupported optional fields.
	if s.currentConfig().Routing.SanitizeEnabled {
		contract := s.capStore.Get(deployment.ID)
		if res, err := compat.Sanitize(payload, contract, dialect, profile); err == nil && res.Changed {
			s.observeCurrentRoute(deployment, a, func() {
				s.capStore.SetRepair(deployment.ID, "pre-dispatch: "+fmt.Sprintf("removed %v, renamed %v", res.Removed, res.Renamed))
				s.bus.Add(events.Event{
					RequestID: requestID, Kind: "compat_sanitize", Deployment: deployment.ID,
					Message:   fmt.Sprintf("removed %v renamed %v (pre-dispatch)", res.Removed, res.Renamed),
					ErrorType: "parameter_sanitized",
				})
			})
			payload = res.Payload
		}
	}
	for {
		var resp *http.Response
		var err error
		if bundle.path != "" {
			resp, err = a.DoPath(ctx, http.MethodPost, bundle.path, payload, stream, forward)
		} else {
			resp, err = a.Do(ctx, payload, stream, forward)
		}
		if err != nil {
			return nil, payload, out, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, payload, out, nil
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if readErr != nil {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return resp, payload, out, nil
		}
		cls := compat.ClassifyUpstreamError(resp.StatusCode, body)
		if !cls.CapabilityFailure || attempts <= 0 {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return resp, payload, out, nil
		}
		repaired, plan, ok := compat.Repair(cls, payload, dialect, profile)
		if !ok {
			// Semantics-critical capability failure with no safe repair.
			// Learn the fact only when this is still the current deployment.
			s.observeCurrentRoute(deployment, a, func() {
				if cls.Capability != "" {
					s.capStore.LearnUnsupported(deployment.ID, cls.Capability, compat.SourceRuntime, cls.Message, key)
				}
				s.capStore.SetIssue(deployment.ID, cls.CapabilityLabel())
			})
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return resp, payload, out, nil
		}
		attempts--
		out.Repaired = true
		out.Description = compat.DescribePlan(plan)
		s.observeCurrentRoute(deployment, a, func() {
			for _, rule := range plan.Rules {
				if rule.Capability != "" {
					s.capStore.LearnUnsupported(deployment.ID, rule.Capability, compat.SourceRuntime, cls.Message, key)
				}
			}
			s.capStore.SetRepair(deployment.ID, out.Description)
		})
		if canReq != nil {
			applyRepairToCanonical(canReq, plan)
		}
		s.bus.Add(events.Event{
			RequestID: requestID, Kind: "compat_repair", Deployment: deployment.ID,
			Message:   fmt.Sprintf("%s (%s); retrying once with adapted payload", cls.CapabilityLabel(), out.Description),
			ErrorType: "parameter_repaired", StatusCode: resp.StatusCode,
		})
		payload = repaired
	}
}

// applyRepairToCanonical mirrors the applied payload repair into the canonical
// request so response-side encoders stay consistent with what the upstream
// actually received.
func applyRepairToCanonical(canReq *canonical.Request, plan compat.RepairPlan) {
	if canReq == nil {
		return
	}
	for _, rule := range plan.Rules {
		switch rule.Name {
		case "drop:temperature":
			canReq.Temperature = nil
		case "drop:top_p", "drop:top_k":
			canReq.TopP, canReq.TopK = nil, nil
		case "drop:reasoning-controls":
			canReq.Reasoning = nil
		case "drop:response_format":
			canReq.ResponseFormat = nil
		case "drop:parallel_tool_calls":
			canReq.ParallelToolCalls = nil
		case "drop:stop":
			canReq.Stop = nil
		}
	}
}

// canonicalFromOpenAIRequest rebuilds the canonical request for repair
// bookkeeping on the OpenAI ingress without re-decoding overhead.
func canonicalFromOpenAIRequest(in core.OpenAIRequest) canonical.Request {
	return canonical.DecodeOpenAIChatRequest(in, in.Model)
}

var _ = translate.NameMap{}
var _ = canonical.StopEndTurn
var _ = time.Now

// maybeRepairUpstream inspects a 4xx upstream response on the legacy data
// path; when the classifier identifies a repairable capability failure and
// repair budget remains, it applies the deterministic repair and re-dispatches
// on the same deployment. Capability failures never mark the deployment
// unhealthy. The response is returned with its body restored for the caller.
func (s *Server) maybeRepairUpstream(
	ctx context.Context,
	requestID string,
	bundle hedgeAttemptBundle,
	payload []byte,
	resp *http.Response,
	stream bool,
	forward http.Header,
	maxRepairs int,
	profile compat.RequirementProfile,
) (*http.Response, []byte, repairOutcome) {
	out := repairOutcome{}
	if resp == nil {
		return resp, payload, out
	}
	deployment := bundle.c.Deployment
	dialect := s.dialectFor(deployment.ProviderID, deployment.ProviderType, "")
	p, _ := s.providerConfigFor(deployment.ProviderID)
	key := compat.InvalidationKey(p.BaseURL, dialect.Name, deployment.Model, credentialScope(p))
	attempts := maxRepairs
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 2 {
		attempts = 2
	}
	for {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if readErr != nil {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return resp, payload, out
		}
		cls := compat.ClassifyUpstreamError(resp.StatusCode, body)
		if !cls.CapabilityFailure || attempts <= 0 {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return resp, payload, out
		}
		repaired, plan, ok := compat.Repair(cls, payload, dialect, profile)
		if !ok {
			s.observeCurrentRoute(deployment, bundle.a, func() {
				if cls.Capability != "" {
					s.capStore.LearnUnsupported(deployment.ID, cls.Capability, compat.SourceRuntime, cls.Message, key)
				}
				s.capStore.SetIssue(deployment.ID, cls.CapabilityLabel())
			})
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return resp, payload, out
		}
		attempts--
		out.Repaired = true
		out.Description = compat.DescribePlan(plan)
		s.observeCurrentRoute(deployment, bundle.a, func() {
			for _, rule := range plan.Rules {
				if rule.Capability != "" {
					s.capStore.LearnUnsupported(deployment.ID, rule.Capability, compat.SourceRuntime, cls.Message, key)
				}
			}
			s.capStore.SetRepair(deployment.ID, out.Description)
		})
		s.bus.Add(events.Event{
			RequestID: requestID, Kind: "compat_repair", Deployment: deployment.ID,
			Message:   fmt.Sprintf("%s (%s); retrying with adapted payload", cls.CapabilityLabel(), out.Description),
			ErrorType: "parameter_repaired", StatusCode: resp.StatusCode,
		})
		payload = repaired
		var err error
		if bundle.path != "" {
			resp, err = bundle.a.DoPath(ctx, http.MethodPost, bundle.path, payload, stream, forward)
		} else {
			resp, err = bundle.a.Do(ctx, payload, stream, forward)
		}
		if err != nil {
			return nil, payload, out
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			out.StatusCode = resp.StatusCode
			return resp, payload, out
		}
	}
}

// adapterTransport adapts a providers.Adapter onto compat.ProbeTransport for
// admin-initiated capability suites.
type adapterTransport struct {
	a providers.Adapter
}

func (t adapterTransport) Do(ctx context.Context, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	return t.a.Do(ctx, payload, stream, forward)
}

func (t adapterTransport) RedactBody(b []byte) []byte {
	return t.a.RedactBody(b)
}

// finishCanonicalAttempt encodes the canonical request for the candidate's
// provider family (openai_chat | anthropic | gemini | openai_responses) and
// completes the dispatch bundle, including model-in-path addressing for
// Gemini and Responses-native upstreams.
func (s *Server) finishCanonicalAttempt(bundle hedgeAttemptBundle, canReq canonical.Request) (hedgeAttemptBundle, bool) {
	p, ok := s.providerConfigFor(bundle.c.Deployment.ProviderID)
	if !ok {
		return bundle, false
	}
	kind := upstreamKindFor(p.Type)
	bundle.canonicalKind = kind
	var payload []byte
	var err error
	switch kind {
	case "anthropic":
		var an core.AnthropicRequest
		an, err = canonical.EncodeAnthropicRequest(canReq, bundle.c.Deployment.Model)
		if err == nil {
			payload, err = json.Marshal(an)
		}
	case "gemini":
		payload, _, err = canonical.EncodeGeminiRequest(canReq, bundle.c.Deployment.Model)
		bundle.path = providers.GeminiModelPath(bundle.c.Deployment.Model, canReq.Stream)
	case "openai_responses":
		payload, err = canonical.EncodeResponsesRequest(canReq, bundle.c.Deployment.Model)
		bundle.path = p.ResponsesPath
		if bundle.path == "" {
			bundle.path = "/v1/responses"
		}
	default:
		var oai core.OpenAIRequest
		oai, err = canonical.EncodeOpenAIChatRequest(canReq, bundle.c.Deployment.Model, canReq.Stream)
		if err == nil {
			payload, err = json.Marshal(oai)
		}
	}
	if err != nil {
		s.bus.Add(events.Event{Kind: "encode_fail", Deployment: bundle.c.Deployment.ID, Message: err.Error(), ErrorType: "canonical_encode_failed"})
		return bundle, false
	}
	bundle.payload = payload
	return bundle, true
}

// sanitizeOutgoingPayload applies the proactive parameter sanitizer using the
// cached capability contract before dispatch. It is a no-op unless the
// contract holds verified UNSUPPORTED entries for fields present in the
// payload.
func (s *Server) sanitizeOutgoingPayload(bundle hedgeAttemptBundle, payload []byte, req router.Requirement) []byte {
	cfg := s.currentConfig()
	if !cfg.Routing.SanitizeEnabled {
		return payload
	}
	deployment := bundle.c.Deployment
	p, ok := s.providerConfigFor(deployment.ProviderID)
	if !ok {
		return payload
	}
	dialect := compat.DetectDialect(p.ID, p.Type, p.BaseURL, p.Dialect)
	contract := s.capStore.Get(deployment.ID)
	profile := profileFromRequirement(req, nil)
	res, err := compat.Sanitize(payload, contract, dialect, profile)
	if err != nil || !res.Changed {
		return payload
	}
	s.observeCurrentRoute(deployment, bundle.a, func() {
		s.capStore.SetRepair(deployment.ID, "pre-dispatch: removed "+fmt.Sprint(res.Removed)+" renamed "+fmt.Sprint(res.Renamed))
		s.bus.Add(events.Event{
			Kind: "compat_sanitize", Deployment: deployment.ID,
			Message:   fmt.Sprintf("removed %v renamed %v (pre-dispatch)", res.Removed, res.Renamed),
			ErrorType: "parameter_sanitized",
		})
	})
	return res.Payload
}
