package errors

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Structured upstream error taxonomy. See spec section 8.
//
// The classifier answers WHY a request failed so the gateway can decide:
//   - does this implicate deployment health, or only a capability?
//   - is a bounded repair/sanitize retry safe?
//   - should another candidate be tried (failover)?
type Class int

const (
	Unknown Class = iota
	AuthError
	InvalidKey
	QuotaExhausted
	RateLimit
	ModelNotFound
	EndpointNotFound
	UnsupportedParameter
	UnsupportedToolCalling
	UnsupportedReasoning
	UnsupportedVision
	UnsupportedStructuredOutput
	ContextOverflow
	InvalidRequestSchema
	UpstreamOverload
	UpstreamInternalError
	Timeout
	NetworkError
	StreamProtocolError
	MalformedResponse
)

func (c Class) String() string {
	switch c {
	case AuthError:
		return "AUTH_ERROR"
	case InvalidKey:
		return "INVALID_KEY"
	case QuotaExhausted:
		return "QUOTA_EXHAUSTED"
	case RateLimit:
		return "RATE_LIMIT"
	case ModelNotFound:
		return "MODEL_NOT_FOUND"
	case EndpointNotFound:
		return "ENDPOINT_NOT_FOUND"
	case UnsupportedParameter:
		return "UNSUPPORTED_PARAMETER"
	case UnsupportedToolCalling:
		return "UNSUPPORTED_TOOL_CALLING"
	case UnsupportedReasoning:
		return "UNSUPPORTED_REASONING"
	case UnsupportedVision:
		return "UNSUPPORTED_VISION"
	case UnsupportedStructuredOutput:
		return "UNSUPPORTED_STRUCTURED_OUTPUT"
	case ContextOverflow:
		return "CONTEXT_OVERFLOW"
	case InvalidRequestSchema:
		return "INVALID_REQUEST_SCHEMA"
	case UpstreamOverload:
		return "UPSTREAM_OVERLOAD"
	case UpstreamInternalError:
		return "UPSTREAM_INTERNAL_ERROR"
	case Timeout:
		return "TIMEOUT"
	case NetworkError:
		return "NETWORK_ERROR"
	case StreamProtocolError:
		return "STREAM_PROTOCOL_ERROR"
	case MalformedResponse:
		return "MALFORMED_RESPONSE"
	default:
		return "UNKNOWN"
	}
}

// Result is a classified upstream failure.
type Result struct {
	Class Class `json:"class"`
	// Parameter names the offending field for UNSUPPORTED_PARAMETER.
	Parameter string `json:"parameter,omitempty"`
	// Capability maps the failure onto a capability key when applicable
	// (e.g. temperature, tools, vision, reasoning).
	Capability string `json:"capability,omitempty"`
	// Message is the redacted upstream message.
	Message string `json:"message,omitempty"`
	// Failover reports whether trying the next candidate is safe.
	Failover bool `json:"failover"`
	// AffectsHealth reports whether deployment health may be recorded.
	AffectsHealth bool `json:"affects_health"`
	// AffectsProvider reports whether provider-incident evidence applies.
	AffectsProvider bool `json:"affects_provider"`
	// RetryableRepair reports whether a bounded sanitize/repair retry is safe.
	RetryableRepair bool `json:"retryable_repair"`
}

// Classify maps an HTTP status + response body onto the taxonomy.
// body is expected to be already size-bounded by the caller.
func Classify(status int, body []byte) Result {
	msg := extractMessage(body)
	lower := strings.ToLower(msg)
	switch {
	case status == 0:
		if isTimeoutText(lower) {
			return Result{Class: Timeout, Message: msg, Failover: true, AffectsHealth: true, AffectsProvider: true}
		}
		return Result{Class: NetworkError, Message: msg, Failover: true, AffectsHealth: true, AffectsProvider: true}
	case status == http.StatusUnauthorized:
		if isInvalidKeyText(lower) || lower == "" {
			return Result{Class: InvalidKey, Message: msg, Failover: true, AffectsHealth: true, AffectsProvider: true}
		}
		return Result{Class: AuthError, Message: msg, Failover: true, AffectsHealth: true, AffectsProvider: true}
	case status == http.StatusForbidden:
		if isQuotaText(lower) {
			return Result{Class: QuotaExhausted, Message: msg, Failover: true, AffectsHealth: true, AffectsProvider: true}
		}
		return Result{Class: AuthError, Message: msg, Failover: true, AffectsHealth: true, AffectsProvider: true}
	case status == http.StatusPaymentRequired:
		return Result{Class: QuotaExhausted, Message: msg, Failover: true, AffectsHealth: true, AffectsProvider: true}
	case status == http.StatusNotFound:
		if isModelText(lower) || lower == "" {
			return Result{Class: ModelNotFound, Message: msg, Failover: true, AffectsHealth: true}
		}
		return Result{Class: EndpointNotFound, Message: msg, Failover: true, AffectsHealth: true}
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return Result{Class: Timeout, Message: msg, Failover: true, AffectsHealth: true, AffectsProvider: true}
	case status == http.StatusTooManyRequests:
		if isQuotaText(lower) {
			return Result{Class: QuotaExhausted, Message: msg, Failover: true, AffectsHealth: true, AffectsProvider: true}
		}
		return Result{Class: RateLimit, Message: msg, Failover: true, AffectsHealth: true, AffectsProvider: true}
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return classifyBadRequest(msg, lower)
	case status == http.StatusConflict || status == http.StatusTooEarly:
		return Result{Class: UpstreamOverload, Message: msg, Failover: true}
	case status == http.StatusServiceUnavailable || status == 529:
		return Result{Class: UpstreamOverload, Message: msg, Failover: true, AffectsHealth: true, AffectsProvider: true}
	case status >= 500:
		return Result{Class: UpstreamInternalError, Message: msg, Failover: true, AffectsHealth: true, AffectsProvider: true}
	default:
		return Result{Class: Unknown, Message: msg}
	}
}

func classifyBadRequest(msg, lower string) Result {
	// Capability failures must NEVER poison deployment health.
	// See spec sections 7 and 27.
	if isContextOverflowText(lower) {
		return Result{Class: ContextOverflow, Message: msg, Capability: "context_window"}
	}
	if param, ok := unsupportedParameter(lower); ok {
		return Result{
			Class: UnsupportedParameter, Parameter: param,
			Capability: paramCapability(param), Message: msg,
			RetryableRepair: true,
		}
	}
	if isToolCallingText(lower) {
		return Result{Class: UnsupportedToolCalling, Message: msg, Capability: "tools"}
	}
	if isReasoningText(lower) {
		return Result{Class: UnsupportedReasoning, Message: msg, Capability: "reasoning"}
	}
	if isVisionText(lower) {
		return Result{Class: UnsupportedVision, Message: msg, Capability: "vision"}
	}
	if isStructuredOutputText(lower) {
		return Result{Class: UnsupportedStructuredOutput, Message: msg, Capability: "structured_output"}
	}
	if isModelText(lower) {
		return Result{Class: ModelNotFound, Message: msg, Failover: true, AffectsHealth: true}
	}
	// Anything else 4xx-shaped is caller-invalid and health-neutral.
	return Result{Class: InvalidRequestSchema, Message: msg}
}

func extractMessage(body []byte) string {
	msg := strings.TrimSpace(string(body))
	if len(msg) == 0 {
		return ""
	}
	if len(msg) > 2048 {
		msg = msg[:2048]
	}
	// Prefer structured error.message when present (OpenAI + Anthropic shapes).
	var root map[string]any
	if err := json.Unmarshal([]byte(msg), &root); err == nil {
		if e, ok := root["error"].(map[string]any); ok {
			if m, _ := e["message"].(string); strings.TrimSpace(m) != "" {
				return strings.TrimSpace(m)
			}
		} else if m, _ := root["message"].(string); strings.TrimSpace(m) != "" {
			return strings.TrimSpace(m)
		} else if m, _ := root["detail"].(string); strings.TrimSpace(m) != "" {
			return strings.TrimSpace(m)
		}
	}
	return msg
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func isTimeoutText(lower string) bool {
	return containsAny(lower, "timeout", "timed out", "deadline exceeded", "context deadline")
}

func isInvalidKeyText(lower string) bool {
	return containsAny(lower, "invalid api key", "invalid x-api-key", "incorrect api key", "invalid auth", "invalid key", "unauthorized")
}

func isQuotaText(lower string) bool {
	return containsAny(lower, "quota", "billing", "payment", "credit", "insufficient funds", "account balance")
}

func isModelText(lower string) bool {
	return containsAny(lower, "model not found", "unknown model", "no such model", "does not exist", "invalid model")
}

func isContextOverflowText(lower string) bool {
	if strings.Contains(lower, "too long") && strings.Contains(lower, "token") {
		return true
	}
	return containsAny(lower,
		"context", "too many tokens", "max context", "context length",
		"maximum context", "token limit", "input too long", "prompt too long",
		"context_window", "context window exceeded")
}

// unsupportedParameter detects "parameter X is not supported" style errors
// and returns the normalized parameter name. Only concrete request fields
// qualify here; model-level features (tools, vision, reasoning) fall through
// to their dedicated classes so learning stays precise.
func unsupportedParameter(lower string) (string, bool) {
	if !containsAny(lower, "not supported", "unsupported", "unknown parameter",
		"unrecognized", "unexpected keyword", "extra field", "additional properties",
		"not permitted", "extra inputs", "does not support", "no support for", "not allowed") {
		return "", false
	}
	for _, p := range []string{
		"reasoning_effort",
		"max_completion_tokens", "max_tokens",
		"parallel_tool_calls", "tool_choice",
		"temperature", "top_p", "top_k", "stop", "seed",
		"stream_options", "response_format", "json_schema",
		"frequency_penalty", "presence_penalty", "logprobs",
	} {
		if strings.Contains(lower, p) {
			return p, true
		}
	}
	// No recognized field: let the model-level checkers (tools, vision,
	// reasoning) or the generic invalid-schema fallback handle it.
	return "", false
}

func paramCapability(param string) string {
	switch param {
	case "temperature", "top_p", "top_k", "stop", "seed", "frequency_penalty", "presence_penalty", "logprobs":
		return param
	case "max_tokens", "max_completion_tokens":
		return param
	case "parallel_tool_calls":
		return "parallel_tools"
	case "tool_choice":
		// Ambiguous without the value (auto vs required); stay
		// health-neutral without teaching a possibly wrong capability.
		return ""
	case "reasoning_effort":
		return "reasoning_effort"
	case "response_format":
		return "structured_output"
	case "json_schema":
		return "json_schema"
	case "stream_options":
		return "streaming"
	default:
		return ""
	}
}

func capabilityDenied(lower string) bool {
	return containsAny(lower, "not supported", "unsupported", "not available",
		"not enabled", "does not support", "no support for", "not permitted")
}

func isToolCallingText(lower string) bool {
	return containsAny(lower, "tool", "function call", "function_call") && capabilityDenied(lower)
}

func isReasoningText(lower string) bool {
	return containsAny(lower, "reasoning", "thinking") && capabilityDenied(lower)
}

func isVisionText(lower string) bool {
	return containsAny(lower, "image", "vision", "multimodal") && capabilityDenied(lower)
}

func isStructuredOutputText(lower string) bool {
	return containsAny(lower, "response_format", "json_schema", "structured output", "json mode") && capabilityDenied(lower)
}
