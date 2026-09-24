package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
)

// Gemini capability probes are intentionally wire-native. Reusing Chat
// Completions payloads against a Gemini adapter is invalid because the model
// and operation live in the URL and request/response envelopes differ.
//
// The suite mirrors the common probe matrix where Gemini has a real wire
// equivalent. Reasoning stays inconclusive until the gateway exposes a
// thinkingConfig mapping; silently dropping reasoning_effort and accepting a
// normal text response would create a false-positive capability verdict.

type geminiProbeSpec struct {
	capability string
	payload    []byte
	stream     bool
	expect     ToolExpectation
}

func geminiProbePath(model string, stream bool) string {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	model = url.PathEscape(model)
	if stream {
		return "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
	}
	return "/v1beta/models/" + model + ":generateContent"
}

func geminiProbePayload(prompt, system string, generation map[string]any, tools []map[string]any, mode string, vision bool) []byte {
	parts := []any{map[string]any{"text": prompt}}
	if vision {
		parts = append(parts, map[string]any{
			"inlineData": map[string]any{"mimeType": "image/png", "data": tinyPNG},
		})
	}
	out := map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": parts}},
	}
	if strings.TrimSpace(system) != "" {
		out["systemInstruction"] = map[string]any{
			"parts": []any{map[string]any{"text": system}},
		}
	}
	if len(generation) > 0 {
		out["generationConfig"] = generation
	}
	if len(tools) > 0 {
		out["tools"] = []any{map[string]any{"functionDeclarations": tools}}
	}
	if mode != "" {
		out["toolConfig"] = map[string]any{
			"functionCallingConfig": map[string]any{"mode": mode},
		}
	}
	b, _ := json.Marshal(out)
	return b
}

func geminiToolDeclarations(two bool) []map[string]any {
	tools := []map[string]any{{
		"name":        "get_weather",
		"description": "Get the current weather for a city",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string"},
			},
			"required": []string{"city"},
		},
	}}
	if two {
		tools = append(tools, map[string]any{
			"name":        "get_time",
			"description": "Get the current local time for a city",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{"type": "string"},
				},
				"required": []string{"city"},
			},
		})
	}
	return tools
}

func runGeminiProbe(ctx context.Context, t PathProbeTransport, model string, spec geminiProbeSpec) (ProbeOutcome, error) {
	out := ProbeOutcome{Capability: spec.capability, Verdict: UnknownSupport}
	start := time.Now()
	defer func() { out.LatencyMS = time.Since(start).Milliseconds() }()

	resp, err := t.DoPath(ctx, http.MethodPost, geminiProbePath(model, spec.stream), spec.payload, spec.stream, nil)
	if err != nil {
		out.Detail = "transport: " + err.Error()
		return out, err
	}
	defer resp.Body.Close()
	if spec.stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			out.Detail = "200 but content-type is not event-stream"
			return out, nil
		}
		out.Verdict = Supported
		out.Detail = "Gemini SSE endpoint verified"
		return out, nil
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if readErr != nil {
		out.Detail = "response read: " + readErr.Error()
		return out, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cls := ClassifyUpstreamError(resp.StatusCode, body)
		out.Detail = "http " + fmt.Sprint(resp.StatusCode) + ": " + cls.Message
		if cls.CapabilityFailure && (cls.Capability == spec.capability || mapsToCapability(cls.Parameter) == spec.capability) {
			out.Verdict = Unsupported
			return out, nil
		}
		if cls.Class == ClassContextOverflow || cls.CallerError {
			out.Detail += " (probe inconclusive: request rejected)"
			return out, nil
		}
		return out, fmt.Errorf("probe transport status %d: %s", resp.StatusCode, cls.Message)
	}

	canResp, err := canonical.DecodeGeminiResponse(body)
	if err != nil {
		out.Detail = "200 but body is not a parseable Gemini response: " + truncateMessage(string(body))
		return out, fmt.Errorf("probe malformed response: %s", out.Detail)
	}
	toolCalls := 0
	for _, b := range canResp.Blocks {
		if b.Type == canonical.PartToolCall && b.ToolCall != nil {
			toolCalls++
		}
	}
	switch spec.expect {
	case ExpectToolCall:
		if toolCalls >= 1 {
			out.Verdict = Supported
			out.Detail = fmt.Sprintf("tool call produced (%d)", toolCalls)
		} else {
			out.Detail = "200 without a function call"
		}
	case ExpectParallelToolCalls:
		if toolCalls >= 2 {
			out.Verdict = Supported
			out.Detail = fmt.Sprintf("parallel tool calls produced (%d)", toolCalls)
		} else if toolCalls == 1 {
			out.Detail = "single function call; parallelism unproven"
		} else {
			out.Detail = "200 without a function call"
		}
	case ExpectJSON:
		text := strings.TrimSpace(canResp.Text())
		if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
			out.Verdict = Supported
			out.Detail = "JSON output verified"
		} else {
			out.Detail = "200 but output is not JSON"
		}
	default:
		if strings.TrimSpace(canResp.Text()) != "" {
			out.Verdict = Supported
			out.Detail = "generation verified"
		} else {
			out.Detail = "200 without text content"
		}
	}
	return out, nil
}

// RunCapabilitySuiteGemini executes the Level-B matrix using native Gemini
// GenerateContent payloads and model-in-path endpoints.
func RunCapabilitySuiteGemini(ctx context.Context, t PathProbeTransport, deploymentID, model string) ProbeReport {
	report := ProbeReport{
		Deployment: deploymentID, Model: model, Dialect: "gemini",
		Level: "capability", StartedAt: time.Now(),
	}
	oneTool := geminiToolDeclarations(false)
	twoTools := geminiToolDeclarations(true)
	specs := []geminiProbeSpec{
		{CapText, geminiProbePayload("Reply with the single word: OK", "", map[string]any{"maxOutputTokens": 16}, nil, "", false), false, ExpectText},
		{CapSystemMessage, geminiProbePayload("Reply with the single word: OK", "You always answer in uppercase.", map[string]any{"maxOutputTokens": 16}, nil, "", false), false, ExpectText},
		{CapStreaming, geminiProbePayload("Reply with the single word: OK", "", map[string]any{"maxOutputTokens": 16}, nil, "", false), true, ExpectText},
		{CapTools, geminiProbePayload("What is the weather in Paris? Use get_weather.", "", map[string]any{"maxOutputTokens": 128}, oneTool, "AUTO", false), false, ExpectToolCall},
		{CapToolChoiceRequired, geminiProbePayload("What is the weather in Paris? Use get_weather.", "", map[string]any{"maxOutputTokens": 128}, oneTool, "ANY", false), false, ExpectToolCall},
		{CapParallelToolCalls, geminiProbePayload("Get the weather in Paris and time in Tokyo. Use both tools.", "", map[string]any{"maxOutputTokens": 128}, twoTools, "ANY", false), false, ExpectParallelToolCalls},
		{CapTemperature, geminiProbePayload("Reply with OK", "", map[string]any{"maxOutputTokens": 16, "temperature": 0.5}, nil, "", false), false, ExpectText},
		{CapTopP, geminiProbePayload("Reply with OK", "", map[string]any{"maxOutputTokens": 16, "topP": 0.9}, nil, "", false), false, ExpectText},
		{CapStop, geminiProbePayload("Reply with OK and do not write |END|", "", map[string]any{"maxOutputTokens": 16, "stopSequences": []string{"|END|"}}, nil, "", false), false, ExpectText},
		{CapMaxCompletionTokens, geminiProbePayload("Reply with OK", "", map[string]any{"maxOutputTokens": 16}, nil, "", false), false, ExpectText},
		{CapJSONObject, geminiProbePayload("Return a JSON object with key ok and value true.", "", map[string]any{"maxOutputTokens": 32, "responseMimeType": "application/json"}, nil, "", false), false, ExpectJSON},
		{CapVision, geminiProbePayload("What color is this image? Answer with one word.", "", map[string]any{"maxOutputTokens": 32}, nil, "", true), false, ExpectText},
	}
	for _, spec := range specs {
		out, err := runGeminiProbe(ctx, t, model, spec)
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

	report.Outcomes = append(report.Outcomes, ProbeOutcome{
		Capability: CapReasoningEffort,
		Verdict:    UnknownSupport,
		Detail:     "Gemini thinkingConfig is not mapped by the current gateway wire encoder",
	})
	report.Inconclusive++
	report.FinishedAt = time.Now()
	report.OK = report.TransportFail == 0
	return report
}

func readGeminiAgentResponse(resp *http.Response) (canonical.Response, []byte, error) {
	if resp == nil {
		return canonical.Response{}, nil, fmt.Errorf("nil Gemini response")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return canonical.Response{}, body, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cls := ClassifyUpstreamError(resp.StatusCode, body)
		return canonical.Response{}, body, fmt.Errorf("http %d: %s", resp.StatusCode, cls.Message)
	}
	out, err := canonical.DecodeGeminiResponse(body)
	return out, body, err
}

// RunAgentLoopSimulationGemini performs a real two-turn function-call round
// trip using Gemini functionCall/functionResponse parts.
func RunAgentLoopSimulationGemini(ctx context.Context, t PathProbeTransport, deploymentID, model string) ProbeReport {
	report := ProbeReport{
		Deployment: deploymentID, Model: model, Dialect: "gemini",
		Level: "agent", StartedAt: time.Now(),
	}
	tools := geminiToolDeclarations(false)
	step1Payload := geminiProbePayload(
		"What is the weather in Paris right now? Use get_weather.",
		"", map[string]any{"maxOutputTokens": 128}, tools, "ANY", false,
	)
	start := time.Now()
	resp, err := t.DoPath(ctx, http.MethodPost, geminiProbePath(model, false), step1Payload, false, nil)
	if err != nil {
		report.AgentSteps = append(report.AgentSteps, AgentStep{Step: "tool_call", Passed: false, Detail: err.Error(), LatencyMS: time.Since(start).Milliseconds()})
		report.FinishedAt = time.Now()
		return report
	}
	first, _, err := readGeminiAgentResponse(resp)
	lat1 := time.Since(start).Milliseconds()
	if err != nil {
		report.AgentSteps = append(report.AgentSteps, AgentStep{Step: "tool_call", Passed: false, Detail: err.Error(), LatencyMS: lat1})
		report.FinishedAt = time.Now()
		return report
	}
	var call *canonical.ToolCall
	for _, b := range first.Blocks {
		if b.Type == canonical.PartToolCall && b.ToolCall != nil {
			tc := *b.ToolCall
			call = &tc
			break
		}
	}
	if call == nil || strings.TrimSpace(call.Name) == "" {
		report.AgentSteps = append(report.AgentSteps, AgentStep{Step: "tool_call", Passed: false, Detail: "no function call in Gemini response", LatencyMS: lat1})
		report.FinishedAt = time.Now()
		return report
	}
	report.AgentSteps = append(report.AgentSteps, AgentStep{Step: "tool_call", Passed: true, Detail: "function call produced: " + call.Name, LatencyMS: lat1})

	args := map[string]any{}
	if strings.TrimSpace(call.Arguments) != "" {
		_ = json.Unmarshal([]byte(call.Arguments), &args)
	}
	step2 := map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "What is the weather in Paris right now? Use get_weather."}}},
			map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": call.Name, "args": args}}}},
			map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{
				"name": call.Name, "response": map[string]any{"result": "15C, sunny, light wind"},
			}}}},
		},
		"tools": []any{map[string]any{"functionDeclarations": tools}},
		"generationConfig": map[string]any{"maxOutputTokens": 64},
	}
	step2Payload, _ := json.Marshal(step2)
	start2 := time.Now()
	resp2, err := t.DoPath(ctx, http.MethodPost, geminiProbePath(model, false), step2Payload, false, nil)
	if err != nil {
		report.AgentSteps = append(report.AgentSteps, AgentStep{Step: "tool_result_continuation", Passed: false, Detail: err.Error(), LatencyMS: time.Since(start2).Milliseconds()})
		report.FinishedAt = time.Now()
		return report
	}
	second, _, err := readGeminiAgentResponse(resp2)
	lat2 := time.Since(start2).Milliseconds()
	if err != nil {
		report.AgentSteps = append(report.AgentSteps, AgentStep{Step: "tool_result_continuation", Passed: false, Detail: err.Error(), LatencyMS: lat2})
		report.FinishedAt = time.Now()
		return report
	}
	finalText := strings.TrimSpace(second.Text())
	passed := finalText != ""
	report.AgentSteps = append(report.AgentSteps, AgentStep{
		Step: "tool_result_continuation", Passed: passed,
		Detail: truncatedSnippet(finalText, 120), LatencyMS: lat2,
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
