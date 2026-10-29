package nexaflow

import (
	"context"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// nexaroute.llm.chat.v1 — one non-streaming chat completion through the
// existing gateway. SideEffect billable, so Execute requires operator
// policy approval. The node validates the selector through the Backend
// port (route, alias, or deployment must exist) and then hands the call
// to the gateway, which does the actual routing; this package never
// reimplements routing, fallback, or provider selection.
// Retry semantics (Phase 7 hardening): the node is NOT retried.
// A timeout on a billable chat call is an ambiguous outcome — the
// gateway may already have executed and billed it — and this node sends
// no idempotency key, so no retry can prove it is not a duplicate bill.
// An earlier version declared MaxAttempts 2 with retryable code
// "timeout"; that invited exactly the double-billing retry this package
// must not invite. A runtime that needs safe LLM retries must first add
// an idempotency key to the gateway call and re-declare the policy.
// Cancellation: context cancellation aborts the gateway call; a response
// already returned is not un-billed by cancellation.
// The prompt and messages are NEVER logged, stored, or returned: output
// carries only the model text, token counts, and the resolved route.
func llmNodeSpecs() []*NodeSpec {
	return []*NodeSpec{{
		ID: "nexaroute.llm.chat", Version: SupportedVersion, Category: "llm",
		Title:       "LLM chat completion",
		Description: "Non-streaming chat completion via the existing gateway using an explicitly selected route, model alias, or deployment. Billable; requires operator policy approval.",
		InputSchema: []Field{
			{Name: "messages", Type: "array", Description: "Chat messages: objects with role and content. Mutually exclusive with prompt."},
			{Name: "prompt", Type: "string", Description: "Single user prompt, used when messages is absent."},
		},
		OutputSchema: []Field{
			{Name: "model", Type: "string", Description: "Model reported by the gateway."},
			{Name: "text", Type: "string", Description: "Assistant text, truncated to the node output bound. The prompt is never echoed."},
			{Name: "prompt_tokens", Type: "number", Description: "Prompt tokens reported by the gateway."},
			{Name: "completion_tokens", Type: "number", Description: "Completion tokens reported by the gateway."},
			{Name: "total_tokens", Type: "number", Description: "Total tokens reported by the gateway."},
			{Name: "resolved_route", Type: "string", Description: "Validated selector that was sent to the gateway."},
		},
		ConfigSchema: []Field{
			{Name: "route_or_alias", Type: "string", Required: true, Description: "Exactly one selector: an existing virtual-endpoint public model, a deployment model alias, or a deployment id."},
			{Name: "max_tokens", Type: "number", Description: "Maximum completion tokens, 1..8192, default 1024."},
			{Name: "temperature", Type: "number", Description: "Sampling temperature, 0..2, optional."},
		},
		Permission: authz.ReadRouting, SideEffect: SideEffectBillable,
		Retry:          RetryPolicy{MaxAttempts: 1},
		DefaultTimeout: 60 * time.Second, MaxTimeout: 300 * time.Second,
		MaxInputBytes: 1 << 20, MaxOutputBytes: 256 << 10,
		RequiresOperatorPolicy: true,
		Run:                    runLLMChat,
	}}
}

func runLLMChat(ctx context.Context, rc *RunContext) (map[string]any, error) {
	if err := requireBackend(rc); err != nil {
		return nil, err
	}
	selector, ok := getString(rc.Config, "route_or_alias")
	if !ok || strings.TrimSpace(selector) == "" {
		return nil, NewNodeError("validation_error", false, "config route_or_alias is required")
	}
	maxTokens := 1024
	if v, ok := getNumber(rc.Config, "max_tokens"); ok {
		if !isFinite(v) || v < 1 || v > 8192 {
			return nil, NewNodeError("validation_error", false, "max_tokens must be in [1,8192]")
		}
		maxTokens = int(v)
	}
	var temperature *float64
	if v, ok := getNumber(rc.Config, "temperature"); ok {
		if !isFinite(v) || v < 0 || v > 2 {
			return nil, NewNodeError("validation_error", false, "temperature must be in [0,2]")
		}
		t := v
		temperature = &t
	}
	messages, err := parseLLMMessages(rc.Input)
	if err != nil {
		return nil, err
	}
	info, err := rc.Backend.ResolveSelector(ctx, selector)
	if err != nil {
		return nil, NewNodeError("selector_not_found", false, "selector %q did not resolve: %s", selector, RedactError(err))
	}
	_ = info
	resp, err := rc.Backend.Chat(ctx, ChatRequest{
		Selector: selector, Messages: messages,
		MaxTokens: maxTokens, Temperature: temperature,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"model":             resp.Model,
		"text":              truncateForOutput(resp.Text, 64<<10),
		"prompt_tokens":     resp.PromptTokens,
		"completion_tokens": resp.CompletionTokens,
		"total_tokens":      resp.TotalTokens,
		"resolved_route":    resp.ResolvedRoute,
	}, nil
}

func parseLLMMessages(input map[string]any) ([]ChatMessage, error) {
	prompt, hasPrompt := getString(input, "prompt")
	rawMsgs, hasMsgs := getArray(input, "messages")
	if hasPrompt && hasMsgs {
		return nil, NewNodeError("validation_error", false, "input must contain either messages or prompt, not both")
	}
	if !hasPrompt && !hasMsgs {
		return nil, NewNodeError("validation_error", false, "input requires messages or prompt")
	}
	if hasPrompt {
		if strings.TrimSpace(prompt) == "" {
			return nil, NewNodeError("validation_error", false, "prompt must not be empty")
		}
		if len(prompt) > 8000 {
			return nil, NewNodeError("validation_error", false, "prompt exceeds 8000 characters")
		}
		return []ChatMessage{{Role: "user", Content: prompt}}, nil
	}
	if len(rawMsgs) == 0 || len(rawMsgs) > 100 {
		return nil, NewNodeError("validation_error", false, "messages must contain 1..100 entries")
	}
	out := make([]ChatMessage, 0, len(rawMsgs))
	for _, raw := range rawMsgs {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, NewNodeError("validation_error", false, "each message must be an object with role and content")
		}
		role, _ := getString(item, "role")
		content, _ := getString(item, "content")
		if role == "" || content == "" {
			return nil, NewNodeError("validation_error", false, "each message requires non-empty role and content")
		}
		if len(content) > 8000 {
			return nil, NewNodeError("validation_error", false, "message content exceeds 8000 characters")
		}
		out = append(out, ChatMessage{Role: role, Content: content})
	}
	return out, nil
}
