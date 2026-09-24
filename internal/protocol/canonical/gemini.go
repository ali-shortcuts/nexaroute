package canonical

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---------- Google Gemini GenerateContent <-> Canonical ----------
//
// Gemini deviates from the OpenAI/Anthropic families in three structural
// ways: the model id lives in the URL path, the roles are user/model, and
// function calls/results are first-class parts inside content turns.

// GeminiRequest is the GenerateContent request body (permissive).
type GeminiRequest struct {
	Contents          []GeminiContent         `json:"contents"`
	SystemInstruction *GeminiContent          `json:"systemInstruction,omitempty"`
	Tools             []GeminiTool            `json:"tools,omitempty"`
	ToolConfig        *GeminiToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *GeminiGenerationConfig `json:"generationConfig,omitempty"`
	SafetySettings    []map[string]any        `json:"safetySettings,omitempty"`
}

type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts"`
}

type GeminiPart struct {
	Text               string          `json:"text,omitempty"`
	InlineData         *GeminiBlob     `json:"inlineData,omitempty"`
	InlineDataV1       *GeminiBlob     `json:"inline_data,omitempty"`
	FunctionCall       *GeminiCall     `json:"functionCall,omitempty"`
	FunctionCallV1     *GeminiCall     `json:"function_call,omitempty"`
	FunctionResponse   *GeminiResponse `json:"functionResponse,omitempty"`
	FunctionResponseV1 *GeminiResponse `json:"function_response,omitempty"`
	Thought            *bool           `json:"thought,omitempty"`
}

type GeminiBlob struct {
	MimeType string `json:"mimeType,omitempty"`
	MimeV1   string `json:"mime_type,omitempty"`
	Data     string `json:"data"`
}

func (b *GeminiBlob) Mime() string {
	if b.MimeType != "" {
		return b.MimeType
	}
	return b.MimeV1
}

type GeminiCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type GeminiResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response,omitempty"`
}

type GeminiTool struct {
	FunctionDeclarations []map[string]any `json:"functionDeclarations,omitempty"`
}

type GeminiToolConfig struct {
	FunctionCallingConfig *struct {
		Mode                 string   `json:"mode,omitempty"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig,omitempty"`
}

type GeminiGenerationConfig struct {
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"topP,omitempty"`
	TopK             *float64        `json:"topK,omitempty"`
	MaxOutputTokens  int             `json:"maxOutputTokens,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
	CandidateCount   int             `json:"candidateCount,omitempty"`
}

// EncodeGeminiRequest builds a Gemini GenerateContent payload from the IR.
// The upstream model id is returned separately because Gemini carries it in
// the URL path (POST .../models/{model}:generateContent).
func EncodeGeminiRequest(in Request, upstreamModel string) (payload []byte, model string, err error) {
	out := GeminiRequest{}
	if txt := partsToText(in.System); strings.TrimSpace(txt) != "" {
		out.SystemInstruction = &GeminiContent{Parts: []GeminiPart{{Text: txt}}}
	}
	for _, m := range in.Messages {
		role := "user"
		if m.Role == RoleAssistant {
			role = "model"
		}
		parts := make([]GeminiPart, 0, len(m.Parts))
		for _, p := range m.Parts {
			switch p.Type {
			case PartText:
				if p.Text != "" {
					parts = append(parts, GeminiPart{Text: p.Text})
				}
			case PartImage:
				if p.Image == nil {
					continue
				}
				mt, data := p.Image.MediaType, p.Image.Data
				if p.Image.URL != "" && data == "" {
					if parsed := parseDataURL(p.Image.URL); parsed != nil {
						mt, data = parsed.mediaType, parsed.data
					} else {
						continue // Remote URLs are not supported by Gemini inline data.
					}
				}
				if data == "" {
					continue
				}
				if mt == "" {
					mt = "image/png"
				}
				parts = append(parts, GeminiPart{InlineData: &GeminiBlob{MimeType: mt, Data: data}})
			case PartToolCall:
				if p.ToolCall != nil && p.ToolCall.Name != "" {
					args := p.ToolCall.Arguments
					if strings.TrimSpace(args) == "" {
						args = "{}"
					}
					parts = append(parts, GeminiPart{FunctionCall: &GeminiCall{Name: p.ToolCall.Name, Args: json.RawMessage(args)}})
				}
			case PartToolResult:
				if p.ToolResult == nil {
					continue
				}
				resp := parseOrQuote(p.ToolResult.Content)
				parts = append(parts, GeminiPart{FunctionResponse: &GeminiResponse{
					Name:     toolNameForID(in, p.ToolResult.ToolUseID),
					Response: resp,
				}})
			}
		}
		if len(parts) == 0 {
			parts = []GeminiPart{{Text: ""}}
		}
		out.Contents = append(out.Contents, GeminiContent{Role: role, Parts: parts})
	}
	if len(in.Tools) > 0 {
		decls := make([]map[string]any, 0, len(in.Tools))
		for _, t := range in.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.Parameters) > 0 {
				var schema map[string]any
				if json.Unmarshal(t.Parameters, &schema) == nil && schema != nil {
					fn["parameters"] = stripSchemaKeywords(schema)
				}
			}
			decls = append(decls, fn)
		}
		out.Tools = []GeminiTool{{FunctionDeclarations: decls}}
	}
	if in.ToolChoice != nil && len(in.Tools) > 0 {
		mode := "MODE_AUTO"
		switch in.ToolChoice.Mode {
		case "none":
			mode = "MODE_NONE"
		case "required", "any":
			mode = "MODE_ANY"
		case "tool":
			mode = "MODE_ANY"
		}
		cfg := &struct {
			Mode                 string   `json:"mode,omitempty"`
			AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
		}{Mode: mode}
		if in.ToolChoice.Mode == "tool" && in.ToolChoice.Name != "" {
			cfg.AllowedFunctionNames = []string{in.ToolChoice.Name}
		}
		out.ToolConfig = &GeminiToolConfig{FunctionCallingConfig: cfg}
	}
	gc := &GeminiGenerationConfig{}
	if in.Temperature != nil {
		gc.Temperature = in.Temperature
	}
	if in.TopP != nil {
		gc.TopP = in.TopP
	}
	if in.TopK != nil {
		gc.TopK = in.TopK
	}
	if in.MaxOutputTokens > 0 {
		gc.MaxOutputTokens = in.MaxOutputTokens
	}
	if len(in.Stop) > 0 {
		gc.StopSequences = in.Stop
	}
	if in.ResponseFormat != nil {
		switch in.ResponseFormat.Kind {
		case FormatJSONObject:
			gc.ResponseMimeType = "application/json"
		case FormatJSONSchema:
			gc.ResponseMimeType = "application/json"
			if len(in.ResponseFormat.Schema) > 0 {
				var schema map[string]any
				if json.Unmarshal(in.ResponseFormat.Schema, &schema) == nil {
					gc.ResponseSchema = mustJSON(stripSchemaKeywords(schema))
				}
			}
		}
	}
	out.GenerationConfig = gc
	payload, err = json.Marshal(out)
	return payload, upstreamModel, err
}

// stripSchemaKeywords removes JSON Schema keywords Gemini rejects
// (e.g. $schema, additionalProperties) recursively. Unknown keywords are
// tolerated by Gemini, but $-prefixed ones are not.
func stripSchemaKeywords(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		if strings.HasPrefix(k, "$") || k == "additionalProperties" {
			continue
		}
		switch tv := v.(type) {
		case map[string]any:
			out[k] = stripSchemaKeywords(tv)
		case []any:
			arr := make([]any, 0, len(tv))
			for _, item := range tv {
				if m, ok := item.(map[string]any); ok {
					arr = append(arr, stripSchemaKeywords(m))
				} else {
					arr = append(arr, item)
				}
			}
			out[k] = arr
		default:
			out[k] = v
		}
	}
	return out
}

func toolNameForID(in Request, id string) string {
	for _, m := range in.Messages {
		if m.Role != RoleAssistant {
			continue
		}
		for _, p := range m.Parts {
			if p.Type == PartToolCall && p.ToolCall != nil && p.ToolCall.ID == id {
				return p.ToolCall.Name
			}
		}
	}
	if id == "" {
		return "unknown"
	}
	return id
}

func parseOrQuote(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{"result": ""}`)
	}
	var probe any
	if json.Unmarshal([]byte(s), &probe) == nil {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(map[string]any{"result": s})
	return b
}

// GeminiResponse_ is the GenerateContent response body (permissive).
type GeminiResponse_ struct {
	Candidates     []GeminiCandidate `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason,omitempty"`
	} `json:"promptFeedback,omitempty"`
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
		ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
	} `json:"usageMetadata,omitempty"`
}

type GeminiCandidate struct {
	Content      *GeminiContent `json:"content"`
	FinishReason string         `json:"finishReason,omitempty"`
}

// DecodeGeminiResponse converts a GenerateContent body into the IR.
func DecodeGeminiResponse(b []byte) (Response, error) {
	var in GeminiResponse_
	if err := json.Unmarshal(b, &in); err != nil {
		return Response{}, fmt.Errorf("invalid Gemini response: %w", err)
	}
	if len(in.Candidates) == 0 && in.PromptFeedback == nil && in.UsageMetadata == nil {
		return Response{}, fmt.Errorf("Gemini response has no candidates, feedback or usage")
	}
	out := Response{ID: fmt.Sprintf("msg_%d", messageClock()), StopReason: StopEndTurn, Raw: append(json.RawMessage(nil), b...)}
	if in.PromptFeedback != nil && in.PromptFeedback.BlockReason != "" {
		out.StopReason = StopRefusal
	}
	if len(in.Candidates) > 0 {
		c := in.Candidates[0]
		sawToolCall := false
		if c.Content != nil {
			for partIndex, p := range c.Content.Parts {
				switch {
				case p.Text != "":
					if p.Thought != nil && *p.Thought {
						out.Blocks = append(out.Blocks, Block{Type: PartThinking, Thinking: &Thinking{Text: p.Text}})
					} else {
						out.Blocks = append(out.Blocks, Block{Type: PartText, Text: p.Text})
					}
				case p.FunctionCall != nil:
					sawToolCall = true
					args := string(p.FunctionCall.Args)
					if strings.TrimSpace(args) == "" {
						args = "{}"
					}
					out.Blocks = append(out.Blocks, Block{Type: PartToolCall, ToolCall: &ToolCall{ID: fmt.Sprintf("call_%d", partIndex), Name: p.FunctionCall.Name, Arguments: args}})
				case p.FunctionCallV1 != nil:
					sawToolCall = true
					args := string(p.FunctionCallV1.Args)
					if strings.TrimSpace(args) == "" {
						args = "{}"
					}
					out.Blocks = append(out.Blocks, Block{Type: PartToolCall, ToolCall: &ToolCall{ID: fmt.Sprintf("call_%d", partIndex), Name: p.FunctionCallV1.Name, Arguments: args}})
				}
			}
		}
		out.StopReason = geminiFinishToStop(c.FinishReason)
		// Gemini reports STOP for a turn that ends in function calls;
		// Anthropic/OpenAI clients expect the tool_use/tool_calls signal.
		if sawToolCall && out.StopReason == StopEndTurn {
			out.StopReason = StopToolUse
		}
	}
	if in.UsageMetadata != nil {
		out.Usage = Usage{
			InputTokens:     in.UsageMetadata.PromptTokenCount,
			OutputTokens:    in.UsageMetadata.CandidatesTokenCount,
			CacheReadTokens: in.UsageMetadata.CachedContentTokenCount,
			ReasoningTokens: in.UsageMetadata.ThoughtsTokenCount,
		}
	}
	if out.Blocks == nil {
		out.Blocks = []Block{}
	}
	return out, nil
}

func geminiFinishToStop(reason string) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "MAX_TOKENS":
		return StopMaxTokens
	case "SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII":
		return StopRefusal
	case "STOP", "":
		return StopEndTurn
	default:
		return StopEndTurn
	}
}
