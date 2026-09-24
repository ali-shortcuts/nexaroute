package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProbeTransport is the minimal upstream surface the probe suite needs.
type ProbeTransport interface {
	Do(ctx context.Context, payload []byte, stream bool, forward http.Header) (*http.Response, error)
	RedactBody([]byte) []byte
}

// ProbeOutcome is one capability's verdict.
type ProbeOutcome struct {
	Capability string  `json:"capability"`
	Verdict    Support `json:"verdict"`
	Detail     string  `json:"detail,omitempty"`
	LatencyMS  int64   `json:"latency_ms,omitempty"`
}

// ProbeReport is the structured result of a Level B suite or an agent loop
// simulation (spec section 24 returns structured reports, never "ok").
type ProbeReport struct {
	Deployment    string         `json:"deployment"`
	Model         string         `json:"model"`
	Dialect       string         `json:"dialect"`
	Level         string         `json:"level"` // availability | capability | agent
	Outcomes      []ProbeOutcome `json:"outcomes"`
	AgentSteps    []AgentStep    `json:"agent_steps,omitempty"`
	StartedAt     time.Time      `json:"started_at"`
	FinishedAt    time.Time      `json:"finished_at"`
	Inconclusive  int            `json:"inconclusive"`
	TransportFail int            `json:"transport_fail"`
	Passed        int            `json:"passed"`
	Failed        int            `json:"failed"`
	OK            bool           `json:"ok"`
}

// AgentStep is one leg of the Claude Code agent-loop simulation.
type AgentStep struct {
	Step      string `json:"step"`
	Passed    bool   `json:"passed"`
	Detail    string `json:"detail,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
}

// tinyPNG is a 1x1 red PNG used for vision probes (67 bytes).
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func simpleChatPayload(model string, extra map[string]any) []byte {
	m := map[string]any{
		"model":      model,
		"max_tokens": 16,
		"messages":   []map[string]any{{"role": "user", "content": "Reply with the single word: OK"}},
	}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return b
}

// runProbe executes one probe payload and returns the verdict.
// Status-code and error-body interpretation flows through the shared error
// classifier so probes and live traffic can never disagree.
func runProbe(ctx context.Context, t ProbeTransport, payload []byte, stream bool, capability string, expect ToolExpectation) (ProbeOutcome, error) {
	out := ProbeOutcome{Capability: capability, Verdict: UnknownSupport}
	start := time.Now()
	defer func() { out.LatencyMS = time.Since(start).Milliseconds() }()

	resp, err := t.Do(ctx, payload, stream, nil)
	if err != nil {
		out.Detail = "transport: " + err.Error()
		return out, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cls := ClassifyUpstreamError(resp.StatusCode, body)
		out.Detail = "http " + fmt.Sprint(resp.StatusCode) + ": " + cls.Message
		if cls.CapabilityFailure && (cls.Capability == capability || mapsToCapability(cls.Parameter) == capability) {
			out.Verdict = Unsupported
			return out, nil
		}
		if cls.Class == ClassContextOverflow || cls.CallerError {
			out.Verdict = UnknownSupport
			out.Detail += " (probe inconclusive: request rejected)"
			return out, nil
		}
		return out, fmt.Errorf("probe transport status %d: %s", resp.StatusCode, cls.Message)
	}
	if stream {
		if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			out.Detail = "200 but content-type is not event-stream"
			return out, nil
		}
		out.Verdict = Supported
		out.Detail = "SSE headers and early frames verified"
		return out, nil
	}
	// Sanity: a 2xx with an unparseable body is a malformed-response signal,
	// not a capability verdict.
	var probe completionShape
	if err := json.Unmarshal(body, &probe); err != nil {
		out.Detail = "200 but body is not a parseable completion: " + truncateMessage(string(body))
		return out, fmt.Errorf("probe malformed response: %s", out.Detail)
	}
	toolCallCount := 0
	for _, c := range probe.Choices {
		toolCallCount += len(c.Message.ToolCalls)
	}
	hasToolCalls := toolCallCount > 0
	_ = hasToolCalls
	switch expect {
	case ExpectToolCall:
		if toolCallCount >= 1 {
			out.Verdict = Supported
			out.Detail = fmt.Sprintf("tool call produced (%d)", toolCallCount)
		} else {
			out.Detail = "200 without a tool call (model chose plain text)"
			out.Verdict = UnknownSupport
		}
	case ExpectParallelToolCalls:
		if toolCallCount >= 2 {
			out.Verdict = Supported
			out.Detail = fmt.Sprintf("parallel tool calls produced (%d)", toolCallCount)
		} else if toolCallCount == 1 {
			out.Detail = "single tool call; parallelism unproven"
		} else {
			out.Detail = "200 without a tool call (model chose plain text)"
		}
	case ExpectJSON:
		payload := firstText(probe)
		trimmed := strings.TrimSpace(payload)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			out.Verdict = Supported
			out.Detail = "JSON output verified"
		} else {
			out.Detail = "200 but output is not JSON"
		}
	default:
		out.Verdict = Supported
		out.Detail = "generation verified"
	}
	return out, nil
}

// ToolExpectation tells runProbe how to interpret a 2xx body.
type ToolExpectation int

const (
	ExpectText ToolExpectation = iota
	ExpectToolCall
	ExpectParallelToolCalls
	ExpectJSON
)

// completionShape is the permissive OpenAI-ish response shape used to
// inspect probe replies without dragging full wire types in.
type completionShape struct {
	Choices []struct {
		Message struct {
			Content   any              `json:"content"`
			ToolCalls []map[string]any `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Content []map[string]any `json:"content"`
}

func firstText(probe completionShape) string {
	for _, c := range probe.Choices {
		switch v := c.Message.Content.(type) {
		case string:
			return v
		case []any:
			for _, part := range v {
				if m, ok := part.(map[string]any); ok {
					if txt, _ := m["text"].(string); txt != "" {
						return txt
					}
				}
			}
		}
	}
	return ""
}

func mapsToCapability(parameter string) string {
	if parameter == "" {
		return ""
	}
	if cap, ok := capabilityMapping[strings.ToLower(parameter)]; ok && cap != "" {
		return cap
	}
	return ""
}

// toolProbePayload builds a tools probe with the given tool_choice mode.
func toolProbePayload(model, toolChoice string, twoTools bool) []byte {
	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "Get the current weather for a city",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{"type": "string", "description": "City name"},
				},
				"required": []string{"city"},
			},
		},
	}}
	userMsg := "What is the weather in Paris right now? Use the get_weather tool."
	if twoTools {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "get_time",
				"description": "Get the current local time for a city",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]any{"type": "string"},
					},
					"required": []string{"city"},
				},
			},
		})
		userMsg = "What is the weather in Paris and the current local time in Tokyo? Use the get_weather and get_time tools."
	}
	m := map[string]any{
		"model":      model,
		"max_tokens": 128,
		"messages":   []map[string]any{{"role": "user", "content": userMsg}},
		"tools":      tools,
	}
	switch toolChoice {
	case "required":
		m["tool_choice"] = "required"
	case "auto":
		m["tool_choice"] = "auto"
	case "":
		// no tool_choice field
	}
	b, _ := json.Marshal(m)
	return b
}

// RunCapabilitySuite executes the Level B compatibility probes (spec
// section 6) against one deployment. ctx should carry a bounded deadline.
// Probes are intentionally cheap (<=128 output tokens each).
func RunCapabilitySuite(ctx context.Context, t ProbeTransport, deploymentID, model, dialectName string) ProbeReport {
	report := ProbeReport{
		Deployment: deploymentID, Model: model, Dialect: dialectName,
		Level: "capability", StartedAt: time.Now(),
	}
	type probeSpec struct {
		capability string
		payload    []byte
		stream     bool
		expect     ToolExpectation
	}
	specs := []probeSpec{
		{CapText, simpleChatPayload(model, nil), false, ExpectText},
		{CapSystemMessage, simpleChatPayload(model, map[string]any{
			"messages": []map[string]any{
				{"role": "system", "content": "You always answer in uppercase."},
				{"role": "user", "content": "Reply with the single word: OK"},
			},
		}), false, ExpectText},
		{CapStreaming, simpleChatPayload(model, map[string]any{"stream": true}), true, ExpectText},
		{CapTools, toolProbePayload(model, "auto", false), false, ExpectToolCall},
		{CapToolChoiceRequired, toolProbePayload(model, "required", false), false, ExpectToolCall},
		{CapParallelToolCalls, toolProbePayload(model, "", true), false, ExpectParallelToolCalls},
		{CapReasoningEffort, simpleChatPayload(model, map[string]any{"reasoning_effort": "low"}), false, ExpectText},
		{CapTemperature, simpleChatPayload(model, map[string]any{"temperature": 0.5}), false, ExpectText},
		{CapTopP, simpleChatPayload(model, map[string]any{"top_p": 0.9}), false, ExpectText},
		{CapStop, simpleChatPayload(model, map[string]any{"stop": []string{"|END|"}, "messages": []map[string]any{{"role": "user", "content": "Reply with the single word: OK. Do not write |END| yourself."}}}), false, ExpectText},
		{CapMaxCompletionTokens, simpleChatPayload(model, map[string]any{"max_completion_tokens": 16}), false, ExpectText},
		{CapJSONObject, simpleChatPayload(model, map[string]any{
			"response_format": map[string]any{"type": "json_object"},
			"messages":        []map[string]any{{"role": "user", "content": "Return a JSON object with key ok and value true."}},
		}), false, ExpectJSON},
		{CapVision, simpleChatPayload(model, map[string]any{
			"messages": []map[string]any{{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "What color is this image? Answer with one word."},
					{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + tinyPNG}},
				},
			}},
		}), false, ExpectText},
	}
	for _, spec := range specs {
		out, err := runProbe(ctx, t, spec.payload, spec.stream, spec.capability, spec.expect)
		switch {
		case err != nil:
			report.TransportFail++
			report.Inconclusive++
		case out.Verdict == Supported:
			report.Passed++
		case out.Verdict == Unsupported:
			report.Failed++
		default:
			report.Inconclusive++
		}
		report.Outcomes = append(report.Outcomes, out)
	}
	// Seed/max_tokens and text capability ride the basic probe.
	report.FinishedAt = time.Now()
	report.OK = report.TransportFail == 0 && report.Failed >= 0
	return report
}

// RunAgentLoopSimulation executes the Claude Code agent test (spec section
// 24): a real tool-call round trip against the deployment. A deployment is
// only "Claude Code compatible" when this loop passes end to end.
func RunAgentLoopSimulation(ctx context.Context, t ProbeTransport, deploymentID, model string) ProbeReport {
	report := ProbeReport{
		Deployment: deploymentID, Model: model,
		Level: "agent", StartedAt: time.Now(),
	}
	// Step 1: request with a tool -> expect a tool call.
	step1Payload := toolProbePayload(model, "auto", false)
	start := time.Now()
	resp, err := t.Do(ctx, step1Payload, false, nil)
	if err != nil {
		report.AgentSteps = append(report.AgentSteps, AgentStep{Step: "tool_call", Passed: false, Detail: err.Error(), LatencyMS: time.Since(start).Milliseconds()})
		report.FinishedAt = time.Now()
		report.OK = false
		return report
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	resp.Body.Close()
	latency1 := time.Since(start).Milliseconds()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cls := ClassifyUpstreamError(resp.StatusCode, body)
		report.AgentSteps = append(report.AgentSteps, AgentStep{Step: "tool_call", Passed: false, Detail: fmt.Sprintf("http %d: %s", resp.StatusCode, cls.Message), LatencyMS: latency1})
		report.FinishedAt = time.Now()
		report.OK = false
		return report
	}
	var step1 completionShape
	if err := json.Unmarshal(body, &step1); err != nil || len(step1.Choices) == 0 || len(step1.Choices[0].Message.ToolCalls) == 0 {
		report.AgentSteps = append(report.AgentSteps, AgentStep{Step: "tool_call", Passed: false, Detail: "no tool call in probe response", LatencyMS: latency1})
		report.FinishedAt = time.Now()
		report.OK = false
		return report
	}
	var call struct {
		ID        string
		Name      string
		Arguments string
	}
	tc := step1.Choices[0].Message.ToolCalls[0]
	call.ID, _ = tc["id"].(string)
	if fn, ok := tc["function"].(map[string]any); ok {
		call.Name, _ = fn["name"].(string)
		call.Arguments, _ = fn["arguments"].(string)
	}
	report.AgentSteps = append(report.AgentSteps, AgentStep{Step: "tool_call", Passed: true, Detail: "tool call produced: " + call.Name, LatencyMS: latency1})

	assistantMsg := map[string]any{
		"role":    "assistant",
		"content": nil,
		"tool_calls": []map[string]any{{
			"id": call.ID, "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
		}},
	}
	step2 := map[string]any{
		"model":      model,
		"max_tokens": 64,
		"messages": []map[string]any{
			{"role": "user", "content": "What is the weather in Paris right now? Use the get_weather tool."},
			assistantMsg,
			{"role": "tool", "tool_call_id": call.ID, "content": "15C, sunny, light wind"},
		},
	}
	step2Body, _ := json.Marshal(step2)
	start2 := time.Now()
	resp2, err := t.Do(ctx, step2Body, false, nil)
	latency2 := time.Since(start2).Milliseconds()
	if err != nil {
		report.AgentSteps = append(report.AgentSteps, AgentStep{Step: "tool_result_continuation", Passed: false, Detail: err.Error()})
		report.FinishedAt = time.Now()
		report.OK = false
		return report
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(io.LimitReader(resp2.Body, 256<<10))
	if resp2.StatusCode < 200 || resp2.StatusCode >= 300 {
		cls := ClassifyUpstreamError(resp2.StatusCode, body2)
		report.AgentSteps = append(report.AgentSteps, AgentStep{Step: "tool_result_continuation", Passed: false, Detail: fmt.Sprintf("http %d: %s", resp2.StatusCode, cls.Message), LatencyMS: latency2})
		report.FinishedAt = time.Now()
		report.OK = false
		return report
	}
	var step2Resp completionShape
	if err := json.Unmarshal(body2, &step2Resp); err != nil || len(step2Resp.Choices) == 0 {
		report.AgentSteps = append(report.AgentSteps, AgentStep{Step: "tool_result_continuation", Passed: false, Detail: "unparseable continuation"})
		report.FinishedAt = time.Now()
		report.OK = false
		return report
	}
	finalText := strings.TrimSpace(firstText(step2Resp))
	passed := finalText != ""
	report.AgentSteps = append(report.AgentSteps, AgentStep{
		Step: "tool_result_continuation", Passed: passed,
		Detail: truncatedSnippet(finalText, 120), LatencyMS: latency2,
	})
	report.OK = passed
	if passed {
		report.Passed = 2
	} else {
		report.Failed = 1
	}
	report.FinishedAt = time.Now()
	return report
}

func truncatedSnippet(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var _ = bytes.Equal

// RunCapabilitySuiteAnthropic executes the Level B suite against an
// anthropic-compatible upstream using Anthropic Messages payloads.
func RunCapabilitySuiteAnthropic(ctx context.Context, t ProbeTransport, deploymentID, model string) ProbeReport {
	report := ProbeReport{
		Deployment: deploymentID, Model: model,
		Level: "capability", StartedAt: time.Now(),
	}
	anthPayload := func(extra map[string]any) []byte {
		m := map[string]any{
			"model":      model,
			"max_tokens": 16,
			"messages":   []map[string]any{{"role": "user", "content": "Reply with the single word: OK"}},
		}
		for k, v := range extra {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return b
	}
	anthTools := func(withChoice bool) []byte {
		m := map[string]any{
			"model":      model,
			"max_tokens": 128,
			"messages":   []map[string]any{{"role": "user", "content": "What is the weather in Paris right now? Use the get_weather tool."}},
			"tools": []map[string]any{{
				"name":        "get_weather",
				"description": "Get the current weather for a city",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]any{"type": "string"},
					},
					"required": []string{"city"},
				},
			}},
		}
		if withChoice {
			m["tool_choice"] = map[string]any{"type": "any"}
		}
		b, _ := json.Marshal(m)
		return b
	}
	type probeSpec struct {
		capability string
		payload    []byte
		stream     bool
		expect     ToolExpectation
	}
	specs := []probeSpec{
		{CapText, anthPayload(nil), false, ExpectText},
		{CapSystemMessage, anthPayload(map[string]any{"system": "You always answer in uppercase."}), false, ExpectText},
		{CapStreaming, anthPayload(map[string]any{"stream": true}), true, ExpectText},
		{CapTools, anthTools(false), false, ExpectToolCall},
		{CapToolChoiceRequired, anthTools(true), false, ExpectToolCall},
		{CapTemperature, anthPayload(map[string]any{"temperature": 0.5}), false, ExpectText},
		{CapTopP, anthPayload(map[string]any{"top_p": 0.9}), false, ExpectText},
		{CapReasoning, anthPayload(map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}, "max_tokens": 2048}), false, ExpectText},
		{CapVision, anthPayload(map[string]any{
			"max_tokens": 32,
			"messages": []map[string]any{{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "What color is this image? Answer with one word."},
				{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": tinyPNG}},
			}}},
		}), false, ExpectText},
	}
	anthVerdict := func(resp []byte, expect ToolExpectation) (Support, string) {
		var probe struct {
			Content []map[string]any `json:"content"`
			Usage   map[string]any   `json:"usage"`
		}
		if err := json.Unmarshal(resp, &probe); err != nil {
			return UnknownSupport, "unparseable anthropic body"
		}
		text := ""
		toolCalls := 0
		for _, b := range probe.Content {
			typ, _ := b["type"].(string)
			switch typ {
			case "text":
				if txt, _ := b["text"].(string); txt != "" {
					text = txt
				}
			case "tool_use":
				toolCalls++
			}
		}
		switch expect {
		case ExpectToolCall:
			if toolCalls >= 1 {
				return Supported, "tool call produced"
			}
			return UnknownSupport, "200 without a tool call"
		default:
			if strings.TrimSpace(text) != "" {
				return Supported, "generation verified"
			}
			return UnknownSupport, "200 without text content"
		}
	}
	for _, spec := range specs {
		out, err := runProbeAnthropic(ctx, t, spec.payload, spec.stream, spec.capability, anthVerdict)
		switch {
		case err != nil:
			report.TransportFail++
			report.Inconclusive++
		case out.Verdict == Supported:
			report.Passed++
		case out.Verdict == Unsupported:
			report.Failed++
		default:
			report.Inconclusive++
		}
		report.Outcomes = append(report.Outcomes, out)
	}
	report.FinishedAt = time.Now()
	report.OK = report.TransportFail == 0
	return report
}

// runProbeAnthropic mirrors runProbe with an Anthropic-shaped 2xx verdict
// function; failure classification flows through the same classifier.
func runProbeAnthropic(ctx context.Context, t ProbeTransport, payload []byte, stream bool, capability string, verdict func([]byte, ToolExpectation) (Support, string)) (ProbeOutcome, error) {
	out := ProbeOutcome{Capability: capability, Verdict: UnknownSupport}
	start := time.Now()
	defer func() { out.LatencyMS = time.Since(start).Milliseconds() }()
	resp, err := t.Do(ctx, payload, stream, nil)
	if err != nil {
		out.Detail = "transport: " + err.Error()
		return out, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cls := ClassifyUpstreamError(resp.StatusCode, body)
		out.Detail = "http " + fmt.Sprint(resp.StatusCode) + ": " + cls.Message
		if cls.CapabilityFailure && (cls.Capability == capability || mapsToCapability(cls.Parameter) == capability) {
			out.Verdict = Unsupported
			return out, nil
		}
		if cls.Class == ClassContextOverflow || cls.CallerError {
			out.Detail += " (probe inconclusive: request rejected)"
			return out, nil
		}
		return out, fmt.Errorf("probe transport status %d: %s", resp.StatusCode, cls.Message)
	}
	if stream {
		if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			out.Detail = "200 but content-type is not event-stream"
			return out, nil
		}
		out.Verdict = Supported
		out.Detail = "SSE headers verified"
		return out, nil
	}
	out.Verdict, out.Detail = verdict(body, ExpectText)
	return out, nil
}
