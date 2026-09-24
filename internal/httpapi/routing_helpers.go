package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func routeContext(parent context.Context, streaming bool, timeout time.Duration) (context.Context, context.CancelFunc) {
	if streaming || timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

func gatewayDeadlineExceeded(routeCtx, clientCtx context.Context) bool {
	return routeCtx.Err() == context.DeadlineExceeded && clientCtx.Err() == nil
}

func clientRequestGone(clientCtx context.Context) bool {
	return clientCtx.Err() != nil
}

func jitteredRetryBackoff(base time.Duration, attempt int, requestID string) time.Duration {
	if base <= 0 {
		return 0
	}
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 6 {
		attempt = 6
	}
	const capDelay = 5 * time.Second
	if base > capDelay {
		base = capDelay
	}
	max := base
	for n := 0; n < attempt && max < capDelay; n++ {
		if max > capDelay/2 {
			max = capDelay
			break
		}
		max *= 2
	}
	if max > capDelay {
		max = capDelay
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(requestID))
	_, _ = h.Write([]byte{byte(attempt)})
	// Full jitter in [0,max], deterministic for a request+attempt so tests and
	// incident replay remain reproducible while concurrent clients desynchronize.
	return time.Duration(h.Sum64() % uint64(max+1))
}

func patchJSONModel(raw []byte, model string) ([]byte, error) {
	if !utf8.ValidString(model) {
		return nil, fmt.Errorf("model id is not valid UTF-8")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	modelJSON, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	obj["model"] = modelJSON
	return json.Marshal(obj)
}

type upstreamFailurePolicy struct {
	ErrorType            string
	Failover             bool
	QuarantineDeployment bool
	SignalProvider       bool
	HardCooldown         bool
}

func policyForStatus(code int) upstreamFailurePolicy {
	switch code {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return upstreamFailurePolicy{ErrorType: "caller_invalid_request"}
	case http.StatusUnauthorized, http.StatusForbidden:
		return upstreamFailurePolicy{ErrorType: "provider_auth_failed", Failover: true, QuarantineDeployment: true, SignalProvider: true, HardCooldown: true}
	case http.StatusPaymentRequired:
		return upstreamFailurePolicy{ErrorType: "provider_billing", Failover: true, QuarantineDeployment: true, SignalProvider: true, HardCooldown: true}
	case http.StatusNotFound:
		return upstreamFailurePolicy{ErrorType: "provider_request_rejected", Failover: true, QuarantineDeployment: true}
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return upstreamFailurePolicy{ErrorType: "provider_timeout", Failover: true, QuarantineDeployment: true, SignalProvider: true}
	case http.StatusConflict, http.StatusTooEarly:
		return upstreamFailurePolicy{ErrorType: "provider_transient_request", Failover: true}
	case http.StatusTooManyRequests:
		return upstreamFailurePolicy{ErrorType: "provider_rate_limited", Failover: true, QuarantineDeployment: true, SignalProvider: true, HardCooldown: true}
	case http.StatusServiceUnavailable, 529:
		return upstreamFailurePolicy{ErrorType: "provider_overloaded", Failover: true, QuarantineDeployment: true, SignalProvider: true}
	default:
		if code >= 500 {
			return upstreamFailurePolicy{ErrorType: "provider_server_error", Failover: true, QuarantineDeployment: true, SignalProvider: true}
		}
		return upstreamFailurePolicy{ErrorType: "_OTHER"}
	}
}

func errorTypeForStatus(code int) string { return policyForStatus(code).ErrorType }
func retryable(code int) bool            { return policyForStatus(code).Failover }
func failoverEligible(code int) bool     { return policyForStatus(code).Failover }
func hardCooldownStatus(code int) bool   { return policyForStatus(code).HardCooldown }

func retryAfterDuration(h http.Header, max time.Duration) time.Duration {
	fallback := 30 * time.Second
	if max > 0 && fallback > max {
		fallback = max
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return fallback
	}
	if sec, err := strconv.ParseInt(v, 10, 64); err == nil && sec > 0 {
		if max > 0 {
			maxSeconds := int64(max / time.Second)
			if maxSeconds < 1 || sec >= maxSeconds {
				return max
			}
		}
		if sec > int64((time.Duration(1<<63-1))/time.Second) {
			if max > 0 {
				return max
			}
			return fallback
		}
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(time.Now()) {
		d := time.Until(t)
		if max > 0 && d > max {
			return max
		}
		return d
	}
	return fallback
}

func redactProviderBody(p config.ProviderConfig, b []byte) []byte {
	out := append([]byte(nil), b...)
	for _, key := range p.ResolvedCredentials() {
		if key == "" {
			continue
		}
		out = bytes.ReplaceAll(out, []byte(key), []byte("[REDACTED]"))
	}
	return out
}

func upstreamError(status int, b []byte) string {
	msg := strings.TrimSpace(string(b))
	if len(msg) > 1024 {
		msg = msg[:1024] + "…"
	}
	return fmt.Sprintf("upstream %d: %s", status, msg)
}

func blockedClientForwardHeader(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "authorization", "proxy-authorization", "x-api-key", "x-admin-key",
		"cookie", "set-cookie", "connection", "proxy-connection",
		"transfer-encoding", "content-length", "host":
		return true
	default:
		return false
	}
}

func copySelectedRequestHeaders(r *http.Request) http.Header {
	h := make(http.Header, len(r.Header))
	for k, vals := range r.Header {
		if blockedClientForwardHeader(k) {
			continue
		}
		h[k] = append([]string(nil), vals...)
	}
	return h
}

const maxRequestInspectionNodes = 100000

type requestInspection struct {
	Vision         bool
	Reasoning      bool
	TooComplex     bool
	BodySessionKey string
	// EstimatedPromptTokens is a fast chars/4 heuristic over the message
	// subtree plus per-message overhead. It is intentionally conservative
	// (an estimate, never exact) and only used for context-window
	// pre-routing; billing uses real upstream usage numbers.
	EstimatedPromptTokens int
}

func boundedSessionValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 256 {
		v = v[:256]
	}
	return v
}

func bodySessionKey(root map[string]any) string {
	if v, _ := root["session_id"].(string); boundedSessionValue(v) != "" {
		return boundedSessionValue(v)
	}
	meta, _ := root["metadata"].(map[string]any)
	if meta == nil {
		return ""
	}
	if v, _ := meta["session_id"].(string); boundedSessionValue(v) != "" {
		return boundedSessionValue(v)
	}
	if user, ok := meta["user_id"].(map[string]any); ok {
		if v, _ := user["session_id"].(string); boundedSessionValue(v) != "" {
			return boundedSessionValue(v)
		}
	}
	return ""
}

func inspectRequestJSON(raw []byte, visionType string, reasoningKeys []string) requestInspection {
	return inspectRequestJSONFields(raw, visionType, reasoningKeys, []string{"messages"})
}

func inspectResponsesRequestJSON(raw []byte) requestInspection {
	return inspectRequestJSONFields(raw, "input_image", []string{"reasoning"}, []string{"input", "instructions"})
}

func inspectRequestJSONFields(raw []byte, visionType string, reasoningKeys, contentFields []string) requestInspection {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return requestInspection{}
	}
	out := requestInspection{BodySessionKey: bodySessionKey(root)}

	// Reasoning controls are protocol-level request options. Do not scan tool
	// schemas or arbitrary user/tool payloads for keys with the same name.
	for _, wanted := range reasoningKeys {
		for key := range root {
			if strings.EqualFold(key, wanted) {
				out.Reasoning = true
				break
			}
		}
		if out.Reasoning {
			break
		}
	}

	// Inspect only protocol-defined conversation/input fields. This keeps tool
	// schemas and arbitrary metadata from falsely triggering vision while still
	// giving Responses API requests their real input/instructions estimate.
	stack := make([]any, 0, len(contentFields))
	for _, field := range contentFields {
		if v, ok := root[field]; ok {
			stack = append(stack, v)
		}
	}
	if len(stack) == 0 {
		return out
	}

	chars := 0
	messageCount := 0
	nodes := 0
	for len(stack) > 0 {
		last := len(stack) - 1
		v := stack[last]
		stack = stack[:last]
		nodes++
		if nodes > maxRequestInspectionNodes {
			out.TooComplex = true
			return out
		}
		switch x := v.(type) {
		case string:
			chars += len(x)
		case map[string]any:
			if typ, _ := x["type"].(string); visionType != "" && strings.EqualFold(typ, visionType) {
				out.Vision = true
			}
			if _, isMsg := x["role"]; isMsg {
				messageCount++
			}
			if len(x) > maxRequestInspectionNodes-nodes-len(stack) {
				out.TooComplex = true
				return out
			}
			for _, child := range x {
				stack = append(stack, child)
			}
		case []any:
			if len(x) > maxRequestInspectionNodes-nodes-len(stack) {
				out.TooComplex = true
				return out
			}
			stack = append(stack, x...)
		}
	}
	// ~4 chars per token plus a small per-message framing overhead and a
	// fixed conversation floor; rounded up. Text is counted by bytes, which
	// slightly overestimates multi-byte content — a safe bias for routing.
	out.EstimatedPromptTokens = chars/4 + messageCount*8 + 16
	return out
}

func sessionKeyFromRequestParts(r *http.Request, bodyKey string) string {
	for _, header := range []string{"x-claude-code-session-id", "x-litellm-session-id", "x-litellm-trace-id", "x-session-id"} {
		if v := boundedSessionValue(r.Header.Get(header)); v != "" {
			return v
		}
	}
	return boundedSessionValue(bodyKey)
}

func sessionKeyFromRequest(r *http.Request, raw []byte) string {
	inspection := inspectRequestJSON(raw, "", nil)
	return sessionKeyFromRequestParts(r, inspection.BodySessionKey)
}

func quotaRemainingPressure(remaining, limit int64) float64 {
	if remaining < 0 || limit <= 0 {
		return 0
	}
	if remaining == 0 {
		return 4
	}
	ratio := float64(remaining) / float64(limit)
	// Stay neutral while at least 25% of a reported budget remains, then
	// increase pressure smoothly to the same maximum as a saturated provider.
	if ratio >= 0.25 {
		return 0
	}
	p := (0.25 - ratio) / 0.25 * 4
	if p < 0 {
		return 0
	}
	if p > 4 {
		return 4
	}
	return p
}

func providerLoadFromStats(st providers.ProviderStats, nowUnix int64) router.ProviderLoad {
	requestResetPending := st.RequestResetUnix > nowUnix
	tokenResetPending := st.TokenResetUnix > nowUnix
	// Effective remaining quota subtracts data-plane requests/tokens that
	// are already in flight but may not yet be reflected in a provider's
	// latest remaining-* response headers.
	remainingRequests := st.EffectiveRemainingRequests
	if remainingRequests < 0 {
		remainingRequests = st.RemainingRequests
	}
	remainingTokens := st.EffectiveRemainingTokens
	if remainingTokens < 0 {
		remainingTokens = st.RemainingTokens
	}
	// Backward compatibility for adapters/providers exposing only a shared
	// reset deadline: use it for a zero-remaining signal, but never for
	// ratio-based predictive pressure without a resource-specific deadline.
	sharedResetPending := st.RateLimitResetUnix > nowUnix
	quotaExhausted := (remainingRequests == 0 && (requestResetPending || sharedResetPending)) ||
		(remainingTokens == 0 && (tokenResetPending || sharedResetPending))
	quotaPressure := 0.0
	if requestResetPending {
		quotaPressure = quotaRemainingPressure(remainingRequests, st.RequestLimit)
	}
	if tokenResetPending {
		if p := quotaRemainingPressure(remainingTokens, st.TokenLimit); p > quotaPressure {
			quotaPressure = p
		}
	}
	return router.ProviderLoad{
		Active:         st.ActiveRequests,
		Waiting:        st.WaitingRequests,
		Limit:          st.MaxConcurrency,
		QuotaExhausted: quotaExhausted,
		QuotaPressure:  quotaPressure,
	}
}

func (s *Server) prepareRequirement(req router.Requirement, r *http.Request, bodySessionKey string) router.Requirement {
	req.SessionKey = sessionKeyFromRequestParts(r, bodySessionKey)
	if req.SessionKey != "" {
		// A session identifier is only unique within a client. Bind pins to
		// the presented client key so two tenants using the same session ID
		// cannot change each other's preferred deployment. Never retain the
		// credential itself in the router's session table.
		req.SessionKey = keyDigest(extractClientKey(r)) + ":" + req.SessionKey
	}
	req.SelectionKey = r.Header.Get("x-request-id")
	cache := make(map[string]router.ProviderLoad, 4)
	req.LoadForProvider = func(id string) router.ProviderLoad {
		if load, ok := cache[id]; ok {
			return load
		}
		st, ok := s.reg.Stat(id)
		if !ok {
			cache[id] = router.ProviderLoad{}
			return router.ProviderLoad{}
		}
		load := providerLoadFromStats(st, time.Now().Unix())
		cache[id] = load
		return load
	}
	return req
}

// routeStillCurrent is called under runtimeMu.RLock. In-flight responses from
// an old model/adapter must not restore ready health after hot reload has
// invalidated that deployment ID for its replacement.
func (s *Server) routeStillCurrent(d router.Deployment, a providers.Adapter) bool {
	fresh, ok := s.rt.Deployment(d.ID)
	if !ok || fresh.ProviderID != d.ProviderID || fresh.ProviderType != d.ProviderType ||
		fresh.Model != d.Model || fresh.ContextWindow != d.ContextWindow || fresh.Capabilities != d.Capabilities {
		return false
	}
	current, ok := s.reg.Get(d.ProviderID)
	return ok && current == a
}

// observeCurrentRoute performs both the identity check and the observation
// under the config lock, so reload cannot slip between them.
func (s *Server) observeCurrentRoute(d router.Deployment, a providers.Adapter, apply func()) bool {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	if !s.routeStillCurrent(d, a) {
		return false
	}
	apply()
	return true
}

func (s *Server) recordRouteSuccess(req router.Requirement, d router.Deployment, a providers.Adapter, latency time.Duration) {
	s.observeCurrentRoute(d, a, func() {
		s.hm.RecordSuccess(d.ID, latency)
		s.hm.RecordProviderSuccess(d.ProviderID)
		s.hm.RecordScopeSuccess(d.ID, req.Scopes())
		s.rt.ObserveSession(req, d.ID)
	})
}

func (s *Server) recordResponseFailure(cfg config.Config, d router.Deployment, a providers.Adapter, streaming bool, reason string, latency time.Duration, committed bool) {
	s.observeCurrentRoute(d, a, func() {
		if cfg.Routing.Strategy == "ready_mesh" && streaming {
			s.hm.RecordScopeFailure(d.ID, []string{"streaming"}, reason)
		} else if router.IsReadyStrategy(cfg.Routing.Strategy) {
			s.hm.Quarantine(d.ID, reason, latency)
			s.probe.Recover(d.ID)
		} else {
			s.hm.RecordFailure(d.ID, reason, latency)
		}
		if committed {
			s.hm.RecordProviderFailure(d.ProviderID, d.ID, reason)
		}
	})
}

func (s *Server) recordProviderFailure(providerID, deploymentID, reason string, policy upstreamFailurePolicy) {
	if policy.SignalProvider {
		s.hm.RecordProviderFailure(providerID, deploymentID, reason)
	}
}

type firstByteBody struct {
	io.ReadCloser
	once    sync.Once
	start   time.Time
	observe func(time.Duration)
}

func (b *firstByteBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && b.observe != nil {
		b.once.Do(func() { b.observe(time.Since(b.start)) })
	}
	return n, err
}

func observeFirstByte(rc io.ReadCloser, start time.Time, observe func(time.Duration)) io.ReadCloser {
	return &firstByteBody{ReadCloser: rc, start: start, observe: observe}
}

// stripStreamOptions removes the stream_options field from a translated
// OpenAI payload. Providers that predate the option reject it with a 400
// whose body mentions the field; the caller retries once with the stripped
// payload so streaming keeps working against conservative upstreams.
func stripStreamOptions(payload []byte) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return payload, false
	}
	if _, ok := obj["stream_options"]; !ok {
		return payload, false
	}
	delete(obj, "stream_options")
	stripped, err := json.Marshal(obj)
	if err != nil {
		return payload, false
	}
	return stripped, true
}
