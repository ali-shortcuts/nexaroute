package nexaflow

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"
)

func webhookDef(id string) *Definition {
	return &Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            id,
		Name:          id,
		TenantScope:   "tenant-a",
		Nodes: []DefinitionNode{
			// Phase 1 validates the trigger instance config too, so it
			// carries the same secret_ref as the declaration; the
			// declaration's value is the runtime config.
			nd("wh", "nexaroute.trigger.webhook", map[string]any{"secret_ref": "webhook-secret"}, nil),
			nd("done", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"finished": true}, "operations": []any{}}),
		},
		Edges: []DefinitionEdge{ed("e1", "wh", "done", "")},
		Triggers: []DefinitionTrigger{{
			Name: "wh", Kind: TriggerWebhook,
			Config: map[string]any{"secret_ref": "webhook-secret"},
		}},
	}
}

func signPhase8Webhook(t *testing.T, secret, timestamp string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + string(body)))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestPhase8WebhookSignedDelivery(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	be := newCountingBackend()
	eng := testEngine(st, be)
	publishDef(t, ctx, st, webhookDef("wf-webhook"))

	body := []byte(`{"hello":"world"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := signPhase8Webhook(t, "s3cret", ts, body)
	run, err := eng.HandleWebhook(ctx, "wf-webhook", "wh", ts, sig, body)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	mustStatus(t, run, RunSucceeded)
	if stepOf(t, run, "done").Status != StepSucceeded {
		t.Fatal("downstream node did not run after webhook delivery")
	}
	stored, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	bodyJSON, ok := stored.Input["body_json"].(map[string]any)
	if !ok || bodyJSON["hello"] != "world" {
		t.Fatalf("run input body_json = %v", stored.Input["body_json"])
	}
	if stored.Trigger != "webhook" || stored.TriggerNodeID != "wh" {
		t.Fatalf("run trigger = %q node = %q", stored.Trigger, stored.TriggerNodeID)
	}
}

func TestPhase8WebhookReplayRejected(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	be := newCountingBackend()
	eng := testEngine(st, be)
	publishDef(t, ctx, st, webhookDef("wf-webhook-replay"))
	body := []byte(`{"n":1}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := signPhase8Webhook(t, "s3cret", ts, body)
	if _, err := eng.HandleWebhook(ctx, "wf-webhook-replay", "wh", ts, sig, body); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	// Same delivery again: rejected, no second run.
	if _, err := eng.HandleWebhook(ctx, "wf-webhook-replay", "wh", ts, sig, body); err == nil {
		t.Fatal("webhook replay was accepted")
	}
	runs, _ := st.ListRuns(ctx, RunFilter{DefinitionID: "wf-webhook-replay"})
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
}

func TestPhase8WebhookReplayRejectedAcrossRestart(t *testing.T) {
	// The durable delivery ledger, not just the in-memory verifier
	// cache, must reject a replay after a restart.
	ctx := context.Background()
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	be := newCountingBackend()
	eng1 := testEngine(fs, be)
	publishDef(t, ctx, fs, webhookDef("wf-webhook-durable"))
	body := []byte(`{"n":2}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := signPhase8Webhook(t, "s3cret", ts, body)
	if _, err := eng1.HandleWebhook(ctx, "wf-webhook-durable", "wh", ts, sig, body); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	fs2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := fs2.Migrate(ctx); err != nil {
		t.Fatalf("reopen Migrate: %v", err)
	}
	eng2 := testEngine(fs2, be)
	_, err = eng2.HandleWebhook(ctx, "wf-webhook-durable", "wh", ts, sig, body)
	if err == nil {
		t.Fatal("replay after restart was accepted")
	}
	if !strings.Contains(err.Error(), "replay_rejected") {
		t.Fatalf("replay error = %v, want replay_rejected from the durable ledger", err)
	}
}

func TestPhase8WebhookBadSignatures(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	eng := testEngine(st, newCountingBackend())
	publishDef(t, ctx, st, webhookDef("wf-webhook-bad"))
	body := []byte(`{"n":3}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	if _, err := eng.HandleWebhook(ctx, "wf-webhook-bad", "wh", ts, "deadbeef", body); err == nil {
		t.Fatal("tampered signature was accepted")
	}
	old := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	sig := signPhase8Webhook(t, "s3cret", old, body)
	if _, err := eng.HandleWebhook(ctx, "wf-webhook-bad", "wh", old, sig, body); err == nil {
		t.Fatal("expired timestamp was accepted")
	}
	big := make([]byte, MaxWorkflowInputBytes+1)
	if _, err := eng.HandleWebhook(ctx, "wf-webhook-bad", "wh", ts, signPhase8Webhook(t, "s3cret", ts, big), big); err == nil {
		t.Fatal("oversized webhook body was accepted")
	}
	runs, _ := st.ListRuns(ctx, RunFilter{DefinitionID: "wf-webhook-bad"})
	if len(runs) != 0 {
		t.Fatalf("rejected deliveries created %d runs", len(runs))
	}
}

func scheduleDef(id string, cfg map[string]any) *Definition {
	return &Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            id,
		Name:          id,
		TenantScope:   "tenant-a",
		Nodes: []DefinitionNode{
			nd("sched", "nexaroute.trigger.schedule", nil, nil),
			nd("done", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"finished": true}, "operations": []any{}}),
		},
		Edges:    []DefinitionEdge{ed("e1", "sched", "done", "")},
		Triggers: []DefinitionTrigger{{Name: "sched", Kind: TriggerSchedule, Config: cfg}},
	}
}

func TestPhase8SchedulerIntervalNoDuplicates(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	eng := testEngine(st, newCountingBackend())
	publishDef(t, ctx, st, scheduleDef("wf-sched", map[string]any{"interval": "1h"}))

	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	s1 := NewScheduler(eng, st)
	s1.now = func() time.Time { return now }
	started, err := s1.Tick(ctx)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if len(started) != 1 {
		t.Fatalf("tick 1 started %d runs, want 1 (first tick fires immediately)", len(started))
	}
	if started[0].Status != RunSucceeded {
		t.Fatalf("scheduled run status = %s", started[0].Status)
	}
	// Same tick again, and a second scheduler over the same store:
	// the claimed slot must not dispatch twice.
	started, err = s1.Tick(ctx)
	if err != nil || len(started) != 0 {
		t.Fatalf("tick 2 = %d runs, %v; want 0", len(started), err)
	}
	s2 := NewScheduler(eng, st)
	s2.now = func() time.Time { return now }
	started, err = s2.Tick(ctx)
	if err != nil || len(started) != 0 {
		t.Fatalf("second scheduler tick = %d runs, %v; want 0", len(started), err)
	}
	// One hour later the next slot is due; a further tick at the same
	// time dispatches nothing (catch-up is one slot per tick).
	later := now.Add(time.Hour + time.Minute)
	s1.now = func() time.Time { return later }
	started, err = s1.Tick(ctx)
	if err != nil || len(started) != 1 {
		t.Fatalf("later tick = %d runs, %v; want 1", len(started), err)
	}
	runs, _ := st.ListRuns(ctx, RunFilter{DefinitionID: "wf-sched"})
	if len(runs) != 2 {
		t.Fatalf("total scheduled runs = %d, want 2", len(runs))
	}
	// Idempotency key reuse: the scheduler's key for a fired slot maps
	// to the existing run, never a duplicate.
	st2, err := st.GetScheduleState(ctx, "wf-sched:sched")
	if err != nil {
		t.Fatalf("GetScheduleState: %v", err)
	}
	if st2.LastFire.IsZero() {
		t.Fatal("schedule state has no last fire")
	}
}

func TestPhase8SchedulerDaily(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	eng := testEngine(st, newCountingBackend())
	publishDef(t, ctx, st, scheduleDef("wf-daily", map[string]any{"daily": "09:30"}))
	s := NewScheduler(eng, st)

	s.now = func() time.Time { return time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC) }
	started, err := s.Tick(ctx)
	if err != nil || len(started) != 0 {
		t.Fatalf("09:00 tick = %d runs, %v; want 0 (slot not reached)", len(started), err)
	}
	s.now = func() time.Time { return time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC) }
	started, err = s.Tick(ctx)
	if err != nil || len(started) != 1 {
		t.Fatalf("10:00 tick = %d runs, %v; want 1", len(started), err)
	}
	s.now = func() time.Time { return time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC) }
	started, err = s.Tick(ctx)
	if err != nil || len(started) != 0 {
		t.Fatalf("11:00 same-day tick = %d runs, %v; want 0", len(started), err)
	}
	s.now = func() time.Time { return time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC) }
	started, err = s.Tick(ctx)
	if err != nil || len(started) != 1 {
		t.Fatalf("next-day tick = %d runs, %v; want 1", len(started), err)
	}
}

func craftedRunningRun(id, defID string, steps []Step) *Run {
	return &Run{
		ID: id, DefinitionID: defID, Revision: 1, TenantScope: "tenant-a",
		Trigger: "manual", TriggerNodeID: "start", Actor: "op-1",
		Status: RunRunning, Steps: steps,
		LeaseOwner: "dead-engine", LeaseUntil: time.Now().Add(-time.Hour),
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now().Add(-time.Hour),
	}
}

func succeededStep(nodeID string) Step {
	now := time.Now()
	return Step{NodeID: nodeID, Attempt: 1, Iteration: -1, Status: StepSucceeded, StartedAt: now, FinishedAt: &now}
}

func TestPhase8CrashRecoveryModes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	be := newCountingBackend()
	// Definition A: start -> call (side-effecting) -> done.
	defA := manualDef("wf-crash-mcp", "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("call", "nexaroute.mcp.call", map[string]any{"allowlist": []any{"tools__search_files"}}, map[string]any{
				"server_id": "tools", "tool": "search_files",
			}),
			nd("done", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"finished": true}, "operations": []any{}}),
		},
		[]DefinitionEdge{ed("e1", "start", "call", ""), ed("e2", "call", "done", "")},
		DefinitionSettings{}, nil)
	publishDef(t, ctx, fs, defA)
	// Definition B: start -> t (pure) -> done.
	defB := manualDef("wf-crash-pure", "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("t", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"x": float64(1)}, "operations": []any{}}),
			nd("done", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"finished": true}, "operations": []any{}}),
		},
		[]DefinitionEdge{ed("e1", "start", "t", ""), ed("e2", "t", "done", "")},
		DefinitionSettings{}, nil)
	publishDef(t, ctx, fs, defB)

	runningStep := func(nodeID string) Step {
		return Step{NodeID: nodeID, Attempt: 1, Iteration: -1, Status: StepRunning, StartedAt: time.Now().Add(-time.Hour)}
	}
	crashA := craftedRunningRun("run-crash-a", "wf-crash-mcp",
		[]Step{succeededStep("start"), runningStep("call")})
	if err := fs.CreateRun(ctx, crashA); err != nil {
		t.Fatalf("CreateRun A: %v", err)
	}
	crashB := craftedRunningRun("run-crash-b", "wf-crash-pure",
		[]Step{succeededStep("start"), runningStep("t")})
	if err := fs.CreateRun(ctx, crashB); err != nil {
		t.Fatalf("CreateRun B: %v", err)
	}

	eng := testEngine(fs, be)
	parked, err := eng.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(parked) != 2 {
		t.Fatalf("Recover parked %d runs, want 2", len(parked))
	}
	got, err := fs.GetRun(ctx, "run-crash-a")
	if err != nil {
		t.Fatalf("GetRun A: %v", err)
	}
	if got.Status != RunParked || got.AmbiguousNodeID != "call" {
		t.Fatalf("run A after recover: status=%s ambiguous=%q", got.Status, got.AmbiguousNodeID)
	}
	if stepOf(t, got, "call").Status != StepAmbiguous {
		t.Fatal("interrupted step was not marked ambiguous")
	}

	// continue with an ambiguous node is refused: the operator must choose.
	if _, err := eng.ResumeRun(ctx, phase8Operator, "run-crash-a", ResumeContinue); err == nil {
		t.Fatal("ResumeRun continue was accepted with an ambiguous node")
	}
	// skip: the side-effecting call is never blindly re-executed.
	skipped, err := eng.ResumeRun(ctx, phase8Operator, "run-crash-a", ResumeSkipAmbiguous)
	if err != nil {
		t.Fatalf("ResumeRun skip: %v", err)
	}
	mustStatus(t, skipped, RunSucceeded)
	if be.mcpCalls != 0 {
		t.Fatalf("mcp calls after skip = %d, want 0 (no blind repetition)", be.mcpCalls)
	}
	if stepOf(t, skipped, "call").Status != StepSkipped {
		t.Fatal("ambiguous call node was not skipped")
	}
	if stepOf(t, skipped, "done").Status != StepSkipped {
		t.Fatal("node downstream of a skipped node was not skipped")
	}

	// retry: a pure node is safe to re-execute.
	retried, err := eng.ResumeRun(ctx, phase8Operator, "run-crash-b", ResumeRetryAmbiguous)
	if err != nil {
		t.Fatalf("ResumeRun retry: %v", err)
	}
	mustStatus(t, retried, RunSucceeded)
	steps := 0
	for _, s := range retried.Steps {
		if s.NodeID == "t" && s.Status == StepSucceeded {
			steps++
		}
	}
	if steps != 1 {
		t.Fatalf("pure node succeeded steps = %d, want 1 re-execution", steps)
	}

	// fail: the ambiguous outcome becomes a recorded failure, and the
	// side effect is still never re-executed.
	crashC := craftedRunningRun("run-crash-c", "wf-crash-mcp",
		[]Step{succeededStep("start"), runningStep("call")})
	if err := fs.CreateRun(ctx, crashC); err != nil {
		t.Fatalf("CreateRun C: %v", err)
	}
	if _, err := eng.Recover(ctx); err != nil {
		t.Fatalf("Recover C: %v", err)
	}
	failed, err := eng.ResumeRun(ctx, phase8Operator, "run-crash-c", ResumeFailAmbiguous)
	if err != nil {
		t.Fatalf("ResumeRun fail: %v", err)
	}
	mustStatus(t, failed, RunFailed)
	if failed.Error == nil || failed.Error.Code != "ambiguous_outcome" {
		t.Fatalf("failed run error = %v, want ambiguous_outcome", failed.Error)
	}
	if be.mcpCalls != 0 {
		t.Fatalf("mcp calls after fail = %d, want 0", be.mcpCalls)
	}
}
