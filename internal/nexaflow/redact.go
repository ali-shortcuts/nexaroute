package nexaflow

import (
	"encoding/json"
	"regexp"
	"strings"
)

// MaxErrorMessageChars bounds every error message this package surfaces.
const MaxErrorMessageChars = 512

var bearerPattern = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9\-._~+/=]+`)

// secretKeys are JSON/object keys whose values are never surfaced, whatever
// their source. Matching is case-insensitive and boundary-aware: a bare
// substring match would wrongly redact legitimate counters such as
// "total_prompt_tokens" (which contains "token" but is not a credential).
var secretKeys = []string{
	"authorization", "api_key", "apikey", "api-key", "token",
	"secret", "password", "client_secret", "access_token", "refresh_token",
}

// RedactError returns a secret-safe rendering of err: bearer tokens are
// replaced, secret-keyed values are dropped by callers via redactJSONValue,
// and the message is truncated to MaxErrorMessageChars.
func RedactError(err error) string {
	if err == nil {
		return ""
	}
	msg := bearerPattern.ReplaceAllString(err.Error(), "Bearer [REDACTED]")
	// Strip anything that looks like a long opaque credential after a
	// key-like assignment, without guessing at its value.
	msg = strings.TrimSpace(msg)
	if len(msg) > MaxErrorMessageChars {
		msg = msg[:MaxErrorMessageChars]
	}
	return msg
}

func isSecretKey(key string) bool {
	lower := strings.ToLower(key)
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
	// Hyphenated credential headers ("x-api-key") and compound names
	// ("client_secret") that the underscore rule misses.
	if strings.Contains(lower, "api_key") || strings.Contains(lower, "apikey") ||
		strings.Contains(lower, "client_secret") || strings.Contains(lower, "authorization") {
		return true
	}
	return false
}

// redactJSONValue deep-copies v, replacing values under secret keys with
// "[REDACTED]" and truncating over-long strings. It never mutates v.
func redactJSONValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for key, child := range val {
			if isSecretKey(key) {
				out[key] = "[REDACTED]"
				continue
			}
			out[key] = redactJSONValue(child)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, child := range val {
			out[i] = redactJSONValue(child)
		}
		return out
	case string:
		if bearerPattern.MatchString(val) {
			return bearerPattern.ReplaceAllString(val, "Bearer [REDACTED]")
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
