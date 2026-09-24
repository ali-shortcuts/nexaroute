package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat/capabilities"
	compaterrors "github.com/ali-shortcuts/nexaroute/internal/compat/errors"
	"github.com/ali-shortcuts/nexaroute/internal/compat/quirks"
	"github.com/ali-shortcuts/nexaroute/internal/compat/repair"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
)

// compatStore returns the server capability store, lazily creating it for
// tests that build Server without New().
func (s *Server) compatStore() *capabilities.Store {
	s.runtimeMu.RLock()
	st := s.compat
	s.runtimeMu.RUnlock()
	if st != nil {
		return st
	}
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if s.compat == nil {
		s.compat = capabilities.NewStore()
	}
	return s.compat
}

// compatConfig returns the current compat tuning.
func (s *Server) compatConfig() config.CompatConfig {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.cfg.Compat
}

// providerDialect resolves the quirk profile for a provider.
func providerDialect(p config.ProviderConfig) quirks.DialectProfile {
	return quirks.Infer(p.Dialect, p.ID, p.BaseURL, p.Type)
}

// seedCompatContract ensures a deployment has a contract seeded from static
// config flags. Static true becomes SUPPORTED; static false stays UNKNOWN so
// probing can verify real behavior.
func (s *Server) seedCompatContract(deploymentID string, caps config.Capabilities, nativeProtocol string) {
	st := s.compatStore()
	if _, ok := st.Get(deploymentID); ok {
		return
	}
	m := capabilities.FromStaticBools(caps.Streaming, caps.Tools, caps.Vision, caps.Reasoning)
	m.NativeProtocol = nativeProtocol
	st.Set(deploymentID, m)
}

// classifyUpstream maps a status + body onto the structured taxonomy.
func classifyUpstream(status int, body []byte) compaterrors.Result {
	return compaterrors.Classify(status, body)
}

// recordCompatObservation applies conservative runtime learning: only
// high-confidence capability evidence updates the contract, and capability
// failures never touch deployment health.
func (s *Server) recordCompatObservation(deploymentID string, classified compaterrors.Result) {
	if classified.Capability == "" {
		return
	}
	switch classified.Class {
	case compaterrors.UnsupportedParameter, compaterrors.UnsupportedToolCalling,
		compaterrors.UnsupportedReasoning, compaterrors.UnsupportedVision,
		compaterrors.UnsupportedStructuredOutput:
		s.compatStore().Learn(deploymentID, classified.Capability,
			capabilities.Unsupported, capabilities.SourceRuntime,
			capabilities.ConfidenceHigh,
			"upstream: "+truncateCompat(classified.Message, 160))
		// Mirror into the existing scope circuit so ready_mesh stops routing
		// the broken capability while the deployment stays usable otherwise.
		scope := compatScope(classified.Capability)
		if scope != "" {
			s.hm.RecordScopeFailure(deploymentID, []string{scope}, "compat: "+classified.Class.String())
		}
	}
}

// compatScope maps capability keys onto existing health scope names.
func compatScope(capability string) string {
	switch capability {
	case "tools", "tool_choice_auto", "tool_choice_required", "parallel_tools":
		return "tools"
	case "vision":
		return "vision"
	case "streaming":
		return "streaming"
	case "reasoning", "reasoning_effort":
		return "reasoning"
	default:
		return ""
	}
}

func truncateCompat(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// maybeRepairUpstream attempts bounded same-deployment repair for classified
// UNSUPPORTED_PARAMETER failures. It returns the replacement response when a
// repair rule applied; ok=false means the caller should use the original
// status/body handling. The caller supplies the dispatch closure so
// model-scoped upstreams (Gemini) repair against the right URL.
func (s *Server) maybeRepairUpstream(
	ctx context.Context,
	send func(context.Context, []byte) (*http.Response, error),
	redact func([]byte) []byte,
	deploymentID string,
	payload []byte,
	status int,
	body []byte,
	requestID string,
) (resp *http.Response, repairedPayload []byte, attempt repair.Attempt, ok bool) {
	cc := s.compatConfig()
	if !cc.RepairsAllowed() {
		return nil, nil, repair.Attempt{}, false
	}
	budget := cc.RepairBudget()
	if budget < 1 {
		return nil, nil, repair.Attempt{}, false
	}
	classified := classifyUpstream(status, body)
	if !classified.RetryableRepair {
		return nil, nil, repair.Attempt{}, false
	}
	current := payload
	for i := 0; i < budget && i < repair.MaxAttempts; i++ {
		next, att, good := repair.Apply(current, classified)
		if !good {
			return nil, nil, repair.Attempt{}, false
		}
		current = next
		r, err := send(ctx, current)
		if err != nil {
			s.bus.Add(events.Event{RequestID: requestID, Kind: "compat_repair_error",
				Deployment: deploymentID, Message: "repair attempt transport error: " + err.Error()})
			return nil, nil, repair.Attempt{}, false
		}
		if r.StatusCode < 200 || r.StatusCode >= 300 {
			rb, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
			r.Body.Close()
			if redact != nil {
				rb = redact(rb)
			}
			nextClass := classifyUpstream(r.StatusCode, rb)
			s.bus.Add(events.Event{RequestID: requestID, Kind: "compat_repair_fail",
				Deployment: deploymentID,
				Message:    fmt.Sprintf("rule=%s still failing: %s", att.Rule, nextClass.Class.String()),
				StatusCode: r.StatusCode})
			// Chain at most one more repair if the new failure is also repairable.
			if i+1 < budget && nextClass.RetryableRepair {
				classified = nextClass
				continue
			}
			// Restore a readable body for the normal error path.
			r.Body = io.NopCloser(strings.NewReader(string(rb)))
			r.ContentLength = int64(len(rb))
			return nil, nil, repair.Attempt{}, false
		}
		s.bus.Add(events.Event{RequestID: requestID, Kind: "compat_repair_ok",
			Deployment: deploymentID,
			Message:    fmt.Sprintf("rule=%s removed=%s mapped=%s", att.Rule, att.Removed, att.Mapped)})
		// Learn the repaired capability as unsupported for future sanitizing.
		if classified.Capability != "" {
			s.compatStore().Learn(deploymentID, classified.Capability,
				capabilities.Unsupported, capabilities.SourceRuntime,
				capabilities.ConfidenceHigh, "bounded repair rule "+att.Rule)
		}
		return r, current, att, true
	}
	return nil, nil, repair.Attempt{}, false
}

// compatSnapshot exposes contracts + scorecards for the admin UI.
func (s *Server) compatSnapshot() map[string]any {
	st := s.compatStore()
	contracts := st.Snapshot()
	cards := make([]capabilities.Scorecard, 0, len(contracts))
	for id, m := range contracts {
		avail := string(s.hm.Get(id).Status)
		if avail == "" {
			avail = "unknown"
		}
		cards = append(cards, m.Score(id, avail))
	}
	return map[string]any{
		"contracts":       contracts,
		"scorecards":      cards,
		"dialect_version": quirks.Version,
	}
}

// adminCompat handles GET /admin/api/compat (contracts + scorecards).
func (s *Server) adminCompat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, s.compatSnapshot())
}

// classifyUpstreamFailureForPolicy overlays the structured taxonomy on the
// legacy status policy: capability failures are health-neutral and never
// signal provider incidents, even when the legacy table would quarantine.
func classifyUpstreamFailureForPolicy(status int, body []byte, legacy upstreamFailurePolicy) (upstreamFailurePolicy, compaterrors.Result) {
	classified := classifyUpstream(status, body)
	switch classified.Class {
	case compaterrors.UnsupportedParameter, compaterrors.UnsupportedToolCalling,
		compaterrors.UnsupportedReasoning, compaterrors.UnsupportedVision,
		compaterrors.UnsupportedStructuredOutput, compaterrors.ContextOverflow:
		legacy.QuarantineDeployment = false
		legacy.SignalProvider = false
		legacy.HardCooldown = false
		legacy.Failover = false
		legacy.ErrorType = "capability_" + strings.ToLower(classified.Class.String())
	}
	return legacy, classified
}

var _ = json.Marshal
var _ = time.Now
