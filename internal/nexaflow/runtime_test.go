package nexaflow

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

var testPrincipal = authz.Identity{Subject: "starter", Roles: []authz.Role{authz.RoleAdmin}}

func mustService(t *testing.T) *Service {
	t.Helper()
	svc, err := NewService(DefaultRegistry(), nil, NewStore(""))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func mustWorkflow(t *testing.T, svc *Service, name string, draft Graph) *Workflow {
	t.Helper()
	w, err := svc.CreateWorkflow(name, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateDraft(w.ID, draft, testPrincipal); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishWorkflow(w.ID, testPrincipal); err != nil {
		t.Fatal(err)
	}
	return w
}

func waitRun(t *testing.T, svc *Service, id string, want string, timeout time.Duration) *Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, err := svc.GetRun(id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status == want {
			return r
		}
		if r.Status == RunFailed || r.Status == RunCancelled || r.Status == RunSucceeded {
			if r.Status != want {
				t.Fatalf("run ended %s (want %s), error %q", r.Status, want, r.Error)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := svc.GetRun(id)
	t.Fatalf("run did not reach %s, last status %s", want, r.Status)
	return nil
}

func TestRunPureGraphSucceeds(t *testing.T) {
	svc := mustService(t)
	w := mustWorkflow(t, svc, "pure", validGraph())
	run, err := svc.StartRun(w.ID, 0, map[string]any{
		"payload":    map[string]any{"a": float64(1), "b": float64(2)},
		"operations": []any{map[string]any{"op": "pick", "field": "a"}},
	}, false, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	got := waitRun(t, svc, run.ID, RunSucceeded, 10*time.Second)
	if got.Nodes["x1"].Output["value"] == nil {
		t.Fatalf("transform node produced no value output: %+v", got.Nodes["x1"])
	}
	if got.Nodes["x1"].Attempts != 1 {
		t.Fatalf("expected 1 attempt, got %d", got.Nodes["x1"].Attempts)
	}
}

func TestRunApprovalApproveAndReject(t *testing.T) {
	for _, tc := range []struct {
		decision string
		approved bool
	}{
		{"approved", true},
		{"rejected", false},
	} {
		svc := mustService(t)
		// No edges: the approval node has no required inputs, so it
		// pauses and waits for ApproveRun.
		g := Graph{
			Nodes: []GraphNode{
				{ID: "t1", Type: "nexaroute.trigger.manual"},
				{ID: "a1", Type: "nexaroute.control.approval"},
			},
		}
		w := mustWorkflow(t, svc, "approval-"+tc.decision, g)
		run, err := svc.StartRun(w.ID, 0, map[string]any{}, false, testPrincipal)
		if err != nil {
			t.Fatal(err)
		}
		waitRun(t, svc, run.ID, RunAwaitingApproval, 10*time.Second)
		if _, err := svc.ApproveRun(run.ID, tc.decision, "boss"); err != nil {
			t.Fatal(err)
		}
		got := waitRun(t, svc, run.ID, RunSucceeded, 10*time.Second)
		if got.Nodes["a1"].Output["approved"] != tc.approved {
			t.Fatalf("decision %s: approved=%v, want %v", tc.decision, got.Nodes["a1"].Output["approved"], tc.approved)
		}
	}
}

func TestRunApprovalSelfApprovalFails(t *testing.T) {
	svc := mustService(t)
	g := Graph{
		Inputs: []string{"requester"},
		Nodes: []GraphNode{
			{ID: "t1", Type: "nexaroute.trigger.manual"},
			{ID: "a1", Type: "nexaroute.control.approval"},
		},
	}
	w := mustWorkflow(t, svc, "self-approval", g)
	// The requester is declared by the workflow (run input); the same
	// subject approving must be rejected by the node.
	run, err := svc.StartRun(w.ID, 0, map[string]any{"requester": "starter"}, false, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, svc, run.ID, RunAwaitingApproval, 10*time.Second)
	// Approver == starter ("starter") must be rejected by the node.
	if _, err := svc.ApproveRun(run.ID, "approved", "starter"); err != nil {
		t.Fatal(err)
	}
	got := waitRun(t, svc, run.ID, RunFailed, 10*time.Second)
	if got.Nodes["a1"].ErrorCode != "validation_error" {
		t.Fatalf("expected validation_error, got %+v", got.Nodes["a1"])
	}
}

func TestRunCancelDuringWait(t *testing.T) {
	svc := mustService(t)
	g := Graph{
		Inputs: []string{"duration_ms"},
		Nodes: []GraphNode{
			{ID: "t1", Type: "nexaroute.trigger.manual"},
			{ID: "w1", Type: "nexaroute.control.wait"},
		},
	}
	// duration_ms comes from run input, not from the trigger payload.
	w := mustWorkflow(t, svc, "cancel-wait", g)
	run, err := svc.StartRun(w.ID, 0, map[string]any{"duration_ms": float64(120000)}, false, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := svc.CancelRun(run.ID); err != nil {
		t.Fatal(err)
	}
	got := waitRun(t, svc, run.ID, RunCancelled, 10*time.Second)
	if got.Nodes["w1"].Status != RunCancelled {
		t.Fatalf("wait node status %s, want cancelled", got.Nodes["w1"].Status)
	}
}

func TestRunFailureIsRecordedSafely(t *testing.T) {
	svc := mustService(t)
	g := Graph{
		Inputs: []string{"prompt"},
		Nodes: []GraphNode{
			{ID: "t1", Type: "nexaroute.trigger.manual"},
			{ID: "l1", Type: "nexaroute.llm.chat", Config: map[string]any{"route_or_alias": "no-such-route"}},
		},
	}
	w := mustWorkflow(t, svc, "llm-fail", g)
	run, err := svc.StartRun(w.ID, 0, map[string]any{"prompt": "hello"}, false, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	got := waitRun(t, svc, run.ID, RunFailed, 10*time.Second)
	if got.Nodes["l1"].Error == "" || got.Nodes["l1"].ErrorCode == "" {
		t.Fatalf("expected recorded node error, got %+v", got.Nodes["l1"])
	}
}

func TestPublishSnapshotImmutable(t *testing.T) {
	svc := mustService(t)
	w, err := svc.CreateWorkflow("snap", testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateDraft(w.ID, validGraph(), testPrincipal); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishWorkflow(w.ID, testPrincipal); err != nil {
		t.Fatal(err)
	}
	// Edit the draft: the published version must not move.
	edited := validGraph()
	edited.Nodes[2].Config["expected"] = "999"
	if _, err := svc.UpdateDraft(w.ID, edited, testPrincipal); err != nil {
		t.Fatal(err)
	}
	got, err := svc.store.GetWorkflow(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Versions[0].Graph.Nodes[2].Config["expected"] != "1" {
		t.Fatalf("published version mutated: %v", got.Versions[0].Graph.Nodes[2].Config)
	}
}

func TestRestartReconciliationAndApprovalResume(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexaflow.json")
	svc1, err := NewService(DefaultRegistry(), nil, NewStore(path))
	if err != nil {
		t.Fatal(err)
	}
	g := Graph{
		Nodes: []GraphNode{
			{ID: "t1", Type: "nexaroute.trigger.manual"},
			{ID: "a1", Type: "nexaroute.control.approval"},
		},
	}
	w := mustWorkflow(t, svc1, "restart", g)
	run, err := svc1.StartRun(w.ID, 0, map[string]any{}, false, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, svc1, run.ID, RunAwaitingApproval, 10*time.Second)
	// Simulate a restart: a fresh service over the same store.
	svc2, err := NewService(DefaultRegistry(), nil, NewStore(path))
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc2.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != RunAwaitingApproval {
		t.Fatalf("awaiting run after restart: %s", got.Status)
	}
	if _, err := svc2.ApproveRun(run.ID, "approved", "boss"); err != nil {
		t.Fatal(err)
	}
	waitRun(t, svc2, run.ID, RunSucceeded, 10*time.Second)
}

func TestDuplicateAndDisable(t *testing.T) {
	svc := mustService(t)
	w := mustWorkflow(t, svc, "orig", validGraph())
	dup, err := svc.DuplicateWorkflow(w.ID, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if dup.Name == w.Name || len(dup.Versions) != 0 {
		t.Fatalf("bad duplicate: %+v", dup)
	}
	if _, err := svc.SetEnabled(w.ID, false, testPrincipal); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartRun(w.ID, 0, map[string]any{}, false, testPrincipal); err == nil {
		t.Fatal("disabled workflow started a run")
	}
}
