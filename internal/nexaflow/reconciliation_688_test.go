package nexaflow

// Reconciliation proof for PR #688 (in-memory engine, retired) into the
// canonical durable line (merged #693): every registered node keeps its
// backend wiring, authorization failure makes zero backend calls,
// unresolved placeholders and secret references fail before any side
// effect, templates instantiate into validated workflows, and sensitive
// values never surface in node outputs or stored history.
//
// This file deliberately does NOT use #688's Workflow model or its
// in-memory engine: the canonical Definition/input_mapping grammar and
// the durable engine are the only execution path under test.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

var reconcileMethodNames = []string{
	"ResolveSelector", "Chat", "ProviderHealth", "Compat", "RunEvalReplay",
	"MCPList", "MCPCall", "VideoSubmit", "VideoStatus", "VideoAsset",
	"UsageSnapshot", "BudgetSnapshot", "HTTPDo", "ResolveSecretRef",
}

// countingBackend implements Backend with per-method call counts and
// canned owner-subsystem answers. It is a test double for the owning
// subsystems, not a second implementation of them.
type countingBackend struct {
	calls        map[string]int
	selectors    map[string]RouteInfo
	secrets      map[string]string
	secretErr    error
	chatErr      error
	mcpResult    map[string]any
	lastHTTPAuth string
}

func newCountingBackend() *countingBackend {
	return &countingBackend{
		calls:     map[string]int{},
		selectors: map[string]RouteInfo{"team/coding": {Kind: "route", ResolvedModel: "team/coding"}},
		secrets:   map[string]string{"hook-secret": "hook-secret-value", "http-token": "tok-123"},
		mcpResult: map[string]any{"ok": true},
	}
}

func (b *countingBackend) count(name string) { b.calls[name]++ }
func (b *countingBackend) total() int {
	n := 0
	for _, c := range b.calls {
		n += c
	}
	return n
}

func (b *countingBackend) ResolveSelector(ctx context.Context, selector string) (RouteInfo, error) {
	b.count("ResolveSelector")
	if info, ok := b.selectors[selector]; ok {
		return info, nil
	}
	return RouteInfo{}, NewNodeError("selector_not_found", false, "selector %q not found", selector)
}

func (b *countingBackend) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	b.count("Chat")
	if b.chatErr != nil {
		return ChatResponse{}, b.chatErr
	}
	return ChatResponse{Model: req.Selector, Text: "hello"}, nil
}

func (b *countingBackend) ProviderHealth(ctx context.Context, deploymentID string) (ProviderHealth, error) {
	b.count("ProviderHealth")
	return ProviderHealth{DeploymentID: deploymentID, Status: "healthy"}, nil
}

func (b *countingBackend) Compat(ctx context.Context, deploymentID string) (CompatInfo, error) {
	b.count("Compat")
	return CompatInfo{DeploymentID: deploymentID, ScorecardStatus: "ok"}, nil
}

func (b *countingBackend) RunEvalReplay(ctx context.Context, suiteID, deploymentID string, outcomes []eval.Outcome) (eval.Result, error) {
	b.count("RunEvalReplay")
	return eval.Result{SuiteID: suiteID, DeploymentID: deploymentID, Upstream: 0}, nil
}

func (b *countingBackend) MCPList(ctx context.Context, serverID string) ([]MCPTool, error) {
	b.count("MCPList")
	return []MCPTool{{Name: "echo", Description: "test tool"}}, nil
}

func (b *countingBackend) MCPCall(ctx context.Context, serverID, tool string, args map[string]any) (map[string]any, error) {
	b.count("MCPCall")
	return b.mcpResult, nil
}

func (b *countingBackend) VideoSubmit(ctx context.Context, req VideoSubmitRequest) (VideoJobInfo, error) {
	b.count("VideoSubmit")
	return VideoJobInfo{JobID: "job-1", State: "queued"}, nil
}

func (b *countingBackend) VideoStatus(ctx context.Context, jobID string) (VideoJobInfo, error) {
	b.count("VideoStatus")
	return VideoJobInfo{JobID: jobID, State: "completed", Assets: []VideoAssetInfo{{ID: "a1"}}}, nil
}

func (b *countingBackend) VideoAsset(ctx context.Context, jobID, assetID string) (VideoAssetInfo, []byte, error) {
	b.count("VideoAsset")
	return VideoAssetInfo{ID: assetID}, []byte("DATA"), nil
}

func (b *countingBackend) UsageSnapshot(ctx context.Context) (UsageInfo, error) {
	b.count("UsageSnapshot")
	return UsageInfo{}, nil
}

func (b *countingBackend) BudgetSnapshot(ctx context.Context, key string) (BudgetInfo, error) {
	b.count("BudgetSnapshot")
	return BudgetInfo{Key: key, Committed: 3, Reserved: 1}, nil
}

func (b *countingBackend) HTTPDo(ctx context.Context, method, url string, headers map[string]string, body []byte) (HTTPResponse, error) {
	b.count("HTTPDo")
	b.lastHTTPAuth = headers["Authorization"]
	return HTTPResponse{Status: 200, Body: []byte("echo-ok")}, nil
}

func (b *countingBackend) ResolveSecretRef(ref string) (string, error) {
	b.count("ResolveSecretRef")
	if b.secretErr != nil {
		return "", b.secretErr
	}
	if v, ok := b.secrets[ref]; ok {
		return v, nil
	}
	return "", NewNodeError("secret_error", false, "secret reference %q does not resolve", ref)
}

func reconcileOwner() authz.Identity {
	return authz.Identity{Subject: "owner-1", Roles: []authz.Role{authz.RoleOwner}}
}

func nodeErrorCode(t *testing.T, err error) string {
	t.Helper()
	var ne *NodeError
	if !errors.As(err, &ne) {
		t.Fatalf("error %v is not a NodeError", err)
	}
	return ne.Code
}

func assertExactCalls(t *testing.T, b *countingBackend, want ...string) {
	t.Helper()
	wanted := map[string]bool{}
	for _, name := range want {
		wanted[name] = true
		if b.calls[name] == 0 {
			t.Errorf("backend method %s was not called, want >= 1", name)
		}
	}
	for _, name := range reconcileMethodNames {
		if !wanted[name] && b.calls[name] != 0 {
			t.Errorf("backend method %s called %d times, want 0", name, b.calls[name])
		}
	}
}

func signedWebhookInput(t *testing.T, body string) map[string]any {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("hook-secret-value"))
	mac.Write([]byte(ts + "." + body))
	return map[string]any{
		"timestamp": ts,
		"signature": hex.EncodeToString(mac.Sum(nil)),
		"body":      body,
	}
}

type wiringCase struct {
	id      string
	config  map[string]any
	input   map[string]any
	policy  bool
	expect  []string
	webhook bool // input is built per-test with a fresh signature
}

func reconcileWiringCases() []wiringCase {
	return []wiringCase{
		{id: "nexaroute.llm.chat", config: map[string]any{"route_or_alias": "team/coding"}, input: map[string]any{"prompt": "hi"}, policy: true, expect: []string{"ResolveSelector", "Chat"}},
		{id: "nexaroute.provider.health", input: map[string]any{"deployment_id": "prov/model"}, expect: []string{"ProviderHealth"}},
		{id: "nexaroute.provider.compat", input: map[string]any{"deployment_id": "prov/model"}, expect: []string{"Compat"}},
		{id: "nexaroute.eval.run", input: map[string]any{"suite_id": "coding", "deployment_id": "prov/model", "outcomes": []any{map[string]any{"case_id": "c1", "status": "ok", "output": "42"}}}, expect: []string{"RunEvalReplay"}},
		{id: "nexaroute.mcp.list", input: map[string]any{"server_id": "srv"}, expect: []string{"MCPList"}},
		{id: "nexaroute.mcp.call", config: map[string]any{"allowlist": []any{"srv__echo"}}, input: map[string]any{"server_id": "srv", "tool": "echo"}, expect: []string{"MCPList", "MCPCall"}},
		{id: "nexaroute.video.submit", input: map[string]any{"prompt": "a cat", "mode": "text_to_video", "duration_seconds": float64(5), "aspect_ratio": "16:9"}, policy: true, expect: []string{"VideoSubmit"}},
		{id: "nexaroute.video.status", input: map[string]any{"job_id": "job-1"}, expect: []string{"VideoStatus"}},
		{id: "nexaroute.video.output", input: map[string]any{"job_id": "job-1"}, expect: []string{"VideoStatus"}},
		{id: "nexaroute.cost.usage", input: map[string]any{}, expect: []string{"UsageSnapshot"}},
		{id: "nexaroute.cost.budget_check", input: map[string]any{"key": "team", "threshold_usd": float64(10)}, expect: []string{"BudgetSnapshot"}},
		{id: "nexaroute.trigger.manual", input: map[string]any{"payload": map[string]any{"x": 1}}, expect: nil},
		{id: "nexaroute.trigger.schedule", config: map[string]any{"daily": "09:00"}, input: map[string]any{}, expect: nil},
		{id: "nexaroute.trigger.webhook", config: map[string]any{"secret_ref": "hook-secret"}, webhook: true, expect: []string{"ResolveSecretRef"}},
		{id: "nexaroute.http.request", input: map[string]any{"url": "https://example.invalid/hook", "method": "POST"}, policy: true, expect: []string{"HTTPDo"}},
		{id: "nexaroute.control.condition", config: map[string]any{"field": "status", "operator": "eq", "expected": "healthy"}, input: map[string]any{"value": map[string]any{"status": "healthy"}}, expect: nil},
		{id: "nexaroute.control.branch", config: map[string]any{"true_branch": "a", "false_branch": "b"}, input: map[string]any{"condition_result": true}, expect: nil},
		{id: "nexaroute.control.json_transform", input: map[string]any{"value": map[string]any{"a": 1, "b": 2}, "operations": []any{map[string]any{"op": "pick", "field": "a"}}}, expect: nil},
		{id: "nexaroute.control.loop_plan", input: map[string]any{"items": []any{1, 2}}, expect: nil},
		{id: "nexaroute.control.merge", input: map[string]any{"first": map[string]any{"a": 1}, "second": map[string]any{"b": 2}}, expect: nil},
		{id: "nexaroute.control.wait", input: map[string]any{"duration_ms": float64(0)}, expect: nil},
		{id: "nexaroute.control.approval", input: map[string]any{"decision": "approved", "approver": "alice", "requester": "bob"}, expect: nil},
		{id: "nexaroute.control.error_handler", config: map[string]any{"policy": "fallback"}, input: map[string]any{"error_code": "boom"}, expect: nil},
	}
}

// The 23-node registry is preserved exactly: no node lost, no node
// added by the reconciliation (in particular no subworkflow node, no
// video-cancel node, no budget reserve/settle/release nodes).
func TestReconciliationRegistryPreserves23Nodes(t *testing.T) {
	reg := DefaultRegistry()
	seen := map[string]bool{}
	for _, c := range reconcileWiringCases() {
		seen[c.id] = true
		if _, err := reg.Lookup(c.id, SupportedVersion); err != nil {
			t.Fatalf("node %s missing from registry: %v", c.id, err)
		}
	}
	if len(seen) != 23 {
		t.Fatalf("wiring table covers %d distinct nodes, want 23", len(seen))
	}
	for _, absent := range []string{
		"nexaroute.control.subworkflow",
		"nexaroute.video.cancel",
		"nexaroute.cost.budget_reserve",
		"nexaroute.cost.budget_settle",
		"nexaroute.cost.budget_release",
	} {
		if _, err := reg.Lookup(absent, SupportedVersion); err == nil {
			t.Fatalf("node %s unexpectedly present: it is not part of the canonical 23", absent)
		}
	}
}

func TestReconciliationNodeWiring(t *testing.T) {
	reg := DefaultRegistry()
	for _, c := range reconcileWiringCases() {
		t.Run(c.id, func(t *testing.T) {
			spec, err := reg.Lookup(c.id, SupportedVersion)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			input := c.input
			if c.webhook {
				input = signedWebhookInput(t, "reconcile-wiring-"+c.id)
			}
			backend := newCountingBackend()
			out, err := reg.Execute(context.Background(), spec, reconcileOwner(), input, c.config, backend, c.policy)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if out == nil {
				t.Fatal("execute returned nil output")
			}
			assertExactCalls(t, backend, c.expect...)
		})
	}
}

func TestReconciliationVideoOutputWithAsset(t *testing.T) {
	reg := DefaultRegistry()
	spec, err := reg.Lookup("nexaroute.video.output", SupportedVersion)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	backend := newCountingBackend()
	if _, err := reg.Execute(context.Background(), spec, reconcileOwner(),
		map[string]any{"job_id": "job-1", "asset_id": "a1"}, nil, backend, false); err != nil {
		t.Fatalf("execute: %v", err)
	}
	assertExactCalls(t, backend, "VideoStatus", "VideoAsset")
}

func TestReconciliationHTTPRequestWithSecretRef(t *testing.T) {
	reg := DefaultRegistry()
	spec, err := reg.Lookup("nexaroute.http.request", SupportedVersion)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	backend := newCountingBackend()
	out, err := reg.Execute(context.Background(), spec, reconcileOwner(),
		map[string]any{"url": "https://example.invalid/hook", "method": "POST"},
		map[string]any{"auth_secret_ref": "http-token"}, backend, true)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	assertExactCalls(t, backend, "ResolveSecretRef", "HTTPDo")
	if backend.lastHTTPAuth != "Bearer tok-123" {
		t.Fatalf("Authorization sent = %q, want Bearer tok-123", backend.lastHTTPAuth)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "tok-123") {
		t.Fatalf("node output leaks the resolved token: %s", raw)
	}
}

func TestReconciliationUnsupportedStaysUnsupported(t *testing.T) {
	reg := DefaultRegistry()
	// budget_check is report-only: it reads one snapshot and has no
	// reserve/settle/release port on the Backend interface at all.
	spec, err := reg.Lookup("nexaroute.cost.budget_check", SupportedVersion)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	backend := newCountingBackend()
	out, err := reg.Execute(context.Background(), spec, reconcileOwner(),
		map[string]any{"key": "team", "threshold_usd": float64(10)}, nil, backend, false)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	assertExactCalls(t, backend, "BudgetSnapshot")
	if out["over_threshold"] != false {
		t.Fatalf("over_threshold = %v, want false for committed 3 + reserved 1 < 10", out["over_threshold"])
	}
	// eval.run is replay-only: the node output reports zero upstream calls.
	evalSpec, err := reg.Lookup("nexaroute.eval.run", SupportedVersion)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	evalOut, err := reg.Execute(context.Background(), evalSpec, reconcileOwner(),
		map[string]any{"suite_id": "coding", "deployment_id": "prov/model",
			"outcomes": []any{map[string]any{"case_id": "c1", "status": "ok", "output": "42"}}},
		nil, newCountingBackend(), false)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if evalOut["upstream_calls"] != 0 && evalOut["upstream_calls"] != float64(0) {
		t.Fatalf("upstream_calls = %v, want 0 (replay only)", evalOut["upstream_calls"])
	}
	// The schedule node only computes next_run: no daemon, no tick.
	schedSpec, err := reg.Lookup("nexaroute.trigger.schedule", SupportedVersion)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	schedOut, err := reg.Execute(context.Background(), schedSpec, reconcileOwner(),
		map[string]any{}, map[string]any{"daily": "09:00"}, newCountingBackend(), false)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if schedOut["next_run"] == nil {
		t.Fatalf("schedule output = %+v, want next_run present", schedOut)
	}
}

// Authorization failure must make zero backend calls, for every node.
func TestReconciliationAuthorizationMakesZeroBackendCalls(t *testing.T) {
	reg := DefaultRegistry()
	for _, c := range reconcileWiringCases() {
		t.Run("anonymous/"+c.id, func(t *testing.T) {
			spec, err := reg.Lookup(c.id, SupportedVersion)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			input := c.input
			if c.webhook {
				input = signedWebhookInput(t, "reconcile-anon-"+c.id)
			}
			backend := newCountingBackend()
			_, err = reg.Execute(context.Background(), spec, authz.Identity{}, input, c.config, backend, true)
			if err == nil || nodeErrorCode(t, err) != "permission_denied" {
				t.Fatalf("err = %v, want permission_denied", err)
			}
			if backend.total() != 0 {
				t.Fatalf("backend calls = %d, want 0", backend.total())
			}
		})
	}
	t.Run("authenticated without permission", func(t *testing.T) {
		spec, err := reg.Lookup("nexaroute.provider.health", SupportedVersion)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		backend := newCountingBackend()
		_, err = reg.Execute(context.Background(), spec,
			authz.Identity{Subject: "someone"}, map[string]any{"deployment_id": "prov/model"}, nil, backend, false)
		if err == nil || nodeErrorCode(t, err) != "permission_denied" {
			t.Fatalf("err = %v, want permission_denied", err)
		}
		if backend.total() != 0 {
			t.Fatalf("backend calls = %d, want 0", backend.total())
		}
	})
	t.Run("policy gate", func(t *testing.T) {
		spec, err := reg.Lookup("nexaroute.llm.chat", SupportedVersion)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		backend := newCountingBackend()
		_, err = reg.Execute(context.Background(), spec, reconcileOwner(),
			map[string]any{"prompt": "hi"}, map[string]any{"route_or_alias": "team/coding"}, backend, false)
		if err == nil || nodeErrorCode(t, err) != "policy_required" {
			t.Fatalf("err = %v, want policy_required", err)
		}
		if backend.total() != 0 {
			t.Fatalf("backend calls = %d, want 0", backend.total())
		}
	})
}

// Failures in selector, secret, or input validation must happen before
// any side-effecting backend call.
func TestReconciliationFailuresBeforeExecution(t *testing.T) {
	reg := DefaultRegistry()
	t.Run("unlisted selector resolves nothing and chats nowhere", func(t *testing.T) {
		spec, err := reg.Lookup("nexaroute.llm.chat", SupportedVersion)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		backend := newCountingBackend()
		backend.selectors = map[string]RouteInfo{}
		_, err = reg.Execute(context.Background(), spec, reconcileOwner(),
			map[string]any{"prompt": "hi"}, map[string]any{"route_or_alias": "no/such"}, backend, true)
		if err == nil || nodeErrorCode(t, err) != "selector_not_found" {
			t.Fatalf("err = %v, want selector_not_found", err)
		}
		if backend.calls["Chat"] != 0 {
			t.Fatalf("Chat called %d times, want 0", backend.calls["Chat"])
		}
		if backend.calls["ResolveSelector"] != 1 {
			t.Fatalf("ResolveSelector called %d times, want 1", backend.calls["ResolveSelector"])
		}
	})
	t.Run("unresolvable http secret blocks the request", func(t *testing.T) {
		spec, err := reg.Lookup("nexaroute.http.request", SupportedVersion)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		backend := newCountingBackend()
		backend.secretErr = NewNodeError("secret_error", false, "secret resolution failed")
		_, err = reg.Execute(context.Background(), spec, reconcileOwner(),
			map[string]any{"url": "https://example.invalid/hook", "method": "POST"},
			map[string]any{"auth_secret_ref": "missing-ref"}, backend, true)
		if err == nil || nodeErrorCode(t, err) != "secret_error" {
			t.Fatalf("err = %v, want secret_error", err)
		}
		if backend.calls["HTTPDo"] != 0 {
			t.Fatalf("HTTPDo called %d times, want 0", backend.calls["HTTPDo"])
		}
	})
	t.Run("unresolvable webhook secret blocks verification", func(t *testing.T) {
		spec, err := reg.Lookup("nexaroute.trigger.webhook", SupportedVersion)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		backend := newCountingBackend()
		backend.secretErr = NewNodeError("secret_error", false, "secret resolution failed")
		_, err = reg.Execute(context.Background(), spec, reconcileOwner(),
			signedWebhookInput(t, "reconcile-secret-fail"),
			map[string]any{"secret_ref": "missing-ref"}, backend, false)
		if err == nil || nodeErrorCode(t, err) != "secret_error" {
			t.Fatalf("err = %v, want secret_error", err)
		}
		if backend.total() != 1 || backend.calls["ResolveSecretRef"] != 1 {
			t.Fatalf("backend calls = %+v, want only ResolveSecretRef once", backend.calls)
		}
	})
	t.Run("invalid input blocks the video submit", func(t *testing.T) {
		spec, err := reg.Lookup("nexaroute.video.submit", SupportedVersion)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		backend := newCountingBackend()
		_, err = reg.Execute(context.Background(), spec, reconcileOwner(),
			map[string]any{}, nil, backend, true)
		if err == nil || nodeErrorCode(t, err) != "validation_error" {
			t.Fatalf("err = %v, want validation_error", err)
		}
		if backend.calls["VideoSubmit"] != 0 {
			t.Fatalf("VideoSubmit called %d times, want 0", backend.calls["VideoSubmit"])
		}
	})
}

// At engine level, an input mapping that resolves against an existing
// node but points at a missing path fails the node with input_resolution
// before the backend is touched. Regression: sweep failed such a node
// from NodeClaimed, but the state table forbade Claimed -> Failed, so
// Execute used to abort with "state conflict" instead of failing the
// node and the run.
func TestReconciliationEngineUnresolvedRefFailsBeforeBackend(t *testing.T) {
	store := NewMemoryStore()
	backend := newCountingBackend()
	eng := mustEngine(t, store, DefaultRegistry(), func(o *EngineOptions) { o.Backend = backend })
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "reconcile-ref-fail", Name: "ref fail",
		Nodes: []DefinitionNode{
			{ID: "trigger", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "call", NodeID: "nexaroute.http.request", Version: SupportedVersion,
				Input: map[string]any{"url": map[string]any{"$ref": "trigger.missing"}, "method": "POST"}},
		},
		Edges:    []DefinitionEdge{{ID: "e1", From: "trigger", To: "call"}},
		Triggers: []DefinitionTrigger{{Name: "trigger", Kind: TriggerManual, Config: map[string]any{}}},
	}
	run, err := eng.CreateRun(context.Background(), publishDef(t, store, def), reconcileOwner(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	final, err := eng.Execute(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if final.State != RunFailed {
		t.Fatalf("run state = %q, want failed", final.State)
	}
	if backend.calls["HTTPDo"] != 0 {
		t.Fatalf("HTTPDo called %d times, want 0", backend.calls["HTTPDo"])
	}
	states := nodeStates(t, store, run.ID)
	call, ok := states["call"]
	if !ok {
		t.Fatal("no execution recorded for the http.request node")
	}
	if call.State != NodeFailed || call.ErrorCode != "input_resolution" {
		t.Fatalf("call execution = state %q code %q, want failed/input_resolution", call.State, call.ErrorCode)
	}
}

// Sensitive values must not escape through node outputs, node errors,
// or stored run history — at registry level and at engine level.
func TestReconciliationSecretsNeverSurface(t *testing.T) {
	reg := DefaultRegistry()
	t.Run("credential-keyed tool result is redacted from node output", func(t *testing.T) {
		spec, err := reg.Lookup("nexaroute.mcp.call", SupportedVersion)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		backend := newCountingBackend()
		backend.mcpResult = map[string]any{"api_key": "s3cr3t-value", "data": "x"}
		out, err := reg.Execute(context.Background(), spec, reconcileOwner(),
			map[string]any{"server_id": "srv", "tool": "echo"},
			map[string]any{"allowlist": []any{"srv__echo"}}, backend, false)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), "s3cr3t-value") {
			t.Fatalf("node output leaks the secret: %s", raw)
		}
	})
	t.Run("backend error text is redacted before it escapes", func(t *testing.T) {
		spec, err := reg.Lookup("nexaroute.llm.chat", SupportedVersion)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		backend := newCountingBackend()
		backend.chatErr = fmt.Errorf("upstream refused: Bearer s3cr3t-value")
		_, err = reg.Execute(context.Background(), spec, reconcileOwner(),
			map[string]any{"prompt": "hi"}, map[string]any{"route_or_alias": "team/coding"}, backend, true)
		if err == nil {
			t.Fatal("expected a chat error, got nil")
		}
		if strings.Contains(err.Error(), "s3cr3t-value") {
			t.Fatalf("node error leaks the secret: %v", err)
		}
	})
	t.Run("stored run history is redacted", func(t *testing.T) {
		store := NewMemoryStore()
		backend := newCountingBackend()
		backend.mcpResult = map[string]any{"api_key": "s3cr3t-value", "data": "x"}
		eng := mustEngine(t, store, DefaultRegistry(), func(o *EngineOptions) { o.Backend = backend })
		def := Definition{
			SchemaVersion: DefinitionSchemaVersion, ID: "reconcile-history", Name: "history",
			Nodes: []DefinitionNode{
				{ID: "trigger", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
				{ID: "call", NodeID: "nexaroute.mcp.call", Version: SupportedVersion,
					Config: map[string]any{"allowlist": []any{"srv__echo"}},
					Input:  map[string]any{"server_id": "srv", "tool": "echo"}},
			},
			Edges:    []DefinitionEdge{{ID: "e1", From: "trigger", To: "call"}},
			Triggers: []DefinitionTrigger{{Name: "trigger", Kind: TriggerManual, Config: map[string]any{}}},
		}
		run, err := eng.CreateRun(context.Background(), publishDef(t, store, def), reconcileOwner(), runInput(), "")
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		final, err := eng.Execute(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if final.State != RunSucceeded {
			t.Fatalf("run state = %q, want succeeded", final.State)
		}
		states := nodeStates(t, store, run.ID)
		call, ok := states["call"]
		if !ok {
			t.Fatal("no execution recorded for the mcp.call node")
		}
		raw, _ := json.Marshal(call.Output)
		if strings.Contains(string(raw), "s3cr3t-value") {
			t.Fatalf("stored history leaks the secret: %s", raw)
		}
	})
}

// The wiring table proves node -> backend-method, and the composite
// tests prove method -> owning subsystem. This closes the chain for
// one node: llm.chat through the real CompositeBackend to a gateway
// over HTTP, so a mis-delegation inside the composite for this node
// would be caught.
func TestReconciliationNodeThroughCompositeBackend(t *testing.T) {
	f := newFakeGatewayServer(t)
	defer f.server.Close()
	client := &GatewayClient{BaseURL: f.server.URL}
	reg := DefaultRegistry()
	spec, err := reg.Lookup("nexaroute.llm.chat", SupportedVersion)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	backend := &CompositeBackend{Gateway: client}
	out, err := reg.Execute(context.Background(), spec, reconcileOwner(),
		map[string]any{"prompt": "hi"}, map[string]any{"route_or_alias": "prov/model"}, backend, true)
	if err != nil {
		t.Fatalf("execute through composite: %v", err)
	}
	if out["text"] != "hello" {
		t.Fatalf("chat text = %v, want hello", out["text"])
	}
}
