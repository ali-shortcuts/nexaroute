package nexaflow

// Agent B coverage tests for the pure (non-Postgres, non-engine) gaps in
// internal/nexaflow. Every test drives the real production code path with
// concrete inputs and asserts on behaviour, outputs, or error text.
// Branches proven unreachable are NOT faked here; they are reported to
// the parent agent with proof (see the final report).

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// agentBErrBackend wraps a real backend and fails selected ports with a
// deterministic error, so node error-propagation branches can be driven
// without a live gateway.
type agentBErrBackend struct {
	Backend
	usageErr  error
	budgetErr error
	evalErr   error
}

func (b *agentBErrBackend) UsageSnapshot(ctx context.Context) (UsageInfo, error) {
	if b.usageErr != nil {
		return UsageInfo{}, b.usageErr
	}
	return b.Backend.UsageSnapshot(ctx)
}

func (b *agentBErrBackend) BudgetSnapshot(ctx context.Context, key string) (BudgetInfo, error) {
	if b.budgetErr != nil {
		return BudgetInfo{}, b.budgetErr
	}
	return b.Backend.BudgetSnapshot(ctx, key)
}

func (b *agentBErrBackend) RunEvalReplay(ctx context.Context, suiteID, deploymentID string, outcomes []eval.Outcome) (eval.Result, error) {
	if b.evalErr != nil {
		return eval.Result{}, b.evalErr
	}
	return b.Backend.RunEvalReplay(ctx, suiteID, deploymentID, outcomes)
}

func TestAgentBReplayEvalBackendRunError(t *testing.T) {
	// backend.go:198-199 — the runner reports an unknown suite, and the
	// adapter must surface that error instead of an empty result.
	b := &ReplayEvalBackend{}
	_, err := b.RunEvalReplay(context.Background(), "no-such-suite", "dep-1", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown evaluation suite") {
		t.Fatalf("RunEvalReplay unknown suite: err = %v, want unknown-suite error", err)
	}

	// Control: a real suite replays cleanly through the nil-Runner path.
	res, err := b.RunEvalReplay(context.Background(), "coding", "dep-1", nil)
	if err != nil {
		t.Fatalf("RunEvalReplay coding: err = %v", err)
	}
	if res.SuiteID != "coding" {
		t.Fatalf("RunEvalReplay coding: SuiteID = %q, want coding", res.SuiteID)
	}
}

func TestAgentBCompositeResolveSecretRef(t *testing.T) {
	// composite_backend.go:153 — the success return of ResolveSecretRef
	// through a wired gateway client.
	cb := &CompositeBackend{Gateway: &GatewayClient{SecretResolver: func(ref string) (string, error) {
		return "resolved:" + ref, nil
	}}}
	got, err := cb.ResolveSecretRef("my-ref")
	if err != nil {
		t.Fatalf("ResolveSecretRef: err = %v", err)
	}
	if got != "resolved:my-ref" {
		t.Fatalf("ResolveSecretRef = %q, want %q", got, "resolved:my-ref")
	}
}

func TestAgentBDirectCostUsageSnapshotRows(t *testing.T) {
	// direct_backend.go:86-92 — the ByDeployment loop maps every tracker
	// row verbatim into the node-facing snapshot.
	tr := usage.New()
	tr.Record("dep-a", 10, 5)
	tr.Record("dep-b", 3, 7)
	be := &DirectCostBackend{Tracker: tr}
	snap, err := be.UsageSnapshot(context.Background())
	if err != nil {
		t.Fatalf("UsageSnapshot: err = %v", err)
	}
	if len(snap.ByDeployment) != 2 {
		t.Fatalf("ByDeployment = %d rows, want 2", len(snap.ByDeployment))
	}
	byName := map[string]DeploymentUsage{}
	for _, row := range snap.ByDeployment {
		byName[row.Deployment] = row
	}
	a := byName["dep-a"]
	if a.PromptTokens != 10 || a.CompletionTokens != 5 || a.Requests != 1 {
		t.Fatalf("dep-a row = %+v, want prompt 10 / completion 5 / requests 1", a)
	}
	if snap.TotalPromptTokens != 13 || snap.TotalCompletionTokens != 12 || snap.TotalRequests != 2 {
		t.Fatalf("totals = %+v, want prompt 13 / completion 12 / requests 2", snap)
	}
}

func TestAgentBNodeDefByIDMissing(t *testing.T) {
	// graph.go:66 — an unknown instance id falls back to a bare node
	// carrying only the requested id.
	def := Definition{Nodes: []DefinitionNode{{ID: "a", NodeID: "nexaroute.control.branch"}}}
	got := nodeDefByID(def, "missing")
	if got.ID != "missing" || got.NodeID != "" {
		t.Fatalf("nodeDefByID missing = %+v, want {ID:missing}", got)
	}
	if got := nodeDefByID(def, "a"); got.NodeID != "nexaroute.control.branch" {
		t.Fatalf("nodeDefByID a = %+v, want the stored node", got)
	}
}

func TestAgentBMappedNodeInputNonObject(t *testing.T) {
	// input_mapping.go:202-203 — a mapping that resolves to a scalar is
	// an error, never a silently wrapped value.
	run := Run{Input: map[string]any{"name": "workflow-x"}}
	node := DefinitionNode{ID: "n1", Input: map[string]any{"$ref": "input.name"}}
	_, err := mappedNodeInput(run, node, nil)
	if err == nil || !strings.Contains(err.Error(), "must resolve to an object") {
		t.Fatalf("mappedNodeInput scalar: err = %v, want must-resolve-to-object", err)
	}

	// Control: a mapping resolving to an object succeeds.
	node.Input = map[string]any{"$ref": "input"}
	got, err := mappedNodeInput(run, node, nil)
	if err != nil {
		t.Fatalf("mappedNodeInput object: err = %v", err)
	}
	if got["name"] != "workflow-x" {
		t.Fatalf("mappedNodeInput object = %v, want name=workflow-x", got)
	}
}

func TestAgentBResolveMappingPathEmpty(t *testing.T) {
	// input_mapping.go:88-89 — an empty path is an error.
	_, err := resolveMappingPath(map[string]any{}, "")
	if err == nil || !strings.Contains(err.Error(), "empty input-mapping path") {
		t.Fatalf("resolveMappingPath empty: err = %v, want empty-path error", err)
	}
}

func TestAgentBResolveMappingPathArray(t *testing.T) {
	// input_mapping.go:101-107 — array index resolution: valid index,
	// out of range, non-numeric, negative, and walking into a scalar.
	ctx := map[string]any{"arr": []any{"a", "b", "c"}}
	got, err := resolveMappingPath(ctx, "arr.1")
	if err != nil || got != "b" {
		t.Fatalf("arr.1 = %v, err = %v, want b", got, err)
	}
	for _, path := range []string{"arr.5", "arr.x", "arr.-1", "arr.0.x"} {
		if _, err := resolveMappingPath(ctx, path); err == nil {
			t.Fatalf("resolveMappingPath %q: got no error, want not-found", path)
		} else if !strings.Contains(err.Error(), "not found") {
			t.Fatalf("resolveMappingPath %q: err = %v, want not-found", path, err)
		}
	}
}

func TestAgentBResolveMappingValueNestedError(t *testing.T) {
	// input_mapping.go:70-71 — an unresolvable reference inside a
	// slice element propagates out instead of being dropped.
	_, err := resolveMappingValue(
		[]any{map[string]any{"$ref": "missing.deep"}},
		map[string]any{"input": map[string]any{}},
	)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("resolveMappingValue nested: err = %v, want not-found", err)
	}
	// Control: the same shape with a resolvable reference succeeds.
	got, err := resolveMappingValue(
		[]any{map[string]any{"$ref": "input.name"}},
		map[string]any{"input": map[string]any{"name": "x"}},
	)
	if err != nil {
		t.Fatalf("resolveMappingValue control: err = %v", err)
	}
	if arr, ok := got.([]any); !ok || len(arr) != 1 || arr[0] != "x" {
		t.Fatalf("resolveMappingValue control = %#v, want [x]", got)
	}
}

func TestAgentBValidateInputMappingsTrigger(t *testing.T) {
	// input_mapping.go:155-156 — a trigger node must not declare an
	// input mapping.
	def := &Definition{ID: "d1", Nodes: []DefinitionNode{
		{ID: "trig", NodeID: "nexaroute.trigger.manual", Input: map[string]any{"x": 1}},
	}}
	specs := map[string]*NodeSpec{"trig": {ID: "nexaroute.trigger.manual"}}
	err := validateInputMappings(def, specs, map[string]bool{"trig": true})
	if err == nil || !strings.Contains(err.Error(), "must not declare an input mapping") {
		t.Fatalf("trigger mapping: err = %v, want must-not-declare error", err)
	}
}

func TestAgentBValidateInputMappingsUnknownKey(t *testing.T) {
	// input_mapping.go:167-168 — a mapping key outside the node's
	// declared input schema is rejected.
	def := &Definition{ID: "d1", Nodes: []DefinitionNode{
		{ID: "n1", NodeID: "nexaroute.control.condition", Input: map[string]any{"bogus": 1}},
	}}
	specs := map[string]*NodeSpec{"n1": {
		ID:          "nexaroute.control.condition",
		InputSchema: []Field{{Name: "value", Type: "object"}},
	}}
	err := validateInputMappings(def, specs, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "is not a declared input field") {
		t.Fatalf("unknown key: err = %v, want not-a-declared-field error", err)
	}
}

func TestAgentBValidateInputMappingsUnknownRef(t *testing.T) {
	// input_mapping.go:179-180 — a reference rooted at an unknown node
	// instance is rejected.
	def := &Definition{ID: "d1", Nodes: []DefinitionNode{
		{ID: "n1", NodeID: "nexaroute.control.condition", Input: map[string]any{
			"value": map[string]any{"$ref": "ghost.field"},
		}},
	}}
	specs := map[string]*NodeSpec{"n1": {
		ID:          "nexaroute.control.condition",
		InputSchema: []Field{{Name: "value", Type: "object"}},
	}}
	err := validateInputMappings(def, specs, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "references unknown node instance") {
		t.Fatalf("unknown ref root: err = %v, want unknown-node-instance error", err)
	}
}

func TestAgentBLookupPath(t *testing.T) {
	// nodes_control.go:188-189 (empty path) and 194-195 (a scalar
	// mid-path is not traversable).
	if _, ok := lookupPath(map[string]any{"a": "x"}, ""); ok {
		t.Fatalf("lookupPath empty path: got found, want not-found")
	}
	if _, ok := lookupPath(map[string]any{"a": "str"}, "a.b"); ok {
		t.Fatalf("lookupPath through scalar: got found, want not-found")
	}
	got, ok := lookupPath(map[string]any{"a": map[string]any{"b": 42}}, "a.b")
	if !ok || got != 42 {
		t.Fatalf("lookupPath a.b = %v, %v, want 42, true", got, ok)
	}
}

func TestAgentBRunConditionNe(t *testing.T) {
	// nodes_control.go:249 — the ne operator compares with !=.
	for _, tc := range []struct {
		expected string
		want     bool
	}{
		{"bad", true},
		{"ok", false},
	} {
		out, err := runCondition(context.Background(), &RunContext{
			Input:  map[string]any{"value": map[string]any{"status": "ok"}},
			Config: map[string]any{"field": "status", "operator": "ne", "expected": tc.expected},
		})
		if err != nil {
			t.Fatalf("runCondition ne %q: err = %v", tc.expected, err)
		}
		if out["result"] != tc.want {
			t.Fatalf("runCondition ne %q: result = %v, want %v", tc.expected, out["result"], tc.want)
		}
	}
}

func TestAgentBToNumberTypes(t *testing.T) {
	// nodes_control.go:209-210 (float32), 212 (int), 214 (int64),
	// 225 (default: non-numeric types).
	if f, ok := toNumber(float32(2.5)); !ok || f != 2.5 {
		t.Fatalf("toNumber float32 = %v, %v, want 2.5, true", f, ok)
	}
	if _, ok := toNumber(float32(math.NaN())); ok {
		t.Fatalf("toNumber float32 NaN: got ok, want not-ok")
	}
	if f, ok := toNumber(7); !ok || f != 7 {
		t.Fatalf("toNumber int = %v, %v, want 7, true", f, ok)
	}
	if f, ok := toNumber(int64(9)); !ok || f != 9 {
		t.Fatalf("toNumber int64 = %v, %v, want 9, true", f, ok)
	}
	for _, v := range []any{true, nil, []any{1}} {
		if _, ok := toNumber(v); ok {
			t.Fatalf("toNumber %T: got ok, want not-ok", v)
		}
	}
}

func TestAgentBBudgetCheckBackendErrors(t *testing.T) {
	ctx := context.Background()
	// nodes_cost.go:107-108 — no backend wired at all.
	if _, err := runBudgetCheck(ctx, &RunContext{
		Input: map[string]any{"key": "k", "threshold_usd": 1.0},
	}); err == nil || !strings.Contains(err.Error(), "backend_unavailable") {
		t.Fatalf("runBudgetCheck nil backend: err = %v, want backend_unavailable", err)
	}
	// nodes_cost.go:119-120 — the snapshot call itself fails.
	be := &agentBErrBackend{Backend: newFakeBackend(), budgetErr: errors.New("ledger boom")}
	if _, err := runBudgetCheck(ctx, &RunContext{
		Backend: be,
		Input:   map[string]any{"key": "team", "threshold_usd": 1.0},
	}); err == nil || !strings.Contains(err.Error(), "ledger boom") {
		t.Fatalf("runBudgetCheck snapshot error: err = %v, want ledger boom", err)
	}
}

func TestAgentBBudgetCheckValidation(t *testing.T) {
	ctx := context.Background()
	be := newFakeBackend()
	for name, input := range map[string]map[string]any{
		// nodes_cost.go:111-112 — key missing / blank.
		"missing key": {"threshold_usd": 1.0},
		"blank key":   {"key": "   ", "threshold_usd": 1.0},
		// nodes_cost.go:115-116 — threshold missing / negative / NaN.
		"missing threshold":  {"key": "team"},
		"negative threshold": {"key": "team", "threshold_usd": -1.0},
		"NaN threshold":      {"key": "team", "threshold_usd": math.NaN()},
	} {
		if _, err := runBudgetCheck(ctx, &RunContext{Backend: be, Input: input}); err == nil {
			t.Fatalf("runBudgetCheck %s: got no error, want validation_error", name)
		} else if !strings.Contains(err.Error(), "validation_error") {
			t.Fatalf("runBudgetCheck %s: err = %v, want validation_error", name, err)
		}
	}
	// Control: a valid check reports the ledger figures.
	out, err := runBudgetCheck(ctx, &RunContext{
		Backend: be,
		Input:   map[string]any{"key": "team", "threshold_usd": 10.0},
	})
	if err != nil {
		t.Fatalf("runBudgetCheck valid: err = %v", err)
	}
	if out["total"] != 5.0 || out["over_threshold"] != false {
		t.Fatalf("runBudgetCheck valid = %v, want total 5.0 / over_threshold false", out)
	}
}

func TestAgentBCostUsageBackendErrorAndRows(t *testing.T) {
	ctx := context.Background()
	// nodes_cost.go:79-80 — no backend wired at all.
	if _, err := runCostUsage(ctx, &RunContext{}); err == nil || !strings.Contains(err.Error(), "backend_unavailable") {
		t.Fatalf("runCostUsage nil backend: err = %v, want backend_unavailable", err)
	}
	// nodes_cost.go:82-84 (error path, adjacent to the listed 79-80) —
	// UsageSnapshot failure propagates.
	be := &agentBErrBackend{Backend: newFakeBackend(), usageErr: errors.New("tracker boom")}
	if _, err := runCostUsage(ctx, &RunContext{Backend: be}); err == nil || !strings.Contains(err.Error(), "tracker boom") {
		t.Fatalf("runCostUsage snapshot error: err = %v, want tracker boom", err)
	}
	// nodes_cost.go:87-93 — the per-deployment loop maps every row.
	fake := newFakeBackend()
	fake.Usage.ByDeployment = []DeploymentUsage{{
		Deployment: "d1", PromptTokens: 10, CompletionTokens: 5, Requests: 2,
		EstimatedCostUSD: 0.5, CostBasis: "recorded", CostCompleteness: "complete",
	}}
	out, err := runCostUsage(ctx, &RunContext{Backend: fake})
	if err != nil {
		t.Fatalf("runCostUsage rows: err = %v", err)
	}
	rows, ok := out["by_deployment"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("runCostUsage by_deployment = %#v, want 1 row", out["by_deployment"])
	}
	row, ok := rows[0].(map[string]any)
	if !ok || row["deployment"] != "d1" || row["prompt_tokens"] != int64(10) || row["cost_basis"] != "recorded" {
		t.Fatalf("runCostUsage row = %#v, want d1 / 10 / recorded", rows[0])
	}
}

func TestAgentBEvalReplayValidation(t *testing.T) {
	ctx := context.Background()
	be := newFakeBackend()
	// nodes_eval.go:57-58 — no backend wired.
	if _, err := runEvalReplay(ctx, &RunContext{Input: map[string]any{}}); err == nil || !strings.Contains(err.Error(), "backend_unavailable") {
		t.Fatalf("runEvalReplay nil backend: err = %v, want backend_unavailable", err)
	}
	cases := []struct {
		name  string
		input map[string]any
		want  string
	}{
		// 61-62
		{"missing suite", map[string]any{"deployment_id": "d", "outcomes": []any{}}, "input suite_id is required"},
		// 65-66
		{"missing deployment", map[string]any{"suite_id": "coding", "outcomes": []any{}}, "input deployment_id is required"},
		// 72-73
		{"outcomes not array", map[string]any{"suite_id": "coding", "deployment_id": "d"}, "input outcomes must be an array"},
		// 75-76
		{"too many outcomes", map[string]any{"suite_id": "coding", "deployment_id": "d", "outcomes": make([]any, 513)}, "outcomes exceed 512"},
		// 79-80
		{"unencodable outcomes", map[string]any{"suite_id": "coding", "deployment_id": "d", "outcomes": []any{math.NaN()}}, "outcomes are not JSON-encodable"},
		// 83-84
		{"wrong outcome shape", map[string]any{"suite_id": "coding", "deployment_id": "d", "outcomes": []any{"not-an-object"}}, "outcomes do not match the eval.Outcome shape"},
	}
	for _, tc := range cases {
		if _, err := runEvalReplay(ctx, &RunContext{Backend: be, Input: tc.input}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("runEvalReplay %s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// nodes_eval.go:87-88 — the backend replay itself fails.
	errBe := &agentBErrBackend{Backend: be, evalErr: errors.New("eval boom")}
	_, err := runEvalReplay(ctx, &RunContext{Backend: errBe, Input: map[string]any{
		"suite_id": "coding", "deployment_id": "d", "outcomes": []any{},
	}})
	if err == nil || !strings.Contains(err.Error(), "eval boom") {
		t.Fatalf("runEvalReplay backend error: err = %v, want eval boom", err)
	}
	// Control: a valid replay succeeds through the fake backend.
	out, err := runEvalReplay(ctx, &RunContext{Backend: be, Input: map[string]any{
		"suite_id": "coding", "deployment_id": "d", "outcomes": []any{},
	}})
	if err != nil {
		t.Fatalf("runEvalReplay valid: err = %v", err)
	}
	if out["mode"] != "replay" || out["suite_id"] != "coding" {
		t.Fatalf("runEvalReplay valid = %v, want mode replay / suite coding", out)
	}
}

func TestAgentBParseLLMMessages(t *testing.T) {
	// nodes_llm.go:144,146 — a valid messages array is converted
	// message by message and returned.
	msgs, err := parseLLMMessages(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello back"},
	}})
	if err != nil {
		t.Fatalf("parseLLMMessages: err = %v", err)
	}
	if len(msgs) != 2 || msgs[0] != (ChatMessage{Role: "user", Content: "hi"}) || msgs[1].Role != "assistant" {
		t.Fatalf("parseLLMMessages = %+v, want 2 converted messages", msgs)
	}
}

func TestAgentBRedactJSONMarshalError(t *testing.T) {
	// redact.go:145-146 — a value JSON cannot encode renders as "{}".
	if got := RedactJSON(map[string]any{"f": func() {}}); got != "{}" {
		t.Fatalf("RedactJSON unencodable = %q, want {}", got)
	}
}

func TestAgentBIsSecretKeyCompound(t *testing.T) {
	// redact.go:69-70 — the compound Contains rule catches a secret
	// name embedded in a longer key, while a mere plural suffix does not.
	if !isSecretKey("my_api_key_here") {
		t.Fatalf("isSecretKey my_api_key_here = false, want true")
	}
	if isSecretKey("total_prompt_tokens") {
		t.Fatalf("isSecretKey total_prompt_tokens = true, want false")
	}
}

func TestAgentBRedactStringSlice(t *testing.T) {
	// redact.go:112-116 — []string values are traversed (not passed
	// through) and bearer tokens inside them are redacted.
	got := redactJSONValue([]string{"Bearer tok123", "plain"})
	arr, ok := got.([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("redact []string = %#v, want 2-element slice", got)
	}
	if arr[0] != "Bearer [REDACTED]" || arr[1] != "plain" {
		t.Fatalf("redact []string = %#v, want [Bearer [REDACTED] plain]", arr)
	}
}

func TestAgentBRedactStringPatterns(t *testing.T) {
	// redact.go:125-126 (Bearer) and 128-129 (Basic) inside a string value.
	got := redactJSONValue(map[string]any{
		"a": "call with Bearer abcDEF123 inside",
		"b": "auth Basic dXNlcjpwYXNz here",
	})
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("redact patterns: got %#v, want map", got)
	}
	if m["a"] != "call with Bearer [REDACTED] inside" {
		t.Fatalf("bearer redact = %q", m["a"])
	}
	if m["b"] != "auth Basic [REDACTED] here" {
		t.Fatalf("basic redact = %q", m["b"])
	}
	// redact.go:131-132 — an over-long string is cut at 8192 chars.
	long := strings.Repeat("x", 9000)
	got = redactJSONValue(map[string]any{"k": long})
	m = got.(map[string]any)
	if len(m["k"].(string)) != 8192 {
		t.Fatalf("redact long string len = %d, want 8192", len(m["k"].(string)))
	}
}

func TestAgentBTruncateText(t *testing.T) {
	// redact.go:154-155 — truncation only when max > 0 and len > max.
	if got := truncateText("hello world", 5); got != "hello" {
		t.Fatalf("truncateText = %q, want hello", got)
	}
	if got := truncateText("hi", 5); got != "hi" {
		t.Fatalf("truncateText short = %q, want hi", got)
	}
	if got := truncateText("hello world", 0); got != "hello world" {
		t.Fatalf("truncateText max 0 = %q, want unchanged", got)
	}
}

func TestAgentBExecuteTimeoutClamp(t *testing.T) {
	// registry.go:333-334 — a spec whose DefaultTimeout exceeds its
	// MaxTimeout runs under the max, not the default.
	reg := DefaultRegistry()
	spec := &NodeSpec{
		ID: "agentb.test", Version: SupportedVersion,
		Retry:          RetryPolicy{MaxAttempts: 1},
		DefaultTimeout: 10 * time.Second, MaxTimeout: 5 * time.Second,
		MaxInputBytes: 1024, MaxOutputBytes: 1024,
		Run: func(_ context.Context, _ *RunContext) (map[string]any, error) {
			return map[string]any{"ok": true}, nil
		},
	}
	out, err := reg.Execute(context.Background(), spec, ownerPrincipal(), nil, nil, nil, false)
	if err != nil {
		t.Fatalf("Execute clamped: err = %v", err)
	}
	if out["ok"] != true {
		t.Fatalf("Execute clamped = %v, want ok=true", out)
	}
}

func TestAgentBGetNumberTypes(t *testing.T) {
	// registry.go:388 (float32), 390 (int), 392 (int64).
	if f, ok := getNumber(map[string]any{"x": float32(1.5)}, "x"); !ok || f != 1.5 {
		t.Fatalf("getNumber float32 = %v, %v, want 1.5, true", f, ok)
	}
	if f, ok := getNumber(map[string]any{"x": 3}, "x"); !ok || f != 3 {
		t.Fatalf("getNumber int = %v, %v, want 3, true", f, ok)
	}
	if f, ok := getNumber(map[string]any{"x": int64(4)}, "x"); !ok || f != 4 {
		t.Fatalf("getNumber int64 = %v, %v, want 4, true", f, ok)
	}
}

func TestAgentBListRunsStateFilter(t *testing.T) {
	// state.go:300 — the state filter skips non-matching runs.
	s := newStoreState()
	now := time.Now()
	for _, id := range []string{"r1", "r2"} {
		if err := s.createRun(Run{ID: id, DefinitionID: "d1", Definition: Definition{ID: "d1"}}, now); err != nil {
			t.Fatalf("createRun %s: %v", id, err)
		}
	}
	// Both runs are created, so a failed-state filter matches neither
	// and the continue branch runs for every stored run.
	got, err := s.listRuns(RunFilter{State: RunFailed})
	if err != nil {
		t.Fatalf("listRuns filtered: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("listRuns RunFailed = %d runs, want 0", len(got))
	}
	got, err = s.listRuns(RunFilter{State: RunCreated})
	if err != nil || len(got) != 2 {
		t.Fatalf("listRuns RunCreated = %d runs, err = %v, want 2", len(got), err)
	}
}

func TestAgentBValidRunTransition(t *testing.T) {
	// states.go:94-95 — a terminal state rejects any different target.
	if validRunTransition(RunSucceeded, RunRunning) {
		t.Fatalf("validRunTransition succeeded->running = true, want false")
	}
	// states.go:107 — an unknown from-state (waiting_timer is reserved
	// and never emitted) has no valid transition.
	if validRunTransition(RunWaitingTimer, RunRunning) {
		t.Fatalf("validRunTransition waiting_timer->running = true, want false")
	}
	// Controls.
	if !validRunTransition(RunCreated, RunRunning) || !validRunTransition(RunFailed, RunFailed) {
		t.Fatalf("validRunTransition controls failed")
	}
}

func TestAgentBValidNodeTransition(t *testing.T) {
	// states.go:118-119 — a terminal node state rejects a different target.
	if validNodeTransition(NodeSucceeded, NodeRunning) {
		t.Fatalf("validNodeTransition succeeded->running = true, want false")
	}
	// states.go:140 — needs_manual_action only goes to cancelled/failed.
	if !validNodeTransition(NodeNeedsManualAction, NodeCancelled) || !validNodeTransition(NodeNeedsManualAction, NodeFailed) {
		t.Fatalf("validNodeTransition needs_manual_action->cancelled/failed = false, want true")
	}
	if validNodeTransition(NodeNeedsManualAction, NodeRunning) {
		t.Fatalf("validNodeTransition needs_manual_action->running = true, want false")
	}
	// states.go:142 — an unknown from-state has no valid transition.
	if validNodeTransition(NodeState("bogus"), NodeRunning) {
		t.Fatalf("validNodeTransition bogus->running = true, want false")
	}
}

func TestAgentBItoa(t *testing.T) {
	// store.go:247-248 (negation) and 257-259 (minus sign).
	for n, want := range map[int]string{0: "0", 123: "123", -42: "-42", -1: "-1"} {
		if got := itoa(n); got != want {
			t.Fatalf("itoa(%d) = %q, want %q", n, got, want)
		}
	}
	if got := NodeIDKey("run", "node", -1); got != "run:node:-1" {
		t.Fatalf("NodeIDKey negative = %q, want run:node:-1", got)
	}
}

func TestAgentBInstantiateDefinitionEdgeErrors(t *testing.T) {
	// templates.go:236-237 — an edge from a node outside the template.
	tpl := Template{ID: "t-edge-from", Title: "T", Nodes: []TemplateNode{
		{NodeID: "nexaroute.trigger.manual", Version: SupportedVersion, Config: map[string]any{}},
	}, Edges: [][2]string{{"nexaroute.provider.health", "nexaroute.trigger.manual"}}}
	if _, err := tpl.InstantiateDefinition(nil, false); err == nil || !strings.Contains(err.Error(), "outside the template") {
		t.Fatalf("edge from outside: err = %v, want outside-the-template", err)
	}
	// templates.go:240-241 — an edge to a node outside the template.
	tpl = Template{ID: "t-edge-to", Title: "T", Nodes: []TemplateNode{
		{NodeID: "nexaroute.trigger.manual", Version: SupportedVersion, Config: map[string]any{}},
	}, Edges: [][2]string{{"nexaroute.trigger.manual", "nexaroute.provider.health"}}}
	if _, err := tpl.InstantiateDefinition(nil, false); err == nil || !strings.Contains(err.Error(), "outside the template") {
		t.Fatalf("edge to outside: err = %v, want outside-the-template", err)
	}
}

func TestAgentBInstantiateDefinitionCoerceConfigError(t *testing.T) {
	// templates.go:254-255 — a config string that cannot coerce to the
	// schema's number type is an error naming template, node, field.
	tpl := Template{ID: "t-coerce", Title: "T", Nodes: []TemplateNode{
		{NodeID: "nexaroute.control.loop_plan", Version: SupportedVersion,
			Config: map[string]any{"max_iterations": "abc"}},
	}}
	if _, err := tpl.InstantiateDefinition(nil, false); err == nil || !strings.Contains(err.Error(), "is not a number") {
		t.Fatalf("coerce config: err = %v, want is-not-a-number", err)
	}
}

func TestAgentBInstantiateDefinitionRequiredInputFromMapping(t *testing.T) {
	// templates.go:270 — a required input supplied by the node's own
	// Input mapping satisfies the check (continue) without an edge.
	tpl := Template{ID: "t-req", Title: "T", Nodes: []TemplateNode{
		{NodeID: "nexaroute.trigger.manual", Version: SupportedVersion, Config: map[string]any{}},
		{NodeID: "nexaroute.cost.budget_check", Version: SupportedVersion,
			Input: map[string]any{"key": "team", "threshold_usd": 5.0}},
	}, Edges: [][2]string{{"nexaroute.trigger.manual", "nexaroute.cost.budget_check"}}}
	def, err := tpl.InstantiateDefinition(nil, false)
	if err != nil {
		t.Fatalf("InstantiateDefinition required-via-mapping: err = %v", err)
	}
	if len(def.Nodes) != 2 || def.Nodes[1].Input["key"] != "team" {
		t.Fatalf("InstantiateDefinition = %+v, want budget_check with key=team", def.Nodes)
	}
}

func TestAgentBInstantiateDefinitionInvalidDefinition(t *testing.T) {
	// templates.go:290-291 — a template that assembles but fails
	// ValidateDefinition (trigger with an incoming edge) is rejected
	// with the wrapped validation error.
	tpl := Template{ID: "t-invalid", Title: "T", Nodes: []TemplateNode{
		{NodeID: "nexaroute.trigger.manual", Version: SupportedVersion, Config: map[string]any{}},
		{NodeID: "nexaroute.provider.health", Version: SupportedVersion},
	}, Edges: [][2]string{
		{"nexaroute.trigger.manual", "nexaroute.provider.health"},
		{"nexaroute.provider.health", "nexaroute.trigger.manual"},
	}}
	if _, err := tpl.InstantiateDefinition(nil, false); err == nil || !strings.Contains(err.Error(), "produced an invalid definition") {
		t.Fatalf("invalid definition: err = %v, want produced-an-invalid-definition", err)
	}
}

func TestAgentBCoerceTemplateBoolean(t *testing.T) {
	// templates.go:330 (switch), 332 ("true"), 334 ("false"), 336 (error)
	// — boolean coercion of a template Input string, via the branch
	// node's boolean condition_result field.
	build := func(value string) Template {
		return Template{ID: "t-bool", Title: "T", Nodes: []TemplateNode{
			{NodeID: "nexaroute.trigger.manual", Version: SupportedVersion, Config: map[string]any{}},
			{NodeID: "nexaroute.control.branch", Version: SupportedVersion,
				Config: map[string]any{"true_branch": "yes", "false_branch": "no"},
				Input:  map[string]any{"condition_result": value}},
		}, Edges: [][2]string{{"nexaroute.trigger.manual", "nexaroute.control.branch"}}}
	}
	def, err := build("true").InstantiateDefinition(nil, false)
	if err != nil {
		t.Fatalf("coerce true: err = %v", err)
	}
	if def.Nodes[1].Input["condition_result"] != true {
		t.Fatalf("coerce true = %#v, want bool true", def.Nodes[1].Input["condition_result"])
	}
	def, err = build("false").InstantiateDefinition(nil, false)
	if err != nil {
		t.Fatalf("coerce false: err = %v", err)
	}
	if def.Nodes[1].Input["condition_result"] != false {
		t.Fatalf("coerce false = %#v, want bool false", def.Nodes[1].Input["condition_result"])
	}
	if _, err := build("maybe").InstantiateDefinition(nil, false); err == nil || !strings.Contains(err.Error(), "is not a boolean") {
		t.Fatalf("coerce maybe: err = %v, want is-not-a-boolean", err)
	}
}

func TestAgentBSubstituteTemplateValueNested(t *testing.T) {
	// templates.go:136-137 — an unresolved placeholder inside a nested
	// map propagates the error out.
	_, err := substituteTemplateValue(
		map[string]any{"nested": map[string]any{"url": "http://x/{{missing}}"}},
		map[string]string{"other": "v"}, "t", "n", "config", "k",
	)
	if err == nil || !strings.Contains(err.Error(), "unresolved placeholder") {
		t.Fatalf("substitute nested map: err = %v, want unresolved-placeholder", err)
	}
	// templates.go:142-150 — slices substitute element-wise, including
	// nested maps, and return a fresh slice.
	got, err := substituteTemplateValue(
		[]any{"{{a}}", "plain", map[string]any{"x": "{{a}}"}},
		map[string]string{"a": "A"}, "t", "n", "config", "k",
	)
	if err != nil {
		t.Fatalf("substitute slice: err = %v", err)
	}
	arr, ok := got.([]any)
	if !ok || len(arr) != 3 || arr[0] != "A" || arr[1] != "plain" {
		t.Fatalf("substitute slice = %#v, want [A plain {x:A}]", got)
	}
	if m, ok := arr[2].(map[string]any); !ok || m["x"] != "A" {
		t.Fatalf("substitute slice nested = %#v, want {x:A}", arr[2])
	}
	// templates.go:144-147 — an error inside a slice element propagates.
	if _, err := substituteTemplateValue([]any{"{{missing}}"}, map[string]string{}, "t", "n", "config", "k"); err == nil {
		t.Fatalf("substitute slice error: got no error, want unresolved-placeholder")
	}
}

func TestAgentBTriggerKindForNodeUnknown(t *testing.T) {
	// templates.go:301 — a non-trigger node has no trigger kind.
	if kind, ok := triggerKindForNode("nexaroute.control.condition"); ok || kind != "" {
		t.Fatalf("triggerKindForNode condition = %q, %v, want \"\", false", kind, ok)
	}
	if kind, ok := triggerKindForNode("nexaroute.trigger.manual"); !ok || kind != "manual" {
		t.Fatalf("triggerKindForNode manual = %q, %v, want manual, true", kind, ok)
	}
}

// agentBBadJSON marshals to syntactically invalid JSON, so a JSON
// round trip through it fails at the unmarshal step.
type agentBBadJSON struct{}

func (agentBBadJSON) MarshalJSON() ([]byte, error) { return []byte(`{"unclosed`), nil }

func TestAgentBJSONHelpers(t *testing.T) {
	// util.go:11-12 — jsonLen reports an unmarshalable value as over
	// every bound.
	if got := jsonLen(map[string]any{"x": math.NaN()}); got != 1<<30 {
		t.Fatalf("jsonLen NaN = %d, want %d", got, 1<<30)
	}
	// util.go:35-36 — normalizeJSON fails when marshaling fails.
	if _, err := normalizeJSON(math.NaN()); err == nil {
		t.Fatalf("normalizeJSON NaN: got no error, want marshal error")
	}
	// util.go:39-40 — normalizeJSON fails when the marshaled bytes do
	// not unmarshal back.
	if _, err := normalizeJSON(agentBBadJSON{}); err == nil {
		t.Fatalf("normalizeJSON bad JSON: got no error, want unmarshal error")
	}
	// util.go:23-24 and 27-28 — sameJSONValue is false when either side
	// cannot be normalized.
	if sameJSONValue(math.NaN(), 1) {
		t.Fatalf("sameJSONValue NaN,1 = true, want false")
	}
	if sameJSONValue(1, math.NaN()) {
		t.Fatalf("sameJSONValue 1,NaN = true, want false")
	}
	if !sameJSONValue(1, 1.0) {
		t.Fatalf("sameJSONValue 1,1.0 = false, want true")
	}
}

func agentBValidDefWithTrigger() *Definition {
	return &Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "d1", Name: "D",
		Nodes: []DefinitionNode{
			{ID: "manual", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "health", NodeID: "nexaroute.provider.health", Version: SupportedVersion},
		},
		Triggers: []DefinitionTrigger{{Name: "manual", Kind: "manual"}},
		Edges:    []DefinitionEdge{{ID: "e1", From: "manual", To: "health"}},
	}
}

func TestAgentBValidateDefinitionTriggerAndEdge(t *testing.T) {
	reg := DefaultRegistry()
	// workflow.go:286-287 — a trigger declaration with an empty name.
	def := agentBValidDefWithTrigger()
	def.Triggers[0].Name = ""
	if err := ValidateDefinition(def, reg); err == nil || !strings.Contains(err.Error(), "trigger name is empty") {
		t.Fatalf("empty trigger name: err = %v, want trigger-name-is-empty", err)
	}
	// workflow.go:321-322 — an edge with an empty id.
	def = agentBValidDefWithTrigger()
	def.Edges[0].ID = ""
	if err := ValidateDefinition(def, reg); err == nil || !strings.Contains(err.Error(), "edge id is empty") {
		t.Fatalf("empty edge id: err = %v, want edge-id-is-empty", err)
	}
	// Control: the same definition with ids filled validates.
	if err := ValidateDefinition(agentBValidDefWithTrigger(), reg); err != nil {
		t.Fatalf("control definition: err = %v", err)
	}
}

func TestAgentBFileLock(t *testing.T) {
	// filelock_unix.go:32-33 — releasing a nil lock is a no-op success.
	if err := releaseFileLock(nil); err != nil {
		t.Fatalf("releaseFileLock nil: err = %v", err)
	}
	// filelock_unix.go:19-20 — opening a lock whose parent directory
	// does not exist fails.
	badPath := filepath.Join(t.TempDir(), "no-such-dir", "lock")
	if _, err := acquireFileLock(badPath); err == nil || !strings.Contains(err.Error(), "open file store lock") {
		t.Fatalf("acquireFileLock missing dir: err = %v, want open-file-store-lock", err)
	}
	// Control: acquire and release in a real directory succeed.
	f, err := acquireFileLock(filepath.Join(t.TempDir(), "lock"))
	if err != nil {
		t.Fatalf("acquireFileLock: err = %v", err)
	}
	if err := releaseFileLock(f); err != nil {
		t.Fatalf("releaseFileLock: err = %v", err)
	}
}
