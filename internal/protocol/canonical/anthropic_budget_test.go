package canonical

import (
	"encoding/json"
	"testing"
)

// Regression: EncodeAnthropicRequest defaulted max_tokens to 4096 but allowed
// thinking budgets of 8192/16384, producing a payload real Anthropic APIs
// reject because max_tokens must exceed budget_tokens.
func TestEncodeAnthropicRequestRaisesMaxTokensAboveThinkingBudget(t *testing.T) {
	cases := []struct {
		name    string
		req     Request
		wantMin int
	}{
		{"default effort", Request{Reasoning: &Reasoning{}}, 8192},
		{"high effort", Request{Reasoning: &Reasoning{Effort: "high"}}, 16384},
		{"explicit small max", Request{MaxOutputTokens: 1024, Reasoning: &Reasoning{BudgetTokens: 4096}}, 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := EncodeAnthropicRequest(tc.req, "m")
			if err != nil {
				t.Fatal(err)
			}
			var thinking struct {
				BudgetTokens int `json:"budget_tokens"`
			}
			if len(out.Thinking) > 0 {
				if err := json.Unmarshal(out.Thinking, &thinking); err != nil {
					t.Fatal(err)
				}
			}
			if thinking.BudgetTokens <= 0 {
				t.Fatalf("thinking budget not encoded: %s", out.Thinking)
			}
			if out.MaxTokens <= thinking.BudgetTokens {
				t.Fatalf("max_tokens=%d <= budget_tokens=%d; Anthropic rejects this", out.MaxTokens, thinking.BudgetTokens)
			}
			if out.MaxTokens < tc.wantMin {
				t.Fatalf("max_tokens=%d, want at least %d", out.MaxTokens, tc.wantMin)
			}
		})
	}
}
