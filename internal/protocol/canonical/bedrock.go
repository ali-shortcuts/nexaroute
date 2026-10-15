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
	// CachePoint is a Converse cache checkpoint (CachePointBlock):
	// a standalone union member placed immediately AFTER the
	// content it caches. Nil unless the IR part carried a
	// prompt-cache marker, so unmarked requests encode
	// byte-identically to before.
	CachePoint *BedrockCachePoint `json:"cachePoint,omitempty"`
}

// BedrockCachePoint is the Converse CachePointBlock shape
// (API_runtime_CachePointBlock): {"type": "default"} with an
// optional "ttl" of "5m" | "1h".
type BedrockCachePoint struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// BedrockMaxCacheCheckpoints caps the cachePoint blocks emitted per
// Converse request. The AWS prompt-caching table allows at most 4
// cache checkpoints per request for the Claude and Nova families
// (minimums 512-4096 tokens per checkpoint depending on model;
// checkpoints are evaluated tools -> system -> messages in order).
// Markers beyond the cap are dropped while their content is kept
// (documented limit, same surfacing as the other encoder drops in
// this file).
const BedrockMaxCacheCheckpoints = 4

// bedrockCachePoint maps an IR prompt-cache marker onto a Converse
// CachePointBlock. The IR carries "ephemeral" (Anthropic default),
// "5m", or "1h" (see anthCacheValue); Converse defaults to the
// 5-minute behavior when "ttl" is omitted, so only "1h" sets it —
// omitting the field is valid on every model, while sending
// "ttl": "1h" to a 5-minute-only model is a ValidationException
// (caller's responsibility, documented in COMPATIBILITY.md).
func bedrockCachePoint(marker string) BedrockCachePoint {
	if strings.TrimSpace(marker) == "1h" {
		return BedrockCachePoint{Type: "default", TTL: "1h"}
	}
	return BedrockCachePoint{Type: "default"}
}

// appendBedrockCachePoint emits the checkpoint for one marked part
// immediately after its content block. When the per-request cap is
// exhausted the marker is dropped and the content is left untouched:
// the returned count is unchanged and no block is appended.
func appendBedrockCachePoint(blocks []BedrockContentBlock, marker string, emitted int) ([]BedrockContentBlock, int) {
	if marker == "" || emitted >= BedrockMaxCacheCheckpoints {
		return blocks, emitted
	}
	cp := bedrockCachePoint(marker)
	return append(blocks, BedrockContentBlock{CachePoint: &cp}), emitted + 1
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
//
// Prompt-cache markers (Part.CacheControl, decoded from Anthropic
// cache_control) become standalone {"cachePoint": ...} blocks placed
// immediately after the marked content, in system and in message
// content (text and tool_use/tool_result blocks alike — all are
// ContentBlock union members alongside cachePoint). The same encoder
// serves streaming (ConverseStream) and non-streaming (Converse)
// dispatches; only the URL path differs (BedrockModelPath).
func EncodeBedrockRequest(in Request, upstreamModel string) ([]byte, string, error) {
	out := BedrockRequest{}
	emitted := 0 // cachePoint checkpoints emitted so far this request
	for _, part := range in.System {
		if part.Type == PartText && part.Text != "" {
			out.System = append(out.System, BedrockContentBlock{Text: part.Text})
			out.System, emitted = appendBedrockCachePoint(out.System, part.CacheControl, emitted)
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
					bm.Content, emitted = appendBedrockCachePoint(bm.Content, part.CacheControl, emitted)
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
					// Image parts rarely carry markers (the Anthropic
					// decoder does not set them), but a directly built
					// IR may; honor it like any other content block.
					bm.Content, emitted = appendBedrockCachePoint(bm.Content, part.CacheControl, emitted)
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
					bm.Content, emitted = appendBedrockCachePoint(bm.Content, part.CacheControl, emitted)
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
					bm.Content, emitted = appendBedrockCachePoint(bm.Content, part.CacheControl, emitted)
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
	//
	// Two further documented limits on prompt caching: the Converse
	// Tool union also accepts a cachePoint member, but the IR carries
	// no per-tool-definition marker (ToolDef has no CacheControl, and
	// the Anthropic decoder does not parse one), so no checkpoint is
	// emitted in toolConfig; and per-checkpoint minimum token counts
	// (512-4096 depending on model) are enforced server-side — Bedrock
	// silently skips caching for a below-threshold prefix while still
	// returning 200, so the encoder never drops content for it.
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
		InputTokens           int `json:"inputTokens"`
		OutputTokens          int `json:"outputTokens"`
		CacheReadInputTokens  int `json:"cacheReadInputTokens,omitempty"`
		CacheWriteInputTokens int `json:"cacheWriteInputTokens,omitempty"`
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
	out.Usage = Usage{InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens,
		CacheReadTokens: in.Usage.CacheReadInputTokens, CacheWriteTokens: in.Usage.CacheWriteInputTokens}
	// Note the Bedrock convention (prompt-caching user guide): when
	// caching applies, inputTokens counts only NON-cached tokens, so
	// total input = inputTokens + cacheReadInputTokens +
	// cacheWriteInputTokens. The wire values are preserved verbatim
	// here; per-dialect billing conventions live with the cost layer.
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
				InputTokens           int `json:"inputTokens"`
				OutputTokens          int `json:"outputTokens"`
				CacheReadInputTokens  int `json:"cacheReadInputTokens,omitempty"`
				CacheWriteInputTokens int `json:"cacheWriteInputTokens,omitempty"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, false, fmt.Errorf("invalid Bedrock metadata event: %w", err)
		}
		return []StreamEvent{{Type: StreamUsage, Usage: &Usage{
			InputTokens: p.Usage.InputTokens, OutputTokens: p.Usage.OutputTokens,
			CacheReadTokens: p.Usage.CacheReadInputTokens, CacheWriteTokens: p.Usage.CacheWriteInputTokens,
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
