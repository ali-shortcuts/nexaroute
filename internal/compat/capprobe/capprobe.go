package ccaprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat/capabilities"
)

// Two-layer adaptive capability probing. See spec section 6.
//
// Level A: lightweight availability (auth + endpoint + basic generation).
// Level B: per-capability compatibility probes, each independently cached.
// Expensive probes never run on the request hot path: they belong to
// startup, background supervision, provider creation, manual probe, and
// recovery/revalidation.

type Outcome string

const (
	OutcomePass     Outcome = "PASS"
	OutcomeFail     Outcome = "FAIL"
	OutcomeUnknown  Outcome = "UNKNOWN"
	OutcomeSkipped  Outcome = "SKIPPED"
	OutcomeUntested Outcome = "UNTESTED"
)

// Case is one Level B capability probe definition.
type Case struct {
	Key         string `json:"key"`
	Description string `json:"description"`
}

// LevelB enumerates the capability probe matrix in stable order.
func LevelB() []Case {
	return []Case{
		{Key: "basic_text", Description: "Basic non-stream text generation"},
		{Key: "system_message", Description: "System prompt handling"},
		{Key: "streaming", Description: "Streaming generation"},
		{Key: "tools", Description: "Single tool definition + invocation"},
		{Key: "tool_choice_auto", Description: "Automatic tool selection"},
		{Key: "tool_choice_required", Description: "Required tool selection"},
		{Key: "parallel_tools", Description: "Parallel tool calls in one turn"},
		{Key: "structured_output", Description: "JSON object response format"},
		{Key: "json_schema", Description: "JSON schema response format"},
		{Key: "reasoning", Description: "Reasoning/thinking controls"},
		{Key: "vision", Description: "Image input"},
		{Key: "temperature", Description: "Temperature sampling parameter"},
		{Key: "top_p", Description: "Top-p sampling parameter"},
		{Key: "stop", Description: "Stop sequences"},
		{Key: "max_tokens", Description: "max_tokens output limit"},
		{Key: "max_completion_tokens", Description: "max_completion_tokens output limit"},
	}
}

// Result is the outcome of one probe case.
type Result struct {
	Key       string  `json:"key"`
	Outcome   Outcome `json:"outcome"`
	LatencyMS int64   `json:"latency_ms"`
	Detail    string  `json:"detail,omitempty"`
}

// Report is the full compatibility report for one deployment.
type Report struct {
	Deployment string    `json:"deployment"`
	LevelA     Result    `json:"level_a"`
	LevelB     []Result  `json:"level_b"`
	DurationMS int64     `json:"duration_ms"`
	VerifiedAt time.Time `json:"verified_at"`
}

// ChatDoer sends one chat payload and reports the outcome.
// status is the HTTP status; body is a bounded response snippet.
type ChatDoer func(ctx context.Context, payload map[string]any, stream bool) (status int, body []byte, err error)

// Runner executes the two-layer probe suite against one deployment.
type Runner struct {
	Model   string
	Timeout time.Duration
	Do      ChatDoer
}

func (r Runner) timeout() time.Duration {
	if r.Timeout <= 0 {
		return 15 * time.Second
	}
	return r.Timeout
}

func (r Runner) basePayload() map[string]any {
	return map[string]any{
		"model":      r.Model,
		"max_tokens": 1,
		"messages":   []any{map[string]any{"role": "user", "content": "Reply OK"}},
		"stream":     false,
	}
}

func outcomeFromCall(status int, body []byte, err error) (Outcome, string) {
	if err != nil {
		return OutcomeUnknown, err.Error()
	}
	if status >= 200 && status < 300 {
		if looksLikeChatCompletion(body) {
			return OutcomePass, ""
		}
		return OutcomeFail, "response is not a valid chat completion envelope"
	}
	lower := strings.ToLower(string(body))
	if status == 400 || status == 422 {
		if strings.Contains(lower, "not supported") || strings.Contains(lower, "unsupported") ||
			strings.Contains(lower, "unknown parameter") || strings.Contains(lower, "unrecognized") {
			return OutcomeFail, truncate(string(body), 300)
		}
		return OutcomeFail, fmt.Sprintf("http %d: %s", status, truncate(string(body), 300))
	}
	if status == 401 || status == 403 {
		return OutcomeUnknown, fmt.Sprintf("auth failure http %d (credential problem, not capability)", status)
	}
	if status == 404 {
		return OutcomeFail, fmt.Sprintf("http 404: %s", truncate(string(body), 300))
	}
	if status == 429 || status >= 500 {
		return OutcomeUnknown, fmt.Sprintf("transient http %d (not capability evidence)", status)
	}
	return OutcomeUnknown, fmt.Sprintf("http %d", status)
}

func looksLikeChatCompletion(body []byte) bool {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		// Anthropic-shaped envelope also counts as basic generation proof.
		return false
	}
	if _, ok := root["choices"]; ok {
		return true
	}
	if typ, _ := root["type"].(string); typ == "message" {
		return true
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// RunLevelA executes the lightweight availability probe.
func (r Runner) RunLevelA(ctx context.Context) Result {
	start := time.Now()
	pctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	status, body, err := r.Do(pctx, r.basePayload(), false)
	out, detail := outcomeFromCall(status, body, err)
	return Result{Key: "availability", Outcome: out, LatencyMS: time.Since(start).Milliseconds(), Detail: detail}
}

func (r Runner) runCase(ctx context.Context, key string, mutate func(map[string]any)) Result {
	start := time.Now()
	payload := r.basePayload()
	mutate(payload)
	pctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	status, body, err := r.Do(pctx, payload, false)
	out, detail := outcomeFromCall(status, body, err)
	if out == OutcomeFail && isUnsupportedParameter(body) {
		detail = "unsupported: " + detail
	}
	return Result{Key: key, Outcome: out, LatencyMS: time.Since(start).Milliseconds(), Detail: detail}
}

func isUnsupportedParameter(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "not supported") || strings.Contains(lower, "unsupported")
}

// RunLevelB executes the full compatibility matrix.
func (r Runner) RunLevelB(ctx context.Context) []Result {
	mutations := map[string]func(map[string]any){
		"basic_text": func(p map[string]any) {},
		"system_message": func(p map[string]any) {
			p["messages"] = []any{
				map[string]any{"role": "system", "content": "You are helpful."},
				map[string]any{"role": "user", "content": "Reply OK"},
			}
		},
		"streaming": func(p map[string]any) { p["stream"] = true },
		"tools": func(p map[string]any) {
			p["tools"] = []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":       "get_time",
					"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
				},
			}}
			p["tool_choice"] = "auto"
		},
		"tool_choice_auto": func(p map[string]any) {
			p["tools"] = []any{map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "get_time", "parameters": map[string]any{"type": "object"}},
			}}
			p["tool_choice"] = "auto"
		},
		"tool_choice_required": func(p map[string]any) {
			p["tools"] = []any{map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "get_time", "parameters": map[string]any{"type": "object"}},
			}}
			p["tool_choice"] = "required"
		},
		"parallel_tools": func(p map[string]any) {
			p["tools"] = []any{map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "get_time", "parameters": map[string]any{"type": "object"}},
			}}
			p["tool_choice"] = "auto"
			p["parallel_tool_calls"] = true
		},
		"structured_output": func(p map[string]any) {
			p["response_format"] = map[string]any{"type": "json_object"}
		},
		"json_schema": func(p map[string]any) {
			p["response_format"] = map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   "ok",
					"schema": map[string]any{"type": "object"},
				},
			}
		},
		"reasoning": func(p map[string]any) { p["reasoning_effort"] = "low" },
		"vision": func(p map[string]any) {
			p["messages"] = []any{map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "What is this?"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="}},
				},
			}}
		},
		"temperature": func(p map[string]any) { p["temperature"] = 0.7 },
		"top_p":       func(p map[string]any) { p["top_p"] = 0.9 },
		"stop":        func(p map[string]any) { p["stop"] = []string{"\n\n"} },
		"max_tokens":  func(p map[string]any) { p["max_tokens"] = 1 },
		"max_completion_tokens": func(p map[string]any) {
			delete(p, "max_tokens")
			p["max_completion_tokens"] = 1
		},
	}
	results := make([]Result, 0, len(LevelB()))
	// Streaming uses a streaming call; the doer decides how to validate it.
	for _, c := range LevelB() {
		if ctx.Err() != nil {
			results = append(results, Result{Key: c.Key, Outcome: OutcomeSkipped, Detail: "context done"})
			continue
		}
		mut := mutations[c.Key]
		if c.Key == "streaming" {
			start := time.Now()
			payload := r.basePayload()
			payload["stream"] = true
			pctx, cancel := context.WithTimeout(ctx, r.timeout())
			status, body, err := r.Do(pctx, payload, true)
			cancel()
			out, detail := outcomeFromCall(status, body, err)
			// Streaming endpoints answer 200 with event bytes, not JSON.
			if err == nil && status >= 200 && status < 300 {
				out = OutcomePass
				detail = ""
			}
			results = append(results, Result{Key: c.Key, Outcome: out, LatencyMS: time.Since(start).Milliseconds(), Detail: detail})
			continue
		}
		results = append(results, r.runCase(ctx, c.Key, mut))
	}
	return results
}

// Run executes both layers and returns the full report.
func (r Runner) Run(ctx context.Context) Report {
	start := time.Now()
	a := r.RunLevelA(ctx)
	rep := Report{Deployment: r.Model, LevelA: a, VerifiedAt: time.Now()}
	if a.Outcome == OutcomePass {
		rep.LevelB = r.RunLevelB(ctx)
	} else {
		rep.LevelB = []Result{}
		for _, c := range LevelB() {
			rep.LevelB = append(rep.LevelB, Result{Key: c.Key, Outcome: OutcomeSkipped, Detail: "level A did not pass"})
		}
	}
	rep.DurationMS = time.Since(start).Milliseconds()
	return rep
}

// ApplyToStore records verified probe outcomes into the capability store.
// PASS -> SUPPORTED, FAIL with unsupported-parameter evidence -> UNSUPPORTED,
// everything else leaves the value untouched (UNKNOWN stays UNKNOWN).
func (rep Report) ApplyToStore(store *capabilities.Store, deployment string) {
	if store == nil {
		return
	}
	keyMap := map[string]string{
		"basic_text": "text", "system_message": "system_message",
		"streaming": "streaming", "tools": "tools",
		"tool_choice_auto":     "tool_choice_auto",
		"tool_choice_required": "tool_choice_required",
		"parallel_tools":       "parallel_tools",
		"structured_output":    "structured_output",
		"json_schema":          "json_schema",
		"reasoning":            "reasoning", "vision": "vision",
		"temperature": "temperature", "top_p": "top_p", "stop": "stop",
		"max_tokens": "max_tokens", "max_completion_tokens": "max_completion_tokens",
	}
	for _, res := range rep.LevelB {
		capKey, ok := keyMap[res.Key]
		if !ok {
			continue
		}
		switch res.Outcome {
		case OutcomePass:
			store.MarkVerified(deployment, capKey, capabilities.Supported, capabilities.SourceProbe, "capability probe PASS")
		case OutcomeFail:
			if strings.HasPrefix(res.Detail, "unsupported:") || isUnsupportedParameter([]byte(res.Detail)) {
				store.MarkVerified(deployment, capKey, capabilities.Unsupported, capabilities.SourceProbe, truncate(res.Detail, 200))
			}
		}
	}
	if rep.LevelA.Outcome == OutcomePass {
		store.MarkVerified(deployment, "text", capabilities.Supported, capabilities.SourceProbe, "level A availability PASS")
	}
}
