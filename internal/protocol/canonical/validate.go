package canonical

import (
	"encoding/json"
	"fmt"
)

// ValidateOpenAIChatResponseJSON checks that an upstream 200 is a Chat Completions
// message, not an error envelope or a superficially valid empty JSON object.
func ValidateOpenAIChatResponseJSON(b []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(b, &root); err != nil {
		return fmt.Errorf("invalid OpenAI response JSON: %w", err)
	}
	if raw, ok := root["error"]; ok && len(raw) > 0 && string(raw) != "null" {
		return fmt.Errorf("OpenAI response contains an error envelope")
	}
	rawChoices := root["choices"]
	if len(rawChoices) == 0 {
		return fmt.Errorf("invalid OpenAI response: choices missing")
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(rawChoices, &choices); err != nil {
		return fmt.Errorf("invalid OpenAI response choices: %w", err)
	}
	if len(choices) == 0 || len(choices[0]["message"]) == 0 {
		return fmt.Errorf("invalid OpenAI response: message missing")
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(choices[0]["message"], &message); err != nil || message == nil {
		if err != nil {
			return fmt.Errorf("invalid OpenAI response message: %w", err)
		}
		return fmt.Errorf("invalid OpenAI response message")
	}
	if len(message) == 0 {
		return fmt.Errorf("invalid OpenAI response: empty message")
	}
	return nil
}

// ValidateAnthropicResponseJSON checks the native Messages envelope before conversion.
func ValidateAnthropicResponseJSON(b []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(b, &root); err != nil {
		return fmt.Errorf("invalid Anthropic response JSON: %w", err)
	}
	if raw, ok := root["error"]; ok && len(raw) > 0 && string(raw) != "null" {
		return fmt.Errorf("Anthropic response contains an error envelope")
	}
	var typ, role string
	if raw := root["type"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &typ)
	}
	if raw := root["role"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &role)
	}
	var content []json.RawMessage
	if raw := root["content"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &content); err != nil {
			return fmt.Errorf("invalid Anthropic response content: %w", err)
		}
	}
	if typ != "message" || role != "assistant" || content == nil {
		return fmt.Errorf("invalid Anthropic message envelope")
	}
	return nil
}
