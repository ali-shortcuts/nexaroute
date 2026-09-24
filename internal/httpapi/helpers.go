package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
)

const (
	maxJSONBodyBytes     = 16 << 20
	maxUpstreamJSONBytes = 32 << 20
)

func readJSON(r *http.Request, dst any) ([]byte, error) {
	if strings.HasPrefix(r.URL.Path, "/admin/api/") {
		ct := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
		if ct != "application/json" {
			return nil, fmt.Errorf("Content-Type must be application/json")
		}
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxJSONBodyBytes {
		return b[:maxJSONBodyBytes], &requestTooLargeError{limit: maxJSONBodyBytes}
	}
	if err = json.Unmarshal(b, dst); err != nil {
		return b, err
	}
	return b, nil
}

type requestTooLargeError struct{ limit int }

func (e *requestTooLargeError) Error() string {
	return fmt.Sprintf("request body exceeds %d bytes", e.limit)
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func errorJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"type": "gateway_error", "message": msg}})
}
func hasVisionAnth(raw []byte) bool {
	return inspectRequestJSON(raw, "image", nil).Vision
}
func hasVisionOpenAI(raw []byte) bool {
	return inspectRequestJSON(raw, "image_url", nil).Vision
}

func hasReasoningAnth(raw []byte) bool {
	return inspectRequestJSON(raw, "", []string{"thinking", "reasoning"}).Reasoning
}

func hasReasoningOpenAI(raw []byte) bool {
	return inspectRequestJSON(raw, "", []string{"reasoning_effort", "reasoning"}).Reasoning
}

type jsonEnvelopeValidator func([]byte) error

func readJSONLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxUpstreamJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxUpstreamJSONBytes {
		return nil, fmt.Errorf("upstream JSON response exceeds %d bytes", maxUpstreamJSONBytes)
	}
	return b, nil
}

func decodeValidatedJSONLimited(r io.Reader, dst any, validate jsonEnvelopeValidator) error {
	b, err := readJSONLimited(r)
	if err != nil {
		return err
	}
	if validate != nil {
		if err := validate(b); err != nil {
			return err
		}
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return err
	}
	return nil
}

// Keep the native passthrough and cross-protocol conversions on the same
// envelope rules, so neither can mistake an upstream 200 error for a reply.
func validateOpenAIResponseJSON(b []byte) error {
	return canonical.ValidateOpenAIChatResponseJSON(b)
}

func validateAnthropicResponseJSON(b []byte) error {
	return canonical.ValidateAnthropicResponseJSON(b)
}

func normalizeRequestID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	const max = 128
	var b strings.Builder
	b.Grow(min(len(v), max))
	for _, r := range v {
		if b.Len() >= max {
			break
		}
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

func anthropicErrorJSON(w http.ResponseWriter, code int, msg string) {
	typ := "api_error"
	switch code {
	case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
		typ = "invalid_request_error"
	case http.StatusUnauthorized:
		typ = "authentication_error"
	case http.StatusForbidden:
		typ = "permission_error"
	case http.StatusNotFound:
		typ = "not_found_error"
	case http.StatusRequestEntityTooLarge:
		typ = "request_too_large"
	case http.StatusTooManyRequests:
		typ = "rate_limit_error"
	}
	writeJSON(w, code, map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
}

func uniqueStreamID(prefix string, requestID ...string) string {
	rid := ""
	if len(requestID) > 0 {
		rid = requestID[0]
	}
	rid = strings.TrimSpace(rid)
	if rid == "" {
		rid = fmt.Sprintf("%x", time.Now().UnixNano())
	}
	var b strings.Builder
	for _, r := range rid {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	clean := b.String()
	if clean == "" {
		clean = fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return prefix + "_" + clean
}
