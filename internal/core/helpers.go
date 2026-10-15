package core

import (
	"encoding/json"
	"strings"
)

// AnthCacheControl is an Anthropic prompt-cache marker on a block.
type AnthCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type AnthBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text,omitempty"`
	CacheControl *AnthCacheControl `json:"cache_control,omitempty"`
	ID           string            `json:"id,omitempty"`
	Name         string            `json:"name,omitempty"`
	Input        map[string]any    `json:"input,omitempty"`
	Source       map[string]any    `json:"source,omitempty"`
	ToolUseID    string            `json:"tool_use_id,omitempty"`
	Content      json.RawMessage   `json:"content,omitempty"`
	IsError      bool              `json:"is_error,omitempty"`
}

func ParseAnthContent(raw json.RawMessage) ([]AnthBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []AnthBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []AnthBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func ParseSystem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []AnthBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, x := range blocks {
			if x.Type == "text" {
				b.WriteString(x.Text)
				b.WriteByte('\n')
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}
