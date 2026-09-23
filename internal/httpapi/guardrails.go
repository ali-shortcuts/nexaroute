package httpapi

import (
	"fmt"
	"net/http"
	"regexp"

	"github.com/ali-shortcuts/nexaroute/internal/events"
)

const maxGuardrailScanBytes = 64 << 20

type guardrailViolation struct {
	Rule    string `json:"rule"`
	Detail  string `json:"detail,omitempty"`
	Pattern string `json:"pattern,omitempty"`
}

// guardrailProblem applies the configured input guardrails to a raw request
// body. It returns a violation description or nil when the request passes.
// Patterns are user-supplied regexes evaluated case-insensitively against the
// raw body text; this is deliberately simple, deterministic and bounded —
// not a content-understanding filter.
func (s *Server) guardrailProblem(raw []byte) *guardrailViolation {
	g := s.currentConfig().Guardrails
	if g.MaxPromptChars > 0 && len(raw) > g.MaxPromptChars {
		return &guardrailViolation{Rule: "max_prompt_chars", Detail: fmt.Sprintf("request body is %d chars, limit is %d", len(raw), g.MaxPromptChars)}
	}
	if len(g.BlockedPatterns) == 0 || len(raw) == 0 {
		return nil
	}
	scan := raw
	if len(scan) > maxGuardrailScanBytes {
		scan = scan[:maxGuardrailScanBytes]
	}
	for _, pattern := range g.BlockedPatterns {
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			continue // validation guarantees compilable patterns; skip defensively
		}
		if re.Match(scan) {
			return &guardrailViolation{Rule: "blocked_pattern", Pattern: pattern}
		}
	}
	return nil
}

func (s *Server) rejectGuardrail(w http.ResponseWriter, r *http.Request, requestID string, v *guardrailViolation) {
	s.bus.Add(events.Event{RequestID: requestID, Kind: "guardrail_blocked", Message: v.Rule + ": " + v.Detail, ErrorType: "guardrail_blocked"})
	switch r.URL.Path {
	case "/v1/messages", "/v1/messages/count_tokens":
		anthropicErrorJSON(w, http.StatusBadRequest, "request blocked by guardrail ("+v.Rule+"): "+v.Detail)
	default:
		errorJSON(w, http.StatusBadRequest, "request blocked by guardrail ("+v.Rule+"): "+v.Detail)
	}
}

// POST /admin/api/guardrails/test {text} → evaluates the saved patterns
// against arbitrary text so the operator can dry-run the rules.
func (s *Server) adminGuardrailsTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, 405, "method not allowed")
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if _, err := readJSON(r, &in); err != nil {
		errorJSON(w, 400, "invalid JSON: "+err.Error())
		return
	}
	g := s.currentConfig().Guardrails
	matches := []string{}
	for _, pattern := range g.BlockedPatterns {
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			continue
		}
		if re.MatchString(in.Text) {
			matches = append(matches, pattern)
			if len(matches) >= 64 {
				break
			}
		}
	}
	lengthBlocked := g.MaxPromptChars > 0 && len(in.Text) > g.MaxPromptChars
	writeJSON(w, 200, map[string]any{
		"blocked":        lengthBlocked || len(matches) > 0,
		"length_blocked": lengthBlocked,
		"matches":        matches,
		"length":         len(in.Text),
	})
}
