package compat

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// ErrorClass is the structured taxonomy every upstream failure is mapped
// onto (spec section 8). The class decides policy: failover, cooldown,
// capability learning, or caller error - never a blind "model dead".
type ErrorClass string

const (
	ClassAuthError            ErrorClass = "auth_error"
	ClassInvalidKey           ErrorClass = "invalid_key"
	ClassQuotaExhausted       ErrorClass = "quota_exhausted"
	ClassRateLimit            ErrorClass = "rate_limit"
	ClassModelNotFound        ErrorClass = "model_not_found"
	ClassEndpointNotFound     ErrorClass = "endpoint_not_found"
	ClassUnsupportedParameter ErrorClass = "unsupported_parameter"
	ClassUnsupportedTools     ErrorClass = "unsupported_tool_calling"
	ClassUnsupportedReasoning ErrorClass = "unsupported_reasoning"
	ClassUnsupportedVision    ErrorClass = "unsupported_vision"
	ClassContextOverflow      ErrorClass = "context_overflow"
	ClassInvalidRequestSchema ErrorClass = "invalid_request_schema"
	ClassUpstreamOverload     ErrorClass = "upstream_overload"
	ClassUpstreamInternal     ErrorClass = "upstream_internal_error"
	ClassTimeout              ErrorClass = "timeout"
	ClassNetworkError         ErrorClass = "network_error"
	ClassStreamProtocolError  ErrorClass = "stream_protocol_error"
	ClassMalformedResponse    ErrorClass = "malformed_response"
	ClassUnknown              ErrorClass = "unknown"
)

// Classified is the verdict of the error classifier.
type Classified struct {
	Class ErrorClass `json:"class"`
	// Parameter names the offending request field for parameter-family
	// classes (e.g. "temperature", "reasoning_effort", "stream_options").
	Parameter string `json:"parameter,omitempty"`
	// Capability is the contract capability the failure proves something
	// about (e.g. CapTemperature). Empty when unrelated to compatibility.
	Capability string `json:"capability,omitempty"`
	Message    string `json:"message,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
	// CapabilityFailure marks failures that belong to the compatibility
	// plane, not deployment health. Health must remain untouched for these.
	CapabilityFailure bool `json:"capability_failure"`
	// CallerError marks malformed client requests that will fail on every
	// upstream; failover is pointless.
	CallerError bool `json:"caller_error"`
	// RetryableSamePayload marks transient upstream conditions where the
	// identical payload may succeed later (or on another deployment).
	RetryableSamePayload bool `json:"retryable_same_payload"`
}

// capabilityMapping maps offending parameter names onto contract
// capabilities. Keys are lowercase.
var capabilityMapping = map[string]string{
	"temperature":           CapTemperature,
	"top_p":                 CapTopP,
	"top_k":                 CapTopP,
	"stop":                  CapStop,
	"stop_sequences":        CapStop,
	"seed":                  CapSeed,
	"max_tokens":            CapMaxTokens,
	"max_completion_tokens": CapMaxCompletionTokens,
	"max_output_tokens":     CapMaxCompletionTokens,
	"reasoning":             CapReasoning,
	"reasoning_effort":      CapReasoningEffort,
	"reasoning_content":     CapReasoning,
	"thinking":              CapReasoning,
	"thinking_budget":       CapReasoning,
	"stream_options":        CapStreaming,
	"include_usage":         CapStreaming,
	"parallel_tool_calls":   CapParallelToolCalls,
	"tool_choice":           CapToolChoiceAuto,
	"tool":                  CapTools,
	"tools":                 CapTools,
	"functions":             CapTools,
	"function_call":         CapTools,
	"response_format":       CapStructuredOutput,
	"json_schema":           CapJSONSchema,
	"response_schema":       CapJSONSchema,
	"json":                  CapJSONObject,
	"image_url":             CapVision,
	"image":                 CapVision,
	"images":                CapVision,
	"inline_data":           CapVision,
	"system":                CapSystemMessage,
	"system_message":        CapSystemMessage,
	"logit_bias":            "",
	"logprobs":              "",
	"top_logprobs":          "",
	"frequency_penalty":     "",
	"presence_penalty":      "",
}

// parameterPatterns matches provider error phrasings that reject a specific
// request parameter. Ordered: first match wins.
var parameterPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:unknown|unexpected|unrecognized|unsupported|invalid|not supported|does not support|not support|disallowed|forbidden) (?:request )?(?:field|parameter|argument|key|option)s?[:"' ]+['"]?([a-z_][a-z0-9_.]{1,40})`),
	regexp.MustCompile(`(?i)['"]([a-z_][a-z0-9_.]{1,40})['"] is (?:not )?(?:supported|allowed|recognized|valid|available)`),
	regexp.MustCompile(`(?i)(?:parameter|field|option|feature) ['"]?([a-z_][a-z0-9_.]{1,40})['"]? is (?:not )?(?:supported|allowed|recognized|available)`),
	regexp.MustCompile(`(?i)['"]?([a-z_][a-z0-9_.]{1,40})['"]? (?:is|are) not (?:supported|allowed|recognized|available)`),
	regexp.MustCompile(`(?i)does not (?:support|allow|recognize)(?: the)? ['"]?([a-z_][a-z0-9_.]{1,40})['"]?`),
	regexp.MustCompile(`(?i)(?:use|using|set|setting|passing|supplying)['" ]+([a-z_][a-z0-9_.]{1,40})['"]? (?:is not|is currently|aren't) `),
	regexp.MustCompile(`(?i)(temperature|top_p|top_k|stop|seed|max_tokens|max_completion_tokens|max_output_tokens|reasoning_effort|reasoning|thinking|stream_options|include_usage|parallel_tool_calls|tool_choice|tools|functions|function_call|response_format|json_schema|logprobs|logit_bias|frequency_penalty|presence_penalty|user|n)\b[^.]{0,80}(?:is not|are not|cannot|can't|not supported|unsupported|not allowed|unknown|unexpected|unrecognized|invalid)`),
	regexp.MustCompile(`(?i)(?:not supported|unsupported)[^.]{0,40}?['"]?([a-z_][a-z0-9_.]{1,40})['"]?`),
}

// excludeFromParameter prevents matching generic words as parameters.
var excludeFromParameter = map[string]bool{
	"request": true, "response": true, "model": true, "api": true, "key": true,
	"endpoint": true, "provider": true, "value": true, "type": true, "json": false,
	"message": true, "messages": true, "param": true, "params": true, "header": true,
	"account": true, "organization": true, "token": true, "input": true, "output": true,
	"stream": false, "function": false, "tool": false, "image": false, "vision": false,
}

// classPatterns maps error-message phrases onto classes for 400/404/422
// responses where the status alone is too coarse.
var classPatterns = []struct {
	re    *regexp.Regexp
	class ErrorClass
	cap   string
}{
	{regexp.MustCompile(`(?i)(maximum context length|context length|context window|exceed[s]? (?:the )?(?:maximum|context)|too many tokens|input tokens? .*(?:exceed|limit)|prompt is too long|request too large|payload too large)`), ClassContextOverflow, ""},
	{regexp.MustCompile(`(?i)(does not support (?:tool|function)|tool (?:use|calling|calls) (?:is |are )?not supported|tools? (?:is |are )?not supported|function calling (?:is )?not supported|tools is not|tools are not)`), ClassUnsupportedTools, CapTools},
	{regexp.MustCompile(`(?i)(vision|image[s]?|multimodal).{0,40}(?:not supported|unsupported|not enabled|cannot)`), ClassUnsupportedVision, CapVision},
	{regexp.MustCompile(`(?i)(?:reasoning|thinking).{0,40}(?:not supported|unsupported|not enabled)`), ClassUnsupportedReasoning, CapReasoning},
	{regexp.MustCompile(`(?i)(?:response_format|json schema|json_schema|structured output).{0,40}(?:not supported|unsupported)`), ClassUnsupportedParameter, CapStructuredOutput},
	{regexp.MustCompile(`(?i)(no such model|model not found|unknown model|invalid model|model does not exist|is not a valid model|not a valid model id|decommissioned|model not available|does not exist)`), ClassModelNotFound, ""},
	{regexp.MustCompile(`(?i)(not found|does not exist|unknown url|invalid url|no such endpoint|404)`), ClassEndpointNotFound, ""},
}

// ClassifyUpstreamError maps an upstream HTTP failure onto the taxonomy.
// body may be nil (transport errors use status 0 conventions separately).
func ClassifyUpstreamError(status int, body []byte) Classified {
	msg := messageFromBody(body)
	c := Classified{StatusCode: status, Message: truncateMessage(msg)}
	switch status {
	case http.StatusUnauthorized:
		c.Class = ClassAuthError
		if looksLikeInvalidKey(msg) {
			c.Class = ClassInvalidKey
		}
		return c
	case http.StatusForbidden:
		c.Class = ClassAuthError
		if looksLikeInvalidKey(msg) {
			c.Class = ClassInvalidKey
		} else if mentionsQuota(msg) {
			c.Class = ClassQuotaExhausted
			c.RetryableSamePayload = false
		}
		return c
	case http.StatusPaymentRequired:
		c.Class = ClassQuotaExhausted
		return c
	case http.StatusTooManyRequests:
		c.Class = ClassRateLimit
		c.RetryableSamePayload = true
		return c
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		c.Class = ClassTimeout
		c.RetryableSamePayload = true
		return c
	case http.StatusServiceUnavailable, 529:
		c.Class = ClassUpstreamOverload
		c.RetryableSamePayload = true
		return c
	case http.StatusNotFound:
		// A 404 can be a wrong path (endpoint problem) or a model problem.
		if modelMentioned(msg) {
			c.Class = ClassModelNotFound
		} else {
			c.Class = ClassEndpointNotFound
		}
		return c
	}
	if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
		cls := classifyClientError(status, msg)
		if cls.Parameter == "" && len(body) > 0 {
			cls.Parameter = structuredParamFromBody(body)
		}
		return cls
	}
	if status >= 500 {
		c.Class = ClassUpstreamInternal
		c.RetryableSamePayload = true
		return c
	}
	c.Class = ClassUnknown
	return c
}

func classifyClientError(status int, msg string) Classified {
	c := Classified{StatusCode: status, Message: truncateMessage(msg)}
	// Structured OpenAI-style bodies often carry a parameter name.
	if param := parameterFromBody(msg); param != "" {
		c.Parameter = param
	}
	// Context overflow first: it frequently mentions invalid-request shapes.
	if classPatterns[0].re.MatchString(msg) {
		c.Class = ClassContextOverflow
		c.CallerError = true // request itself is too large for this window
		return c
	}
	// Capability-family rejections.
	if classPatterns[2].re.MatchString(msg) {
		c.Class = ClassUnsupportedVision
		c.Capability = CapVision
		c.CapabilityFailure = true
		if c.Parameter == "" {
			c.Parameter = "image"
		}
		return c
	}
	if classPatterns[1].re.MatchString(msg) {
		c.Class = ClassUnsupportedTools
		c.Capability = CapTools
		c.CapabilityFailure = true
		if c.Parameter == "" {
			c.Parameter = "tools"
		}
		return c
	}
	if classPatterns[3].re.MatchString(msg) {
		c.Class = ClassUnsupportedReasoning
		c.Capability = CapReasoning
		c.CapabilityFailure = true
		if c.Parameter == "" {
			c.Parameter = "reasoning"
		}
		return c
	}
	// Parameter-level rejections.
	if c.Parameter != "" && parameterIsCapability(c.Parameter) {
		c.Capability = capabilityMapping[c.Parameter]
		switch c.Capability {
		case CapTools, CapToolChoiceAuto:
			c.Class = ClassUnsupportedTools
		case CapReasoning, CapReasoningEffort:
			c.Class = ClassUnsupportedReasoning
		case CapVision:
			c.Class = ClassUnsupportedVision
		default:
			c.Class = ClassUnsupportedParameter
		}
		c.CapabilityFailure = true
		return c
	}
	if c.Parameter != "" {
		c.Class = ClassUnsupportedParameter
		c.CapabilityFailure = true
		return c
	}
	// Structured payload/schema errors that mention no specific parameter.
	if regexp.MustCompile(`(?i)(invalid (?:request |payload|body|schema)|failed to (?:parse|decode|validate)|schema validation|malformed)`).MatchString(msg) {
		c.Class = ClassInvalidRequestSchema
		c.CallerError = true
		return c
	}
	if classPatterns[5].re.MatchString(msg) {
		c.Class = ClassModelNotFound
		return c
	}
	c.Class = ClassInvalidRequestSchema
	c.CallerError = true
	return c
}

func parameterIsCapability(p string) bool {
	_, ok := capabilityMapping[strings.ToLower(p)]
	return ok
}

func messageFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var env struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err == nil {
		if len(env.Error) > 0 {
			var e struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(env.Error, &e) == nil && e.Message != "" {
				return e.Message
			}
			// Some providers use error as a plain string.
			var s string
			if json.Unmarshal(env.Error, &s) == nil && s != "" {
				return s
			}
		}
		if env.Message != "" {
			return env.Message
		}
	}
	return string(body)
}

func truncateMessage(m string) string {
	m = strings.TrimSpace(m)
	if len(m) > 512 {
		return m[:512]
	}
	return m
}

func looksLikeInvalidKey(msg string) bool {
	return regexp.MustCompile(`(?i)(invalid api key|invalid key|incorrect api key|api key (?:is )?invalid|invalid x-api-key|authentication|unauthorized|invalid authorization|credentials?)`).MatchString(msg)
}

func mentionsQuota(msg string) bool {
	return regexp.MustCompile(`(?i)(quota|billing|credit|balance|exceeded your|payment)`).MatchString(msg)
}

func modelMentioned(msg string) bool {
	return regexp.MustCompile(`(?i)(model|deployment|engine)`).MatchString(msg)
}

// structuredParamFromBody extracts "error.param" / "param" / "parameter" /
// "field" from a raw JSON body without pattern guessing.
func structuredParamFromBody(body []byte) string {
	var env struct {
		Error struct {
			Param string `json:"param"`
		} `json:"error"`
		Param     string `json:"param"`
		Parameter string `json:"parameter"`
		Field     string `json:"field"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	for _, p := range []string{env.Error.Param, env.Param, env.Parameter, env.Field} {
		if p != "" {
			return strings.ToLower(p)
		}
	}
	return ""
}

// parameterFromBody extracts a concrete parameter name from an error message
// using the pattern list.
func parameterFromBody(msg string) string {
	for _, re := range parameterPatterns {
		if m := re.FindStringSubmatch(msg); len(m) >= 2 {
			p := strings.ToLower(strings.TrimPrefix(m[1], "request."))
			p = strings.TrimPrefix(p, "body.")
			p = strings.TrimPrefix(p, "input.")
			p = strings.TrimPrefix(p, "generationconfig.")
			if excludeFromParameter[p] {
				continue
			}
			if len(p) < 2 {
				continue
			}
			return p
		}
	}
	return ""
}

// ClassifyTransportError classifies client-side transport failures.
func ClassifyTransportError(err error) Classified {
	c := Classified{Class: ClassNetworkError, RetryableSamePayload: true}
	if err == nil {
		return c
	}
	msg := err.Error()
	c.Message = truncateMessage(msg)
	switch {
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "deadline exceeded"):
		c.Class = ClassTimeout
	case strings.Contains(msg, "context canceled"):
		c.Class = ClassNetworkError
		c.RetryableSamePayload = false
	case strings.Contains(msg, "connection reset"), strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "EOF"), strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"), strings.Contains(msg, "TLS handshake"):
		c.Class = ClassNetworkError
	}
	return c
}

// ClassifyMalformedResponse marks an upstream 2xx whose body/stream could
// not be decoded (spec: STREAM_PROTOCOL_ERROR / MALFORMED_RESPONSE).
func ClassifyMalformedResponse(detail string) Classified {
	return Classified{
		Class: ClassMalformedResponse, Message: truncateMessage(detail),
		RetryableSamePayload: true,
	}
}

// ClassifyStreamProtocolError marks an upstream SSE stream that violated its
// own protocol (bad JSON frames, missing terminal event, embedded error).
func ClassifyStreamProtocolError(detail string) Classified {
	return Classified{
		Class: ClassStreamProtocolError, Message: truncateMessage(detail),
		RetryableSamePayload: true,
	}
}

// PolicyFromError maps a classified error onto routing/health policy flags
// so every protocol path shares one source of truth. Existing behavior for
// transport/5xx/429/auth is preserved; capability failures are carved out.
type PolicyFromError struct {
	ErrorType            string
	Failover             bool
	QuarantineDeployment bool
	SignalProvider       bool
	HardCooldown         bool
	Repairable           bool
}

// Policy returns the failure policy for a classified upstream error.
func (c Classified) Policy() PolicyFromError {
	if c.CapabilityFailure {
		// The deployment is healthy; the payload needs adaptation.
		return PolicyFromError{ErrorType: string(c.Class), Failover: false, Repairable: true}
	}
	switch c.Class {
	case ClassAuthError, ClassInvalidKey:
		return PolicyFromError{ErrorType: "provider_auth_failed", Failover: true, QuarantineDeployment: true, SignalProvider: true, HardCooldown: true}
	case ClassQuotaExhausted:
		return PolicyFromError{ErrorType: "provider_billing", Failover: true, QuarantineDeployment: true, SignalProvider: true, HardCooldown: true}
	case ClassRateLimit:
		return PolicyFromError{ErrorType: "provider_rate_limited", Failover: true, QuarantineDeployment: true, SignalProvider: true, HardCooldown: true}
	case ClassModelNotFound:
		return PolicyFromError{ErrorType: "provider_model_not_found", Failover: true, QuarantineDeployment: true}
	case ClassEndpointNotFound:
		return PolicyFromError{ErrorType: "provider_endpoint_not_found", Failover: true, QuarantineDeployment: true}
	case ClassTimeout:
		return PolicyFromError{ErrorType: "provider_timeout", Failover: true, QuarantineDeployment: true, SignalProvider: true}
	case ClassUpstreamOverload:
		return PolicyFromError{ErrorType: "provider_overloaded", Failover: true, QuarantineDeployment: true, SignalProvider: true}
	case ClassUpstreamInternal:
		return PolicyFromError{ErrorType: "provider_server_error", Failover: true, QuarantineDeployment: true, SignalProvider: true}
	case ClassContextOverflow:
		// The request exceeds this deployment's window; other deployments
		// with larger windows remain valid targets. Never marks health.
		return PolicyFromError{ErrorType: "context_overflow", Failover: true}
	case ClassInvalidRequestSchema:
		// The caller's payload is broken for every upstream; no failover.
		return PolicyFromError{ErrorType: "caller_invalid_request"}
	case ClassNetworkError:
		return PolicyFromError{ErrorType: "provider_connection_failed", Failover: true, QuarantineDeployment: true, SignalProvider: true}
	default:
		return PolicyFromError{ErrorType: string(c.Class), Failover: true}
	}
}

// HTTPStatus suggests the client-facing status for a classified error.
func (c Classified) HTTPStatus() int {
	switch c.Class {
	case ClassContextOverflow:
		return http.StatusBadRequest
	case ClassRateLimit:
		return http.StatusTooManyRequests
	case ClassUpstreamOverload:
		return http.StatusServiceUnavailable
	default:
		if c.StatusCode > 0 {
			return c.StatusCode
		}
		return http.StatusBadGateway
	}
}

// ParseContextTokens extracts a token count from context-overflow messages
// like "maximum context length is 8192 tokens, however you requested 9000".
func ParseContextTokens(msg string) (limit, requested int) {
	re := regexp.MustCompile(`(\d{2,12})\s*(?:k\+?|k\b)?\s*tokens?`)
	nums := re.FindAllStringSubmatch(msg, -1)
	for _, n := range nums {
		v, err := strconv.Atoi(n[1])
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(n[0]), "k") {
			v *= 1000
		}
		if limit == 0 {
			limit = v
		} else if requested == 0 && v != limit {
			requested = v
		}
	}
	return limit, requested
}

// CapabilityLabel renders the human-facing label of the capability the
// failure belongs to (events + observability UI).
func (c Classified) CapabilityLabel() string {
	if c.Capability == "" && c.Parameter == "" {
		return string(c.Class)
	}
	if c.Capability != "" && c.Capability != c.Parameter {
		return c.Capability + "/" + c.Parameter
	}
	if c.Capability != "" {
		return c.Capability
	}
	return c.Parameter
}
