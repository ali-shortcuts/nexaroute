package canonical

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---------- Gemini GenerateContent ingress <-> Canonical ----------
//
// This file is the CLIENT side of the Gemini dialect: it decodes a
// GenerateContent request sent by a Gemini-protocol client (Gemini CLI via
// the @google/genai SDK) into the canonical IR, and encodes a canonical
// response back into GenerateContent shape. It is the mirror of the
// upstream codec in gemini.go (EncodeGeminiRequest / DecodeGeminiResponse);
// the two must stay consistent: anything decoded here must survive a
// round trip through EncodeGeminiRequest without changing meaning.
//
// Fail-closed rules (never fabricated provider features):
//   - candidateCount > 1 is rejected: the gateway serves one candidate.
//   - safetySettings are rejected: they are provider-specific policy and
//     cannot be honored generically across routed upstreams.
//   - non-TEXT responseModalities are rejected.
//   - content parts the IR cannot represent (fileData, unknown shapes)
//     are rejected instead of silently dropped.
//   - unknown top-level/metadata fields are ignored: the SDK sends
//     deployment metadata (labels, cachedContent, thinkingConfig) that
//     carries no request semantics for routing.

// DecodeGeminiIngressRequest converts a GenerateContent request body into
// the canonical IR. The model id comes from the URL path, not the body,
// because Gemini carries it there.
func DecodeGeminiIngressRequest(in GeminiRequest, model string) (Request, error) {
	if strings.TrimSpace(model) == "" {
		return Request{}, fmt.Errorf("model is required")
	}
	if len(in.Contents) == 0 {
		return Request{}, fmt.Errorf("contents are required")
	}
	gc := in.GenerationConfig
	if gc != nil {
		if gc.CandidateCount > 1 {
			return Request{}, fmt.Errorf("candidateCount %d is not supported; only a single candidate is served", gc.CandidateCount)
		}
		for _, m := range gc.ResponseModalities {
			if strings.ToUpper(strings.TrimSpace(m)) != "TEXT" {
				return Request{}, fmt.Errorf("responseModality %q is not supported; only TEXT is served", m)
			}
		}
	}
	if len(in.SafetySettings) > 0 {
		return Request{}, fmt.Errorf("safetySettings are not supported through the gateway; remove them from the request")
	}
	out := Request{Model: model, ClientProtocol: "gemini"}
	if in.SystemInstruction != nil {
		for _, p := range in.SystemInstruction.Parts {
			if p.Text == "" {
				return Request{}, fmt.Errorf("unsupported systemInstruction part: only text parts are supported")
			}
			out.System = append(out.System, Part{Type: PartText, Text: p.Text})
		}
	}
	for ci, c := range in.Contents {
		role := strings.ToLower(strings.TrimSpace(c.Role))
		if role == "" {
			// Gemini permits an omitted role on the first content only;
			// a later role-less turn would be misrouted as user input.
			if ci != 0 {
				return Request{}, fmt.Errorf("contents[%d]: role is required", ci)
			}
			role = "user"
		}
		var msgRole string
		switch role {
		case "user":
			msgRole = RoleUser
		case "model":
			msgRole = RoleAssistant
		case "function":
			msgRole = RoleTool
		default:
			return Request{}, fmt.Errorf("contents[%d]: unsupported role %q", ci, c.Role)
		}
		msg := Message{Role: msgRole}
		for pi, p := range c.Parts {
			part, err := geminiIngressPart(p)
			if err != nil {
				return Request{}, fmt.Errorf("contents[%d].parts[%d]: %w", ci, pi, err)
			}
			msg.Parts = append(msg.Parts, part)
		}
		if len(msg.Parts) == 0 {
			return Request{}, fmt.Errorf("contents[%d]: at least one part is required", ci)
		}
		out.Messages = append(out.Messages, msg)
	}
	for _, t := range in.Tools {
		for _, decl := range t.FunctionDeclarations {
			name, _ := decl["name"].(string)
			if strings.TrimSpace(name) == "" {
				return Request{}, fmt.Errorf("function declaration without a name is not supported")
			}
			def := ToolDef{Name: name}
			if d, _ := decl["description"].(string); d != "" {
				def.Description = d
			}
			if params, ok := decl["parameters"]; ok && params != nil {
				b, err := json.Marshal(params)
				if err != nil {
					return Request{}, fmt.Errorf("function %q: parameters are not serializable", name)
				}
				var obj map[string]any
				if err := json.Unmarshal(b, &obj); err != nil {
					return Request{}, fmt.Errorf("function %q: parameters must be a JSON object", name)
				}
				def.Parameters = b
			}
			out.Tools = append(out.Tools, def)
		}
	}
	if in.ToolConfig != nil && in.ToolConfig.FunctionCallingConfig != nil {
		fcc := in.ToolConfig.FunctionCallingConfig
		// The SDK spells modes ANY/AUTO/NONE; the REST wire form
		// prefixes them MODE_. Accept both, reject VALIDATED either way.
		mode := strings.ToUpper(strings.TrimSpace(fcc.Mode))
		mode = strings.TrimPrefix(mode, "MODE_")
		switch mode {
		case "", "AUTO":
			out.ToolChoice = &ToolChoice{Mode: "auto"}
		case "NONE":
			out.ToolChoice = &ToolChoice{Mode: "none"}
		case "ANY":
			if len(fcc.AllowedFunctionNames) == 1 {
				out.ToolChoice = &ToolChoice{Mode: "tool", Name: fcc.AllowedFunctionNames[0]}
			} else {
				out.ToolChoice = &ToolChoice{Mode: "required"}
			}
		default:
			return Request{}, fmt.Errorf("unsupported function calling mode %q", fcc.Mode)
		}
	}
	if gc != nil {
		out.Temperature = gc.Temperature
		out.TopP = gc.TopP
		out.TopK = gc.TopK
		out.MaxOutputTokens = gc.MaxOutputTokens
		out.Stop = append(out.Stop, gc.StopSequences...)
		switch strings.ToLower(strings.TrimSpace(gc.ResponseMimeType)) {
		case "", "text/plain":
			if len(gc.ResponseSchema) > 0 {
				return Request{}, fmt.Errorf("responseSchema requires responseMimeType application/json")
			}
		case "application/json":
			if len(gc.ResponseSchema) > 0 {
				out.ResponseFormat = &ResponseFormat{Kind: FormatJSONSchema, Schema: gc.ResponseSchema}
			} else {
				out.ResponseFormat = &ResponseFormat{Kind: FormatJSONObject}
			}
		default:
			return Request{}, fmt.Errorf("unsupported responseMimeType %q", gc.ResponseMimeType)
		}
	}
	return out, nil
}

func geminiIngressPart(p GeminiPart) (Part, error) {
	call := p.FunctionCall
	if call == nil {
		call = p.FunctionCallV1
	}
	resp := p.FunctionResponse
	if resp == nil {
		resp = p.FunctionResponseV1
	}
	blob := p.InlineData
	if blob == nil {
		blob = p.InlineDataV1
	}
	switch {
	case call != nil:
		if strings.TrimSpace(call.Name) == "" {
			return Part{}, fmt.Errorf("functionCall without a name")
		}
		args := strings.TrimSpace(string(call.Args))
		if args == "" {
			args = "{}"
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(args), &obj); err != nil {
			return Part{}, fmt.Errorf("functionCall arguments must be a JSON object")
		}
		return Part{Type: PartToolCall, ToolCall: &ToolCall{Name: call.Name, Arguments: args}}, nil
	case resp != nil:
		if strings.TrimSpace(resp.Name) == "" {
			return Part{}, fmt.Errorf("functionResponse without a name")
		}
		// ToolUseID carries the function name: the upstream encoder
		// resolves it back via toolNameForID (unknown ids pass through).
		content := ""
		if len(resp.Response) > 0 {
			content = string(resp.Response)
		}
		return Part{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: resp.Name, Content: content}}, nil
	case blob != nil:
		if blob.Data == "" {
			return Part{}, fmt.Errorf("inlineData without data")
		}
		mt := blob.Mime()
		if mt == "" {
			mt = "image/png"
		}
		return Part{Type: PartImage, Image: &Image{MediaType: mt, Data: blob.Data}}, nil
	case p.Text != "":
		if p.Thought != nil && *p.Thought {
			return Part{Type: PartThinking, Thinking: &Thinking{Text: p.Text}}, nil
		}
		return Part{Type: PartText, Text: p.Text}, nil
	default:
		// fileData and any other shape the IR cannot represent: fail
		// closed instead of silently dropping client content.
		return Part{}, fmt.Errorf("unsupported content part")
	}
}

// stopReasonToGeminiFinish maps the canonical stop vocabulary onto Gemini
// finishReason values. Gemini ends function-call turns with STOP, and has
// no pause-turn value, so both map to STOP.
func stopReasonToGeminiFinish(reason string) string {
	switch reason {
	case StopMaxTokens:
		return "MAX_TOKENS"
	case StopRefusal:
		return "SAFETY"
	default:
		return "STOP"
	}
}

// EncodeGeminiResponse converts a canonical response into GenerateContent
// shape: one candidate with model-role content, a finishReason, and
// usageMetadata. It fails closed on tool arguments that are not JSON
// objects rather than emitting a malformed functionCall.
func EncodeGeminiResponse(resp Response) (GeminiResponse_, error) {
	parts := make([]GeminiPart, 0, len(resp.Blocks))
	for _, b := range resp.Blocks {
		switch b.Type {
		case PartText:
			if b.Text != "" {
				parts = append(parts, GeminiPart{Text: b.Text})
			}
		case PartThinking:
			if b.Thinking != nil && b.Thinking.Text != "" {
				thought := true
				parts = append(parts, GeminiPart{Text: b.Thinking.Text, Thought: &thought})
			}
		case PartToolCall:
			if b.ToolCall == nil || strings.TrimSpace(b.ToolCall.Name) == "" {
				return GeminiResponse_{}, fmt.Errorf("tool call without a name cannot be encoded")
			}
			args := strings.TrimSpace(b.ToolCall.Arguments)
			if args == "" {
				args = "{}"
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(args), &obj); err != nil {
				return GeminiResponse_{}, fmt.Errorf("tool call arguments must be a JSON object")
			}
			parts = append(parts, GeminiPart{FunctionCall: &GeminiCall{Name: b.ToolCall.Name, Args: json.RawMessage(args)}})
		}
	}
	if len(parts) == 0 {
		parts = []GeminiPart{{Text: ""}}
	}
	out := GeminiResponse_{Candidates: []GeminiCandidate{{
		Content:      &GeminiContent{Role: "model", Parts: parts},
		FinishReason: stopReasonToGeminiFinish(resp.StopReason),
	}}}
	if resp.StopReason == StopRefusal {
		out.PromptFeedback = &struct {
			BlockReason string `json:"blockReason,omitempty"`
		}{BlockReason: "SAFETY"}
	}
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		out.UsageMetadata = &struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
			ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
		}{
			PromptTokenCount:        resp.Usage.InputTokens,
			CandidatesTokenCount:    resp.Usage.OutputTokens,
			CachedContentTokenCount: resp.Usage.CacheReadTokens,
			ThoughtsTokenCount:      resp.Usage.ReasoningTokens,
		}
	}
	return out, nil
}
