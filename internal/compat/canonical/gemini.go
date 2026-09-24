package canonical

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// Minimal Gemini GenerateContent protocol family (first-class adapter,
// Phase 9; upstream serving in Phase 11). The gateway does not serve Gemini
// ingress; this file provides the Canonical IR <-> Gemini dialect converters
// so Gemini upstreams work without touching Router core.

// GeminiPart is one GenerateContent content part.
type GeminiPart struct {
	Text         string              `json:"text,omitempty"`
	InlineData   *GeminiInlineData   `json:"inlineData,omitempty"`
	FunctionCall *GeminiFunctionCall `json:"functionCall,omitempty"`
	FunctionResp *GeminiFunctionResp `json:"functionResponse,omitempty"`
}

// GeminiInlineData carries base64 media.
type GeminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// GeminiFunctionCall is a model-emitted invocation.
type GeminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// GeminiFunctionResp carries a tool result.
type GeminiFunctionResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response,omitempty"`
}

// GeminiContent is one GenerateContent turn.
type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts"`
}

// GeminiFunctionDecl is a declared tool.
type GeminiFunctionDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// GeminiTool wraps function declarations the way GenerateContent expects
// them: {"tools": [{"functionDeclarations": [...]}]}.
type GeminiTool struct {
	FunctionDeclarations []GeminiFunctionDecl `json:"functionDeclarations"`
}

// GeminiToolConfig carries the function-calling mode (AUTO/ANY/NONE) plus
// optional allowed names for pinned calls.
type GeminiToolConfig struct {
	FunctionCallingConfig map[string]any `json:"functionCallingConfig"`
}

// GeminiRequest is a minimal GenerateContent request.
type GeminiRequest struct {
	SystemInstruction *GeminiContent       `json:"systemInstruction,omitempty"`
	Contents          []GeminiContent      `json:"contents"`
	Tools             []GeminiTool         `json:"tools,omitempty"`
	ToolConfig        *GeminiToolConfig    `json:"toolConfig,omitempty"`
	GenerationConfig  map[string]any       `json:"generationConfig,omitempty"`
}

// GeminiResponse is a minimal GenerateContent response.
type GeminiResponse struct {
	Candidates []GeminiCandidate `json:"candidates"`
	Usage      GeminiUsage       `json:"usageMetadata,omitempty"`
}

// GeminiCandidate is one response candidate.
type GeminiCandidate struct {
	Content      GeminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
}

// GeminiUsage carries token counts.
type GeminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int `json:"candidatesTokenCount,omitempty"`
}

// ToGeminiRequest encodes a canonical request into Gemini shape.
func (r Request) ToGeminiRequest() GeminiRequest {
	out := GeminiRequest{}
	if r.System != "" {
		out.SystemInstruction = &GeminiContent{
			Parts: []GeminiPart{{Text: r.System}},
		}
	}
	for _, m := range r.Messages {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		gc := GeminiContent{Role: role}
		for _, p := range m.Parts {
			switch p.Kind {
			case ContentText:
				if p.Text != "" {
					gc.Parts = append(gc.Parts, GeminiPart{Text: p.Text})
				}
			case ContentImage:
				if p.ImageURL == "" {
					continue
				}
				if strings.HasPrefix(p.ImageURL, "data:") {
					mt, data := splitDataURL(p.ImageURL)
					gc.Parts = append(gc.Parts, GeminiPart{InlineData: &GeminiInlineData{MimeType: mt, Data: data}})
				} else {
					gc.Parts = append(gc.Parts, GeminiPart{Text: p.ImageURL})
				}
			case ContentToolCall:
				args := p.ToolInput
				if args == nil {
					args = map[string]any{}
				}
				gc.Role = "model"
				gc.Parts = append(gc.Parts, GeminiPart{FunctionCall: &GeminiFunctionCall{Name: p.ToolName, Args: args}})
			case ContentToolResult:
				gc.Parts = append(gc.Parts, GeminiPart{FunctionResp: &GeminiFunctionResp{
					Name: p.ToolName, Response: map[string]any{"result": p.ToolContent},
				}})
			case ContentReasoning:
				if p.Text != "" {
					gc.Parts = append(gc.Parts, GeminiPart{Text: p.Text})
				}
			}
		}
		if len(gc.Parts) > 0 {
			out.Contents = append(out.Contents, gc)
		}
	}
	decls := []GeminiFunctionDecl{}
	for _, t := range r.Tools {
		params := t.Parameters
		if params == nil {
			params = map[string]any{"type": "object"}
		}
		decls = append(decls, GeminiFunctionDecl{Name: t.Name, Description: t.Description, Parameters: params})
	}
	if len(decls) > 0 {
		out.Tools = []GeminiTool{{FunctionDeclarations: decls}}
	}
	switch {
	case r.ToolChoice == "required":
		out.ToolConfig = &GeminiToolConfig{FunctionCallingConfig: map[string]any{"mode": "ANY"}}
	case r.ToolChoice == "none":
		out.ToolConfig = &GeminiToolConfig{FunctionCallingConfig: map[string]any{"mode": "NONE"}}
	case strings.HasPrefix(r.ToolChoice, "named:"):
		if name := strings.TrimPrefix(r.ToolChoice, "named:"); name != "" {
			out.ToolConfig = &GeminiToolConfig{FunctionCallingConfig: map[string]any{
				"mode": "ANY", "allowedFunctionNames": []string{name},
			}}
		}
	}
	gen := map[string]any{}
	if r.Temperature != nil {
		gen["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		gen["topP"] = *r.TopP
	}
	if r.MaxTokens > 0 {
		gen["maxOutputTokens"] = r.MaxTokens
	}
	if len(r.Stop) > 0 {
		gen["stopSequences"] = r.Stop
	}
	if len(gen) > 0 {
		out.GenerationConfig = gen
	}
	return out
}

// FromGeminiResponse decodes a Gemini response into canonical form.
func FromGeminiResponse(in GeminiResponse) (Response, error) {
	out := Response{
		InputTokens:  in.Usage.PromptTokenCount,
		OutputTokens: in.Usage.CandidatesTokenCount,
	}
	if len(in.Candidates) == 0 {
		return out, fmt.Errorf("gemini response has no candidates")
	}
	cand := in.Candidates[0]
	out.StopReason = geminiFinish(cand.FinishReason)
	texts := []string{}
	for _, part := range cand.Content.Parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
		if part.FunctionCall != nil {
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				Name: part.FunctionCall.Name, Arguments: part.FunctionCall.Args,
			})
		}
	}
	out.Text = strings.Join(texts, "\n")
	return out, nil
}

func geminiFinish(reason string) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION":
		return "content_filter"
	default:
		if reason == "" {
			return "stop"
		}
		return strings.ToLower(reason)
	}
}

func splitDataURL(u string) (mime, data string) {
	// data:<mime>;base64,<payload>
	rest := strings.TrimPrefix(u, "data:")
	parts := strings.SplitN(rest, ",", 2)
	if len(parts) != 2 {
		return "image/png", ""
	}
	meta := parts[0]
	data = parts[1]
	mime = "image/png"
	if i := strings.Index(meta, ";"); i > 0 {
		mime = meta[:i]
	} else if meta != "" && !strings.Contains(meta, ";") {
		mime = meta
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		if _, err2 := base64.URLEncoding.DecodeString(data); err2 != nil {
			return mime, data
		}
	}
	return mime, data
}
