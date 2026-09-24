package stream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// GeminiDecoder parses Gemini streamGenerateContent SSE (data: {...} chunks,
// each a partial GenerateContent response). Unlike OpenAI there is no [DONE]
// marker: the final chunk carries finishReason (and usually usageMetadata).
// A prompt-level safety block arrives as a chunk with promptFeedback and no
// candidates, which is a hard failure, not an empty completion.
type GeminiDecoder struct {
	line      []byte
	sawFinish bool
	emitted   bool
	finish    string
	in, out   int
	usageSet  bool
	started   map[int]int // candidate index -> tool calls started
}

func (d *GeminiDecoder) Feed(p []byte) ([]Event, error) {
	events := []Event{}
	for len(p) > 0 {
		n := bytes.IndexByte(p, '\n')
		if n < 0 {
			if len(d.line)+len(p) > maxLineBytes {
				return events, fmt.Errorf("gemini SSE line exceeds %d bytes", maxLineBytes)
			}
			d.line = append(d.line, p...)
			return events, nil
		}
		if len(d.line)+n > maxLineBytes {
			return events, fmt.Errorf("gemini SSE line exceeds %d bytes", maxLineBytes)
		}
		d.line = append(d.line, p[:n]...)
		evs, err := d.processLine()
		if err != nil {
			return events, err
		}
		events = append(events, evs...)
		d.line = d.line[:0]
		p = p[n+1:]
	}
	return events, nil
}

func (d *GeminiDecoder) processLine() ([]Event, error) {
	line := bytes.TrimSpace(d.line)
	if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
		return nil, nil
	}
	data := bytes.TrimSpace(line[len("data:"):])
	if len(data) == 0 {
		return nil, nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		// Not native to Gemini, but tolerated from translating proxies.
		d.sawFinish = true
		if d.emitted {
			return nil, nil
		}
		d.emitted = true
		return d.terminalEvents(), nil
	}
	var chunk struct {
		Candidates []struct {
			Index        int `json:"index"`
			Content      *struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback *struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		Usage *struct {
			PromptTokens     int `json:"promptTokenCount"`
			CandidatesTokens int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, fmt.Errorf("invalid Gemini SSE JSON: %w", err)
	}
	if chunk.PromptFeedback != nil && chunk.PromptFeedback.BlockReason != "" {
		reason := chunk.PromptFeedback.BlockReason
		return []Event{{Kind: KindError, Text: "gemini prompt blocked: " + reason}},
			fmt.Errorf("gemini prompt blocked: %s", reason)
	}
	events := []Event{}
	if chunk.Usage != nil {
		d.in, d.out = chunk.Usage.PromptTokens, chunk.Usage.CandidatesTokens
		d.usageSet = true
	}
	for _, cand := range chunk.Candidates {
		if cand.Content != nil {
			for _, part := range cand.Content.Parts {
				if part.Text != "" {
					events = append(events, Event{Kind: KindTextDelta, Text: part.Text})
				}
				if part.FunctionCall != nil {
					if d.started == nil {
						d.started = map[int]int{}
					}
					ordinal := d.started[cand.Index]
					if part.FunctionCall.Name != "" && ordinal == 0 {
						events = append(events, Event{
							Kind: KindToolCallStart, ToolIndex: cand.Index,
							ToolID:   fmt.Sprintf("gemini-call-%d-%d", cand.Index, ordinal),
							ToolName: part.FunctionCall.Name,
						})
						d.started[cand.Index] = ordinal + 1
					}
					if len(part.FunctionCall.Args) > 0 {
						arg, _ := json.Marshal(part.FunctionCall.Args)
						events = append(events, Event{
							Kind: KindToolCallDelta, ToolIndex: cand.Index,
							ToolID:   fmt.Sprintf("gemini-call-%d-%d", cand.Index, ordinal),
							ToolName: part.FunctionCall.Name, ToolArgs: string(arg),
						})
					}
				}
			}
		}
		if strings.TrimSpace(cand.FinishReason) != "" {
			d.sawFinish = true
			d.finish = geminiFinishReason(cand.FinishReason)
		}
	}
	return events, nil
}

func (d *GeminiDecoder) terminalEvents() []Event {
	out := []Event{}
	if d.usageSet {
		out = append(out, Event{Kind: KindUsage, InputTokens: d.in, OutputTokens: d.out})
	}
	finish := d.finish
	if finish == "" {
		finish = "stop"
	}
	out = append(out, Event{Kind: KindEnd, FinishReason: finish})
	return out
}

func (d *GeminiDecoder) Finish() ([]Event, error) {
	if len(d.line) > 0 {
		if _, err := d.processLine(); err != nil {
			return nil, err
		}
		d.line = nil
	}
	if !d.sawFinish {
		return []Event{{Kind: KindError, Text: "upstream stream ended without a terminal Gemini chunk"}},
			fmt.Errorf("gemini stream ended without finishReason")
	}
	if d.emitted {
		return nil, nil
	}
	d.emitted = true
	return d.terminalEvents(), nil
}

// geminiFinishReason maps GenerateContent finish reasons onto the canonical
// stop vocabulary.
func geminiFinishReason(reason string) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	case "":
		return "stop"
	default:
		return strings.ToLower(strings.TrimSpace(reason))
	}
}
