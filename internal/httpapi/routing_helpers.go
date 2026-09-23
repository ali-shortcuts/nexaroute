package httpapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ali-shortcuts/nexaroute/internal/config"
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

func errorTypeForStatus(code int) string {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "provider_auth_failed"
	case http.StatusPaymentRequired:
		return "provider_billing"
	case http.StatusTooManyRequests:
		return "provider_rate_limited"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return "provider_timeout"
	case http.StatusServiceUnavailable, 529:
		return "provider_overloaded"
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "caller_invalid_request"
	case http.StatusNotFound:
		return "provider_request_rejected"
	default:
		if code >= 500 {
			return "provider_server_error"
		}
		return "_OTHER"
	}
}

func retryable(code int) bool {
	return code == 408 || code == 409 || code == 425 || code == 429 || code == 500 || code == 502 || code == 503 || code == 504 || code == 529
}
func failoverEligible(code int) bool {
	// Upstream auth/quota/not-found failures are deployment/provider failures from
	// the gateway's perspective. Trying another configured deployment is useful
	// and does not repeat the same failing upstream. 400/422 are intentionally
	// excluded because they usually indicate a request that every provider would
	// reject in the same way.
	return retryable(code) || code == 401 || code == 402 || code == 403 || code == 404
}
func hardCooldownStatus(code int) bool {
	return code == 401 || code == 402 || code == 403 || code == 429
}

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

	// Vision parts are meaningful inside message content. Restrict traversal to
	// the messages subtree so a tool schema/example containing type=image(_url)
	// cannot accidentally force vision-capable routing.
	if visionType == "" {
		return out
	}
	messages, ok := root["messages"]
	if !ok {
		return out
	}
	stack := []any{messages}
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
		case map[string]any:
			if typ, _ := x["type"].(string); strings.EqualFold(typ, visionType) {
				out.Vision = true
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

func (s *Server) prepareRequirement(req router.Requirement, r *http.Request, bodySessionKey string) router.Requirement {
	req.SessionKey = sessionKeyFromRequestParts(r, bodySessionKey)
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
		load := router.ProviderLoad{
			Active:  st.ActiveRequests,
			Waiting: st.WaitingRequests,
			Limit:   st.MaxConcurrency,
		}
		cache[id] = load
		return load
	}
	return req
}

func (s *Server) recordRouteSuccess(req router.Requirement, deploymentID string, latency time.Duration) {
	s.hm.RecordSuccess(deploymentID, latency)
	s.hm.RecordScopeSuccess(deploymentID, req.Scopes())
	s.rt.ObserveSession(req, deploymentID)
}

// classifyTransportError maps a transport-layer failure to a precise event
// error type. Client cancellation is reported as caller_cancelled (upstream).
// Unrecognized failures keep the historical provider_connection_failed type.
func classifyTransportError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "caller_cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "provider_timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "provider_timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return "dns_not_found"
		}
		return "dns_failure"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return "connection_reset"
	}
	if errors.Is(err, syscall.ECONNABORTED) {
		return "connection_aborted"
	}
	if errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return "network_unreachable"
	}
	var x509Hostname x509.HostnameError
	if errors.As(err, &x509Hostname) {
		return "tls_failure"
	}
	var x509Authority x509.UnknownAuthorityError
	if errors.As(err, &x509Authority) {
		return "tls_failure"
	}
	var tlsRecord tls.RecordHeaderError
	if errors.As(err, &tlsRecord) {
		return "tls_failure"
	}
	// tls.CertificateVerificationError wraps an x509 error and supports
	// Unwrap, so the x509 checks above already classify it as tls_failure.
	return "provider_connection_failed"
}
