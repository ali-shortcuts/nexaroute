package canonical

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------- OpenAI Responses API <-> Canonical ----------

// ResponsesRequest is a permissive subset of POST /v1/responses.
type ResponsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       json.RawMessage `json:"instructions,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	MaxOutputTokens    int             `json:"max_output_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	Tools              []ResponsesTool `json:"tools,omitempty"`
	ToolChoice         any             `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	Reasoning          json.RawMessage `json:"reasoning,omitempty"`
	Text               json.RawMessage `json:"text,omitempty"`
	Metadata           map[string]any  `json:"metadata,omitempty"`
	Stop               any             `json:"stop,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	IncludeUsageHint   bool            `json:"-"`
}

type ResponsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
}

// ResponsesItem is one input/output item of the Responses API.
type ResponsesItem struct {
	Type      string           `json:"type,omitempty"`
	Role      string           `json:"role,omitempty"`
	Content   []map[string]any `json:"content,omitempty"`
	Name      string           `json:"name,omitempty"`    // function name on function_call
	CallID    string           `json:"call_id,omitempty"` // function_call / function_call_output
	Output    string           `json:"output,omitempty"`  // function_call_output payload
	Arguments string           `json:"arguments,omitempty"`
	ID        string           `json:"id,omitempty"`
	Status    string           `json:"status,omitempty"`
	Summary   []map[string]any `json:"summary,omitempty"`
}

// DecodeResponsesRequest converts an OpenAI Responses request into the IR.
// Input accepts either a plain string, a list of content strings/objects, or
// the full item list (message / function_call / function_call_output items).
func DecodeResponsesRequest(in ResponsesRequest) (Request, error) {
	if in.PreviousResponseID != "" {
		return Request{}, fmt.Errorf("previous_response_id is not supported; send the complete conversation in input")
	}
	if in.Store != nil && *in.Store {
		return Request{}, fmt.Errorf("stored Responses are not supported; use store=false")
	}

	out := Request{
		Model:           in.Model,
		Stream:          in.Stream,
		MaxOutputTokens: in.MaxOutputTokens,
		Temperature:     in.Temperature,
		TopP:            in.TopP,
		Stop:            NormalizeStop(in.Stop),
		ClientProtocol:  "openai_responses",
	}
	if len(in.Instructions) > 0 && string(in.Instructions) != "null" {
		var text string
		if err := json.Unmarshal(in.Instructions, &text); err == nil && text != "" {
			out.System = append(out.System, Part{Type: PartText, Text: text})
		}
	}
	parts, messages, err := decodeResponsesInput(in.Input)
	if err != nil {
		return out, err
	}
	if len(parts) > 0 {
		out.Messages = append(out.Messages, Message{Role: RoleUser, Parts: parts})
	}
	out.Messages = append(out.Messages, messages...)
	for _, t := range in.Tools {
		if t.Type == "function" || t.Type == "" {
			schema := t.Parameters
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			b, _ := json.Marshal(schema)
			out.Tools = append(out.Tools, ToolDef{Name: t.Name, Description: t.Description, Parameters: b})
		}
		if t.Type != "function" && t.Type != "" {
			return Request{}, fmt.Errorf("tool type %q is not supported by this gateway", t.Type)
		}
	}
	out.ToolChoice = decodeOpenAIToolChoice(in.ToolChoice)
	out.ParallelToolCalls = in.ParallelToolCalls
	if len(in.Reasoning) > 0 && string(in.Reasoning) != "null" {
		var r struct {
			Effort  string `json:"effort"`
			Summary any    `json:"summary"`
		}
		if json.Unmarshal(in.Reasoning, &r) == nil {
			if r.Effort == "" {
				r.Effort = "medium"
			}
			out.Reasoning = &Reasoning{Effort: r.Effort}
		}
	}
	if len(in.Text) > 0 && string(in.Text) != "null" {
		var tf struct {
			Format json.RawMessage `json:"format"`
		}
		if json.Unmarshal(in.Text, &tf) == nil && len(tf.Format) > 0 {
			out.ResponseFormat = decodeResponsesFormat(tf.Format)
		}
	}
	if len(in.Metadata) > 0 {
		out.Metadata = in.Metadata
	}
	return out, nil
}

func decodeResponsesFormat(raw json.RawMessage) *ResponseFormat {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return nil
	}
	switch probe.Type {
	case "json_object":
		return &ResponseFormat{Kind: FormatJSONObject}
	case "json_schema":
		var js struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
		}
		_ = json.Unmarshal(raw, &js)
		return &ResponseFormat{Kind: FormatJSONSchema, Name: js.Name, Schema: js.Schema}
	}
	return nil
}

func decodeResponsesInput(raw json.RawMessage) ([]Part, []Message, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil, nil
		}
		return []Part{{Type: PartText, Text: text}}, nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, fmt.Errorf("invalid Responses input: %w", err)
	}
	var standalone []Part
	var messages []Message
	for _, itemRaw := range items {
		// An item may be a plain string or a full object.
		var str string
		if err := json.Unmarshal(itemRaw, &str); err == nil {
			if str != "" {
				standalone = append(standalone, Part{Type: PartText, Text: str})
			}
			continue
		}
		var item ResponsesItem
		if err := json.Unmarshal(itemRaw, &item); err != nil {
			return nil, nil, fmt.Errorf("invalid Responses input item: %w", err)
		}
		typ := item.Type
		if typ == "" {
			if item.CallID != "" && item.Output != "" {
				typ = "function_call_output"
			} else if item.Name != "" {
				typ = "function_call"
			} else {
				typ = "message"
			}
		}
		switch typ {
		case "message":
			role := item.Role
			if role == "" {
				role = RoleUser
			}
			parts := responsesContentToParts(item.Content)
			if len(parts) > 0 {
				messages = append(messages, Message{Role: role, Parts: parts})
			}
		case "function_call":
			messages = append(messages, Message{Role: RoleAssistant, Parts: []Part{{
				Type:     PartToolCall,
				ToolCall: &ToolCall{ID: item.CallID, Name: item.Name, Arguments: item.Arguments},
			}}})
		case "function_call_output":
			messages = append(messages, Message{Role: RoleTool, Parts: []Part{{
				Type:       PartToolResult,
				ToolResult: &ToolResult{ToolUseID: item.CallID, Content: item.Output},
			}}})
		case "reasoning":
			// Reasoning items are replay-state only; ignored.
		default:
			// Unknown item types are ignored.
		}
	}
	return standalone, messages, nil
}

func responsesContentToParts(content []map[string]any) []Part {
	parts := make([]Part, 0, len(content))
	for _, c := range content {
		typ, _ := c["type"].(string)
		switch typ {
		case "", "input_text", "output_text", "text":
			txt, _ := c["text"].(string)
			if txt != "" {
				parts = append(parts, Part{Type: PartText, Text: txt})
			}
		case "input_image", "image_url":
			img := decodeOpenAIImage(c)
			if img != nil {
				parts = append(parts, Part{Type: PartImage, Image: img})
			}
		}
	}
	return parts
}

// EncodeResponsesRequest builds an OpenAI Responses payload from the IR.
func EncodeResponsesRequest(in Request, upstreamModel string) ([]byte, error) {
	items := make([]map[string]any, 0, len(in.Messages)+2)
	if txt := partsToText(in.System); strings.TrimSpace(txt) != "" {
		items = append(items, map[string]any{"role": "system", "type": "message", "content": txt})
	}
	for _, m := range in.Messages {
		switch m.Role {
		case RoleAssistant:
			var text strings.Builder
			for _, p := range m.Parts {
				switch p.Type {
				case PartText:
					if p.Text != "" {
						if text.Len() > 0 {
							text.WriteByte('\n')
						}
						text.WriteString(p.Text)
					}
				case PartToolCall:
					if p.ToolCall != nil && p.ToolCall.Name != "" {
						args := p.ToolCall.Arguments
						if strings.TrimSpace(args) == "" {
							args = "{}"
						}
						items = append(items, map[string]any{
							"type": "function_call", "call_id": p.ToolCall.ID,
							"name": p.ToolCall.Name, "arguments": args,
						})
					}
				}
			}
			if text.Len() > 0 {
				items = append(items, map[string]any{"type": "message", "role": "assistant", "content": text.String()})
			}
		case RoleTool:
			for _, p := range m.Parts {
				if p.Type == PartToolResult && p.ToolResult != nil {
					items = append(items, map[string]any{
						"type": "function_call_output", "call_id": p.ToolResult.ToolUseID,
						"output": p.ToolResult.Content,
					})
				}
			}
		default:
			parts := m.Parts
			if len(parts) == 0 {
				continue
			}
			if len(parts) == 1 && parts[0].Type == PartText {
				items = append(items, map[string]any{"type": "message", "role": m.Role, "content": parts[0].Text})
				continue
			}
			content := make([]map[string]any, 0, len(parts))
			for _, p := range parts {
				switch p.Type {
				case PartText:
					content = append(content, map[string]any{"type": "input_text", "text": p.Text})
				case PartImage:
					if p.Image == nil {
						continue
					}
					u := p.Image.URL
					if u == "" && p.Image.Data != "" {
						mt := p.Image.MediaType
						if mt == "" {
							mt = "image/png"
						}
						u = "data:" + mt + ";base64," + p.Image.Data
					}
					if u != "" {
						content = append(content, map[string]any{"type": "input_image", "image_url": u})
					}
				}
			}
			if len(content) > 0 {
				items = append(items, map[string]any{"type": "message", "role": m.Role, "content": content})
			}
		}
	}
	out := map[string]any{
		"model":  upstreamModel,
		"input":  items,
		"stream": in.Stream,
	}
	if in.MaxOutputTokens > 0 {
		out["max_output_tokens"] = in.MaxOutputTokens
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if len(in.Stop) > 0 {
		out["stop"] = in.Stop
	}
	if len(in.Tools) > 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		for _, t := range in.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": rawOrEmptyObject(t.Parameters)})
		}
		out["tools"] = tools
	}
	if in.ToolChoice != nil {
		switch in.ToolChoice.Mode {
		case "auto", "none", "required":
			out["tool_choice"] = in.ToolChoice.Mode
		case "tool":
			out["tool_choice"] = map[string]any{"type": "function", "name": in.ToolChoice.Name}
		}
	}
	if in.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *in.ParallelToolCalls
	}
	if in.Reasoning != nil && !in.Reasoning.Disabled {
		effort := in.Reasoning.Effort
		if effort == "" {
			effort = "medium"
		}
		out["reasoning"] = map[string]any{"effort": effort}
	}
	if in.ResponseFormat != nil {
		switch in.ResponseFormat.Kind {
		case FormatJSONObject:
			out["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
		case FormatJSONSchema:
			fmtPart := map[string]any{"type": "json_schema"}
			if in.ResponseFormat.Name != "" {
				fmtPart["name"] = in.ResponseFormat.Name
			}
			fmtPart["schema"] = rawOrEmptyObject(in.ResponseFormat.Schema)
			out["text"] = map[string]any{"format": fmtPart}
		}
	}
	return json.Marshal(out)
}

func rawOrEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{"type":"object"}`)
	}
	return raw
}

// ResponsesResponse is a permissive decoding target for /v1/responses output.
type ResponsesResponse struct {
	ID                string                `json:"id"`
	Model             string                `json:"model"`
	Status            string                `json:"status"`
	Output            []ResponsesOutputItem `json:"output"`
	Usage             *ResponsesUsage       `json:"usage,omitempty"`
	Error             map[string]any        `json:"error,omitempty"`
	IncompleteDetails map[string]any        `json:"incomplete_details,omitempty"`
}

type ResponsesOutputItem struct {
	Type      string           `json:"type"`
	ID        string           `json:"id,omitempty"`
	Role      string           `json:"role,omitempty"`
	Status    string           `json:"status,omitempty"`
	Content   []map[string]any `json:"content,omitempty"`
	Summary   []map[string]any `json:"summary,omitempty"`
	Name      string           `json:"name,omitempty"`
	CallID    string           `json:"call_id,omitempty"`
	Arguments string           `json:"arguments,omitempty"`
}

type ResponsesUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details,omitempty"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
}

// DecodeResponsesResponse converts a /v1/responses body into the IR.
func DecodeResponsesResponse(b []byte) (Response, error) {
	var in ResponsesResponse
	if err := json.Unmarshal(b, &in); err != nil {
		return Response{}, fmt.Errorf("invalid Responses body: %w", err)
	}
	if in.Status == "failed" || in.Status == "cancelled" || in.Status == "queued" || in.Status == "in_progress" {
		return Response{}, fmt.Errorf("upstream Responses status %q is not a completed synchronous response", in.Status)
	}
	if in.Output == nil {
		return Response{}, fmt.Errorf("upstream Responses output is missing")
	}
	out := Response{ID: in.ID, Model: in.Model, StopReason: StopEndTurn, Raw: append(json.RawMessage(nil), b...)}
	for _, item := range in.Output {
		switch item.Type {
		case "message":
			var sb strings.Builder
			for _, c := range item.Content {
				typ, _ := c["type"].(string)
				if typ == "" || typ == "output_text" || typ == "text" {
					if txt, _ := c["text"].(string); txt != "" {
						if sb.Len() > 0 {
							sb.WriteByte('\n')
						}
						sb.WriteString(txt)
					}
				}
			}
			if sb.Len() > 0 {
				out.Blocks = append(out.Blocks, Block{Type: PartText, Text: sb.String()})
			}
		case "function_call":
			out.Blocks = append(out.Blocks, Block{Type: PartToolCall, ToolCall: &ToolCall{
				ID: item.CallID, Name: item.Name, Arguments: item.Arguments,
			}})
		case "reasoning":
			var sb strings.Builder
			for _, s := range item.Summary {
				if txt, _ := s["text"].(string); txt != "" {
					if sb.Len() > 0 {
						sb.WriteByte('\n')
					}
					sb.WriteString(txt)
				}
			}
			if sb.Len() > 0 {
				out.Blocks = append(out.Blocks, Block{Type: PartThinking, Thinking: &Thinking{Text: sb.String(), Signature: "summary"}})
			}
		}
	}
	switch in.Status {
	case "incomplete":
		out.StopReason = StopMaxTokens
	}
	if in.Usage != nil {
		out.Usage = Usage{InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens}
		if in.Usage.InputTokensDetails != nil {
			out.Usage.CacheReadTokens = in.Usage.InputTokensDetails.CachedTokens
		}
		if in.Usage.OutputTokensDetails != nil {
			out.Usage.ReasoningTokens = in.Usage.OutputTokensDetails.ReasoningTokens
		}
	}
	if out.Blocks == nil {
		out.Blocks = []Block{}
	}
	return out, nil
}

// EncodeResponsesResponse builds a /v1/responses body from the IR.
func EncodeResponsesResponse(in Response, requestedModel string) ResponsesResponse {
	status := "completed"
	switch in.StopReason {
	case StopMaxTokens:
		status = "incomplete"
	case StopRefusal:
		status = "incomplete"
	}
	out := ResponsesResponse{
		ID: in.ID, Model: requestedModel, Status: status,
		Output: []ResponsesOutputItem{},
		Usage:  &ResponsesUsage{InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens},
	}
	if out.ID == "" {
		out.ID = fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	var textID int
	for _, b := range in.Blocks {
		switch b.Type {
		case PartText:
			textID++
			out.Output = append(out.Output, ResponsesOutputItem{
				Type: "message", ID: fmt.Sprintf("msg_%d", textID), Role: "assistant", Status: "completed",
				Content: []map[string]any{{"type": "output_text", "text": b.Text, "annotations": []any{}}},
			})
		case PartToolCall:
			if b.ToolCall != nil && b.ToolCall.Name != "" {
				callID := b.ToolCall.ID
				if callID == "" {
					callID = fmt.Sprintf("call_%d", textID)
				}
				args := b.ToolCall.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				out.Output = append(out.Output, ResponsesOutputItem{
					Type: "function_call", ID: "fc_" + callID, CallID: callID,
					Name: b.ToolCall.Name, Arguments: args, Status: "completed",
				})
			}
		case PartThinking:
			if b.Thinking != nil && b.Thinking.Text != "" {
				out.Output = append(out.Output, ResponsesOutputItem{
					Type: "reasoning", ID: fmt.Sprintf("rs_%d", textID), Status: "completed",
					Summary: []map[string]any{{"type": "summary_text", "text": b.Thinking.Text}},
				})
			}
		}
	}
	out.Usage.OutputTokensDetails = &struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	}{ReasoningTokens: in.Usage.ReasoningTokens}
	out.Usage.InputTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: in.Usage.CacheReadTokens}
	return out
}

// DecodeResponsesStreamEvent decodes one /v1/responses SSE event into
// canonical stream events. The Responses stream is verbose; only the events
// that carry content, tool calls or terminal state are modeled.
func DecodeResponsesStreamEvent(eventName, data string) ([]StreamEvent, bool, error) {
	d := strings.TrimSpace(data)
	if d == "" {
		return nil, false, nil
	}
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(d), &env); err != nil {
		return nil, false, fmt.Errorf("invalid Responses SSE event: %w", err)
	}
	typ := env.Type
	if typ == "" {
		typ = eventName
	}
	switch typ {
	case "response.output_text.delta":
		var ev struct {
			Delta       string `json:"delta"`
			OutputIndex int    `json:"output_index"`
		}
		if json.Unmarshal([]byte(d), &ev) == nil && ev.Delta != "" {
			return []StreamEvent{{Type: StreamText, Text: ev.Delta}}, false, nil
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		var ev struct {
			Delta       string `json:"delta"`
			OutputIndex int    `json:"output_index"`
		}
		if json.Unmarshal([]byte(d), &ev) == nil && ev.Delta != "" {
			return []StreamEvent{{Type: StreamThinking, Text: ev.Delta}}, false, nil
		}
	case "response.output_item.added":
		var ev struct {
			Item        ResponsesOutputItem `json:"item"`
			OutputIndex int                 `json:"output_index"`
		}
		if json.Unmarshal([]byte(d), &ev) == nil && ev.Item.Type == "function_call" {
			return []StreamEvent{{Type: StreamToolStart, ToolIndex: ev.OutputIndex, ToolID: ev.Item.CallID, ToolName: ev.Item.Name}}, false, nil
		}
	case "response.function_call_arguments.delta":
		var ev struct {
			Delta       string `json:"delta"`
			OutputIndex int    `json:"output_index"`
		}
		if json.Unmarshal([]byte(d), &ev) == nil && ev.Delta != "" {
			return []StreamEvent{{Type: StreamToolDelta, ToolIndex: ev.OutputIndex, ArgsDelta: ev.Delta}}, false, nil
		}
	case "response.output_item.done":
		var ev struct {
			Item        ResponsesOutputItem `json:"item"`
			OutputIndex int                 `json:"output_index"`
		}
		if json.Unmarshal([]byte(d), &ev) == nil && ev.Item.Type == "function_call" {
			return []StreamEvent{{Type: StreamToolEnd, ToolIndex: ev.OutputIndex}}, false, nil
		}
	case "response.failed", "error":
		var ev struct {
			Response struct {
				Error map[string]any `json:"error"`
			} `json:"response"`
		}
		_ = json.Unmarshal([]byte(d), &ev)
		msg := "responses stream failed"
		code := ""
		if ev.Response.Error != nil {
			if m, ok := ev.Response.Error["message"].(string); ok {
				msg = m
			}
			if c, ok := ev.Response.Error["code"].(string); ok {
				code = c
			}
		}
		return []StreamEvent{{Type: StreamError, ErrorCode: code, ErrorMsg: msg}}, true, nil
	case "response.completed", "response.done", "response.incomplete":
		var ev struct {
			Response struct {
				Usage *ResponsesUsage `json:"usage"`
			} `json:"response"`
		}
		_ = json.Unmarshal([]byte(d), &ev)
		var events []StreamEvent
		if ev.Response.Usage != nil {
			u := Usage{InputTokens: ev.Response.Usage.InputTokens, OutputTokens: ev.Response.Usage.OutputTokens}
			if ev.Response.Usage.InputTokensDetails != nil {
				u.CacheReadTokens = ev.Response.Usage.InputTokensDetails.CachedTokens
			}
			if ev.Response.Usage.OutputTokensDetails != nil {
				u.ReasoningTokens = ev.Response.Usage.OutputTokensDetails.ReasoningTokens
			}
			events = append(events, StreamEvent{Type: StreamUsage, Usage: &u})
		}
		stop := StopEndTurn
		if typ == "response.incomplete" {
			stop = StopMaxTokens
		}
		events = append(events, StreamEvent{Type: StreamEnd, StopReason: stop})
		return events, true, nil
	}
	return nil, false, nil
}
