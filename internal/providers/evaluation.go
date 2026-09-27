package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Phase H live evaluation support.
//
// Live evaluation must send a real request to one explicitly selected physical
// deployment, and it must do so without becoming a second client architecture
// and without mutating production routing state.
//
// Both properties are enforced here, in the provider layer that already owns the
// protocol shapes:
//
//   - Reuse: LiveComplete builds its request through the adapter's own path
//     resolution, auth application, header handling, concurrency semaphore and
//     HTTP transport. There is no second OpenAI/Anthropic/Gemini client.
//   - Isolation: live traffic runs through an *evaluation twin* of the adapter.
//     The twin shares the transport (connection pool, proxy, TLS settings) but
//     owns private copies of every production-observable counter: credential
//     cooldown/success state, quota accounting and concurrency gauges. A live
//     evaluation therefore cannot improve or degrade the health, pressure or
//     cooldown state the data plane reads.
//
// The twin is marked evaluation-only, and LiveComplete refuses to run against a
// production adapter instance. That makes "live evaluation bypassed isolation" a
// hard failure instead of a code-review convention.

// evaluationOnly is set on adapter clones reserved for live evaluation.
// LiveComplete accepts only those clones.
const (
	// maxLiveResponseBytes bounds how much of an upstream evaluation response is
	// ever read into memory.
	maxLiveResponseBytes = 1 << 20
	// maxLiveOutputTextBytes bounds the extracted completion text that can enter
	// an evaluation artifact.
	maxLiveOutputTextBytes = 32 << 10
	// defaultLiveMaxOutputTokens bounds the completion length a live evaluation
	// may request when the operator does not set one.
	defaultLiveMaxOutputTokens = 512
)

// Live completion error taxonomy. Values are stable, bounded identifiers: they
// are recorded as eval.Outcome.ErrorType and never carry upstream secrets.
const (
	LiveErrNone      = ""
	LiveErrTransport = "transport"
	LiveErrTimeout   = "timeout"
	LiveErrHTTP      = "http"
	LiveErrEmpty     = "empty"
)

// LiveCompletion is the bounded result of exactly one live upstream request.
type LiveCompletion struct {
	StatusCode      int
	LatencyMS       int64
	Output          string
	PromptTokens    int
	OutputTokens    int
	ErrorType       string
	Message         string
	UpstreamAttempt bool
}

// EvaluationTwin returns an isolated clone of a for live evaluation traffic.
//
// The clone shares the production transport (connection pool, proxy and TLS
// configuration) and the identical request-building path, but owns private
// credential, quota and concurrency state. It returns false for adapters that
// cannot be cloned, so callers must fail closed rather than fall back to the
// production adapter.
func EvaluationTwin(a Adapter) (Adapter, bool) {
	if a == nil {
		return nil, false
	}
	ha, ok := a.(*httpAdapter)
	if !ok || ha == nil {
		return nil, false
	}
	return ha.evaluationTwin(), true
}

// LiveCapable returns a when it is an evaluation-isolated adapter that can serve
// live completion requests, and nil otherwise.
//
// Live execution must fail closed: a production adapter — which the caller might
// hold by mistake — is never acceptable, because dispatching live traffic
// through it would let evaluation move credential, quota and concurrency state
// that production routing reads.
func LiveCapable(a Adapter) Adapter {
	if a == nil {
		return nil
	}
	ha, ok := a.(*httpAdapter)
	if !ok || ha == nil || !ha.evaluationOnly {
		return nil
	}
	return a
}

// evaluationTwin clones the adapter for evaluation-only traffic. Every field
// that production routing observes is freshly allocated; every field that is
// immutable configuration is shared by value.
func (a *httpAdapter) evaluationTwin() *httpAdapter {
	a.credMu.RLock()
	keys := make([]string, 0, len(a.creds))
	for i := range a.creds {
		keys = append(keys, a.creds[i].Key)
	}
	a.credMu.RUnlock()

	// The transport is deliberately shared: evaluation reuses the provider's
	// connection pool, proxy and TLS configuration instead of creating a second
	// client stack. Only the *state* the data plane observes is cloned.
	t := &httpAdapter{
		p:              a.p,
		c:              &http.Client{Transport: a.c.Transport, Timeout: a.c.Timeout},
		streamC:        &http.Client{Transport: a.streamC.Transport},
		sem:            make(chan struct{}, cap(a.sem)),
		forwardAllowed: a.forwardAllowed,
		retryAfterCap:  a.retryAfterCap,
		evaluationOnly: true,
	}
	t.requestLimit.Store(-1)
	t.remainingRequests.Store(-1)
	t.tokenLimit.Store(-1)
	t.remainingTokens.Store(-1)
	for _, k := range keys {
		t.creds = append(t.creds, credentialState{Key: k})
	}
	return t
}

// LiveComplete issues one bounded, non-streaming completion request to the
// adapter's physical upstream.
//
// It returns a LiveCompletion for every outcome the upstream can produce —
// including HTTP failures, timeouts and empty completions — because those are
// *evidence* for the evaluation, not errors in the evaluation machinery. A
// non-nil error means the executor itself could not run (missing adapter,
// production adapter, unusable endpoint), never "the model answered badly".
func LiveComplete(ctx context.Context, a Adapter, model, prompt string, maxOutputTokens int) (LiveCompletion, error) {
	if a == nil {
		return LiveCompletion{}, errors.New("live evaluation requires a provider adapter")
	}
	ha, ok := a.(*httpAdapter)
	if !ok || ha == nil {
		return LiveCompletion{}, fmt.Errorf("provider adapter %q does not support live evaluation", a.ID())
	}
	if !ha.evaluationOnly {
		return LiveCompletion{}, errors.New("live evaluation refused: adapter is not evaluation-isolated")
	}
	if strings.TrimSpace(prompt) == "" {
		return LiveCompletion{}, errors.New("live evaluation requires a non-empty prompt")
	}
	if maxOutputTokens <= 0 {
		maxOutputTokens = defaultLiveMaxOutputTokens
	}

	payload, path, err := ha.liveCompletionRequest(model, prompt, maxOutputTokens)
	if err != nil {
		return LiveCompletion{}, err
	}
	start := time.Now()
	resp, err := ha.DoPath(ctx, http.MethodPost, path, payload, false, nil)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		out := LiveCompletion{LatencyMS: latency, ErrorType: LiveErrTransport, UpstreamAttempt: true}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			out.ErrorType = LiveErrTimeout
			out.Message = "upstream deadline exceeded"
			return out, nil
		}
		// Redact before the message can reach a run record, event or log: the
		// provider credential must never be echoed by the evaluation plane.
		out.Message = ha.redactString(err.Error())
		return out, nil
	}
	defer resp.Body.Close()

	out := LiveCompletion{StatusCode: resp.StatusCode, LatencyMS: latency, UpstreamAttempt: true}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxLiveResponseBytes+1))
	if readErr != nil {
		out.ErrorType = LiveErrTransport
		out.Message = "upstream response could not be read"
		return out, nil
	}
	if len(data) > maxLiveResponseBytes {
		out.ErrorType = LiveErrTransport
		out.Message = "upstream response exceeds the evaluation read bound"
		return out, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		out.ErrorType = LiveErrHTTP
		out.Message = "http " + strconv.Itoa(resp.StatusCode) + ": " + ha.safeSnippet(data)
		return out, nil
	}
	text, promptTokens, outputTokens := decodeLiveCompletion(ha.p.Type, data)
	out.Output = truncateLiveText(text)
	out.PromptTokens = promptTokens
	out.OutputTokens = outputTokens
	if strings.TrimSpace(out.Output) == "" {
		out.ErrorType = LiveErrEmpty
		out.Message = "upstream returned no completion text"
	}
	return out, nil
}

// liveCompletionRequest builds the native request body and path for one live
// completion, reusing the adapter's own per-provider path conventions.
func (a *httpAdapter) liveCompletionRequest(model, prompt string, maxOutputTokens int) ([]byte, string, error) {
	var body map[string]any
	switch a.p.Type {
	case "gemini":
		body = map[string]any{
			"contents":         []map[string]any{{"role": "user", "parts": []map[string]any{{"text": prompt}}}},
			"generationConfig": map[string]any{"maxOutputTokens": maxOutputTokens},
		}
	case "openai_responses":
		body = map[string]any{
			"model": model, "input": prompt,
			"max_output_tokens": maxOutputTokens, "stream": false,
		}
	case "anthropic_compatible":
		body = map[string]any{
			"model":      model,
			"max_tokens": maxOutputTokens,
			"messages":   []map[string]any{{"role": "user", "content": prompt}},
		}
	default:
		body = map[string]any{
			"model": model, "messages": []map[string]any{{"role": "user", "content": prompt}},
			"max_tokens": maxOutputTokens, "stream": false, "temperature": 0,
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("live completion request encode: %w", err)
	}
	switch a.p.Type {
	case "gemini":
		return b, a.geminiModelPath(model, false), nil
	case "openai_responses":
		return b, a.p.ResponsesPath, nil
	case "anthropic_compatible":
		return b, a.p.MessagesPath, nil
	default:
		return b, a.p.ChatPath, nil
	}
}

// decodeLiveCompletion extracts the completion text and token usage for the
// provider's native response shape. Unknown or partial shapes yield empty text
// instead of a guess, so the case is scored as invalid evidence.
func decodeLiveCompletion(kind string, data []byte) (string, int, int) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return "", 0, 0
	}
	var text string
	switch kind {
	case "anthropic_compatible":
		var content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if raw, ok := root["content"]; ok {
			_ = json.Unmarshal(raw, &content)
		}
		for _, c := range content {
			if c.Type == "" || c.Type == "text" {
				text += c.Text
			}
		}
		var usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		}
		if raw, ok := root["usage"]; ok {
			_ = json.Unmarshal(raw, &usage)
		}
		return text, usage.InputTokens, usage.OutputTokens
	case "openai_responses":
		if raw, ok := root["output_text"]; ok {
			_ = json.Unmarshal(raw, &text)
		}
		var output []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if raw, ok := root["output"]; ok {
			_ = json.Unmarshal(raw, &output)
		}
		for _, o := range output {
			for _, c := range o.Content {
				if c.Type == "output_text" || c.Type == "text" {
					text += c.Text
				}
			}
		}
		var usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		}
		if raw, ok := root["usage"]; ok {
			_ = json.Unmarshal(raw, &usage)
		}
		return text, usage.InputTokens, usage.OutputTokens
	case "gemini":
		var candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		}
		if raw, ok := root["candidates"]; ok {
			_ = json.Unmarshal(raw, &candidates)
		}
		if len(candidates) > 0 {
			for _, p := range candidates[0].Content.Parts {
				text += p.Text
			}
		}
		var usage struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		}
		if raw, ok := root["usageMetadata"]; ok {
			_ = json.Unmarshal(raw, &usage)
		}
		return text, usage.PromptTokenCount, usage.CandidatesTokenCount
	default:
		var choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if raw, ok := root["choices"]; ok {
			_ = json.Unmarshal(raw, &choices)
		}
		if len(choices) > 0 {
			text = choices[0].Message.Content
		}
		var usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		}
		if raw, ok := root["usage"]; ok {
			_ = json.Unmarshal(raw, &usage)
		}
		return text, usage.PromptTokens, usage.CompletionTokens
	}
}

func truncateLiveText(s string) string {
	if len(s) <= maxLiveOutputTextBytes {
		return s
	}
	return s[:maxLiveOutputTextBytes]
}

// redactString removes configured provider credentials from an arbitrary
// message. Evaluation errors are recorded, so credentials must never survive
// into a run record, event, metric, admin payload or log line.
func (a *httpAdapter) redactString(s string) string {
	if s == "" {
		return ""
	}
	return string(a.RedactBody([]byte(s)))
}
