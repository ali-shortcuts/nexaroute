package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxJSONBodyBytes = 16 << 20

func readJSON(r *http.Request, dst any) ([]byte, error) {
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
	return containsJSONType(raw, "image")
}
func hasVisionOpenAI(raw []byte) bool {
	return containsJSONType(raw, "image_url")
}

func hasReasoningAnth(raw []byte) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	return containsKey(v, "thinking") || containsKey(v, "reasoning")
}

func hasReasoningOpenAI(raw []byte) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	return containsKey(v, "reasoning_effort") || containsKey(v, "reasoning")
}

func containsJSONType(raw []byte, wanted string) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	return containsType(v, wanted)
}

func containsType(v any, wanted string) bool {
	switch x := v.(type) {
	case map[string]any:
		if typ, ok := x["type"].(string); ok && strings.EqualFold(typ, wanted) {
			return true
		}
		for _, child := range x {
			if containsType(child, wanted) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if containsType(child, wanted) {
				return true
			}
		}
	}
	return false
}

func containsKey(v any, wanted string) bool {
	switch x := v.(type) {
	case map[string]any:
		for key, child := range x {
			if strings.EqualFold(key, wanted) || containsKey(child, wanted) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if containsKey(child, wanted) {
				return true
			}
		}
	}
	return false
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
