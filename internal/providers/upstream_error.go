package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Canonical upstream logical-error detection shared by the adapter
// (credential outcomes), probes, provider tests, and the data plane
// (deployment outcomes).
//
// HTTP status alone is not enough: several providers and OpenAI-compatible
// proxies answer HTTP 200 with an error envelope - or even with a
// valid-looking completion whose content is a paywall/quota message (for
// example Pollinations answers 200 with choices[0].message.content set to
// "The account behind this API key doesn't have enough credits...").
// Treating those as success would mark dead deployments healthy, pin
// sessions to them, reset credential-failure state, and forward paywall
// text to clients as if it were a completion.
//
// The classifier is deliberately conservative in the success direction:
// unknown shapes are NOT flagged, so a provider that invents a new success
// field can never be misclassified. Only documented error envelopes and
// well-evidenced paywall/auth/throttle phrases are detected.

type UpstreamErrorClass string

const (
	UpstreamOK         UpstreamErrorClass = ""
	UpstreamAuth       UpstreamErrorClass = "auth"
	UpstreamQuota      UpstreamErrorClass = "quota"
	UpstreamRateLimit  UpstreamErrorClass = "rate_limit"
	UpstreamNotFound   UpstreamErrorClass = "not_found"
	UpstreamInvalid    UpstreamErrorClass = "invalid"
	UpstreamOverloaded UpstreamErrorClass = "overloaded"
	UpstreamServer     UpstreamErrorClass = "server"
	UpstreamBadPayload UpstreamErrorClass = "bad_payload"
)

// KeyScoped reports whether the failure is most likely tied to the single
// credential that served the request (bad key, exhausted credits, per-key
// throttle) rather than to the deployment as a whole. Key-scoped failures
// cool down only that credential so sibling keys stay usable.
func (c UpstreamErrorClass) KeyScoped() bool {
	return c == UpstreamAuth || c == UpstreamQuota || c == UpstreamRateLimit
}

// CooldownStatus maps a key-scoped class onto the credential-cooldown bucket
// with the closest semantics (402 quota -> one hour, 429 throttle ->
// Retry-After bounded, 401 auth -> fifteen minutes).
func (c UpstreamErrorClass) CooldownStatus() int {
	switch c {
	case UpstreamQuota:
		return 402
	case UpstreamRateLimit:
		return 429
	default:
		return 401
	}
}

type UpstreamLogicalError struct {
	Status  int
	Class   UpstreamErrorClass
	Code    string
	Message string
}

func (e *UpstreamLogicalError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("upstream %s error (http %d): %s", e.Class, e.Status, e.Message)
	}
	return fmt.Sprintf("upstream %s error (http %d)", e.Class, e.Status)
}

const (
	maxUpstreamErrorMessage = 300
	// contentHeadWindow bounds paywall sniffing to the start of a
	// completion. Providers inject paywall/quota text as the whole reply, so
	// it always starts at offset zero; a genuine completion that merely
	// *discusses* quota wording later must not match. The window still
	// covers short prefixes such as "Error: ...".
	contentHeadWindow = 120
	// maxSniffedContent caps extracted completion text. Paywall signals sit
	// at the start, so anything beyond the window is never inspected.
	maxSniffedContent = 512
)

// signalLists match lowercase substrings inside provider error text. They
// are consulted for explicit error envelopes/messages (match anywhere) and,
// via headSignalLists, for completion content (match near the start only).
var quotaSignals = []string{
	"insufficient_quota", "insufficient quota",
	"insufficient_credits", "insufficient credits",
	"insufficient_balance", "insufficient balance",
	"insufficient funds", "insufficient account funds",
	"doesn't have enough credits", "does not have enough credits",
	"do not have enough credits", "don't have enough credits", "not enough credits",
	"exceeded your current quota", "exceeded current quota",
	"quota exceeded", "quota_exceeded", "quota exhausted", "exhausted quota",
	"out of credits", "no credits", "zero credits", "no remaining credits",
	"requires more credits", "can only afford", "usage balance exhausted",
	"balance exhausted", "credit balance", "negative credit",
	"credit_limit", "credit limit", "spend cap", "spend limit", "monthly spend",
	"needs paid", "requires paid", "paid pollen", "requires payment",
	"top up", "top-up", "topup", "add credits", "purchase credits",
	"buy credits", "add to credit", "upgrade to a paid", "add funds",
	"payment required", "payment_required", "billing", "arrears",
	"past due", "delinquent", "unpaid",
}

var authSignals = []string{
	"invalid_api_key", "invalid api key", "invalid x-api-key", "invalid x_api_key",
	"invalid key", "incorrect api key", "invalid_api", "invalid token",
	"authentication_error", "authentication failed", "auth failed",
	"unauthorized", "unauthenticated",
	"key was disabled", "key_disabled", "key disabled", "api key disabled",
	"revoked", "expired key", "key expired", "key is invalid", "key is incorrect",
	"key is expired", "key is revoked", "key is missing", "key is required",
	"permission_error", "permission denied", "permissiondenied",
	"forbidden", "access denied", "not allowed", "not authorized",
	"not permitted", "no permission",
	"api key required", "api key missing", "missing api key", "no api key",
	"provide an api key", "api-key required",
}

var rateSignals = []string{
	"rate_limit", "rate limit", "rate limited",
	"too many requests", "throttl", "concurrency limit",
	"too many concurrent", "rpm exceeded", "tpm exceeded",
	"requests per minute", "tokens per minute",
	"slow down", "slowdown", "try again in", "retry in",
}

var overloadSignals = []string{
	"overloaded_error", "overloaded", "over capacity", "at capacity",
	"capacity exceeded", "no capacity", "server is busy",
	"temporarily unavailable",
}

var notFoundSignals = []string{
	"not_found", "not found", "model_not_found", "does not exist",
	"no such", "unknown model", "invalid model", "model not",
	"deployment not found", "endpoint not found", "unexpected endpoint",
	"unknown endpoint",
}

var invalidSignals = []string{
	"invalid_request", "invalid request", "bad request",
	"validation", "invalid parameter", "invalid_param",
	"missing parameter", "required parameter",
	"too long", "too large", "context length", "maximum context",
	"max_tokens", "content policy", "content_policy", "moderation",
	"filtered", "unsupported", "not supported",
	"invalid json", "malformed", "parse error", "schema",
}

// headSignalLists is the conservative subset used for completion-content
// sniffing. Bare generic words ("billing", "quota", "throttl") are excluded:
// only distinctive phrases match, and only near the start of the reply.
var headSignalLists = []struct {
	class   UpstreamErrorClass
	phrases []string
}{
	{UpstreamQuota, []string{
		"doesn't have enough credits", "does not have enough credits",
		"don't have enough credits", "not enough credits",
		"insufficient_quota", "insufficient quota",
		"insufficient credits", "insufficient_credits",
		"insufficient balance", "insufficient_balance",
		"insufficient funds", "insufficient account",
		"exceeded your current quota", "exceeded current quota",
		"quota exceeded", "quota_exceeded",
		"out of credits", "no credits", "zero credits",
		"requires more credits", "can only afford",
		"usage balance exhausted", "needs paid", "requires paid",
		"payment required", "add credits", "purchase credits",
		"credit balance", "negative credit",
	}},
	{UpstreamAuth, []string{
		"invalid api key", "invalid_api_key", "incorrect api key",
		"invalid x-api-key", "authentication failed", "authentication_error",
		"unauthorized", "key was disabled", "permission denied", "access denied",
	}},
	{UpstreamRateLimit, []string{
		"rate limit", "rate_limit", "too many requests", "throttled",
	}},
	{UpstreamOverloaded, []string{
		"overloaded", "at capacity", "over capacity",
	}},
}

// topUpContextWords gates the short "top up" family: the phrase only counts
// as a quota signal when a billing-context word appears in the same head
// window, so "top up your coffee" never matches.
var topUpPhrases = []string{"top up", "top-up", "topup", "upgrade to a paid"}
var topUpContextWords = []string{
	"credit", "balance", "quota", "billing", "payment", "pollen",
	"account", "purchas", "subscrib", "plan",
}

func containsAny(hay string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(hay, n) {
			return true
		}
	}
	return false
}

// classifyErrorText maps provider error code/message text onto a failure
// class. It returns UpstreamOK when nothing matches; callers fall back to
// status-based classification then.
func classifyErrorText(code, message string) UpstreamErrorClass {
	hay := strings.ToLower(strings.TrimSpace(code + " " + message))
	if hay == "" {
		return UpstreamOK
	}
	// Quota first: a 429 carrying insufficient_quota is account state, not a
	// transient throttle, and must not be retried like one.
	if containsAny(hay, quotaSignals) {
		return UpstreamQuota
	}
	if containsAny(hay, authSignals) {
		return UpstreamAuth
	}
	if containsAny(hay, rateSignals) {
		return UpstreamRateLimit
	}
	if containsAny(hay, overloadSignals) {
		return UpstreamOverloaded
	}
	if containsAny(hay, notFoundSignals) {
		return UpstreamNotFound
	}
	if containsAny(hay, invalidSignals) {
		return UpstreamInvalid
	}
	return UpstreamOK
}

func classForStatus(status int) UpstreamErrorClass {
	switch status {
	case 401, 403:
		return UpstreamAuth
	case 402:
		return UpstreamQuota
	case 404:
		return UpstreamNotFound
	case 429:
		return UpstreamRateLimit
	case 400, 405, 413, 415, 422:
		return UpstreamInvalid
	case 529:
		return UpstreamOverloaded
	default:
		if status >= 500 {
			return UpstreamServer
		}
		if status >= 400 {
			return UpstreamInvalid
		}
		return UpstreamServer
	}
}

func boundMessage(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxUpstreamErrorMessage {
		s = s[:maxUpstreamErrorMessage] + "…"
	}
	return s
}

func jsonString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// errorEnvelopeSignal extracts an explicit error signal from a decoded
// response object. It understands OpenAI-style {"error": {...}|"..."},
// Anthropic-style {"type": "error", ...}, proxy {"success": false} /
// {"ok": false} / {"status": "error"} shapes, and FastAPI-style
// {"detail": "..."} bodies.
func errorEnvelopeSignal(root map[string]json.RawMessage) (code, message string, found bool) {
	if raw, ok := root["error"]; ok && !isNull(raw) {
		if s, ok := jsonString(raw); ok {
			return "", s, true
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err == nil && obj != nil {
			if c, ok := obj["code"]; ok && !isNull(c) {
				if s, ok := jsonString(c); ok {
					code = s
				} else {
					code = strings.TrimSpace(string(c))
				}
			}
			if t, ok := obj["type"]; ok && !isNull(t) && code == "" {
				if s, ok := jsonString(t); ok {
					code = s
				}
			}
			if m, ok := obj["message"]; ok && !isNull(m) {
				if s, ok := jsonString(m); ok {
					message = s
				}
			}
			if message == "" {
				if s, ok := jsonString(raw); ok {
					message = s
				}
			}
			return code, message, true
		}
		return "", strings.TrimSpace(string(raw)), true
	}
	if raw, ok := root["type"]; ok {
		if s, ok := jsonString(raw); ok && strings.EqualFold(s, "error") {
			return "error", "", true
		}
	}
	for _, key := range []string{"success", "ok"} {
		if raw, ok := root[key]; ok && !isNull(raw) {
			var b bool
			if err := json.Unmarshal(raw, &b); err == nil && !b {
				msg := ""
				if m, ok := root["message"]; ok {
					msg, _ = jsonString(m)
				}
				return key + "_false", msg, true
			}
		}
	}
	if raw, ok := root["status"]; ok {
		if s, ok := jsonString(raw); ok && (strings.EqualFold(s, "error") || strings.EqualFold(s, "failed")) {
			msg := ""
			if m, ok := root["message"]; ok {
				msg, _ = jsonString(m)
			}
			return "status_" + strings.ToLower(s), msg, true
		}
	}
	// FastAPI-style proxies answer {"detail": "..."}. Only flag it when no
	// success marker is present, so a future success field can never collide.
	if _, hasChoices := root["choices"]; !hasChoices {
		if _, hasContent := root["content"]; !hasContent {
			for _, key := range []string{"detail", "detail"} {
				if raw, ok := root[key]; ok && !isNull(raw) {
					if s, ok := jsonString(raw); ok && strings.TrimSpace(s) != "" {
						return "detail", s, true
					}
				}
			}
		}
	}
	return "", "", false
}

// extractContentHead pulls leading completion text from OpenAI-style
// choices[].message.content (string or content-part array, plus legacy
// choices[].text) and Anthropic-style content[] text blocks. Output is
// bounded to maxSniffedContent characters.
func extractContentHead(root map[string]json.RawMessage) string {
	var b strings.Builder
	b.Grow(256)
	write := func(s string) {
		if b.Len() >= maxSniffedContent || s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		rest := maxSniffedContent - b.Len()
		if len(s) > rest {
			s = s[:rest]
		}
		b.WriteString(s)
	}
	if raw, ok := root["choices"]; ok && !isNull(raw) {
		var choices []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &choices); err == nil {
			for _, ch := range choices {
				if m, ok := ch["message"]; ok && !isNull(m) {
					var msg map[string]json.RawMessage
					if err := json.Unmarshal(m, &msg); err == nil {
						if c, ok := msg["content"]; ok && !isNull(c) {
							if s, ok := jsonString(c); ok {
								write(s)
							} else {
								var parts []map[string]json.RawMessage
								if err := json.Unmarshal(c, &parts); err == nil {
									for _, p := range parts {
										if t, ok := p["text"]; ok {
											if s, ok := jsonString(t); ok {
												write(s)
											}
										}
									}
								}
							}
						}
					}
				}
				if t, ok := ch["text"]; ok && !isNull(t) {
					if s, ok := jsonString(t); ok {
						write(s)
					}
				}
				if b.Len() >= maxSniffedContent {
					break
				}
			}
		}
	}
	if raw, ok := root["content"]; ok && !isNull(raw) {
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &blocks); err == nil {
			for _, bl := range blocks {
				if t, ok := bl["text"]; ok && !isNull(t) {
					if s, ok := jsonString(t); ok {
						write(s)
					}
				}
				if b.Len() >= maxSniffedContent {
					break
				}
			}
		}
	}
	return b.String()
}

// SniffContentHead reports the failure class when the start of a completion
// matches a known paywall/auth/throttle/overload phrase. It returns
// UpstreamOK for clean or unrecognized text. Matching is positional: only
// the first contentHeadWindow characters count, because injected paywall
// text always starts the reply while genuine discussion mentions it later.
func SniffContentHead(text string) UpstreamErrorClass {
	head := strings.ToLower(strings.TrimSpace(text))
	if head == "" {
		return UpstreamOK
	}
	if len(head) > contentHeadWindow {
		head = head[:contentHeadWindow]
	}
	for _, list := range headSignalLists {
		for _, phrase := range list.phrases {
			if strings.Contains(head, phrase) {
				return list.class
			}
		}
	}
	if containsAny(head, topUpPhrases) && containsAny(head, topUpContextWords) {
		return UpstreamQuota
	}
	return UpstreamOK
}

// finishReasonError scans non-streaming OpenAI choices for the documented
// terminal failure finish_reason "error" (used by OpenRouter-style
// gateways), and Anthropic stop_reason "error" for symmetry.
func finishReasonError(root map[string]json.RawMessage) bool {
	if raw, ok := root["choices"]; ok && !isNull(raw) {
		var choices []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &choices); err == nil {
			for _, ch := range choices {
				if fr, ok := ch["finish_reason"]; ok && !isNull(fr) {
					if s, ok := jsonString(fr); ok && strings.EqualFold(s, "error") {
						return true
					}
				}
			}
		}
	}
	if raw, ok := root["stop_reason"]; ok && !isNull(raw) {
		if s, ok := jsonString(raw); ok && strings.EqualFold(s, "error") {
			return true
		}
	}
	return false
}

// ClassifyUpstreamResponse inspects a complete upstream response body and
// returns nil when the body looks like a genuine success. Any non-2xx
// status is an error (classified by status, refined by message). A 2xx
// body is an error when it carries an explicit error envelope, a terminal
// error finish reason, or injected paywall/auth/throttle text at the start
// of the completion.
func ClassifyUpstreamResponse(status int, body []byte) *UpstreamLogicalError {
	trimmed := bytes.TrimSpace(body)
	if status < 200 || status >= 300 {
		code, msg := "", ""
		if len(trimmed) > 0 {
			var root map[string]json.RawMessage
			if err := json.Unmarshal(trimmed, &root); err == nil && root != nil {
				if c, m, found := errorEnvelopeSignal(root); found {
					code, msg = c, m
				} else if m, ok := root["message"]; ok {
					if s, ok := jsonString(m); ok {
						msg = s
					}
				}
				if code == "" {
					if c, ok := root["code"]; ok && !isNull(c) {
						if s, ok := jsonString(c); ok {
							code = s
						} else {
							code = strings.TrimSpace(string(c))
						}
					}
				}
			} else if len(trimmed) > 0 && len(trimmed) < maxUpstreamErrorMessage && !bytes.HasPrefix(trimmed, []byte("<")) {
				msg = string(trimmed)
			}
		}
		class := classifyErrorText(code, msg)
		if class == UpstreamOK {
			class = classForStatus(status)
		}
		if msg == "" {
			msg = strings.TrimSpace(string(trimmed))
			if bytes.HasPrefix(trimmed, []byte("<")) {
				msg = "non-JSON error page"
			}
		}
		return &UpstreamLogicalError{Status: status, Class: class, Code: boundMessage(code), Message: boundMessage(msg)}
	}
	if len(trimmed) == 0 {
		return &UpstreamLogicalError{Status: status, Class: UpstreamBadPayload, Message: "empty response body"}
	}
	if string(trimmed) == "null" {
		return &UpstreamLogicalError{Status: status, Class: UpstreamBadPayload, Message: "literal null response body"}
	}
	if bytes.HasPrefix(trimmed, []byte("<")) {
		return &UpstreamLogicalError{Status: status, Class: UpstreamBadPayload, Message: "HTML body on a 2xx API response"}
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &root); err != nil || root == nil {
		return &UpstreamLogicalError{Status: status, Class: UpstreamBadPayload, Message: "malformed JSON response"}
	}
	if code, msg, found := errorEnvelopeSignal(root); found {
		class := classifyErrorText(code, msg)
		if class == UpstreamOK {
			class = UpstreamServer
		}
		if msg == "" {
			msg = "error envelope without message"
			if code != "" {
				msg += " (code: " + code + ")"
			}
		}
		return &UpstreamLogicalError{Status: status, Class: class, Code: boundMessage(code), Message: boundMessage(msg)}
	}
	if finishReasonError(root) {
		return &UpstreamLogicalError{Status: status, Class: UpstreamServer, Code: "finish_reason_error", Message: "completion ended with finish_reason error"}
	}
	if class := SniffContentHead(extractContentHead(root)); class != UpstreamOK {
		return ContentSniffError(class, false)
	}
	// Top-level {message, code} proxy errors without an error key or
	// success marker: flag only when the message itself matches a known
	// error signature, otherwise leave the shape to envelope validation.
	if _, hasChoices := root["choices"]; !hasChoices {
		if _, hasContent := root["content"]; !hasContent {
			if m, ok := root["message"]; ok {
				if s, ok := jsonString(m); ok && strings.TrimSpace(s) != "" {
					if class := classifyErrorText("", s); class != UpstreamOK {
						return &UpstreamLogicalError{Status: status, Class: class, Message: boundMessage(s)}
					}
				}
			}
		}
	}
	return nil
}

// ClassifySSEData inspects one SSE data payload for in-band error signals:
// a top-level error member (OpenAI/OpenRouter error chunks), Anthropic
// {"type": "error"} events, and finish_reason "error". It returns nil for
// clean chunks and for unparseable payloads (callers report invalid JSON
// themselves, so no double error is produced).
func ClassifySSEData(protocol string, data []byte) *UpstreamLogicalError {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &root); err != nil || root == nil {
		return nil
	}
	if raw, ok := root["error"]; ok && !isNull(raw) {
		code, msg := "", ""
		if s, ok := jsonString(raw); ok {
			msg = s
		} else {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(raw, &obj); err == nil && obj != nil {
				if t, ok := obj["type"]; ok {
					code, _ = jsonString(t)
				}
				if m, ok := obj["message"]; ok {
					msg, _ = jsonString(m)
				}
				if code == "" {
					if c, ok := obj["code"]; ok && !isNull(c) {
						if s, ok := jsonString(c); ok {
							code = s
						} else {
							code = strings.TrimSpace(string(c))
						}
					}
				}
			}
		}
		class := classifyErrorText(code, msg)
		if class == UpstreamOK {
			class = UpstreamServer
		}
		if msg == "" {
			msg = "error event without message"
		}
		return &UpstreamLogicalError{Status: 200, Class: class, Code: boundMessage(code), Message: boundMessage(msg)}
	}
	if protocol == "anthropic" {
		if raw, ok := root["type"]; ok {
			if s, ok := jsonString(raw); ok && s == "error" {
				return &UpstreamLogicalError{Status: 200, Class: UpstreamServer, Code: "error", Message: "anthropic error event"}
			}
		}
		if raw, ok := root["delta"]; ok && !isNull(raw) {
			var delta map[string]json.RawMessage
			if err := json.Unmarshal(raw, &delta); err == nil {
				if sr, ok := delta["stop_reason"]; ok && !isNull(sr) {
					if s, ok := jsonString(sr); ok && strings.EqualFold(s, "error") {
						return &UpstreamLogicalError{Status: 200, Class: UpstreamServer, Code: "stop_reason_error", Message: "stream ended with stop_reason error"}
					}
				}
			}
		}
		return nil
	}
	if raw, ok := root["choices"]; ok && !isNull(raw) {
		var choices []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &choices); err == nil {
			for _, ch := range choices {
				if fr, ok := ch["finish_reason"]; ok && !isNull(fr) {
					if s, ok := jsonString(fr); ok && strings.EqualFold(s, "error") {
						return &UpstreamLogicalError{Status: 200, Class: UpstreamServer, Code: "finish_reason_error", Message: "stream ended with finish_reason error"}
					}
				}
			}
		}
	}
	return nil
}

// StreamContentSniffer accumulates streamed completion text (bounded) so a
// paywall message delivered as stream deltas is still recognized. Only the
// head window is ever inspected; Add discards everything beyond it.
type StreamContentSniffer struct {
	buf []byte
}

// Add appends streamed text; bytes beyond the head window are dropped.
func (s *StreamContentSniffer) Add(text string) {
	if text == "" || len(s.buf) >= contentHeadWindow {
		return
	}
	rest := contentHeadWindow - len(s.buf)
	if len(s.buf) > 0 {
		s.buf = append(s.buf, ' ')
		rest--
	}
	if len(text) > rest {
		text = text[:rest]
	}
	s.buf = append(s.buf, text...)
}

// Sniff reports the failure class when accumulated stream text matches a
// known failure signature, or UpstreamOK when the stream looks clean.
func (s *StreamContentSniffer) Sniff() UpstreamErrorClass {
	return SniffContentHead(string(s.buf))
}

// ContentSniffError builds the logical error for completion text (streamed
// or not) that matches a known failure signature. The matched text itself
// is deliberately not echoed: the class is the actionable signal.
func ContentSniffError(class UpstreamErrorClass, streamed bool) *UpstreamLogicalError {
	what := "completion content"
	if streamed {
		what = "streamed content"
	}
	return &UpstreamLogicalError{
		Status: 200, Class: class, Code: "content_sniff",
		Message: what + " matches a known " + string(class) + " failure signature",
	}
}
