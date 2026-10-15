package compat

import (
	"encoding/json"
	"testing"
)

// Regression (independent review of the Gemini parity PR): when a token
// capability is verified Unsupported, sanitizeGeminiGenerationConfig
// removes generationConfig.maxOutputTokens — but the top-level alias
// loop must not move an OpenAI-style budget key straight back into
// generationConfig.maxOutputTokens. The forbidden knob leaked.
func TestGeminiSanitizeUnsupportedTokenBudgetNotReaddedByAlias(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*Contract)
	}{
		{"max_completion_tokens unsupported", func(c *Contract) { c.Capabilities.MaxCompletionTokens = Unsupported }},
		{"max_tokens unsupported", func(c *Contract) { c.Capabilities.MaxTokens = Unsupported }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contract := Contract{}
			tc.apply(&contract)
			payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"max_tokens":16}`)
			res, err := Sanitize(payload, contract, Dialects()[DialectGemini], RequirementProfile{})
			if err != nil {
				t.Fatalf("sanitize err=%v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(res.Payload, &m); err != nil {
				t.Fatalf("unmarshal sanitized payload: %v", err)
			}
			if gc, has := m["generationConfig"].(map[string]any); has {
				if _, leaked := gc["maxOutputTokens"]; leaked {
					t.Fatalf("unsupported token budget re-added by alias loop: %s", res.Payload)
				}
			}
			if _, has := m["max_tokens"]; has {
				t.Fatalf("top-level max_tokens survived sanitize: %s", res.Payload)
			}
			found := false
			for _, r := range res.Removed {
				if r == "max_tokens" {
					found = true
				}
			}
			if !found {
				t.Fatalf("removal of max_tokens not recorded: removed=%v payload=%s", res.Removed, res.Payload)
			}
		})
	}

	// Control: with no Unsupported token capability the alias still moves
	// the budget into generationConfig.maxOutputTokens.
	contract := Contract{}
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"max_tokens":16}`)
	res, err := Sanitize(payload, contract, Dialects()[DialectGemini], RequirementProfile{})
	if err != nil {
		t.Fatalf("control sanitize err=%v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(res.Payload, &m); err != nil {
		t.Fatalf("control unmarshal: %v", err)
	}
	gc, has := m["generationConfig"].(map[string]any)
	if !has || gc["maxOutputTokens"] != float64(16) {
		t.Fatalf("control: alias no longer moves supported budget: %s", res.Payload)
	}
}
