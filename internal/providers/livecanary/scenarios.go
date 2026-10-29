package livecanary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

type scenario struct {
	ID         string
	Dimensions []providers.ConformanceDimension
	Calls      int
	Applies    func(config.ProviderConfig) bool
}

func allDialects(config.ProviderConfig) bool { return true }

func supportsStructuredOutput(p config.ProviderConfig) bool {
	switch p.Type {
	case "openai_compatible", "azure_openai", "openai_responses", "gemini", "vertex":
		return true
	default:
		return false
	}
}

func supportsParallelTools(p config.ProviderConfig) bool {
	switch p.Type {
	case "openai_compatible", "azure_openai", "openai_responses", "anthropic_compatible":
		return true
	default:
		return false
	}
}

func supportsReasoning(p config.ProviderConfig) bool {
	return p.Type != "bedrock"
}

func appliesWhenCredentialed(p config.ProviderConfig) bool {
	return !strings.EqualFold(strings.TrimSpace(p.AuthMode), "none")
}

func scenarioCatalog() []scenario {
	return []scenario{
		{ID: "text", Dimensions: []providers.ConformanceDimension{providers.DimAddressing, providers.DimAuth, providers.DimNonStream}, Calls: 1, Applies: allDialects},
		{ID: "stream", Dimensions: []providers.ConformanceDimension{providers.DimAddressing, providers.DimAuth, providers.DimStreaming}, Calls: 1, Applies: allDialects},
		{ID: "stream_usage", Dimensions: []providers.ConformanceDimension{providers.DimAddressing, providers.DimAuth, providers.DimStreaming}, Calls: 1, Applies: allDialects},
		{ID: "auth_failure", Dimensions: []providers.ConformanceDimension{providers.DimAddressing, providers.DimAuth}, Calls: 1, Applies: appliesWhenCredentialed},
		{ID: "tools", Dimensions: []providers.ConformanceDimension{providers.DimAddressing, providers.DimAuth, providers.DimToolCalls}, Calls: 1, Applies: allDialects},
		{ID: "tool_result", Dimensions: []providers.ConformanceDimension{providers.DimAddressing, providers.DimAuth, providers.DimToolResults}, Calls: 1, Applies: allDialects},
		{ID: "structured_output", Dimensions: []providers.ConformanceDimension{providers.DimAddressing, providers.DimAuth, providers.DimStructuredOutput}, Calls: 1, Applies: supportsStructuredOutput},
		{ID: "images", Dimensions: []providers.ConformanceDimension{providers.DimAddressing, providers.DimAuth, providers.DimImages}, Calls: 1, Applies: allDialects},
		{ID: "parallel_tools", Dimensions: []providers.ConformanceDimension{providers.DimAddressing, providers.DimAuth, providers.DimParallelTools}, Calls: 1, Applies: supportsParallelTools},
		{ID: "reasoning", Dimensions: []providers.ConformanceDimension{providers.DimAddressing, providers.DimAuth, providers.DimReasoning}, Calls: 1, Applies: supportsReasoning},
		{ID: "error_envelope", Dimensions: []providers.ConformanceDimension{providers.DimErrorEnvelopes}, Calls: 1, Applies: allDialects},
	}
}

var coreScenarioIDs = []string{"text", "stream", "tools", "tool_result", "error_envelope"}
var extendedScenarioIDs = []string{"text", "stream", "tools", "tool_result", "structured_output", "images", "parallel_tools", "reasoning", "error_envelope"}

// acceptanceScenarioIDs is the issue #646 live-acceptance set: the core
// round trips plus the two cases a one-token probe cannot show — that a
// deliberately invalid credential is rejected (auth_failure) and that a
// stream actually reports token usage (stream_usage). It deliberately does
// NOT include rate-limit, timeout, outage, or failover induction: those are
// recorded only when observed, or proven against loopback fakes, and the
// runbook (docs/PROVIDER_LIVE_ACCEPTANCE.md) says so per case.
var acceptanceScenarioIDs = []string{"text", "stream", "stream_usage", "tools", "tool_result", "auth_failure", "error_envelope"}

// ResolveScenarios accepts "core", "extended", "all", "acceptance", or
// scenario ids in a comma-separated list (repeated flags are also accepted
// by the CLI).
func ResolveScenarios(names []string) ([]scenario, error) {
	if len(names) == 0 {
		names = []string{"core"}
	}
	var ids []string
	for _, n := range names {
		for _, part := range strings.Split(n, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part == "" {
				continue
			}
			switch part {
			case "core":
				ids = append(ids, coreScenarioIDs...)
			case "acceptance":
				ids = append(ids, acceptanceScenarioIDs...)
			case "extended", "all":
				ids = append(ids, extendedScenarioIDs...)
			default:
				ids = append(ids, part)
			}
		}
	}
	catalog := scenarioCatalog()
	byID := map[string]scenario{}
	for _, sc := range catalog {
		byID[sc.ID] = sc
	}
	seen := map[string]bool{}
	out := make([]scenario, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		sc, ok := byID[id]
		if !ok {
			names := make([]string, 0, len(catalog))
			for _, c := range catalog {
				names = append(names, c.ID)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("unknown scenario %q (known: core, extended, all, %s)", id, strings.Join(names, ", "))
		}
		seen[id] = true
		out = append(out, sc)
	}
	if len(out) == 0 {
		return nil, errors.New("no canary scenarios selected")
	}
	return out, nil
}

func scenarioUnsupported(dialect string, sc scenario) (bool, string) {
	entry, ok := providers.ManifestEntry(dialect)
	if !ok {
		return false, ""
	}
	for _, dim := range sc.Dimensions {
		claim, ok := entry.Dimensions[dim]
		if ok && claim.State == providers.StateUnsupported {
			return true, fmt.Sprintf("manifest records %s as UNSUPPORTED for dialect %s", dim, dialect)
		}
	}
	return false, ""
}

func dimensionsAsStrings(dims []providers.ConformanceDimension) []string {
	out := make([]string, 0, len(dims))
	for _, d := range dims {
		out = append(out, string(d))
	}
	return out
}

func scenarioCallOutputTokens(id, dialect string, limit int) int {
	if id != "reasoning" {
		return limit
	}
	if dialect == "anthropic_compatible" {
		return 2048
	}
	if limit < 512 {
		return 512
	}
	return limit
}

func baseRequest(model, prompt string, maxTokens int) canonical.Request {
	return canonical.Request{
		Model:           model,
		MaxOutputTokens: maxTokens,
		Temperature:     floatPtr(0),
		Messages: []canonical.Message{{
			Role:  canonical.RoleUser,
			Parts: []canonical.Part{{Type: canonical.PartText, Text: prompt}},
		}},
	}
}

func floatPtr(v float64) *float64 { return &v }

func boolPtr(v bool) *bool { return &v }

const canaryToolSchema = `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`

// A tiny solid-red PNG (1x1) keeps the multimodal check cheap.
const redPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAFgwJ/lK3kAAAAAElFTkSuQmCC"

func (r *Runner) runText(ctx context.Context, sc scenario) ScenarioResult {
	req := baseRequest(r.upstreamModel, "Reply with the single word OK.", r.scenarioOutputTokens(sc.ID))
	raw, err := r.doCanonical(ctx, req, false)
	res := ScenarioResult{ID: sc.ID, Dimensions: dimensionsAsStrings(sc.Dimensions), Calls: raw.calls, LatencyMS: raw.latency.Milliseconds(), HTTPStatus: raw.status}
	if err != nil {
		return fail(res, r.redact(err.Error()))
	}
	if raw.status < 200 || raw.status >= 300 {
		return fail(res, fmt.Sprintf("http %d: %s", raw.status, r.redactSnippet(raw.body)))
	}
	decoded, err := decodeResponse(r.provider.Type, raw.body)
	if err != nil {
		return fail(res, err.Error())
	}
	if strings.TrimSpace(decoded.Text()) == "" && !decoded.HasToolCalls() {
		return fail(res, "upstream returned no completion text")
	}
	res.InputTokens = decoded.Usage.InputTokens
	res.OutputTokens = decoded.Usage.OutputTokens
	res.Evidence = fmt.Sprintf("http %d; stop=%s; blocks=%d", raw.status, decoded.StopReason, len(decoded.Blocks))
	if decoded.Usage.CacheReadTokens > 0 || decoded.Usage.CacheWriteTokens > 0 {
		res.Dimensions = append(res.Dimensions, string(providers.DimUsageCache))
	}
	if hasNativeFinishReason(r.provider.Type, raw.body) {
		res.Dimensions = append(res.Dimensions, string(providers.DimFinishReasons))
	}
	res.Status = StatusPass
	return res
}

func (r *Runner) runStream(ctx context.Context, sc scenario) ScenarioResult {
	req := baseRequest(r.upstreamModel, "Reply with the single word OK.", r.scenarioOutputTokens(sc.ID))
	req.Stream = true
	raw, err := r.doCanonical(ctx, req, true)
	res := ScenarioResult{ID: sc.ID, Dimensions: dimensionsAsStrings(sc.Dimensions), Calls: raw.calls, LatencyMS: raw.latency.Milliseconds(), HTTPStatus: raw.status}
	if err != nil {
		return fail(res, r.redact(err.Error()))
	}
	if raw.status < 200 || raw.status >= 300 {
		return fail(res, fmt.Sprintf("http %d: %s", raw.status, r.redactSnippet(raw.body)))
	}
	ct := strings.ToLower(raw.header.Get("Content-Type"))
	if !strings.Contains(ct, "text/event-stream") {
		return fail(res, "expected text/event-stream, got "+ct)
	}
	body := string(raw.body)
	if !hasSSEData(body) {
		return fail(res, "stream contained no non-terminal data event")
	}
	if !streamTerminal(r.provider.Type, body) {
		return fail(res, "stream did not contain the dialect's terminal event")
	}
	res.Evidence = fmt.Sprintf("http %d; sse bytes=%d; terminal event observed", raw.status, len(raw.body))
	res.Status = StatusPass
	return res
}

func (r *Runner) runStreamUsage(ctx context.Context, sc scenario) ScenarioResult {
	req := baseRequest(r.upstreamModel, "Reply with the single word OK.", r.scenarioOutputTokens(sc.ID))
	req.Stream = true
	raw, err := r.doCanonical(ctx, req, true)
	res := ScenarioResult{ID: sc.ID, Dimensions: dimensionsAsStrings(sc.Dimensions), Calls: raw.calls, LatencyMS: raw.latency.Milliseconds(), HTTPStatus: raw.status}
	if err != nil {
		return fail(res, r.redact(err.Error()))
	}
	if raw.status < 200 || raw.status >= 300 {
		return fail(res, fmt.Sprintf("http %d: %s", raw.status, r.redactSnippet(raw.body)))
	}
	ct := strings.ToLower(raw.header.Get("Content-Type"))
	if !strings.Contains(ct, "text/event-stream") {
		return fail(res, "expected text/event-stream, got "+ct)
	}
	body := string(raw.body)
	if !hasSSEData(body) {
		return fail(res, "stream contained no non-terminal data event")
	}
	if !streamTerminal(r.provider.Type, body) {
		return fail(res, "stream did not contain the dialect's terminal event")
	}
	in, out, found := extractStreamUsage(raw.body)
	if !found {
		return fail(res, "stream completed but reported no input/output usage tokens; usage extraction is not demonstrated for this provider")
	}
	res.InputTokens = in
	res.OutputTokens = out
	res.Evidence = fmt.Sprintf("http %d; sse bytes=%d; stream usage input=%d output=%d", raw.status, len(raw.body), in, out)
	res.Status = StatusPass
	return res
}

// extractStreamUsage scans SSE data events (and Bedrock SSE envelopes) for
// usage tokens in every dialect's shape: OpenAI usage.prompt/completion,
// Anthropic message_start/message_delta usage, Gemini usageMetadata, and
// Bedrock payload usage. It returns found=false unless both an input and an
// output count were observed, so a stream that merely completes cannot pass
// as a usage demonstration.
func extractStreamUsage(body []byte) (input, output int, found bool) {
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			for key, value := range node {
				if key == "usage" || key == "usageMetadata" {
					if usage, ok := value.(map[string]any); ok {
						for uk, uv := range usage {
							n, ok := uv.(float64)
							if !ok {
								continue
							}
							switch uk {
							case "prompt_tokens", "input_tokens", "inputTokens", "promptTokenCount":
								input = int(n)
							case "completion_tokens", "output_tokens", "outputTokens", "candidatesTokenCount":
								output = int(n)
							}
						}
					}
				}
				walk(value)
			}
		case []any:
			for _, item := range node {
				walk(item)
			}
		}
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var decoded any
		if err := json.Unmarshal([]byte(data), &decoded); err != nil {
			continue
		}
		walk(decoded)
	}
	return input, output, input > 0 && output > 0
}

// bogusAuthProvider returns a copy of the provider whose credential is a
// deliberately invalid placeholder, never the operator's real credential.
// The credential pool and any auth-shaped custom headers are cleared so the
// request cannot accidentally authenticate. Bedrock is the one exception to
// full replacement: SigV4 reads the secret only from the environment variable
// named in config at request time, so the bogus probe replaces only the
// access-key id. The signature is an HMAC over the request; it does not
// disclose the secret, and AWS rejects the unknown key id with 403. That
// distinction is recorded in the runbook, not hidden.
func bogusAuthProvider(p config.ProviderConfig) (config.ProviderConfig, error) {
	if strings.EqualFold(strings.TrimSpace(p.AuthMode), "none") {
		return config.ProviderConfig{}, errors.New("auth_failure does not apply to a provider with auth_mode none")
	}
	out := p
	out.APIKey = "nexaroute-acceptance-invalid-credential"
	out.APIKeyEnv = ""
	out.Credentials = nil
	if len(p.Headers) > 0 {
		headers := make(map[string]string, len(p.Headers))
		for name, value := range p.Headers {
			lower := strings.ToLower(name)
			if strings.Contains(lower, "key") || strings.Contains(lower, "token") ||
				strings.Contains(lower, "auth") || strings.Contains(lower, "secret") ||
				strings.Contains(lower, "cookie") {
				continue
			}
			headers[name] = value
		}
		out.Headers = headers
	}
	return out, nil
}

// runAuthFailure expects the provider to reject a deliberately invalid
// credential. A 200 here is a FAIL, not a pass: it would mean the endpoint
// accepted a credential that must not authenticate. Accepted rejection
// statuses cover the dialects: 401/403 everywhere, plus 400 for Gemini,
// whose API-key errors surface as 400. The bogus credential value is never
// written to the report; only the observed status is recorded.
func (r *Runner) runAuthFailure(ctx context.Context, sc scenario) ScenarioResult {
	res := ScenarioResult{ID: sc.ID, Dimensions: dimensionsAsStrings(sc.Dimensions)}
	bogus, err := bogusAuthProvider(r.provider)
	if err != nil {
		return fail(res, err.Error())
	}
	adapter, err := providers.NewAdapter(bogus, r.callTimeout)
	if err != nil {
		return fail(res, r.redact(err.Error()))
	}
	defer providers.CloseIdleConnections(adapter)
	req := baseRequest(r.upstreamModel, "Reply with the single word OK.", r.scenarioOutputTokens(sc.ID))
	payload, path, err := encodeRequest(r.provider, req)
	if err != nil {
		return fail(res, r.redact(err.Error()))
	}
	raw, err := r.doRawWith(adapter, ctx, payload, false, path)
	res.Calls = raw.calls
	res.LatencyMS = raw.latency.Milliseconds()
	res.HTTPStatus = raw.status
	if err != nil {
		return fail(res, r.redact(err.Error()))
	}
	switch raw.status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		res.Evidence = fmt.Sprintf("invalid credential rejected with http %d as expected", raw.status)
		res.Status = StatusPass
		return res
	default:
		return fail(res, fmt.Sprintf("invalid credential was not rejected as expected: http %d", raw.status))
	}
}

func (r *Runner) runTools(ctx context.Context, sc scenario) ScenarioResult {
	req := baseRequest(r.upstreamModel, `Call the lookup tool exactly once with q="canary". Do not answer in text.`, r.scenarioOutputTokens(sc.ID))
	req.Tools = []canonical.ToolDef{{Name: "lookup", Description: "look up a value", Parameters: json.RawMessage(canaryToolSchema)}}
	req.ToolChoice = &canonical.ToolChoice{Mode: "tool", Name: "lookup"}
	raw, err := r.doCanonical(ctx, req, false)
	res := ScenarioResult{ID: sc.ID, Dimensions: dimensionsAsStrings(sc.Dimensions), Calls: raw.calls, LatencyMS: raw.latency.Milliseconds(), HTTPStatus: raw.status}
	if err != nil {
		return fail(res, r.redact(err.Error()))
	}
	if raw.status < 200 || raw.status >= 300 {
		return fail(res, fmt.Sprintf("http %d: %s", raw.status, r.redactSnippet(raw.body)))
	}
	decoded, err := decodeResponse(r.provider.Type, raw.body)
	if err != nil {
		return fail(res, err.Error())
	}
	found := false
	for _, b := range decoded.Blocks {
		if b.Type != canonical.PartToolCall || b.ToolCall == nil {
			continue
		}
		if b.ToolCall.Name != "lookup" {
			continue
		}
		if args := strings.TrimSpace(b.ToolCall.Arguments); args != "" && !json.Valid([]byte(args)) {
			return fail(res, "tool arguments are not valid JSON")
		}
		found = true
	}
	if !found {
		return fail(res, "response did not contain the requested lookup tool call")
	}
	res.UsageFrom(decoded)
	res.Evidence = "tool call round-tripped with name lookup"
	res.Status = StatusPass
	return res
}

func (r *Runner) runToolResult(ctx context.Context, sc scenario) ScenarioResult {
	req := baseRequest(r.upstreamModel, "Use the tool result above. Reply with just the number.", r.scenarioOutputTokens(sc.ID))
	req.Tools = []canonical.ToolDef{{Name: "lookup", Description: "look up a value", Parameters: json.RawMessage(canaryToolSchema)}}
	req.Messages = []canonical.Message{
		{Role: canonical.RoleUser, Parts: []canonical.Part{{Type: canonical.PartText, Text: "Look up the value."}}},
		{Role: canonical.RoleAssistant, Parts: []canonical.Part{{Type: canonical.PartToolCall, ToolCall: &canonical.ToolCall{ID: "canary-call-1", Name: "lookup", Arguments: `{"q":"canary"}`}}}},
		{Role: canonical.RoleUser, Parts: []canonical.Part{
			{Type: canonical.PartToolResult, ToolResult: &canonical.ToolResult{ToolUseID: "canary-call-1", Content: "result=42"}},
			{Type: canonical.PartText, Text: "What was the result? Reply with just the number."},
		}},
	}
	raw, err := r.doCanonical(ctx, req, false)
	res := ScenarioResult{ID: sc.ID, Dimensions: dimensionsAsStrings(sc.Dimensions), Calls: raw.calls, LatencyMS: raw.latency.Milliseconds(), HTTPStatus: raw.status}
	if err != nil {
		return fail(res, r.redact(err.Error()))
	}
	if raw.status < 200 || raw.status >= 300 {
		return fail(res, fmt.Sprintf("http %d: %s", raw.status, r.redactSnippet(raw.body)))
	}
	decoded, err := decodeResponse(r.provider.Type, raw.body)
	if err != nil {
		return fail(res, err.Error())
	}
	if strings.TrimSpace(decoded.Text()) == "" {
		return fail(res, "upstream returned no text after the tool-result turn")
	}
	res.UsageFrom(decoded)
	res.Evidence = "tool-result turn accepted and answered"
	res.Status = StatusPass
	return res
}

func (r *Runner) runStructuredOutput(ctx context.Context, sc scenario) ScenarioResult {
	req := baseRequest(r.upstreamModel, `Return only JSON with status set to "ok".`, r.scenarioOutputTokens(sc.ID))
	req.ResponseFormat = &canonical.ResponseFormat{
		Kind:   canonical.FormatJSONSchema,
		Name:   "canary_status",
		Schema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"}},"required":["status"],"additionalProperties":false}`),
	}
	raw, err := r.doCanonical(ctx, req, false)
	res := ScenarioResult{ID: sc.ID, Dimensions: dimensionsAsStrings(sc.Dimensions), Calls: raw.calls, LatencyMS: raw.latency.Milliseconds(), HTTPStatus: raw.status}
	if err != nil {
		return fail(res, r.redact(err.Error()))
	}
	if raw.status < 200 || raw.status >= 300 {
		return fail(res, fmt.Sprintf("http %d: %s", raw.status, r.redactSnippet(raw.body)))
	}
	decoded, err := decodeResponse(r.provider.Type, raw.body)
	if err != nil {
		return fail(res, err.Error())
	}
	obj, ok := extractJSONObject(decoded.Text())
	if !ok {
		return fail(res, "structured output was not a JSON object")
	}
	if _, ok := obj["status"]; !ok {
		return fail(res, "structured output did not contain the requested status field")
	}
	res.UsageFrom(decoded)
	res.Evidence = "JSON-schema response format honored"
	res.Status = StatusPass
	return res
}

func (r *Runner) runImages(ctx context.Context, sc scenario) ScenarioResult {
	req := baseRequest(r.upstreamModel, "What color is this image? Reply with one word.", r.scenarioOutputTokens(sc.ID))
	req.Messages = []canonical.Message{{
		Role: canonical.RoleUser,
		Parts: []canonical.Part{
			{Type: canonical.PartImage, Image: &canonical.Image{MediaType: "image/png", Data: redPNGBase64}},
			{Type: canonical.PartText, Text: "What color is this image? Reply with one word."},
		},
	}}
	raw, err := r.doCanonical(ctx, req, false)
	res := ScenarioResult{ID: sc.ID, Dimensions: dimensionsAsStrings(sc.Dimensions), Calls: raw.calls, LatencyMS: raw.latency.Milliseconds(), HTTPStatus: raw.status}
	if err != nil {
		return fail(res, r.redact(err.Error()))
	}
	if raw.status < 200 || raw.status >= 300 {
		return fail(res, fmt.Sprintf("http %d: %s", raw.status, r.redactSnippet(raw.body)))
	}
	decoded, err := decodeResponse(r.provider.Type, raw.body)
	if err != nil {
		return fail(res, err.Error())
	}
	text := strings.ToLower(strings.TrimSpace(decoded.Text()))
	if text == "" {
		return fail(res, "upstream returned no text for the image request")
	}
	if !strings.Contains(text, "red") {
		return fail(res, "response did not reference the red test image")
	}
	res.UsageFrom(decoded)
	res.Evidence = "image input accepted and the red test image was described"
	res.Status = StatusPass
	return res
}

func (r *Runner) runParallelTools(ctx context.Context, sc scenario) ScenarioResult {
	req := baseRequest(r.upstreamModel, "Call both lookup_a and lookup_b in parallel with q=\"canary\". Do not answer in text.", r.scenarioOutputTokens(sc.ID))
	req.Tools = []canonical.ToolDef{
		{Name: "lookup_a", Description: "look up a", Parameters: json.RawMessage(canaryToolSchema)},
		{Name: "lookup_b", Description: "look up b", Parameters: json.RawMessage(canaryToolSchema)},
	}
	req.ToolChoice = &canonical.ToolChoice{Mode: "required"}
	req.ParallelToolCalls = boolPtr(true)
	raw, err := r.doCanonical(ctx, req, false)
	res := ScenarioResult{ID: sc.ID, Dimensions: dimensionsAsStrings(sc.Dimensions), Calls: raw.calls, LatencyMS: raw.latency.Milliseconds(), HTTPStatus: raw.status}
	if err != nil {
		return fail(res, r.redact(err.Error()))
	}
	if raw.status < 200 || raw.status >= 300 {
		return fail(res, fmt.Sprintf("http %d: %s", raw.status, r.redactSnippet(raw.body)))
	}
	decoded, err := decodeResponse(r.provider.Type, raw.body)
	if err != nil {
		return fail(res, err.Error())
	}
	calls := 0
	for _, b := range decoded.Blocks {
		if b.Type == canonical.PartToolCall && b.ToolCall != nil {
			calls++
		}
	}
	if calls < 2 {
		return fail(res, fmt.Sprintf("expected at least two tool calls, got %d; parallel behavior not demonstrated", calls))
	}
	res.UsageFrom(decoded)
	res.Evidence = fmt.Sprintf("%d parallel tool calls returned", calls)
	res.Status = StatusPass
	return res
}

func (r *Runner) runReasoning(ctx context.Context, sc scenario) ScenarioResult {
	req := baseRequest(r.upstreamModel, "Think briefly, then answer: 2+2=?", r.scenarioOutputTokens(sc.ID))
	req.Reasoning = &canonical.Reasoning{Effort: "low"}
	raw, err := r.doCanonical(ctx, req, false)
	res := ScenarioResult{ID: sc.ID, Dimensions: dimensionsAsStrings(sc.Dimensions), Calls: raw.calls, LatencyMS: raw.latency.Milliseconds(), HTTPStatus: raw.status}
	if err != nil {
		return fail(res, r.redact(err.Error()))
	}
	if raw.status < 200 || raw.status >= 300 {
		return fail(res, fmt.Sprintf("http %d: %s", raw.status, r.redactSnippet(raw.body)))
	}
	decoded, err := decodeResponse(r.provider.Type, raw.body)
	if err != nil {
		return fail(res, err.Error())
	}
	hasThinking := false
	for _, b := range decoded.Blocks {
		if b.Type == canonical.PartThinking {
			hasThinking = true
		}
	}
	if !hasThinking && decoded.Usage.ReasoningTokens == 0 {
		return fail(res, "response contained neither a thinking block nor reasoning-token usage; reasoning not demonstrated")
	}
	res.UsageFrom(decoded)
	res.Evidence = "reasoning evidence returned"
	res.Status = StatusPass
	return res
}

func (r *Runner) runErrorEnvelope(ctx context.Context, sc scenario) ScenarioResult {
	raw, err := r.doRaw(ctx, []byte(`{"model":`), false)
	res := ScenarioResult{ID: sc.ID, Dimensions: dimensionsAsStrings(sc.Dimensions), Calls: raw.calls, LatencyMS: raw.latency.Milliseconds(), HTTPStatus: raw.status}
	if err != nil {
		return fail(res, r.redact(err.Error()))
	}
	if raw.status < 400 || raw.status >= 500 {
		return fail(res, fmt.Sprintf("expected a 4xx error envelope for malformed JSON, got http %d", raw.status))
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw.body, &obj); err != nil {
		return fail(res, "error response was not a JSON object")
	}
	_, hasError := obj["error"]
	_, hasMessage := obj["message"]
	_, hasUpperMessage := obj["Message"]
	if !hasError && !hasMessage && !hasUpperMessage {
		return fail(res, "error JSON did not contain an error/message envelope")
	}
	res.Evidence = fmt.Sprintf("http %d with a JSON error envelope", raw.status)
	res.Status = StatusPass
	return res
}

type rawResponse struct {
	status  int
	header  http.Header
	body    []byte
	latency time.Duration
	calls   int
}

func (r *Runner) doCanonical(ctx context.Context, req canonical.Request, stream bool) (rawResponse, error) {
	payload, path, err := encodeRequest(r.provider, req)
	if err != nil {
		return rawResponse{}, err
	}
	return r.doRaw(ctx, payload, stream, path)
}

func (r *Runner) doRaw(ctx context.Context, payload []byte, stream bool, path ...string) (rawResponse, error) {
	return r.doRawWith(r.adapter, ctx, payload, stream, path...)
}

func (r *Runner) doRawWith(adapter providers.Adapter, ctx context.Context, payload []byte, stream bool, path ...string) (rawResponse, error) {
	before := r.calls
	r.calls++
	out := rawResponse{calls: r.calls - before}
	start := r.now()
	target := ""
	if len(path) > 0 {
		target = path[0]
	}
	if target == "" {
		target = defaultPath(r.provider)
	}
	resp, err := adapter.DoPath(ctx, http.MethodPost, target, payload, stream, nil)
	out.latency = r.now().Sub(start)
	if out.latency < 0 {
		out.latency = 0
	}
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	out.status = resp.StatusCode
	out.header = resp.Header
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return out, fmt.Errorf("upstream response could not be read: %w", err)
	}
	if int64(len(body)) > maxResponseBytes {
		return out, fmt.Errorf("upstream response exceeds the %d-byte canary read bound", maxResponseBytes)
	}
	out.body = body
	return out, nil
}

func encodeRequest(p config.ProviderConfig, req canonical.Request) ([]byte, string, error) {
	model := req.Model
	switch p.Type {
	case "anthropic_compatible":
		an, err := canonical.EncodeAnthropicRequest(req, model)
		if err != nil {
			return nil, "", err
		}
		body, err := json.Marshal(an)
		return body, pathOr(p.MessagesPath, "/v1/messages"), err
	case "gemini", "vertex":
		body, _, err := canonical.EncodeGeminiRequest(req, model)
		if err != nil {
			return nil, "", err
		}
		if p.Type == "vertex" {
			project := p.VertexProject
			location := p.VertexLocation
			if location == "" {
				location = "us-central1"
			}
			base := "/v1/projects/" + url.PathEscape(project) + "/locations/" + url.PathEscape(location) +
				"/publishers/google/models/" + url.PathEscape(strings.TrimPrefix(model, "models/"))
			if req.Stream {
				return body, base + ":streamGenerateContent?alt=sse", nil
			}
			return body, base + ":generateContent", nil
		}
		base := "/v1beta/models/" + url.PathEscape(strings.TrimPrefix(model, "models/"))
		if req.Stream {
			return body, base + ":streamGenerateContent?alt=sse", nil
		}
		return body, base + ":generateContent", nil
	case "bedrock":
		body, _, err := canonical.EncodeBedrockRequest(req, model)
		if err != nil {
			return nil, "", err
		}
		return body, canonical.BedrockModelPath(model, req.Stream), nil
	case "openai_responses":
		body, err := canonical.EncodeResponsesRequest(req, model)
		return body, pathOr(p.ResponsesPath, "/v1/responses"), err
	default:
		oai, err := canonical.EncodeOpenAIChatRequest(req, model, req.Stream)
		if err != nil {
			return nil, "", err
		}
		body, err := json.Marshal(oai)
		if err != nil {
			return nil, "", err
		}
		if p.Type == "azure_openai" {
			version := p.APIVersion
			if version == "" {
				version = "2024-10-21"
			}
			return body, "/openai/deployments/" + url.PathEscape(model) + "/chat/completions?api-version=" + url.QueryEscape(version), nil
		}
		return body, pathOr(p.ChatPath, "/v1/chat/completions"), nil
	}
}

func defaultPath(p config.ProviderConfig) string {
	switch p.Type {
	case "anthropic_compatible":
		return pathOr(p.MessagesPath, "/v1/messages")
	case "openai_responses":
		return pathOr(p.ResponsesPath, "/v1/responses")
	default:
		return pathOr(p.ChatPath, "/v1/chat/completions")
	}
}

func pathOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func decodeResponse(dialect string, body []byte) (canonical.Response, error) {
	switch dialect {
	case "anthropic_compatible":
		return canonical.DecodeAnthropicResponse(body)
	case "gemini", "vertex":
		return canonical.DecodeGeminiResponse(body)
	case "bedrock":
		return canonical.DecodeBedrockResponse(body)
	case "openai_responses":
		return canonical.DecodeResponsesResponse(body)
	default:
		return canonical.DecodeOpenAIChatResponse(body)
	}
}

func (s *ScenarioResult) UsageFrom(resp canonical.Response) {
	s.InputTokens = resp.Usage.InputTokens
	s.OutputTokens = resp.Usage.OutputTokens
}

func fail(res ScenarioResult, message string) ScenarioResult {
	res.Status = StatusFail
	res.Error = message
	return res
}

func hasNativeFinishReason(dialect string, body []byte) bool {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return false
	}
	switch dialect {
	case "anthropic_compatible":
		return nonEmptyRaw(root["stop_reason"])
	case "gemini", "vertex":
		var candidates []struct {
			FinishReason string `json:"finishReason"`
		}
		if raw, ok := root["candidates"]; ok && json.Unmarshal(raw, &candidates) == nil && len(candidates) > 0 {
			return strings.TrimSpace(candidates[0].FinishReason) != ""
		}
		return false
	case "bedrock":
		return nonEmptyRaw(root["stopReason"])
	case "openai_responses":
		return nonEmptyRaw(root["status"])
	default:
		var choices []struct {
			FinishReason *string `json:"finish_reason"`
		}
		if raw, ok := root["choices"]; ok && json.Unmarshal(raw, &choices) == nil && len(choices) > 0 {
			return choices[0].FinishReason != nil && strings.TrimSpace(*choices[0].FinishReason) != ""
		}
		return false
	}
}

func nonEmptyRaw(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != `""`
}

func hasSSEData(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data != "" && data != "[DONE]" {
			return true
		}
	}
	return false
}

func streamTerminal(dialect, body string) bool {
	switch dialect {
	case "anthropic_compatible":
		return strings.Contains(body, "message_stop")
	case "openai_responses":
		return strings.Contains(body, "response.completed")
	case "gemini", "vertex":
		return streamJSONHasNonEmptyString(body, "finishReason")
	case "bedrock":
		return strings.Contains(body, "messageStop")
	default:
		// A chunk that merely mentions finish_reason with a null value
		// is an open stream, not a terminated one: matching the key
		// alone reported truncated streams as PASS. Require [DONE] or a
		// non-empty finish_reason value in a data event.
		return strings.Contains(body, "[DONE]") || streamJSONHasNonEmptyString(body, "finish_reason")
	}
}

// streamJSONHasNonEmptyString reports whether any SSE data event decodes to
// JSON containing key with a non-empty string value, anywhere in the event.
// Null, absent, and empty-string values do not count as termination.
func streamJSONHasNonEmptyString(body, key string) bool {
	found := false
	var walk func(v any)
	walk = func(v any) {
		if found {
			return
		}
		switch node := v.(type) {
		case map[string]any:
			for k, value := range node {
				if k == key {
					if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
						found = true
						return
					}
				}
				walk(value)
			}
		case []any:
			for _, item := range node {
				walk(item)
			}
		}
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var decoded any
		if err := json.Unmarshal([]byte(data), &decoded); err != nil {
			continue
		}
		walk(decoded)
		if found {
			return true
		}
	}
	return false
}

func extractJSONObject(text string) (map[string]any, bool) {
	s := strings.TrimSpace(text)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, false
	}
	return obj, true
}
