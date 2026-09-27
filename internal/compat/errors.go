package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// ErrorClass is the structured taxonomy every upstream failure is mapped
// onto. The class decides failover, cooldown, capability learning, or caller
// error policy; HTTP status alone is never treated as sufficient evidence.
type ErrorClass string

const (
	ClassAuthError                   ErrorClass = "auth_error"
	ClassCredentialFailure           ErrorClass = "credential_failure"
	ClassInvalidKey                  ErrorClass = "invalid_key"
	ClassQuotaExhausted               ErrorClass = "quota_exhausted"
	ClassBillingCreditExhausted       ErrorClass = "billing_credit_exhausted"
	ClassRateLimit                    ErrorClass = "rate_limit"
	ClassModelRetired                 ErrorClass = "model_retired"
	ClassModelEOL                     ErrorClass = ClassModelRetired
	ClassModelTemporarilyUnavailable  ErrorClass = "model_temporarily_unavailable"
	ClassModelUnavailable             ErrorClass = ClassModelTemporarilyUnavailable
	ClassModelNotFound                ErrorClass = "model_not_found"
	ClassEndpointNotFound             ErrorClass = "endpoint_not_found"
	ClassUnsupportedParameter         ErrorClass = "unsupported_parameter"
	ClassUnsupportedTools             ErrorClass = "unsupported_tool_calling"
	ClassUnsupportedReasoning         ErrorClass = "unsupported_reasoning"
	ClassUnsupportedVision            ErrorClass = "unsupported_vision"
	ClassContextOverflow              ErrorClass = "context_overflow"
	ClassInvalidRequestSchema         ErrorClass = "invalid_request_schema"
	ClassCallerInvalidRequest         ErrorClass = ClassInvalidRequestSchema
	ClassUpstreamOverload             ErrorClass = "upstream_overload"
	ClassUpstreamInternal             ErrorClass = "upstream_internal_error"
	ClassTimeout                      ErrorClass = "timeout"
	ClassNetworkError                 ErrorClass = "network_error"
	ClassStreamProtocolError          ErrorClass = "stream_protocol_error"
	ClassMalformedResponse            ErrorClass = "malformed_response"
	ClassUnknown                      ErrorClass = "unknown"
)

// Classified is the verdict of the error classifier. Message is bounded and
// retained for in-process diagnosis; it must not be copied into client errors
// or privacy-sensitive events. Code, Type and Parameter are structured
// provider fields and are safe to expose after normal event-plane bounds.
type Classified struct {
	Class ErrorClass `json:"class"`
	Code  string     `json:"provider_code,omitempty"`
	Type  string     `json:"provider_type,omitempty"`
	// Parameter names the offending request field for parameter-family
	// classes (e.g. "temperature", "reasoning_effort", "stream_options").
	Parameter string `json:"parameter,omitempty"`
	// Capability is the contract capability the failure proves something
	// about (e.g. CapTemperature). Empty when unrelated to compatibility.
	Capability string `json:"capability,omitempty"`
	Message    string `json:"message,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
	// CapabilityFailure marks a compatibility fact, not deployment health.
	CapabilityFailure bool `json:"capability_failure"`
	// CallerError marks an invalid ingress payload that will fail on every
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

var (
	contextOverflowPattern = regexp.MustCompile(`(?i)(maximum context length|context length|context window|exceed[s]? (?:the )?(?:maximum|context)|too many tokens|input tokens? .*(?:exceed|limit)|prompt is too long|request too large|payload too large)`)
	modelRetiredPattern    = regexp.MustCompile(`(?i)(end[ -]of[ -](?:life|support)|\beol\b|retired|retirement|decommissioned|discontinued|sunset|withdrawn|deprecated.{0,48}(?:removed|unavailable)|permanently.{0,32}(?:unavailable|removed|disabled)|no longer (?:available|supported))`)
	modelTempPattern       = regexp.MustCompile(`(?i)(model.{0,100}(?:currently|temporarily) unavailable|(?:currently|temporarily) unavailable.{0,100}model|model is unavailable|model unavailable)`)
	modelNotFoundPattern   = regexp.MustCompile(`(?i)(no such model|model not found|unknown model|invalid model|model does not exist|is not a valid model|not a valid model id|model not available|model .{0,80}does not exist)`)
	unsupportedPattern     = regexp.MustCompile(`(?i)(unsupported|not supported|does not support|doesn't support|unknown parameter|unknown field|unrecognized parameter|unrecognized field|not allowed|not recognized)`)
	invalidSchemaPattern   = regexp.MustCompile(`(?i)(invalid (?:request |payload|body|schema)|failed to (?:parse|decode|validate)|schema validation|malformed|invalid tool[_ -]?(?:use[_ -]?)?id|tool result identity)`)
)

type errorEvidence struct {
	Code       string
	Type       string
	Param      string
	Message    string
	Detail     string
	Title      string
	BodyStatus int
}

func decodeStringField(m map[string]json.RawMessage, name string) string {
	raw := m[name]
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

func errorObject(body []byte) map[string]json.RawMessage {
	var out map[string]json.RawMessage
	if json.Unmarshal(body, &out) == nil && out != nil {
		return out
	}
	// Probe adapters and a few providers prefix JSON with a status string. Try
	// the first JSON object without trusting or retaining the prefix.
	start := bytes.IndexByte(body, '{')
	if start >= 0 {
		var candidate map[string]json.RawMessage
		if json.Unmarshal(bytes.TrimSpace(body[start:]), &candidate) == nil && candidate != nil {
			return candidate
		}
		if end := bytes.LastIndexByte(body, '}'); end > start {
			if json.Unmarshal(body[start:end+1], &candidate) == nil && candidate != nil {
				return candidate
			}
		}
	}
	return nil
}

func genericEnvelopeToken(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "error", "api_error", "http_error", "about:blank", "problem":
		return true
	default:
		return false
	}
}

func parseErrorEvidence(body []byte) errorEvidence {
	e := errorEvidence{}
	root := errorObject(body)
	if root == nil {
		e.Message = string(body)
		return e
	}
	e.Code = decodeStringField(root, "code")
	e.Type = decodeStringField(root, "type")
	e.Param = decodeStringField(root, "param")
	e.Message = decodeStringField(root, "message")
	e.Detail = decodeStringField(root, "detail")
	e.Title = decodeStringField(root, "title")
	if status := decodeStringField(root, "status"); status != "" {
		e.BodyStatus, _ = strconv.Atoi(status)
	}
	if len(root["error"]) > 0 && !bytes.Equal(bytes.TrimSpace(root["error"]), []byte("null")) {
		var nested map[string]json.RawMessage
		if json.Unmarshal(root["error"], &nested) == nil && nested != nil {
			nestedCode := decodeStringField(nested, "code")
			if nestedCode != "" && (e.Code == "" || genericEnvelopeToken(e.Code)) {
				e.Code = nestedCode
			}
			nestedType := decodeStringField(nested, "type")
			if nestedType != "" && (e.Type == "" || genericEnvelopeToken(e.Type)) {
				e.Type = nestedType
			}
			if e.Param == "" {
				e.Param = decodeStringField(nested, "param")
			}
			if e.Param == "" {
				e.Param = decodeStringField(nested, "parameter")
			}
			if e.Param == "" {
				e.Param = decodeStringField(nested, "field")
			}
			if e.Code == "" || genericEnvelopeToken(e.Code) {
				if code := decodeStringField(nested, "error_code"); code != "" {
					e.Code = code
				}
			}
			if e.Message == "" {
				e.Message = decodeStringField(nested, "message")
			}
			if e.Detail == "" {
				e.Detail = decodeStringField(nested, "detail")
			}
			if e.Title == "" {
				e.Title = decodeStringField(nested, "title")
			}
		} else {
			var plain string
			if json.Unmarshal(root["error"], &plain) == nil && e.Message == "" {
				e.Message = plain
			}
		}
	}
	if e.Param == "" {
		e.Param = decodeStringField(root, "parameter")
	}
	if e.Param == "" {
		e.Param = decodeStringField(root, "field")
	}
	if e.Code == "" {
		e.Code = decodeStringField(root, "error_code")
	}
	return e
}

func (e errorEvidence) text() string {
	parts := []string{e.Code, e.Type, e.Param, e.Message, e.Detail, e.Title}
	var nonempty []string
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			nonempty = append(nonempty, part)
		}
	}
	return strings.Join(nonempty, " ")
}

func normalizedCode(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.ReplaceAll(v, "-", "_")
	v = strings.ReplaceAll(v, " ", "_")
	return v
}

func safeParameter(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if len(v) == 0 || len(v) > 64 {
		return ""
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' && r != '.' && r != '-' {
			return ""
		}
	}
	return v
}

func codeHas(code string, values ...string) bool {
	for _, value := range values {
		if code == value || strings.Contains(code, value) {
			return true
		}
	}
	return false
}

func structuredClass(e errorEvidence, status int) (ErrorClass, bool) {
	code, typ := normalizedCode(e.Code), normalizedCode(e.Type)
	text := e.text()
	modelEvidence := modelMentioned(text) || strings.HasPrefix(code, "model_") || strings.HasPrefix(typ, "model_")

	if codeHas(code, "model_retired", "model_eol", "model_end_of_life", "model_end_of_support", "model_decommissioned", "model_discontinued", "model_sunset", "model_withdrawn", "model_gone") ||
		codeHas(typ, "model_retired", "model_eol", "model_end_of_life", "model_end_of_support", "model_decommissioned", "model_discontinued", "model_sunset", "model_withdrawn") {
		return ClassModelRetired, true
	}
	if modelEvidence && modelRetiredPattern.MatchString(text) {
		return ClassModelRetired, true
	}
	if codeHas(code, "model_unavailable", "model_temporarily_unavailable") ||
		codeHas(typ, "model_unavailable", "model_temporarily_unavailable") {
		return ClassModelTemporarilyUnavailable, true
	}
	if modelEvidence && modelTempPattern.MatchString(text) {
		return ClassModelTemporarilyUnavailable, true
	}
	if codeHas(code, "model_not_found", "unknown_model", "invalid_model") ||
		codeHas(typ, "model_not_found", "unknown_model") || modelNotFoundPattern.MatchString(text) {
		return ClassModelNotFound, true
	}
	if codeHas(code, "insufficient_quota", "quota_exceeded", "quota_exhausted", "quota_limit") ||
		codeHas(typ, "insufficient_quota", "quota_exceeded") {
		return ClassQuotaExhausted, true
	}
	if codeHas(code, "insufficient_credit", "credit_exhausted", "credits_exhausted", "billing", "payment_required", "payment_failed", "balance_exhausted") ||
		codeHas(typ, "billing", "payment_required", "credit_exhausted") {
		return ClassBillingCreditExhausted, true
	}
	lowerText := strings.ToLower(text)
	if strings.Contains(lowerText, "insufficient quota") || strings.Contains(lowerText, "quota exhausted") || strings.Contains(lowerText, "quota exceeded") {
		return ClassQuotaExhausted, true
	}
	if regexp.MustCompile(`(?i)(insufficient credits?|credits? exhausted|credit balance|billing|payment required|payment failed|no credits?)`).MatchString(text) {
		return ClassBillingCreditExhausted, true
	}
	if codeHas(code, "rate_limit", "too_many_requests", "rate_limited") ||
		codeHas(typ, "rate_limit", "too_many_requests") {
		return ClassRateLimit, true
	}
	if codeHas(code, "invalid_api_key", "invalid_key", "invalid_credential", "credential_invalid", "credential_rejected", "authentication_error", "unauthorized") ||
		codeHas(typ, "authentication_error", "credential_error", "unauthorized") {
		if codeHas(code, "invalid_api_key", "invalid_key") || looksLikeInvalidKey(text) {
			return ClassInvalidKey, true
		}
		return ClassCredentialFailure, true
	}
	if codeHas(code, "context_length_exceeded", "context_window_exceeded", "context_overflow", "max_context_length") ||
		codeHas(typ, "context_length_exceeded", "context_overflow") || contextOverflowPattern.MatchString(text) {
		return ClassContextOverflow, true
	}
	if codeHas(code, "unsupported_tools", "tool_not_supported", "unsupported_tool_calling") ||
		codeHas(typ, "unsupported_tools", "tool_not_supported") {
		return ClassUnsupportedTools, true
	}
	if codeHas(code, "unsupported_vision", "vision_not_supported", "unsupported_image") {
		return ClassUnsupportedVision, true
	}
	if codeHas(code, "unsupported_reasoning", "reasoning_not_supported") {
		return ClassUnsupportedReasoning, true
	}
	if codeHas(code, "unsupported_parameter", "unknown_parameter", "unsupported_field", "unknown_field") ||
		codeHas(typ, "unsupported_parameter", "unsupported_field") {
		return ClassUnsupportedParameter, true
	}
	if codeHas(code, "endpoint_not_found", "route_not_found", "invalid_endpoint") ||
		codeHas(typ, "endpoint_not_found") {
		return ClassEndpointNotFound, true
	}
	if status == http.StatusNotFound && modelEvidence {
		return ClassModelNotFound, true
	}
	return ClassUnknown, false
}

// ClassifyUpstreamError maps an upstream HTTP failure onto the taxonomy. It
// understands OpenAI-style envelopes and RFC 7807/problem-details bodies.
func ClassifyUpstreamError(status int, body []byte) Classified {
	e := parseErrorEvidence(body)
	if e.BodyStatus > 0 && status == 0 {
		status = e.BodyStatus
	}
	text := e.text()
	msg := e.Message
	if msg == "" {
		msg = e.Detail
	}
	if msg == "" {
		msg = e.Title
	}
	if msg == "" {
		msg = string(body)
	}
	c := Classified{
		StatusCode: status, Message: truncateMessage(msg), Code: truncateMessage(e.Code),
		Type: truncateMessage(e.Type), Parameter: safeParameter(e.Param),
	}

	if class, ok := structuredClass(e, status); ok {
		c.Class = class
		applyCapabilityClass(&c, e.Param, text)
		setClassRetryable(&c)
		return c
	}

	switch status {
	case http.StatusUnauthorized:
		c.Class = ClassAuthError
		if looksLikeInvalidKey(text) {
			c.Class = ClassInvalidKey
		}
	case http.StatusForbidden:
		c.Class = ClassAuthError
		if looksLikeInvalidKey(text) {
			c.Class = ClassInvalidKey
		}
	case http.StatusPaymentRequired:
		if regexp.MustCompile(`(?i)(quota|request limit|token limit)`).MatchString(text) {
			c.Class = ClassQuotaExhausted
		} else {
			c.Class = ClassBillingCreditExhausted
		}
	case http.StatusTooManyRequests:
		c.Class = ClassRateLimit
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		c.Class = ClassTimeout
	case http.StatusServiceUnavailable, 529:
		c.Class = ClassUpstreamOverload
	case http.StatusNotFound:
		if modelMentioned(text) || modelNotFoundPattern.MatchString(text) {
			c.Class = ClassModelNotFound
		} else {
			c.Class = ClassEndpointNotFound
		}
	case http.StatusGone:
		// 410 is permanent only when provider evidence identifies a retired
		// model. A removed route/endpoint is not a model lifecycle event.
		c.Class = ClassEndpointNotFound
	default:
		if status >= 500 {
			c.Class = ClassUpstreamInternal
			c.RetryableSamePayload = true
			return c
		}
	}
	if c.Class != "" {
		setClassRetryable(&c)
		return c
	}
	if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
		return classifyClientError(status, e, text)
	}
	c.Class = ClassUnknown
	return c
}

func setClassRetryable(c *Classified) {
	switch c.Class {
	case ClassRateLimit, ClassTimeout, ClassNetworkError, ClassUpstreamOverload, ClassUpstreamInternal,
		ClassModelTemporarilyUnavailable, ClassModelRetired, ClassModelNotFound, ClassEndpointNotFound,
		ClassQuotaExhausted, ClassBillingCreditExhausted, ClassAuthError, ClassCredentialFailure, ClassInvalidKey:
		c.RetryableSamePayload = true
	}
}

func applyCapabilityClass(c *Classified, param, text string) {
	p := safeParameter(param)
	if p == "" && strings.TrimSpace(param) == "" {
		p = parameterFromBody(text)
	}
	if p != "" {
		c.Parameter = p
	}
	switch c.Class {
	case ClassUnsupportedTools:
		c.Capability = CapTools
		c.CapabilityFailure = true
		if c.Parameter == "" {
			c.Parameter = "tools"
		}
	case ClassUnsupportedVision:
		c.Capability = CapVision
		c.CapabilityFailure = true
		if c.Parameter == "" {
			c.Parameter = "image"
		}
	case ClassUnsupportedReasoning:
		c.Capability = CapReasoning
		c.CapabilityFailure = true
		if c.Parameter == "" {
			c.Parameter = "reasoning"
		}
	case ClassUnsupportedParameter:
		if cap, ok := capabilityMapping[c.Parameter]; ok {
			c.Capability = cap
		}
		c.CapabilityFailure = true
	}
}

func classifyClientError(status int, e errorEvidence, text string) Classified {
	c := Classified{StatusCode: status, Message: truncateMessage(firstNonempty(e.Message, e.Detail, e.Title, text)),
		Code: truncateMessage(e.Code), Type: truncateMessage(e.Type), Parameter: safeParameter(e.Param)}
	if contextOverflowPattern.MatchString(text) {
		c.Class = ClassContextOverflow
		c.CallerError = true // other deployments may have a larger context
		return c
	}
	if classPatterns[1].re.MatchString(text) || regexp.MustCompile(`(?i)(tool[_ -]?use|function[_ -]?calling).{0,40}(?:not supported|unsupported|does not support)`).MatchString(text) {
		c.Class = ClassUnsupportedTools
		applyCapabilityClass(&c, "tools", text)
		return c
	}
	if classPatterns[2].re.MatchString(text) {
		c.Class = ClassUnsupportedVision
		applyCapabilityClass(&c, "image", text)
		return c
	}
	if classPatterns[3].re.MatchString(text) {
		c.Class = ClassUnsupportedReasoning
		applyCapabilityClass(&c, "reasoning", text)
		return c
	}
	if classPatterns[4].re.MatchString(text) {
		c.Class = ClassUnsupportedParameter
		c.Capability = CapStructuredOutput
		c.CapabilityFailure = true
		return c
	}
	if modelRetiredPattern.MatchString(text) && modelMentioned(text) {
		c.Class = ClassModelRetired
		c.RetryableSamePayload = true
		return c
	}
	if modelTempPattern.MatchString(text) && modelMentioned(text) {
		c.Class = ClassModelTemporarilyUnavailable
		c.RetryableSamePayload = true
		return c
	}
	if modelNotFoundPattern.MatchString(text) {
		c.Class = ClassModelNotFound
		return c
	}

	param := safeParameter(e.Param)
	if param == "" {
		param = parameterFromBody(text)
	}
	if param != "" {
		c.Parameter = param
	}
	if unsupportedPattern.MatchString(text) {
		c.Class = ClassUnsupportedParameter
		applyCapabilityClass(&c, param, text)
		return c
	}
	if e.Code != "" && codeHas(normalizedCode(e.Code), "unsupported_parameter", "unknown_parameter", "unsupported_field", "unknown_field") {
		c.Class = ClassUnsupportedParameter
		applyCapabilityClass(&c, param, text)
		return c
	}
	if invalidSchemaPattern.MatchString(text) || classPatterns[5].re.MatchString(text) {
		c.Class = ClassInvalidRequestSchema
		c.CallerError = true
		return c
	}
	c.Class = ClassInvalidRequestSchema
	c.CallerError = true
	return c
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func parameterIsCapability(p string) bool {
	_, ok := capabilityMapping[strings.ToLower(p)]
	return ok
}

func messageFromBody(body []byte) string {
	e := parseErrorEvidence(body)
	return firstNonempty(e.Message, e.Detail, e.Title, string(body))
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
	e := parseErrorEvidence(body)
	return safeParameter(e.Param)
}

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
	c.Message = truncateMessage(err.Error())
	lower := strings.ToLower(err.Error())
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(lower, "context deadline exceeded"), strings.Contains(lower, "deadline exceeded"):
		c.Class = ClassTimeout
	case errors.Is(err, context.Canceled), strings.Contains(lower, "context canceled"):
		c.Class = ClassNetworkError
		c.RetryableSamePayload = false
		return c
	case strings.Contains(lower, "all configured provider credentials are cooling down"),
		strings.Contains(lower, "credential rejected http 429"), strings.Contains(lower, "rate limit"):
		c.Class = ClassRateLimit
	case strings.Contains(lower, "credential rejected http 401"), strings.Contains(lower, "credential rejected http 403"),
		strings.Contains(lower, "no usable credentials"):
		c.Class = ClassCredentialFailure
	case errors.As(err, &netErr) && netErr.Timeout():
		c.Class = ClassTimeout
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		strings.Contains(lower, "connection reset"), strings.Contains(lower, "broken pipe"),
		strings.Contains(lower, "unexpected eof"), strings.Contains(lower, " eof"),
		strings.Contains(lower, "connection refused"), strings.Contains(lower, "no such host"),
		strings.Contains(lower, "tls handshake"):
		c.Class = ClassNetworkError
	}
	setClassRetryable(&c)
	return c
}

func ClassifyMalformedResponse(detail string) Classified {
	return Classified{Class: ClassMalformedResponse, Message: truncateMessage(detail), RetryableSamePayload: true}
}

func ClassifyStreamProtocolError(detail string) Classified {
	return Classified{Class: ClassStreamProtocolError, Message: truncateMessage(detail), RetryableSamePayload: true}
}

type PolicyFromError struct {
	ErrorType            string
	Failover             bool
	QuarantineDeployment bool
	SignalProvider       bool
	HardCooldown         bool
	Repairable           bool
	RetireDeployment     bool
}

func (c Classified) Policy() PolicyFromError {
	if c.CapabilityFailure {
		// A safe repair is attempted on this deployment first. If semantics
		// forbid repair or the repair fails, the next compatible deployment is
		// still eligible for the same bounded request.
		return PolicyFromError{ErrorType: string(c.Class), Failover: true, Repairable: true}
	}
	switch c.Class {
	case ClassModelRetired:
		return PolicyFromError{ErrorType: string(ClassModelRetired), Failover: true, RetireDeployment: true}
	case ClassModelTemporarilyUnavailable:
		return PolicyFromError{ErrorType: string(ClassModelTemporarilyUnavailable), Failover: true, QuarantineDeployment: true}
	case ClassAuthError, ClassCredentialFailure, ClassInvalidKey:
		return PolicyFromError{ErrorType: "provider_auth_failed", Failover: true, QuarantineDeployment: true, SignalProvider: true, HardCooldown: true}
	case ClassQuotaExhausted:
		return PolicyFromError{ErrorType: string(ClassQuotaExhausted), Failover: true, QuarantineDeployment: true, SignalProvider: true, HardCooldown: true}
	case ClassBillingCreditExhausted:
		return PolicyFromError{ErrorType: string(ClassBillingCreditExhausted), Failover: true, QuarantineDeployment: true, SignalProvider: true, HardCooldown: true}
	case ClassRateLimit:
		return PolicyFromError{ErrorType: "provider_rate_limited", Failover: true, QuarantineDeployment: true, SignalProvider: true, HardCooldown: true}
	case ClassModelNotFound:
		return PolicyFromError{ErrorType: string(ClassModelNotFound), Failover: true, QuarantineDeployment: true}
	case ClassEndpointNotFound:
		return PolicyFromError{ErrorType: string(ClassEndpointNotFound), Failover: true, QuarantineDeployment: true, SignalProvider: true}
	case ClassTimeout:
		return PolicyFromError{ErrorType: "provider_timeout", Failover: true, QuarantineDeployment: true, SignalProvider: true}
	case ClassUpstreamOverload:
		return PolicyFromError{ErrorType: "provider_overloaded", Failover: true, QuarantineDeployment: true, SignalProvider: true}
	case ClassUpstreamInternal:
		return PolicyFromError{ErrorType: "provider_server_error", Failover: true, QuarantineDeployment: true, SignalProvider: true}
	case ClassContextOverflow:
		return PolicyFromError{ErrorType: string(ClassContextOverflow), Failover: true}
	case ClassInvalidRequestSchema:
		return PolicyFromError{ErrorType: "caller_invalid_request"}
	case ClassNetworkError:
		if !c.RetryableSamePayload {
			return PolicyFromError{ErrorType: "caller_cancelled"}
		}
		return PolicyFromError{ErrorType: "provider_connection_failed", Failover: true, QuarantineDeployment: true, SignalProvider: true}
	case ClassMalformedResponse, ClassStreamProtocolError:
		return PolicyFromError{ErrorType: string(c.Class), Failover: true, QuarantineDeployment: true, SignalProvider: true}
	default:
		return PolicyFromError{ErrorType: string(c.Class), Failover: true}
	}
}

func (c Classified) HTTPStatus() int {
	switch c.Class {
	case ClassInvalidRequestSchema, ClassContextOverflow:
		return http.StatusBadRequest
	case ClassRateLimit:
		return http.StatusTooManyRequests
	case ClassAuthError, ClassCredentialFailure, ClassInvalidKey:
		if c.StatusCode == http.StatusForbidden {
			return http.StatusForbidden
		}
		return http.StatusUnauthorized
	case ClassQuotaExhausted:
		return http.StatusTooManyRequests
	case ClassBillingCreditExhausted:
		return http.StatusPaymentRequired
	case ClassModelNotFound, ClassEndpointNotFound:
		return http.StatusNotFound
	case ClassModelRetired, ClassModelTemporarilyUnavailable, ClassUpstreamOverload:
		return http.StatusServiceUnavailable
	case ClassTimeout:
		return http.StatusGatewayTimeout
	case ClassNetworkError, ClassMalformedResponse, ClassStreamProtocolError:
		return http.StatusBadGateway
	case ClassUpstreamInternal:
		if c.StatusCode >= 500 && c.StatusCode <= 599 {
			return c.StatusCode
		}
		return http.StatusBadGateway
	default:
		if c.StatusCode >= 400 && c.StatusCode <= 599 {
			return c.StatusCode
		}
		return http.StatusBadGateway
	}
}

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
