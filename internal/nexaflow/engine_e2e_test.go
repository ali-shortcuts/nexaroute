package nexaflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

func intString(i int) string { return fmt.Sprintf("%04d", i) }

// E2E (a): LLM routing + conditional branch + approval pause/resume +
// video submit/status/output in one workflow, against the fake backend.
func TestE2E_LLM_Branch_Approval_Video(t *testing.T) {
	fake := newFakeBackend()
	eng := testEngine(fake, true)
	wf := &Workflow{ID: "e2e-a", Steps: []Step{
		{ID: "llm", NodeID: "nexaroute.llm.chat", Config: map[string]any{"route_or_alias": "team/coding"}, Input: map[string]any{"prompt": "hello"}},
		{ID: "cond", NodeID: "nexaroute.control.condition", Config: map[string]any{"field": "text", "operator": "contains", "expected": "fake"}, Bindings: map[string]string{"value": "llm"}},
		{ID: "br", NodeID: "nexaroute.control.branch", Config: map[string]any{"true_branch": "proceed", "false_branch": "skip"}, Bindings: map[string]string{"condition_result": "cond.result"}},
		{ID: "appr", NodeID: "nexaroute.control.approval", Branch: "proceed", Input: map[string]any{"requester": "requester-1"}},
		{ID: "vsubmit", NodeID: "nexaroute.video.submit", Branch: "proceed", Input: map[string]any{"prompt": "a cat", "duration_seconds": 5.0, "mode": "text_to_video", "aspect_ratio": "16:9"}},
		{ID: "vstatus", NodeID: "nexaroute.video.status", Branch: "proceed", Bindings: map[string]string{"job_id": "vsubmit.job_id"}},
		{ID: "voutput", NodeID: "nexaroute.video.output", Branch: "proceed", Bindings: map[string]string{"job_id": "vsubmit.job_id"}},
	}, Edges: [][2]string{{"llm", "cond"}, {"cond", "br"}, {"br", "appr"}, {"appr", "vsubmit"}, {"vsubmit", "vstatus"}, {"vstatus", "voutput"}}}
	run := mustRun(t, eng, wf, nil)
	if run.Status != RunPaused || run.ResumeToken == "" {
		t.Fatalf("first run = %+v, want paused at approval", run)
	}
	if got := run.Outputs["br"]["selected_branch"]; got != "proceed" {
		t.Fatalf("branch = %v, want proceed", got)
	}
	if _, submitted := run.Outputs["vsubmit"]; submitted {
		t.Fatal("video submitted before approval resumed")
	}
	resumed := mustRunResume(t, eng, run.ResumeToken, "approved")
	if resumed.Status != RunCompleted {
		t.Fatalf("resumed = %+v", resumed)
	}
	if resumed.Outputs["llm"]["text"] != "fake reply" {
		t.Fatalf("llm text = %#v", resumed.Outputs["llm"])
	}
	if resumed.Outputs["vstatus"]["state"] != "completed" {
		t.Fatalf("video status = %#v, want completed", resumed.Outputs["vstatus"])
	}
	assets, ok := resumed.Outputs["voutput"]["assets"].([]any)
	if !ok || len(assets) != 1 {
		t.Fatalf("video output assets = %#v, want 1 asset", resumed.Outputs["voutput"])
	}
}

// E2E (b): unauthorized MCP use fails closed and stops the workflow.
func TestE2E_MCP_UnauthorizedFailsClosed(t *testing.T) {
	fake := newFakeBackend()
	eng := testEngine(fake, true)
	mcpStep := Step{ID: "call", NodeID: "nexaroute.mcp.call", Config: map[string]any{"allowlist": []any{"search_files"}}, Input: map[string]any{"server_id": "tools", "tool": "search_files"}}
	after := Step{ID: "after", NodeID: "nexaroute.control.merge", Input: map[string]any{"first": map[string]any{}, "second": map[string]any{}}}
	wf := &Workflow{ID: "e2e-b", Steps: []Step{mcpStep, after}, Edges: [][2]string{{"call", "after"}}}

	// No roles at all: the registry rejects before the node runs.
	run, err := eng.Run(context.Background(), wf, authz.Identity{Subject: "nobody"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != RunFailed || run.Error == nil || run.Error.Code != "permission_denied" {
		t.Fatalf("roleless run = %+v, want failed permission_denied", run)
	}
	if len(fake.MCPCalls) != 0 {
		t.Fatalf("backend MCP calls = %v, want none before permission check", fake.MCPCalls)
	}
	if _, executed := run.Outputs["after"]; executed {
		t.Fatal("step after a permission-denied step executed")
	}

	// Allowlisted by the gateway but not by the node: tool_forbidden,
	// also before any backend call.
	fake2 := newFakeBackend()
	eng2 := testEngine(fake2, true)
	denied := Step{ID: "call", NodeID: "nexaroute.mcp.call", Config: map[string]any{"allowlist": []any{"other_tool"}}, Input: map[string]any{"server_id": "tools", "tool": "search_files"}}
	wf2 := &Workflow{ID: "e2e-b2", Steps: []Step{denied, after}, Edges: [][2]string{{"call", "after"}}}
	run2 := mustRun(t, eng2, wf2, nil)
	if run2.Status != RunFailed || run2.Error.Code != "tool_forbidden" {
		t.Fatalf("allowlist run = %+v, want failed tool_forbidden", run2)
	}
	if len(fake2.MCPCalls) != 0 {
		t.Fatalf("backend MCP calls = %v, want none for a non-allowlisted tool", fake2.MCPCalls)
	}

	// In the node allowlist but not listed by the gateway: also
	// tool_forbidden, and the gateway call for the tool is never made.
	fake3 := newFakeBackend()
	fake3.MCPTools = map[string][]MCPTool{"tools": {{Name: "read_file"}}}
	eng3 := testEngine(fake3, true)
	wf3 := &Workflow{ID: "e2e-b3", Steps: []Step{mcpStep, after}, Edges: [][2]string{{"call", "after"}}}
	run3 := mustRun(t, eng3, wf3, nil)
	if run3.Status != RunFailed || run3.Error.Code != "tool_forbidden" {
		t.Fatalf("unlisted run = %+v, want failed tool_forbidden", run3)
	}
	if len(fake3.MCPCalls) != 0 {
		t.Fatalf("backend MCP calls = %v, want none for an unlisted tool", fake3.MCPCalls)
	}
}

// E2E (c): invalid input is rejected before any side effect.
func TestE2E_InvalidInputBeforeSideEffect(t *testing.T) {
	fake := newFakeBackend()
	eng := testEngine(fake, true)
	after := Step{ID: "after", NodeID: "nexaroute.control.merge", Input: map[string]any{"first": map[string]any{}, "second": map[string]any{}}}

	videoWf := &Workflow{ID: "e2e-c1", Steps: []Step{
		{ID: "v", NodeID: "nexaroute.video.submit", Input: map[string]any{"prompt": "a cat", "duration_seconds": 0.0}},
		after,
	}, Edges: [][2]string{{"v", "after"}}}
	run := mustRun(t, eng, videoWf, nil)
	if run.Status != RunFailed || run.Error.Code != "validation_error" {
		t.Fatalf("bad-duration run = %+v, want failed validation_error", run)
	}
	if fake.VideoSubmitCalls != 0 {
		t.Fatalf("video submit calls = %d, want 0 (validation precedes the call)", fake.VideoSubmitCalls)
	}
	if _, executed := run.Outputs["after"]; executed {
		t.Fatal("step after an invalid step executed")
	}

	llmWf := &Workflow{ID: "e2e-c2", Steps: []Step{
		{ID: "llm", NodeID: "nexaroute.llm.chat", Config: map[string]any{"route_or_alias": "team/coding"}, Input: map[string]any{"prompt": "hi", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		after,
	}, Edges: [][2]string{{"llm", "after"}}}
	run = mustRun(t, eng, llmWf, nil)
	if run.Status != RunFailed || run.Error.Code != "validation_error" {
		t.Fatalf("prompt+messages run = %+v, want failed validation_error", run)
	}
	if fake.ChatCalls != 0 {
		t.Fatalf("chat calls = %d, want 0 (validation precedes the call)", fake.ChatCalls)
	}
}

// E2E (d): output bound exceeded -> output_too_large; a cancelled
// context -> cancelled, never partial success.
func TestE2E_OutputBoundAndCancellation(t *testing.T) {
	fake := newFakeBackend()
	// A wide result, not a long string: redaction truncates any single
	// string over 8 KiB before the output bound applies, so a long
	// string alone can never trip output_too_large. Four hundred
	// 1 KiB values (~410 KiB of JSON) exceed the node's 256 KiB bound
	// without any single string being truncated.
	big := make(map[string]any, 400)
	for i := 0; i < 400; i++ {
		big["key-"+intString(i)] = strings.Repeat("x", 1024)
	}
	fake.MCPResult = big
	eng := testEngine(fake, true)
	wf := &Workflow{ID: "e2e-d", Steps: []Step{
		{ID: "call", NodeID: "nexaroute.mcp.call", Config: map[string]any{"allowlist": []any{"search_files"}}, Input: map[string]any{"server_id": "tools", "tool": "search_files"}},
	}}
	run := mustRun(t, eng, wf, nil)
	if run.Status != RunFailed || run.Error == nil || run.Error.Code != "output_too_large" {
		t.Fatalf("oversize run = %+v, want failed output_too_large", run)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waitWf := &Workflow{ID: "e2e-d2", Steps: []Step{
		{ID: "wait", NodeID: "nexaroute.control.wait", Input: map[string]any{"duration_ms": 1000.0}},
	}}
	run, err := eng.Run(ctx, waitWf, ownerPrincipal(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != RunFailed || run.Error == nil || run.Error.Code != "cancelled" {
		t.Fatalf("pre-cancelled run = %+v, want failed cancelled", run)
	}
}

// E2E (e): template instantiation with a missing value fails precisely,
// and the engine refuses the raw template.
func TestE2E_TemplateMissingValueAndRawRefusal(t *testing.T) {
	var tpl Template
	for _, candidate := range DefaultTemplates() {
		if candidate.ID == "video-generation-pipeline" {
			tpl = candidate
		}
	}
	if tpl.ID == "" {
		t.Fatal("video template not found")
	}
	if _, err := tpl.InstantiateWorkflow(map[string]string{}, nil, true); err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("missing-prompt instantiation error = %v, want an error naming prompt", err)
	}
	// Raw template, bypassing instantiation: the engine must refuse it.
	raw := &Workflow{ID: tpl.ID, Steps: []Step{
		{ID: "nexaroute.video.submit", NodeID: "nexaroute.video.submit", Input: map[string]any{"prompt": "{{prompt}}", "duration_seconds": 5.0}},
	}}
	eng := testEngine(newFakeBackend(), true)
	if err := eng.Validate(raw); err == nil || !strings.Contains(err.Error(), "unresolved placeholder") {
		t.Fatalf("raw template validation error = %v, want unresolved placeholder", err)
	}
}

// Secret safety: a resolved secret value must never appear in run
// history, outputs, or errors, and secret-keyed output fields are
// redacted in history.
func TestE2E_SecretNeverSurfaces(t *testing.T) {
	fake := newFakeBackend()
	planted := fake.Secrets["api-token"]
	if planted == "" {
		t.Fatal("fake backend has no api-token secret to plant")
	}
	eng := testEngine(fake, true)
	wf := &Workflow{ID: "secret", Steps: []Step{
		{ID: "http", NodeID: "nexaroute.http.request", Config: map[string]any{"auth_secret_ref": "api-token"}, Input: map[string]any{"url": "http://127.0.0.1:9/notify", "method": "GET"}},
	}}
	run := mustRun(t, eng, wf, nil)
	if run.Status != RunCompleted {
		t.Fatalf("http run = %+v", run)
	}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	if strings.Contains(string(raw), planted) {
		t.Fatalf("resolved secret appears in run history/outputs")
	}
	redacted := RedactJSON(map[string]any{"api_key": planted, "total_prompt_tokens": 5})
	if strings.Contains(redacted, planted) {
		t.Fatalf("secret-keyed value survived redaction: %s", redacted)
	}
	if !strings.Contains(redacted, "total_prompt_tokens") {
		t.Fatalf("legitimate counter was wrongly redacted: %s", redacted)
	}
}
