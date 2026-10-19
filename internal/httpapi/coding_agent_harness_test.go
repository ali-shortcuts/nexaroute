package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// #421 item 5: the coding eval harness. Three scripted agent
// tasks — multi-file edit, failing-test fix, long-context repo
// question — are driven as REAL multi-turn tool-using sessions
// through the gateway against fake providers. The harness
// asserts the session invariants item 5 exists to prove:
//   - the session stays pinned to one deployment across the
//     whole tool loop while it is healthy;
//   - tool definitions, tool calls and tool results pass
//     through the gateway unrewritten (the #415 rule: routing
//     effects are never achieved by mutating agent traffic);
//   - when the repo-question session outgrows the pinned
//     deployment's window, it shifts to the long-context
//     deployment with the full history intact;
//   - the outcomes feed a scorecard the task-profile machinery
//     reads back with its sample size (3 tasks).
//
// Live/key-gated evaluation against real deployments stays in
// the opt-in evaluation plane; this harness is the local,
// fake-provider half the constraints prescribe for CI.

// agentFake is a scripted OpenAI-compatible model. Its next
// move is a pure function of the task tag in the first user
// message and the tool results the agent has sent back so far.
type agentFake struct {
	mu       sync.Mutex
	captured []string
}

func (f *agentFake) handler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := string(raw)
	f.mu.Lock()
	f.captured = append(f.captured, body)
	f.mu.Unlock()

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(raw, &req)
	toolMsgs := 0
	lastTool := ""
	for _, m := range req.Messages {
		if m.Role == "tool" {
			toolMsgs++
			lastTool = m.Content
		}
	}
	toolCall := func(id, name, args string) string {
		return `{"id":"chatcmpl-h","object":"chat.completion","model":"agent-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":"` + args + `"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	}
	toolCalls2 := func(c1, c2 string) string {
		return `{"id":"chatcmpl-h","object":"chat.completion","model":"agent-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[` + c1 + `,` + c2 + `]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	}
	call := func(id, name, args string) string {
		return `{"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":"` + args + `"}}`
	}
	final := func(text string) string {
		return `{"id":"chatcmpl-h","object":"chat.completion","model":"agent-model","choices":[{"index":0,"message":{"role":"assistant","content":"` + text + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	}

	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.Contains(body, "TASK:multi-edit"):
		switch toolMsgs {
		case 0:
			_, _ = io.WriteString(w, toolCalls2(call("c1", "read_file", `{\"path\":\"a.go\"}`), call("c2", "read_file", `{\"path\":\"b.go\"}`)))
		case 2:
			_, _ = io.WriteString(w, toolCalls2(call("c3", "apply_edit", `{\"path\":\"a.go\"}`), call("c4", "apply_edit", `{\"path\":\"b.go\"}`)))
		default:
			_, _ = io.WriteString(w, final("rename complete in 2 files"))
		}
	case strings.Contains(body, "TASK:fix-test"):
		switch {
		case toolMsgs == 0:
			_, _ = io.WriteString(w, toolCall("c1", "run_tests", `{}`))
		case toolMsgs == 1 && strings.Contains(lastTool, "TEST-FAIL-MARKER"):
			_, _ = io.WriteString(w, toolCall("c2", "apply_fix", `{\"path\":\"main.go\"}`))
		default:
			_, _ = io.WriteString(w, final("fixed; the failing test now passes"))
		}
	case strings.Contains(body, "TASK:repo-question"):
		switch toolMsgs {
		case 0:
			_, _ = io.WriteString(w, toolCall("c1", "read_file", `{\"path\":\"repo1.go\"}`))
		case 1:
			_, _ = io.WriteString(w, toolCall("c2", "read_file", `{\"path\":\"repo2.go\"}`))
		default:
			_, _ = io.WriteString(w, final("the answer is grounded in repo1 and repo2"))
		}
	default:
		_, _ = io.WriteString(w, final("ok"))
	}
}

func (f *agentFake) captures() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.captured...)
}

var agentTools = []any{
	map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "description": "read a file", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}},
	map[string]any{"type": "function", "function": map[string]any{"name": "apply_edit", "description": "apply an edit", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}},
	map[string]any{"type": "function", "function": map[string]any{"name": "apply_fix", "description": "apply a fix", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}},
	map[string]any{"type": "function", "function": map[string]any{"name": "run_tests", "description": "run the test suite", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}},
}

// toolResultFor is the scripted agent's side of the world: what
// each tool returns when the model calls it.
func toolResultFor(name, args string) string {
	switch name {
	case "read_file":
		switch {
		case strings.Contains(args, "a.go"):
			return "package a // FILE-A-MARKER\nfunc OldName() {}"
		case strings.Contains(args, "b.go"):
			return "package b // FILE-B-MARKER\nfunc UsesOldName() {}"
		case strings.Contains(args, "repo1.go"):
			return "package repo // REPO1-MARKER\n" + strings.Repeat("context ", 4500)
		case strings.Contains(args, "repo2.go"):
			return "package repo // REPO2-MARKER\n" + strings.Repeat("context ", 4500)
		}
	case "run_tests":
		return "FAIL TestParse: unexpected token — TEST-FAIL-MARKER"
	case "apply_edit", "apply_fix":
		return "edit applied"
	}
	return "ok"
}

// runAgentTask drives one scripted session to completion and
// returns the serving deployment of every turn plus the final
// assistant text.
func runAgentTask(t *testing.T, srv *Server, session, firstUser string) ([]string, string) {
	t.Helper()
	messages := []any{map[string]any{"role": "user", "content": firstUser}}
	var deployments []string
	for turn := 0; turn < 10; turn++ {
		raw, err := json.Marshal(map[string]any{
			"model": "agent-model", "session_id": session, "max_tokens": 256,
			"tools": agentTools, "messages": messages,
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(string(raw)))
		req.Header.Set("x-request-id", session+"-turn")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("session %s turn %d: status %d body %s", session, turn, rr.Code, rr.Body.String())
		}
		deployments = append(deployments, rr.Header().Get("X-Gateway-Deployment"))
		var resp struct {
			Choices []struct {
				Message struct {
					Content   *string `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || len(resp.Choices) == 0 {
			t.Fatalf("session %s turn %d: undecodable response %q err %v", session, turn, rr.Body.String(), err)
		}
		msg := resp.Choices[0].Message
		if len(msg.ToolCalls) == 0 {
			if msg.Content == nil {
				t.Fatalf("session %s turn %d: no content and no tool calls", session, turn)
			}
			return deployments, *msg.Content
		}
		// The agent appends the assistant's tool calls and the
		// scripted tool results, exactly as a real client does.
		var calls []any
		for _, tc := range msg.ToolCalls {
			calls = append(calls, map[string]any{"id": tc.ID, "type": "function", "function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments}})
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": calls})
		for _, tc := range msg.ToolCalls {
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": tc.ID, "content": toolResultFor(tc.Function.Name, tc.Function.Arguments)})
		}
	}
	t.Fatalf("session %s did not complete within 10 turns", session)
	return nil, ""
}

func agentProvider(id, model, baseURL string, priority, weight, window int) config.ProviderConfig {
	p := sessionProvider(id, model, baseURL, priority, weight, window)
	p.Models[0].Capabilities.Tools = true
	return p
}

func agentGateway(t *testing.T, providers []config.ProviderConfig, pool []string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_mesh"
	cfg.Routing.MaxAttempts = 3
	cfg.Routing.HedgingEnabled = false
	cfg.Providers = providers
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "agent-pool", Deployments: pool}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp-agent", CandidatePool: "agent-pool"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve-agent", PublicModel: "agent-model", RouteProfile: "rp-agent"}}
	return testGateway(t, cfg)
}

func TestCodingAgentHarness(t *testing.T) {
	// --- Tasks 1+2: a single healthy deployment; sessions pin
	// and tool traffic passes through unrewritten. ---
	fake := &agentFake{}
	up := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer up.Close()
	srv := agentGateway(t, []config.ProviderConfig{agentProvider("agentprov", "agent-model", up.URL, 5, 10, 200000)}, []string{"agent-model"})

	deps, final := runAgentTask(t, srv, "agent-multi", "TASK:multi-edit rename OldName to NewName everywhere it is used.")
	if final != "rename complete in 2 files" {
		t.Fatalf("multi-edit final = %q", final)
	}
	if len(deps) != 3 {
		t.Fatalf("multi-edit turns = %d, want 3 (%v)", len(deps), deps)
	}
	for i, d := range deps {
		if d != "agentprov/m" {
			t.Fatalf("multi-edit turn %d served by %q, want pinned agentprov/m", i, d)
		}
	}

	deps, final = runAgentTask(t, srv, "agent-fix", "TASK:fix-test the suite is red; find and fix the failure.")
	if final != "fixed; the failing test now passes" {
		t.Fatalf("fix-test final = %q", final)
	}
	for i, d := range deps {
		if d != "agentprov/m" {
			t.Fatalf("fix-test turn %d served by %q, want pinned agentprov/m", i, d)
		}
	}

	// Pass-through evidence: the last upstream request of the
	// multi-edit session must carry the tool results VERBATIM
	// and the same tool definitions the agent sent — nothing
	// stripped, summarized or re-invented by the gateway.
	caps := fake.captures()
	if len(caps) < 3 {
		t.Fatalf("fake captured %d requests, want at least 3", len(caps))
	}
	last := caps[2]
	for _, want := range []string{"FILE-A-MARKER", "FILE-B-MARKER", "read_file", "apply_edit", "run_tests"} {
		if !strings.Contains(last, want) {
			t.Fatalf("upstream request lost %q in translation:\n%s", want, last)
		}
	}
	foundFail := false
	for _, c := range caps {
		if strings.Contains(c, "TEST-FAIL-MARKER") {
			foundFail = true
		}
	}
	if !foundFail {
		t.Fatal("the failing-test tool result never reached the upstream verbatim")
	}

	// --- Task 3: the repo-question session outgrows the small
	// window mid-task and shifts, history intact. ---
	fake3 := &agentFake{}
	up3 := httptest.NewServer(http.HandlerFunc(fake3.handler))
	defer up3.Close()
	var smallCaps, bigCaps []string
	smallUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		smallCaps = append(smallCaps, string(raw))
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		fake3.handler(w, r)
	}))
	defer smallUp.Close()
	bigUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bigCaps = append(bigCaps, string(raw))
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		fake3.handler(w, r)
	}))
	defer bigUp.Close()
	srv3 := agentGateway(t, []config.ProviderConfig{
		agentProvider("smallprov", "small-model", smallUp.URL, 5, 10, 8000),
		agentProvider("bigprov", "big-model", bigUp.URL, 5, 1, 200000),
	}, []string{"small-model", "big-model"})

	deps, final = runAgentTask(t, srv3, "agent-repo", "TASK:repo-question how do repo1 and repo2 interact?")
	if final != "the answer is grounded in repo1 and repo2" {
		t.Fatalf("repo-question final = %q", final)
	}
	if deps[0] != "smallprov/m" {
		t.Fatalf("repo-question turn 0 = %q, want smallprov/m (pin first)", deps[0])
	}
	if deps[len(deps)-1] != "bigprov/m" {
		t.Fatalf("repo-question last turn = %q, want bigprov/m after context growth (%v)", deps[len(deps)-1], deps)
	}
	if len(bigCaps) == 0 || !strings.Contains(bigCaps[len(bigCaps)-1], "REPO1-MARKER") {
		t.Fatal("the shifted request did not carry the full session history to the long-context deployment")
	}

	// --- Feeding: the harness outcomes become a task-scoped
	// scorecard the task-profile machinery reads back with its
	// sample size. Pass rate is reported, never vibes: 3 tasks,
	// 3 passes. ---
	reg := scorecards.NewRegistry(10, 4)
	now := time.Now()
	_, err := reg.Upsert(scorecards.Scorecard{
		DeploymentID: "agentprov/m", TaskType: "coding", ProviderID: "agentprov", Model: "m",
		Version: 1, GeneratedAt: now,
		Evaluation: &scorecards.EvaluationRef{SuiteID: "coding-agent-harness", SuiteVersion: "1", SampleCount: 3, EvaluatedAt: now},
		Values: map[scorecards.Dimension]scorecards.Value{
			scorecards.DimCoding: {Score: 1.0, Provenance: scorecards.ProvenanceEvaluation, SampleCount: 3, Confidence: scorecards.ConfidenceFromSamples(3), SuiteVersion: "1", EvaluatedAt: now},
		},
	})
	if err != nil {
		t.Fatalf("harness scorecard rejected: %v", err)
	}
	v, ok := reg.QualityForTask("coding", "agentprov/m", scorecards.DimCoding)
	if !ok || v.SampleCount != 3 || v.Score != 1.0 {
		t.Fatalf("task profile readback = %+v ok=%v, want score 1.0 over 3 samples", v, ok)
	}
}
