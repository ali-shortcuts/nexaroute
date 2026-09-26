package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Bounds for the completion capability. They exist so one evaluation-shaped
// call can never read or hold unbounded data.
const (
	// MaxCompletionPromptBytes bounds a prompt or system message sent to the
	// model.
	MaxCompletionPromptBytes = 64 << 10
	// maxCompletionReadBytes bounds one upstream completion response body.
	maxCompletionReadBytes = 1 << 20
	// maxCompletionTextBytes bounds the extracted assistant text.
	maxCompletionTextBytes = 64 << 10
	// maxCompletionToolCalls bounds extracted tool calls (only the first is
	// judged today; the rest are dropped, never accumulated).
	maxCompletionToolCalls = 8
	// maxCompletionToolArgsBytes bounds one tool-call argument payload.
	maxCompletionToolArgsBytes = 64 << 10
	// DefaultCompletionMaxTokens keeps a completion cheap when the caller does
	// not declare an output budget.
	DefaultCompletionMaxTokens = 256
	// MaxCompletionTokens bounds the requested completion size.
	MaxCompletionTokens = 4096
)

// CompletionRequest is one bounded, non-streaming, single-turn completion
// against an explicitly named model on this adapter's provider. The Phase H
// live evaluation executor uses it to measure one physical deployment
// directly. It reuses the adapter's transport, endpoint, authentication,
// credential selection and rate-limit observation exactly like the data
// plane — no second client stack — without passing through routing, health,
// cache or session state (those live in the request-path handlers, which the
// evaluation plane never enters).
type CompletionRequest struct {
	Model     string
	System    string
	Prompt    string
	MaxTokens int
}

// CompletionToolCall is one tool invocation found in a completion.
type CompletionToolCall struct {
	Name      string
	Arguments string
}

// Completion is the bounded, protocol-neutral result of one completion call.
type Completion struct {
	StatusCode   int
	Latency      time.Duration
	Text         string
	ToolCalls    []CompletionToolCall
	Refusal      bool
	InputTokens  int
	OutputTokens int
}

// Completer is an optional Adapter capability: one bounded non-streaming
// completion. The HTTP adapter implements it for every supported provider
// type. Callers must type-assert: an adapter without it simply cannot serve
// live evaluation.
type Completer interface {
	Complete(ctx context.Context, req CompletionRequest) (Completion, error)
}

// Complete performs one bounded, non-streaming completion through the existing
// provider adapter. Protocol shaping (payload and path) mirrors the adapter's
// own request path per provider type.
//
// Errors: transport/context failures are returned as errors. Non-2xx HTTP
// responses return a Completion carrying StatusCode plus an error whose text
// is credential-redacted (safeSnippet); callers decide whether the status is
// evidence (HTTP 500) or a hard failure. The error text is never stored by
// the caller-side evaluation runner.
func (a *httpAdapter) Complete(ctx context.Context, req CompletionRequest) (Completion, error) {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		return Completion{}, errors.New("completion requires a model")
	}
	if strings.TrimSpace(req.Prompt) == "" && strings.TrimSpace(req.System) == "" {
		return Completion{}, errors.New("completion requires a prompt")
	}
	if len(req.Prompt) > MaxCompletionPromptBytes || len(req.System) > MaxCompletionPromptBytes {
		return Completion{}, errors.New("completion prompt exceeds safe limit")
	}
	maxTokens := req.MaxTokens
	if maxTokens < 1 {
		maxTokens = DefaultCompletionMaxTokens
	}
	if maxTokens > MaxCompletionTokens {
		maxTokens = MaxCompletionTokens
	}

	payload, path, err := a.completionPayload(model, req.System, req.Prompt, maxTokens)
	if err != nil {
		return Completion{}, err
	}
	start := time.Now()
	resp, err := a.DoPath(ctx, http.MethodPost, path, payload, false, nil)
	lat := time.Since(start)
	if err != nil {
		return Completion{Latency: lat}, err
	}
	defer resp.Body.Close()
	c := Completion{StatusCode: resp.StatusCode, Latency: lat}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxCompletionReadBytes+1))
	if readErr != nil {
		return c, fmt.Errorf("completion response read: %w", readErr)
	}
	if len(data) > maxCompletionReadBytes {
		return c, fmt.Errorf("completion response exceeds %d bytes", maxCompletionReadBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Credentials are always redacted; the body itself may echo whatever
		// the caller sent, so the caller must treat this text as untrusted
		// diagnostic output that is dropped after classification.
		return c, fmt.Errorf("completion http %d: %s", resp.StatusCode, a.safeSnippet(data))
	}
	return decodeCompletion(a.p.Type, data, c)
}

// completionPayload builds the wire payload and path for one completion per
// provider type, mirroring the exact shapes the adapter already sends on the
// data plane (and in Probe).
func (a *httpAdapter) completionPayload(model, system, prompt string, maxTokens int) ([]byte, string, error) {
	var body map[string]any
	var path string
	switch a.p.Type {
	case "gemini":
		content := map[string]any{"role": "user", "parts": []map[string]any{{"text": prompt}}}
		body = map[string]any{
			"contents":         []map[string]any{content},
			"generationConfig": map[string]any{"maxOutputTokens": maxTokens},
		}
		if strings.TrimSpace(system) != "" {
			body["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": system}}}
		}
		path = a.geminiModelPath(model, false)
	case "openai_responses":
		body = map[string]any{
			"model": model, "input": prompt,
			"max_output_tokens": maxTokens, "stream": false,
		}
		if strings.TrimSpace(system) != "" {
			body["instructions"] = system
		}
		path = a.p.ResponsesPath
	case "anthropic_compatible":
		body = map[string]any{
			"model":      model,
			"max_tokens": maxTokens,
			"messages":   []map[string]any{{"role": "user", "content": prompt}},
			"stream":     false,
		}
		if strings.TrimSpace(system) != "" {
			body["system"] = system
		}
		path = a.p.MessagesPath
	default:
		messages := make([]map[string]any, 0, 2)
		if strings.TrimSpace(system) != "" {
			messages = append(messages, map[string]any{"role": "system", "content": system})
		}
		messages = append(messages, map[string]any{"role": "user", "content": prompt})
		body = map[string]any{"model": model, "max_tokens": maxTokens, "messages": messages, "stream": false}
		path = a.p.ChatPath
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("completion request encode: %w", err)
	}
	return b, path, nil
}

func boundedText(s string) string {
	if len(s) > maxCompletionTextBytes {
		return s[:maxCompletionTextBytes]
	}
	return s
}

func boundedToolArgs(s string) string {
	if len(s) > maxCompletionToolArgsBytes {
		return s[:maxCompletionToolArgsBytes]
	}
	return s
}

func appendToolCall(calls []CompletionToolCall, name, args string) []CompletionToolCall {
	if len(calls) >= maxCompletionToolCalls {
		return calls
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 256 {
		return calls
	}
	return append(calls, CompletionToolCall{Name: name, Arguments: boundedToolArgs(args)})
}

// decodeCompletion normalizes one successful upstream body into a bounded
// Completion. Unparseable envelopes are protocol failures, not evidence.
func decodeCompletion(providerType string, data []byte, c Completion) (Completion, error) {
	var err error
	switch providerType {
	case "anthropic_compatible":
		c, err = decodeAnthropicCompletion(data, c)
	case "gemini":
		c, err = decodeGeminiCompletion(data, c)
	case "openai_responses":
		c, err = decodeResponsesCompletion(data, c)
	default:
		c, err = decodeOpenAIChatCompletion(data, c)
	}
	if err != nil {
		return c, err
	}
	c.Text = boundedText(c.Text)
	if len(c.ToolCalls) > maxCompletionToolCalls {
		c.ToolCalls = c.ToolCalls[:maxCompletionToolCalls]
	}
	return c, nil
}

type openAIChatWire struct {
	Choices []struct {
		Message struct {
			Content  json.RawMessage `json:"content"`
			Refusal  string          `json:"refusal"`
			ToolCall []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func decodeOpenAIChatCompletion(data []byte, c Completion) (Completion, error) {
	var w openAIChatWire
	if err := json.Unmarshal(data, &w); err != nil {
		return c, fmt.Errorf("completion decode: %w", err)
	}
	if len(w.Choices) > 0 {
		msg := w.Choices[0].Message
		if len(msg.Content) > 0 {
			var text string
			if err := json.Unmarshal(msg.Content, &text); err == nil {
				c.Text = text
			} else {
				// content may be an array of typed parts
				var parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(msg.Content, &parts); err == nil {
					var b strings.Builder
					for _, p := range parts {
						if p.Type == "text" || p.Type == "output_text" {
							b.WriteString(p.Text)
							if b.Len() >= maxCompletionTextBytes {
								break
							}
						}
					}
					c.Text = b.String()
				}
			}
		}
		if strings.TrimSpace(msg.Refusal) != "" {
			c.Refusal = true
		}
		for _, tc := range msg.ToolCall {
			c.ToolCalls = appendToolCall(c.ToolCalls, tc.Function.Name, tc.Function.Arguments)
		}
	}
	c.InputTokens = w.Usage.PromptTokens
	c.OutputTokens = w.Usage.CompletionTokens
	return c, nil
}

type anthropicWire struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func decodeAnthropicCompletion(data []byte, c Completion) (Completion, error) {
	var w anthropicWire
	if err := json.Unmarshal(data, &w); err != nil {
		return c, fmt.Errorf("completion decode: %w", err)
	}
	var b strings.Builder
	for _, block := range w.Content {
		switch block.Type {
		case "text":
			if b.Len() < maxCompletionTextBytes {
				b.WriteString(block.Text)
			}
		case "tool_use":
			c.ToolCalls = appendToolCall(c.ToolCalls, block.Name, string(block.Input))
		}
	}
	c.Text = b.String()
	if w.StopReason == "refusal" {
		c.Refusal = true
	}
	c.InputTokens = w.Usage.InputTokens
	c.OutputTokens = w.Usage.OutputTokens
	return c, nil
}

type geminiWire struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text         string `json:"text"`
				FunctionCall *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
}

func decodeGeminiCompletion(data []byte, c Completion) (Completion, error) {
	var w geminiWire
	if err := json.Unmarshal(data, &w); err != nil {
		return c, fmt.Errorf("completion decode: %w", err)
	}
	var b strings.Builder
	if len(w.Candidates) > 0 {
		for _, part := range w.Candidates[0].Content.Parts {
			if part.Text != "" && b.Len() < maxCompletionTextBytes {
				b.WriteString(part.Text)
			}
			if part.FunctionCall != nil {
				c.ToolCalls = appendToolCall(c.ToolCalls, part.FunctionCall.Name, string(part.FunctionCall.Args))
			}
		}
	}
	c.Text = b.String()
	c.InputTokens = w.UsageMetadata.PromptTokenCount
	c.OutputTokens = w.UsageMetadata.CandidatesTokenCount
	return c, nil
}

type responsesWire struct {
	Output []struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func decodeResponsesCompletion(data []byte, c Completion) (Completion, error) {
	var w responsesWire
	if err := json.Unmarshal(data, &w); err != nil {
		return c, fmt.Errorf("completion decode: %w", err)
	}
	var b strings.Builder
	for _, item := range w.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if (part.Type == "output_text" || part.Type == "text") && b.Len() < maxCompletionTextBytes {
					b.WriteString(part.Text)
				}
				if part.Type == "refusal" && strings.TrimSpace(part.Text) != "" {
					c.Refusal = true
				}
			}
		case "function_call":
			c.ToolCalls = appendToolCall(c.ToolCalls, item.Name, item.Arguments)
		}
	}
	c.Text = b.String()
	c.InputTokens = w.Usage.InputTokens
	c.OutputTokens = w.Usage.OutputTokens
	return c, nil
}
