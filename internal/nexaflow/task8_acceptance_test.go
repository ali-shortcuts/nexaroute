package nexaflow

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

// Task 8 acceptance tests: every test executes real engine/service
// behaviour (no doc assertions, no skipped-as-passed cases). PG-gated
// tests skip only when NEXAROUTE_TEST_PG_DSN is unset, and say so.

var (
	task8Owner     = authz.Identity{Subject: "owner-1", Roles: []authz.Role{authz.RoleOwner}}
	task8Requester = authz.Identity{Subject: "alice", Roles: []authz.Role{authz.RoleOwner}}
	task8Approver  = authz.Identity{Subject: "op-1", Roles: []authz.Role{authz.RoleOwner}}
	task8Viewer    = authz.Identity{Subject: "viewer-1", Roles: []authz.Role{authz.RoleViewer}}
)

type task8FakeBackend struct {
	mu sync.Mutex

	chatCalls     int
	chatResp      ChatResponse
	chatErr       error
	resolveErr    error
	mcpTools      []MCPTool
	mcpCallCalls  int
	mcpCallErr    error
	mcpCallResult map[string]any
	videoSubmit   int
	videoJobs     map[string]VideoJobInfo
	videoAssets   map[string][]byte
	usageCalls    int
	usageErrs     []error
	usageSnap     UsageInfo
	budgetSnap    BudgetInfo
	httpCalls     int
	httpResp      HTTPResponse
	secretErr     error
	healthErr     error
}

func newTask8FakeBackend() *task8FakeBackend {
	return &task8FakeBackend{
		chatResp:   ChatResponse{Model: "test-model", Text: "hello back", PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8, ResolvedRoute: "test-route"},
		mcpTools:   []MCPTool{{Name: "allowed-tool", Description: "allowed", InputSchema: map[string]any{}}},
		videoJobs:  map[string]VideoJobInfo{},
		usageSnap:  UsageInfo{TotalPromptTokens: 10, TotalCompletionTokens: 20, TotalRequests: 2, TotalEstimatedCostUSD: 0.5, TotalCostBasis: "recorded"},
		budgetSnap: BudgetInfo{Key: "k", Committed: 1.5, Reserved: 0.5},
		httpResp:   HTTPResponse{Status: 200, Body: []byte("ok"), Headers: map[string]string{}},
	}
}

func (b *task8FakeBackend) ResolveSelector(_ context.Context, selector string) (RouteInfo, error) {
	if b.resolveErr != nil {
		return RouteInfo{}, b.resolveErr
	}
	return RouteInfo{Kind: "route", ResolvedModel: selector, DeploymentIDs: []string{"d1"}}, nil
}
func (b *task8FakeBackend) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.chatCalls++
	if b.chatErr != nil {
		return ChatResponse{}, b.chatErr
	}
	return b.chatResp, nil
}
func (b *task8FakeBackend) ProviderHealth(_ context.Context, id string) (ProviderHealth, error) {
	if b.healthErr != nil {
		return ProviderHealth{}, b.healthErr
	}
	return ProviderHealth{DeploymentID: id, Status: "healthy", Successes: 3}, nil
}
func (b *task8FakeBackend) Compat(_ context.Context, id string) (CompatInfo, error) {
	return CompatInfo{DeploymentID: id, Support: map[string]string{"chat": "supported"}, Protocol: "openai"}, nil
}
func (b *task8FakeBackend) RunEvalReplay(_ context.Context, _, _ string, _ []eval.Outcome) (eval.Result, error) {
	return eval.Result{}, nil
}
func (b *task8FakeBackend) MCPList(_ context.Context, _ string) ([]MCPTool, error) {
	return append([]MCPTool{}, b.mcpTools...), nil
}
func (b *task8FakeBackend) MCPCall(_ context.Context, _, tool string, _ map[string]any) (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mcpCallCalls++
	if b.mcpCallErr != nil {
		return nil, b.mcpCallErr
	}
	if b.mcpCallResult != nil {
		return b.mcpCallResult, nil
	}
	return map[string]any{"tool": tool, "ok": true}, nil
}
func (b *task8FakeBackend) VideoSubmit(_ context.Context, req VideoSubmitRequest) (VideoJobInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.videoSubmit++
	job := VideoJobInfo{JobID: "job-1", State: "queued", Terminal: false, Assets: []VideoAssetInfo{{ID: "a1", Filename: "out.mp4", ContentType: "video/mp4", SHA256: "abc", SizeBytes: 4}}}
	b.videoJobs["job-1"] = VideoJobInfo{JobID: "job-1", State: "completed", Terminal: true, Assets: job.Assets}
	_ = req
	return job, nil
}
func (b *task8FakeBackend) VideoStatus(_ context.Context, jobID string) (VideoJobInfo, error) {
	if j, ok := b.videoJobs[jobID]; ok {
		return j, nil
	}
	return VideoJobInfo{}, fmt.Errorf("nexaflow: unknown job %q", jobID)
}
func (b *task8FakeBackend) VideoAsset(_ context.Context, jobID, assetID string) (VideoAssetInfo, []byte, error) {
	if data, ok := b.videoAssets[assetID]; ok {
		_ = jobID
		return VideoAssetInfo{ID: assetID, Filename: "out.mp4", ContentType: "video/mp4", SHA256: "abc", SizeBytes: int64(len(data))}, data, nil
	}
	return VideoAssetInfo{}, nil, fmt.Errorf("nexaflow: unknown asset %q", assetID)
}
func (b *task8FakeBackend) UsageSnapshot(_ context.Context) (UsageInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.usageCalls++
	if len(b.usageErrs) > 0 {
		err := b.usageErrs[0]
		b.usageErrs = b.usageErrs[1:]
		return UsageInfo{}, err
	}
	return b.usageSnap, nil
}
func (b *task8FakeBackend) BudgetSnapshot(_ context.Context, key string) (BudgetInfo, error) {
	out := b.budgetSnap
	out.Key = key
	return out, nil
}
func (b *task8FakeBackend) HTTPDo(_ context.Context, _, _ string, _ map[string]string, _ []byte) (HTTPResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.httpCalls++
	return b.httpResp, nil
}
func (b *task8FakeBackend) ResolveSecretRef(ref string) (string, error) {
	if b.secretErr != nil {
		return "", b.secretErr
	}
	return "resolved-" + ref, nil
}

func (b *task8FakeBackend) chatCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.chatCalls
}
func (b *task8FakeBackend) mcpCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mcpCallCalls
}
func (b *task8FakeBackend) usageCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.usageCalls
}

// ---- definition builders ----

func task8Node(id, nodeID string, config, input map[string]any) DefinitionNode {
	return DefinitionNode{ID: id, NodeID: nodeID, Version: SupportedVersion, Config: config, Input: input}
}
func task8Edge(id, from, to, branch string) DefinitionEdge {
	return DefinitionEdge{ID: id, From: from, To: to, Branch: branch}
}
func task8Def(id string, nodes []DefinitionNode, edges []DefinitionEdge) Definition {
	return Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            id, Name: id,
		Nodes:    nodes,
		Edges:    edges,
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual}},
	}
}
func task8TransformLeaf(id, lane string) DefinitionNode {
	return task8Node(id, "nexaroute.control.json_transform", nil, map[string]any{
		"value":      map[string]any{"lane": lane},
		"operations": []any{map[string]any{"op": "set_const", "field": "done", "value": true}},
	})
}

type task8Setup struct {
	store   *MemoryStore
	engine  *Engine
	service *Service
	backend *task8FakeBackend
}

func newTask8Setup(t *testing.T, backend *task8FakeBackend) *task8Setup {
	t.Helper()
	store := NewMemoryStore()
	reg := DefaultRegistry()
	engine, err := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: backend, WorkerID: "task8-worker"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	svc, err := NewService(store, engine, reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return &task8Setup{store: store, engine: engine, service: svc, backend: backend}
}

func (s *task8Setup) publish(t *testing.T, def Definition) PublishedDefinition {
	t.Helper()
	ctx := context.Background()
	if err := s.service.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft %q: %v", def.ID, err)
	}
	pub, err := s.service.Publish(ctx, def.ID, task8Owner)
	if err != nil {
		t.Fatalf("Publish %q: %v", def.ID, err)
	}
	return pub
}

func (s *task8Setup) runToEnd(t *testing.T, pub PublishedDefinition, principal authz.Identity, input map[string]any, key string) Run {
	t.Helper()
	ctx := context.Background()
	run, err := s.service.StartRun(ctx, pub, principal, input, key)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	run, err = s.service.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	return run
}

func task8NodeStates(t *testing.T, store Store, runID string) map[string]NodeExecution {
	t.Helper()
	execs, err := store.GetNodeExecutions(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetNodeExecutions: %v", err)
	}
	out := map[string]NodeExecution{}
	for _, e := range execs {
		if cur, ok := out[e.NodeID]; !ok || e.Attempt > cur.Attempt {
			out[e.NodeID] = e
		}
	}
	return out
}

func task8BranchDef() Definition {
	return task8Def("branch-wf",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("chk", "nexaroute.control.condition", map[string]any{"field": "health.status", "operator": "eq", "expected": "ok"}, map[string]any{"value": "$.input.payload"}),
			task8Node("br", "nexaroute.control.branch", map[string]any{"true_branch": "fast", "false_branch": "slow"}, map[string]any{"condition_result": "$.chk.result"}),
			task8TransformLeaf("fast", "fast"),
			task8TransformLeaf("slow", "slow"),
		},
		[]DefinitionEdge{
			task8Edge("e1", "start", "chk", ""),
			task8Edge("e2", "chk", "br", ""),
			task8Edge("e3", "br", "fast", "fast"),
			task8Edge("e4", "br", "slow", "slow"),
		})
}

func task8SignWebhook(secret, timestamp, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + body))
	return hex.EncodeToString(mac.Sum(nil))
}

// ---- Scenario 1: create / validate / save / reopen / publish / version / execute ----

func TestTask8Scenario1_DraftPublishExecute(t *testing.T) {
	s := newTask8Setup(t, newTask8FakeBackend())
	ctx := context.Background()
	def := task8BranchDef()
	if err := s.service.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	reopened, err := s.service.GetDraft(ctx, def.ID)
	if err != nil {
		t.Fatalf("GetDraft (reopen): %v", err)
	}
	if reopened.ID != def.ID || len(reopened.Nodes) != len(def.Nodes) {
		t.Fatalf("reopened draft mismatch: %+v", reopened.ID)
	}
	pub1, err := s.service.Publish(ctx, def.ID, task8Owner)
	if err != nil {
		t.Fatalf("Publish rev1: %v", err)
	}
	if pub1.Version.Revision != 1 {
		t.Fatalf("rev1 = %d, want 1", pub1.Version.Revision)
	}
	// Invalid definition must fail validation at save time, never later.
	bad := def
	bad.ID = "bad-wf"
	bad.Nodes = append(append([]DefinitionNode{}, def.Nodes...), task8Node("nope", "nexaroute.control.nope", nil, nil))
	if err := s.service.SaveDraft(ctx, bad); err == nil {
		t.Fatalf("SaveDraft accepted an unknown node")
	}
	// Edit the draft and publish rev2; rev1's snapshot must be immutable.
	def2 := def
	def2.Nodes[1].Config = map[string]any{"field": "health.status", "operator": "eq", "expected": "bad"}
	if err := s.service.SaveDraft(ctx, def2); err != nil {
		t.Fatalf("SaveDraft rev2: %v", err)
	}
	pub2, err := s.service.Publish(ctx, def.ID, task8Owner)
	if err != nil {
		t.Fatalf("Publish rev2: %v", err)
	}
	if pub2.Version.Revision != 2 {
		t.Fatalf("rev2 = %d, want 2", pub2.Version.Revision)
	}
	rev1, err := s.service.GetRevision(ctx, def.ID, 1)
	if err != nil {
		t.Fatalf("GetRevision 1: %v", err)
	}
	if rev1.Definition.Nodes[1].Config["expected"] != "ok" {
		t.Fatalf("rev1 snapshot mutated: %v", rev1.Definition.Nodes[1].Config)
	}
	// Execute the pinned rev1 snapshot: fast branch wins for status ok.
	run := s.runToEnd(t, pub1, task8Requester, map[string]any{"payload": map[string]any{"health": map[string]any{"status": "ok"}}}, "s1-key")
	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded (err %q)", run.State, run.Error)
	}
	states := task8NodeStates(t, s.store, run.ID)
	if states["br"].Output["selected_branch"] != "fast" {
		t.Fatalf("selected_branch = %v, want fast", states["br"].Output)
	}
	if states["fast"].State != NodeSucceeded || states["slow"].State != NodeSkipped {
		t.Fatalf("fast=%q slow=%q, want succeeded/skipped", states["fast"].State, states["slow"].State)
	}
	// And the false branch, on the same rev1 snapshot.
	run2 := s.runToEnd(t, pub1, task8Requester, map[string]any{"payload": map[string]any{"health": map[string]any{"status": "down"}}}, "s1-key-false")
	if states2 := task8NodeStates(t, s.store, run2.ID); states2["br"].Output["selected_branch"] != "slow" || states2["slow"].State != NodeSucceeded || states2["fast"].State != NodeSkipped {
		t.Fatalf("false-branch outcome wrong: %+v", states2)
	}
}

// Unmapped condition->branch must fail CLOSED (not take a wrong branch).
func TestTask8UnmappedBranchFailsClosed(t *testing.T) {
	s := newTask8Setup(t, newTask8FakeBackend())
	def := task8BranchDef()
	def.Nodes[2].Input = nil // branch without mapping: condition outputs {result, field}
	pub := s.publish(t, def)
	run := s.runToEnd(t, pub, task8Requester, map[string]any{"payload": map[string]any{"health": map[string]any{"status": "ok"}}}, "unmapped-key")
	if run.State != RunFailed {
		t.Fatalf("unmapped run state = %q, want failed (fail-closed)", run.State)
	}
	if states := task8NodeStates(t, s.store, run.ID); states["br"].State != NodeFailed {
		t.Fatalf("branch node state = %q, want failed", states["br"].State)
	}
}

// ---- Scenario 2: manual trigger + idempotency ----

func TestTask8Scenario2_Idempotency(t *testing.T) {
	be := newTask8FakeBackend()
	s := newTask8Setup(t, be)
	def := task8Def("idem-wf",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("cost", "nexaroute.cost.usage", nil, map[string]any{}),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "cost", "")})
	pub := s.publish(t, def)
	run1 := s.runToEnd(t, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "idem-key")
	if run1.State != RunSucceeded {
		t.Fatalf("run1 state = %q", run1.State)
	}
	// Same key again: same run, no second execution of the node.
	run2, err := s.service.StartRun(context.Background(), pub, task8Requester, map[string]any{"payload": map[string]any{}}, "idem-key")
	if err != nil {
		t.Fatalf("second StartRun: %v", err)
	}
	if run2.ID != run1.ID {
		t.Fatalf("idempotent run ids differ: %q vs %q", run2.ID, run1.ID)
	}
	if _, err := s.service.ExecuteRun(context.Background(), run2.ID); err != nil {
		t.Fatalf("second ExecuteRun: %v", err)
	}
	if got := be.usageCount(); got != 1 {
		t.Fatalf("usage backend calls = %d, want 1 (single execution)", got)
	}
	// Same key on a different definition is a conflict, not a new run.
	def2 := def
	def2.ID = "idem-wf-2"
	pub2 := s.publish(t, def2)
	if _, err := s.service.StartRun(context.Background(), pub2, task8Requester, map[string]any{"payload": map[string]any{}}, "idem-key"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting key error = %v, want ErrIdempotencyConflict", err)
	}
}

// ---- Scenario 3: signed webhook + durable replay protection ----

func TestTask8Scenario3_Webhook(t *testing.T) {
	dir := t.TempDir()
	ledger, err := NewFileDeliveryLedger(filepath.Join(dir, "ledger"))
	if err != nil {
		t.Fatalf("NewFileDeliveryLedger: %v", err)
	}
	starts := 0
	start := func(ctx context.Context, defID, trigger string, body []byte) (string, error) {
		starts++
		return fmt.Sprintf("run-%d", starts), nil
	}
	svc, err := NewWebhookService(NewWebhookVerifier(), ledger, start)
	if err != nil {
		t.Fatalf("NewWebhookService: %v", err)
	}
	ctx := context.Background()
	secret := "test-webhook-secret"
	ts := fmt.Sprintf("%d", time.Now().Unix())
	body := `{"event":"ping"}`
	sig := task8SignWebhook(secret, ts, body)
	if _, err := svc.Handle(ctx, "wf-1", "hook", ts, sig, body, secret); err != nil {
		t.Fatalf("valid delivery: %v", err)
	}
	if starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
	// Replay of the identical delivery: rejected, no second start.
	if _, err := svc.Handle(ctx, "wf-1", "hook", ts, sig, body, secret); !errors.Is(err, ErrWebhookReplay) {
		t.Fatalf("replay error = %v, want ErrWebhookReplay", err)
	}
	// Stale, malformed, wrong-secret, tampered-body: all rejected.
	stale := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	if _, err := svc.Handle(ctx, "wf-1", "hook", stale, task8SignWebhook(secret, stale, body), body, secret); err == nil {
		t.Fatalf("stale delivery accepted")
	}
	if _, err := svc.Handle(ctx, "wf-1", "hook", ts, "not-hex", body, secret); err == nil {
		t.Fatalf("malformed signature accepted")
	}
	if _, err := svc.Handle(ctx, "wf-1", "hook", ts, sig, body, "wrong-secret"); err == nil {
		t.Fatalf("wrong-secret delivery accepted")
	}
	if _, err := svc.Handle(ctx, "wf-1", "hook", ts, sig, `{"event":"pong"}`, secret); err == nil {
		t.Fatalf("tampered-body delivery accepted")
	}
	if starts != 1 {
		t.Fatalf("starts = %d after rejections, want 1", starts)
	}
	// Restart: a brand-new verifier over the SAME ledger directory must
	// still reject the replay (the durable ledger, not memory, decides).
	svc2, err := NewWebhookService(NewWebhookVerifier(), ledger, start)
	if err != nil {
		t.Fatalf("NewWebhookService #2: %v", err)
	}
	if _, err := svc2.Handle(ctx, "wf-1", "hook", ts, sig, body, secret); !errors.Is(err, ErrWebhookReplay) {
		t.Fatalf("post-restart replay error = %v, want ErrWebhookReplay", err)
	}
	if starts != 1 {
		t.Fatalf("starts = %d after restart replay, want 1", starts)
	}
}

// ---- Scenario 4: durable schedule + no duplicate dispatch ----

func TestTask8Scenario4_SchedulerSingleProcess(t *testing.T) {
	dir := t.TempDir()
	ledger, err := NewFileDeliveryLedger(filepath.Join(dir, "ledger"))
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	starts := 0
	start := func(ctx context.Context, defID, slotKey string) (string, error) {
		starts++
		return fmt.Sprintf("run-%d", starts), nil
	}
	sch1, _ := NewScheduler(ledger, start)
	sch2, _ := NewScheduler(ledger, start) // second "process", same ledger
	now := time.Now().Truncate(time.Hour)
	sched := []Schedule{{DefinitionID: "wf-sched", IntervalSeconds: 3600, Enabled: true}}
	res1 := sch1.Tick(context.Background(), sched, now)
	res2 := sch2.Tick(context.Background(), sched, now)
	res3 := sch1.Tick(context.Background(), sched, now.Add(10*time.Minute))
	started := 0
	for _, r := range append(append(res1, res2...), res3...) {
		if r.Started {
			started++
		}
		if r.Err != nil {
			t.Fatalf("tick error: %v", r.Err)
		}
	}
	if started != 1 || starts != 1 {
		t.Fatalf("started = %d (starts = %d), want exactly 1", started, starts)
	}
	// Disabled schedules never fire.
	res := sch1.Tick(context.Background(), []Schedule{{DefinitionID: "wf-off", IntervalSeconds: 60, Enabled: false}}, now)
	if len(res) != 0 {
		t.Fatalf("disabled schedule produced %d results", len(res))
	}
}

// Cross-process proof: two test-binary subprocesses claim the same slot
// through a shared file ledger; exactly one must win.
func TestTask8SchedulerCrossProcessChild(t *testing.T) {
	dir := os.Getenv("NEXAFLOW_TASK8_SCHED_DIR")
	if dir == "" {
		t.Skip("not a scheduler cross-process child")
	}
	ledger, err := NewFileDeliveryLedger(dir)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	claimed, err := ledger.Claim(context.Background(), "sched:wf-x:12345")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	out := "taken"
	if claimed {
		out = "claimed"
	}
	if err := os.WriteFile(filepath.Join(dir, os.Getenv("NEXAFLOW_TASK8_SCHED_OUT")), []byte(out), 0o600); err != nil {
		t.Fatalf("write result: %v", err)
	}
}

func TestTask8Scenario4_SchedulerCrossProcess(t *testing.T) {
	dir := t.TempDir()
	ledgerDir := filepath.Join(dir, "ledger")
	if _, err := NewFileDeliveryLedger(ledgerDir); err != nil {
		t.Fatalf("ledger: %v", err)
	}
	runChild := func(out string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=TestTask8SchedulerCrossProcessChild", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"NEXAFLOW_TASK8_SCHED_DIR="+ledgerDir,
			"NEXAFLOW_TASK8_SCHED_OUT="+out)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child %s: %v\n%s", out, err, out)
		}
	}
	runChild("r1.txt")
	runChild("r2.txt")
	claims := 0
	for _, f := range []string{"r1.txt", "r2.txt"} {
		raw, err := os.ReadFile(filepath.Join(ledgerDir, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.TrimSpace(string(raw)) == "claimed" {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("cross-process claims = %d, want exactly 1", claims)
	}
}

// ---- Scenario 5: LLM node through the Backend port, with policy gate ----

func TestTask8Scenario5_LLMNode(t *testing.T) {
	be := newTask8FakeBackend()
	s := newTask8Setup(t, be)
	def := task8Def("llm-wf",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("chat", "nexaroute.llm.chat", map[string]any{"route_or_alias": "test-route"}, map[string]any{"prompt": "hello"}),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "chat", "")})
	pub := s.publish(t, def)
	ctx := context.Background()
	run, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "llm-key")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	// First drive parks at the operator-policy gate; no backend call yet.
	run, err = s.service.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun #1: %v", err)
	}
	if run.State != RunWaitingForApproval {
		t.Fatalf("run state = %q, want waiting_for_approval", run.State)
	}
	if be.chatCount() != 0 {
		t.Fatalf("chat calls before approval = %d, want 0", be.chatCount())
	}
	// A separate approver releases the gate; the call then happens once.
	if _, err := s.service.ApproveRun(ctx, run.ID, "chat", task8Approver, "approved"); err != nil {
		t.Fatalf("ApproveRun: %v", err)
	}
	run, err = s.service.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun #2: %v", err)
	}
	if run.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded (err %q)", run.State, run.Error)
	}
	if be.chatCount() != 1 {
		t.Fatalf("chat calls = %d, want 1", be.chatCount())
	}
	states := task8NodeStates(t, s.store, run.ID)
	if states["chat"].Output["text"] != "hello back" || states["chat"].Output["model"] != "test-model" {
		t.Fatalf("chat output = %v", states["chat"].Output)
	}
	if _, hasPrompt := states["chat"].Output["prompt"]; hasPrompt {
		t.Fatalf("chat output echoes the prompt, which must never happen")
	}
	// Self-approval is rejected (separate test path through the service).
	run2, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "llm-key-2")
	if err != nil {
		t.Fatalf("StartRun #2: %v", err)
	}
	if _, err := s.service.ExecuteRun(ctx, run2.ID); err != nil {
		t.Fatalf("ExecuteRun #2a: %v", err)
	}
	if _, err := s.service.ApproveRun(ctx, run2.ID, "chat", task8Requester, "approved"); err == nil {
		t.Fatalf("self-approval accepted")
	}
}

// ---- Scenario 6: MCP allowlist ----

func TestTask8Scenario6_MCP(t *testing.T) {
	be := newTask8FakeBackend()
	be.mcpCallResult = map[string]any{"answer": 42}
	s := newTask8Setup(t, be)
	// Allowed tool: list + call succeed.
	def := task8Def("mcp-ok",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("list", "nexaroute.mcp.list", nil, map[string]any{"server_id": "srv-1"}),
			task8Node("call", "nexaroute.mcp.call", map[string]any{"allowlist": []any{"allowed-tool"}}, map[string]any{"server_id": "srv-1", "tool": "allowed-tool", "arguments": map[string]any{}}),
			task8TransformLeaf("after", "after"),
		},
		[]DefinitionEdge{
			task8Edge("e1", "start", "list", ""),
			task8Edge("e2", "list", "call", ""),
			task8Edge("e3", "call", "after", ""),
		})
	pub := s.publish(t, def)
	run := s.runToEnd(t, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "mcp-ok")
	if run.State != RunSucceeded {
		t.Fatalf("allowed run state = %q (err %q)", run.State, run.Error)
	}
	if be.mcpCount() != 1 {
		t.Fatalf("mcp calls = %d, want 1", be.mcpCount())
	}
	// Unauthorized tool: call fails tool_forbidden and the downstream
	// node never runs, so no later side effect occurs.
	be2 := newTask8FakeBackend()
	s2 := newTask8Setup(t, be2)
	def2 := task8Def("mcp-bad",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("call", "nexaroute.mcp.call", map[string]any{"allowlist": []any{"allowed-tool"}}, map[string]any{"server_id": "srv-1", "tool": "evil-tool", "arguments": map[string]any{}}),
			task8TransformLeaf("after", "after"),
		},
		[]DefinitionEdge{
			task8Edge("e1", "start", "call", ""),
			task8Edge("e2", "call", "after", ""),
		})
	pub2 := s2.publish(t, def2)
	run2 := s2.runToEnd(t, pub2, task8Requester, map[string]any{"payload": map[string]any{}}, "mcp-bad")
	if run2.State != RunFailed {
		t.Fatalf("unauthorized run state = %q, want failed", run2.State)
	}
	states := task8NodeStates(t, s2.store, run2.ID)
	if states["call"].ErrorCode != "tool_forbidden" {
		t.Fatalf("call error = %q, want tool_forbidden", states["call"].ErrorCode)
	}
	if states["after"].State != NodeSkipped {
		t.Fatalf("downstream state = %q, want skipped (no later side effect)", states["after"].State)
	}
	if be2.mcpCount() != 0 {
		t.Fatalf("backend mcp calls = %d, want 0 (rejected before the backend)", be2.mcpCount())
	}
}

// ---- Scenario 7 (part 1): parallel nodes, merge, loop plan ----

func TestTask8Scenario7_ParallelMergeLoop(t *testing.T) {
	s := newTask8Setup(t, newTask8FakeBackend())
	def := task8Def("par-merge",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8TransformLeaf("a", "a"),
			task8TransformLeaf("b", "b"),
			task8Node("m", "nexaroute.control.merge", map[string]any{"conflict": "last"}, map[string]any{
				"first":  "$.a.value",
				"second": "$.b.value",
			}),
		},
		[]DefinitionEdge{
			task8Edge("e1", "start", "a", ""),
			task8Edge("e2", "start", "b", ""),
			task8Edge("e3", "a", "m", ""),
			task8Edge("e4", "b", "m", ""),
		})
	pub := s.publish(t, def)
	run := s.runToEnd(t, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "par-key")
	if run.State != RunSucceeded {
		t.Fatalf("run state = %q (err %q)", run.State, run.Error)
	}
	states := task8NodeStates(t, s.store, run.ID)
	if states["a"].State != NodeSucceeded || states["b"].State != NodeSucceeded {
		t.Fatalf("parallel nodes: a=%q b=%q", states["a"].State, states["b"].State)
	}
	merged, _ := states["m"].Output["merged"].(map[string]any)
	if merged["lane"] != "b" || merged["done"] != true {
		t.Fatalf("merged output = %v, want lane=b done=true", merged)
	}

	// Loop plan: 3 items planned; >100 items fails loop_truncated,
	// never a silently partial iteration.
	loopDef := func(id string, items []any) Definition {
		return task8Def(id,
			[]DefinitionNode{
				task8Node("start", "nexaroute.trigger.manual", nil, nil),
				task8Node("plan", "nexaroute.control.loop_plan", map[string]any{"max_iterations": float64(100)}, map[string]any{"items": items}),
			},
			[]DefinitionEdge{task8Edge("e1", "start", "plan", "")})
	}
	items := []any{1, 2, 3}
	pubLoop := s.publish(t, loopDef("loop-ok", items))
	runLoop := s.runToEnd(t, pubLoop, task8Requester, map[string]any{"payload": map[string]any{}}, "loop-ok")
	if runLoop.State != RunSucceeded {
		t.Fatalf("loop run state = %q", runLoop.State)
	}
	if iters, _ := task8NodeStates(t, s.store, runLoop.ID)["plan"].Output["iterations"].([]any); len(iters) != 3 {
		t.Fatalf("iterations = %d, want 3", len(iters))
	}
	// max_iterations below the item count: truncated plan is a failure
	// (loop_truncated), never a silently partial iteration.
	loopDef2 := func(id string, maxIter float64, items []any) Definition {
		return task8Def(id,
			[]DefinitionNode{
				task8Node("start", "nexaroute.trigger.manual", nil, nil),
				task8Node("plan", "nexaroute.control.loop_plan", map[string]any{"max_iterations": maxIter}, map[string]any{"items": items}),
			},
			[]DefinitionEdge{task8Edge("e1", "start", "plan", "")})
	}
	five := []any{1, 2, 3, 4, 5}
	pubBig := s.publish(t, loopDef2("loop-big", 2, five))
	runBig := s.runToEnd(t, pubBig, task8Requester, map[string]any{"payload": map[string]any{}}, "loop-big")
	if runBig.State != RunFailed {
		t.Fatalf("truncated loop run state = %q, want failed", runBig.State)
	}
	if code := task8NodeStates(t, s.store, runBig.ID)["plan"].ErrorCode; code != "loop_truncated" {
		t.Fatalf("loop error = %q, want loop_truncated", code)
	}
	// More than 100 items is rejected outright by the node itself.
	big := make([]any, 101)
	for i := range big {
		big[i] = i
	}
	pubHuge := s.publish(t, loopDef("loop-huge", big))
	runHuge := s.runToEnd(t, pubHuge, task8Requester, map[string]any{"payload": map[string]any{}}, "loop-huge")
	if runHuge.State != RunFailed {
		t.Fatalf("oversized loop run state = %q, want failed", runHuge.State)
	}
	if code := task8NodeStates(t, s.store, runHuge.ID)["plan"].ErrorCode; code != "validation_error" {
		t.Fatalf("oversized loop error = %q, want validation_error", code)
	}
}
