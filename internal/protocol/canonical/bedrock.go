package canonical

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---------- Amazon Bedrock Converse <-> Canonical ----------
//
// Bedrock's Converse API differs from the other families in auth
// (SigV4, handled by the provider adapter) and in payload shape:
// content blocks are typed unions, tools live under toolConfig, and
// sampling controls under inferenceConfig. The model id travels in
// the URL path (/model/{modelId}/converse[-stream]).

type BedrockRequest struct {
	Messages        []BedrockMessage        `json:"messages"`
	System          []BedrockContentBlock   `json:"system,omitempty"`
	InferenceConfig *BedrockInferenceConfig `json:"inferenceConfig,omitempty"`
	ToolConfig      *BedrockToolConfig      `json:"toolConfig,omitempty"`
}

type BedrockMessage struct {
	Role    string                `json:"role"`
	Content []BedrockContentBlock `json:"content"`
}

type BedrockContentBlock struct {
	Text       string             `json:"text,omitempty"`
	Image      *BedrockImageBlock `json:"image,omitempty"`
	ToolUse    *BedrockToolUse    `json:"toolUse,omitempty"`
	ToolResult *BedrockToolResult `json:"toolResult,omitempty"`
}

type BedrockImageBlock struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"` // base64
	} `json:"source"`
}

type BedrockToolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type BedrockToolResult struct {
	ToolUseID string                 `json:"toolUseId"`
	Content   []BedrockResultContent `json:"content"`
	Status    string                 `json:"status,omitempty"` // success | error
}

type BedrockResultContent struct {
	Text string          `json:"text,omitempty"`
	JSON json.RawMessage `json:"json,omitempty"`
}

type BedrockInferenceConfig struct {
	MaxTokens     int      `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

type BedrockToolConfig struct {
	Tools      []BedrockTool      `json:"tools"`
	ToolChoice *BedrockToolChoice `json:"toolChoice,omitempty"`
}

type BedrockTool struct {
	ToolSpec struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		InputSchema struct {
			JSON json.RawMessage `json:"json"`
		} `json:"inputSchema"`
	} `json:"toolSpec"`
}

type BedrockToolChoice struct {
	Auto *struct{} `json:"auto,omitempty"`
	Any  *struct{} `json:"any,omitempty"`
	Tool *struct {
		Name string `json:"name"`
	} `json:"tool,omitempty"`
}

// EncodeBedrockRequest builds a Converse payload from the IR. The
// upstream model id is returned separately because Converse carries
// it in the URL path.
func EncodeBedrockRequest(in Request, upstreamModel string) ([]byte, string, error) {
	out := BedrockRequest{}
	for _, part := range in.System {
		if part.Type == PartText && part.Text != "" {
			out.System = append(out.System, BedrockContentBlock{Text: part.Text})
		}
	}
	for _, msg := range in.Messages {
		bm := BedrockMessage{Role: msg.Role}
		if bm.Role != "assistant" {
			bm.Role = "user"
		}
		for _, part := range msg.Parts {
			switch part.Type {
			case PartText:
				if part.Text != "" {
					bm.Content = append(bm.Content, BedrockContentBlock{Text: part.Text})
				}
			case PartImage:
				if part.Image != nil && part.Image.Data != "" {
					format := bedrockImageFormat(part.Image.MediaType)
					if format == "" {
						return nil, "", fmt.Errorf("bedrock: unsupported image media type %q", part.Image.MediaType)
					}
					img := &BedrockImageBlock{Format: format}
					img.Source.Bytes = part.Image.Data
					bm.Content = append(bm.Content, BedrockContentBlock{Image: img})
				}
			case PartToolCall:
				if part.ToolCall != nil {
					input := json.RawMessage(part.ToolCall.Arguments)
					if strings.TrimSpace(part.ToolCall.Arguments) == "" {
						input = json.RawMessage(`{}`)
					}
					bm.Content = append(bm.Content, BedrockContentBlock{ToolUse: &BedrockToolUse{
						ToolUseID: part.ToolCall.ID, Name: part.ToolCall.Name, Input: input,
					}})
				}
			case PartToolResult:
				if part.ToolResult != nil {
					tr := &BedrockToolResult{ToolUseID: part.ToolResult.ToolUseID}
					if part.ToolResult.IsError {
						tr.Status = "error"
					}
					content := part.ToolResult.Content
					var asJSON any
					if json.Unmarshal([]byte(content), &asJSON) == nil && strings.HasPrefix(strings.TrimSpace(content), "{") {
						tr.Content = []BedrockResultContent{{JSON: json.RawMessage(content)}}
					} else {
						tr.Content = []BedrockResultContent{{Text: content}}
					}
					bm.Content = append(bm.Content, BedrockContentBlock{ToolResult: tr})
				}
			}
			// PartThinking is intentionally not encoded: Converse
			// reasoning blocks are model-specific (documented limit).
		}
		if len(bm.Content) > 0 {
			out.Messages = append(out.Messages, bm)
		}
	}
	ic := &BedrockInferenceConfig{
		MaxTokens:     in.MaxOutputTokens,
		Temperature:   in.Temperature,
		TopP:          in.TopP,
		StopSequences: in.Stop,
	}
	if ic.MaxTokens != 0 || ic.Temperature != nil || ic.TopP != nil || len(ic.StopSequences) > 0 {
		out.InferenceConfig = ic
	}
	if len(in.Tools) > 0 && (in.ToolChoice == nil || in.ToolChoice.Mode != "none") {
		tc := &BedrockToolConfig{}
		for _, tool := range in.Tools {
			var bt BedrockTool
			bt.ToolSpec.Name = tool.Name
			bt.ToolSpec.Description = tool.Description
			schema := tool.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			bt.ToolSpec.InputSchema.JSON = schema
			tc.Tools = append(tc.Tools, bt)
		}
		if in.ToolChoice != nil {
			switch in.ToolChoice.Mode {
			case "required":
				tc.ToolChoice = &BedrockToolChoice{Any: &struct{}{}}
			case "tool":
				choice := &BedrockToolChoice{Tool: &struct {
					Name string `json:"name"`
				}{}}
				choice.Tool.Name = in.ToolChoice.Name
				tc.ToolChoice = choice
			default: // auto
				tc.ToolChoice = &BedrockToolChoice{Auto: &struct{}{}}
			}
		}
		out.ToolConfig = tc
	}
	// ToolChoice "none" intentionally drops toolConfig: Converse has
	// no none mode, and withholding the tools is the faithful mapping
	// (documented limit).
	payload, err := json.Marshal(out)
	if err != nil {
		return nil, "", err
	}
	return payload, upstreamModel, nil
}

func bedrockImageFormat(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpeg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	}
	return ""
}

func bedrockStopToCanonical(stop string) string {
	switch stop {
	case "end_turn", "stop_sequence":
		// Converse returns stop_sequence distinctly; the IR keeps the
		// finer value where it exists.
		if stop == "stop_sequence" {
			return StopStopSequence
		}
		return StopEndTurn
	case "tool_use":
		return StopToolUse
	case "max_tokens":
		return StopMaxTokens
	case "content_filtered", "guardrail_intervened":
		return StopRefusal
	default:
		return StopEndTurn
	}
}

type bedrockResponse struct {
	Output struct {
		Message struct {
			Role    string                `json:"role"`
			Content []BedrockContentBlock `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string `json:"stopReason"`
	Usage      struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
	} `json:"usage"`
}

// DecodeBedrockResponse decodes a Converse response into the IR.
func DecodeBedrockResponse(b []byte) (Response, error) {
	var in bedrockResponse
	if err := json.Unmarshal(b, &in); err != nil {
		return Response{}, fmt.Errorf("invalid Bedrock response: %w", err)
	}
	out := Response{StopReason: bedrockStopToCanonical(in.StopReason), Raw: append(json.RawMessage(nil), b...)}
	sawTool := false
	for _, block := range in.Output.Message.Content {
		switch {
		case block.Text != "":
			out.Blocks = append(out.Blocks, Block{Type: PartText, Text: block.Text})
		case block.ToolUse != nil:
			sawTool = true
			args := string(block.ToolUse.Input)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			out.Blocks = append(out.Blocks, Block{Type: PartToolCall, ToolCall: &ToolCall{
				ID: block.ToolUse.ToolUseID, Name: block.ToolUse.Name, Arguments: args,
			}})
		}
	}
	if sawTool && out.StopReason == StopEndTurn {
		out.StopReason = StopToolUse
	}
	out.Usage = Usage{InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens}
	return out, nil
}

// bedrockStreamEnvelope is the shape the provider adapter emits when
// translating the AWS binary event stream into SSE frames:
// {"eventType": "<converse event>", "payload": {<raw event JSON>}}.
type bedrockStreamEnvelope struct {
	EventType string          `json:"eventType"`
	Payload   json.RawMessage `json:"payload"`
}

// DecodeBedrockStreamChunk decodes one translated Converse stream
// event into canonical stream events. The boolean reports terminal
// (messageStop or an exception event).
func DecodeBedrockStreamChunk(data string) ([]StreamEvent, bool, error) {
	d := strings.TrimSpace(data)
	if d == "" {
		return nil, false, nil
	}
	var env bedrockStreamEnvelope
	if err := json.Unmarshal([]byte(d), &env); err != nil {
		return nil, false, fmt.Errorf("invalid Bedrock stream envelope: %w", err)
	}
	switch env.EventType {
	case "messageStart":
		return []StreamEvent{{Type: StreamStart}}, false, nil
	case "contentBlockStart":
		var p struct {
			ContentBlockIndex int `json:"contentBlockIndex"`
			Start             struct {
				ToolUse *struct {
					ToolUseID string `json:"toolUseId"`
					Name      string `json:"name"`
				} `json:"toolUse"`
			} `json:"start"`
		}
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, false, fmt.Errorf("invalid Bedrock contentBlockStart: %w", err)
		}
		if p.Start.ToolUse != nil {
			return []StreamEvent{{Type: StreamToolStart, ToolIndex: p.ContentBlockIndex,
				ToolID: p.Start.ToolUse.ToolUseID, ToolName: p.Start.ToolUse.Name}}, false, nil
		}
		return nil, false, nil
	case "contentBlockDelta":
		var p struct {
			ContentBlockIndex int `json:"contentBlockIndex"`
			Delta             struct {
				Text    string `json:"text"`
				ToolUse *struct {
					Input string `json:"input"`
				} `json:"toolUse"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, false, fmt.Errorf("invalid Bedrock contentBlockDelta: %w", err)
		}
		if p.Delta.ToolUse != nil {
			return []StreamEvent{{Type: StreamToolDelta, ToolIndex: p.ContentBlockIndex, ArgsDelta: p.Delta.ToolUse.Input}}, false, nil
		}
		if p.Delta.Text != "" {
			return []StreamEvent{{Type: StreamText, Text: p.Delta.Text}}, false, nil
		}
		return nil, false, nil
	case "contentBlockStop":
		// Deliberately no event: the frame does not say whether the
		// block was text or a tool call, and this decoder is stateless
		// per chunk — emitting StreamToolEnd for a TEXT block would
		// corrupt the client encoder's tool state (arguments-empty
		// validation failure, observed in testing). Client encoders
		// finalize open tool blocks at StreamEnd (messageStop).
		return nil, false, nil
	case "messageStop":
		var p struct {
			StopReason string `json:"stopReason"`
		}
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, false, fmt.Errorf("invalid Bedrock messageStop: %w", err)
		}
		return []StreamEvent{{Type: StreamEnd, StopReason: bedrockStopToCanonical(p.StopReason)}}, true, nil
	case "metadata":
		var p struct {
			Usage struct {
				InputTokens  int `json:"inputTokens"`
				OutputTokens int `json:"outputTokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, false, fmt.Errorf("invalid Bedrock metadata event: %w", err)
		}
		return []StreamEvent{{Type: StreamUsage, Usage: &Usage{
			InputTokens: p.Usage.InputTokens, OutputTokens: p.Usage.OutputTokens,
		}}}, false, nil
	default:
		// Exception events (throttlingException, modelException, ...)
		// surface as stream errors; anything else is ignored for
		// forward compatibility.
		if strings.HasSuffix(env.EventType, "Exception") || strings.HasSuffix(env.EventType, "exception") {
			return []StreamEvent{{Type: StreamError, ErrorCode: env.EventType, ErrorMsg: string(env.Payload)}}, true, nil
		}
		return nil, false, nil
	}
}

// BedrockModelPath builds the Converse model-in-path endpoint. The
// model id is escaped with the SigV4 unreserved rule (only
// A-Z a-z 0-9 - . _ ~ pass through) so the path on the wire is
// byte-identical to the path the SigV4 signer canonicalizes —
// inference-profile ids contain ':' and ARN forms contain '/',
// both of which must be percent-encoded in a single path segment.
func BedrockModelPath(model string, stream bool) string {
	if stream {
		return "/model/" + bedrockEscapeSegment(model) + "/converse-stream"
	}
	return "/model/" + bedrockEscapeSegment(model) + "/converse"
}

func bedrockEscapeSegment(segment string) string {
	var b strings.Builder
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
