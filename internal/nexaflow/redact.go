package nexaflow

import (
	"encoding/json"
	"regexp"
	"strings"
)

// MaxErrorMessageChars bounds every error message this package surfaces.
const MaxErrorMessageChars = 512

// MaxRedactDepth bounds recursion in redactJSONValue: untrusted workflow
// data may nest arbitrarily deep, and unbounded recursion over a
// caller-constructed map would exhaust the stack. Anything deeper than
// this is replaced, never traversed.
const MaxRedactDepth = 100

var bearerPattern = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9\-._~+/=]+`)
var basicPattern = regexp.MustCompile(`(?i)Basic\s+[A-Za-z0-9+/=]+`)

// secretKeys are JSON/object keys whose values are never surfaced, whatever
// their source. Matching is case-insensitive and boundary-aware: a bare
// substring match would wrongly redact legitimate counters such as
// "total_prompt_tokens" (which contains "token" but is not a credential).
// Keys are normalized (hyphens become underscores) before matching, so
// header spellings such as "x-api-key" and "set-cookie" are caught by the
// same rules as "x_api_key" and "set_cookie".
var secretKeys = []string{
	"authorization", "api_key", "apikey", "token",
	"secret", "password", "client_secret", "access_token", "refresh_token",
	"cookie", "set_cookie", "proxy_authorization", "auth_token",
}

// RedactError returns a secret-safe rendering of err: bearer and basic
// credentials are replaced, secret-keyed values are dropped by callers
// via redactJSONValue, and the message is truncated to MaxErrorMessageChars.
func RedactError(err error) string {
	if err == nil {
		return ""
	}
	msg := bearerPattern.ReplaceAllString(err.Error(), "Bearer [REDACTED]")
	msg = basicPattern.ReplaceAllString(msg, "Basic [REDACTED]")
	// Strip anything that looks like a long opaque credential after a
	// key-like assignment, without guessing at its value.
	msg = strings.TrimSpace(msg)
	if len(msg) > MaxErrorMessageChars {
		msg = msg[:MaxErrorMessageChars]
	}
	return msg
}

func isSecretKey(key string) bool {
	// Normalize so "X-Api-Key", "x_api_key", and "x-api-key" all match.
	lower := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	for _, secret := range secretKeys {
		if lower == secret {
			return true
		}
		// Underscore-bounded occurrences: "access_token", "x_api_key".
		// Deliberately NOT a plain substring match: "total_prompt_tokens"
		// ends in "_tokens" (plural), not "_token", and must survive.
		if strings.HasSuffix(lower, "_"+secret) || strings.HasPrefix(lower, secret+"_") {
			return true
		}
	}
	// Compound names that the underscore rule misses.
	if strings.Contains(lower, "api_key") || strings.Contains(lower, "apikey") ||
		strings.Contains(lower, "client_secret") || strings.Contains(lower, "authorization") {
		return true
	}
	return false
}

// redactJSONValue deep-copies v, replacing values under secret keys with
// "[REDACTED]" and truncating over-long strings. It never mutates v.
// Depth is bounded by MaxRedactDepth; deeper values are replaced, never
// traversed, so untrusted nesting cannot exhaust the stack.
func redactJSONValue(v any) any {
	return redactJSONValueDepth(v, 0)
}

func redactJSONValueDepth(v any, depth int) any {
	if depth > MaxRedactDepth {
		return "[TRUNCATED]"
	}
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for key, child := range val {
			if isSecretKey(key) {
				out[key] = "[REDACTED]"
				continue
			}
			out[key] = redactJSONValueDepth(child, depth+1)
		}
		return out
	case map[string]string:
		// Typed string maps bypassed redaction entirely: the HTTP
		// node's response headers (map[string]string) reached run
		// history with Set-Cookie intact, because only
		// map[string]any was traversed. Redact them the same way.
		out := make(map[string]any, len(val))
		for key, child := range val {
			if isSecretKey(key) {
				out[key] = "[REDACTED]"
				continue
			}
			out[key] = redactJSONValueDepth(child, depth+1)
		}
		return out
	case []string:
		out := make([]any, len(val))
		for i, child := range val {
			out[i] = redactJSONValueDepth(child, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, child := range val {
			out[i] = redactJSONValueDepth(child, depth+1)
		}
		return out
	case string:
		if bearerPattern.MatchString(val) {
			val = bearerPattern.ReplaceAllString(val, "Bearer [REDACTED]")
		}
		if basicPattern.MatchString(val) {
			val = basicPattern.ReplaceAllString(val, "Basic [REDACTED]")
		}
		if len(val) > 8192 {
			return val[:8192]
		}
		return val
	default:
		return v
	}
}

// RedactJSON renders v as redacted JSON for logging or diagnostics. The
// input itself is never modified.
func RedactJSON(v any) string {
	redacted := redactJSONValue(v)
	raw, err := json.Marshal(redacted)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// truncateText bounds a model or tool output string without leaking more
// than max characters. Callers pass the node output bound.
func truncateText(s string, max int) string {
	if max > 0 && len(s) > max {
		return s[:max]
	}
	return s
}
