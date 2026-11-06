package nexaflow

// Agent B residual batch 2: the remaining testable lines from the
// cov-nexaflow-3.out profile (99.2%), after the dead-branch removals
// in engine/file_store/http_backend/registry/studio_port/task8_migrate.
// Canonical profile: Go 1.27.3, no build tags, no live backends.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func agentBFailingRand() func([]byte) (int, error) {
	return func([]byte) (int, error) { return 0, errors.New("agentb: no entropy") }
}

func TestAgentBEngineNewEngineRandFailure(t *testing.T) {
	old := nexaflowRandRead
	nexaflowRandRead = agentBFailingRand()
	defer func() { nexaflowRandRead = old }()
	_, err := NewEngine(EngineOptions{Store: NewMemoryStore(), Registry: DefaultRegistry()})
	if err == nil || !strings.Contains(err.Error(), "generate worker id") {
		t.Fatalf("NewEngine rand failure: err = %v", err)
	}
}

func TestAgentBEngineCreateRunRandFailure(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	eng := mustEngine(t, store, DefaultRegistry())
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"work", "nexaroute.control.json_transform", nil})
	pub := publishDef(t, store, def)
	old := nexaflowRandRead
	nexaflowRandRead = agentBFailingRand()
	defer func() { nexaflowRandRead = old }()
	_, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err == nil || !strings.Contains(err.Error(), "generate run id") {
		t.Fatalf("CreateRun rand failure: err = %v", err)
	}
}

func TestAgentBStudioInstantiateTemplateRandFailure(t *testing.T) {
	svc := studioTestService(t)
	old := nexaflowRandRead
	nexaflowRandRead = agentBFailingRand()
	defer func() { nexaflowRandRead = old }()
	_, err := svc.InstantiateTemplate(context.Background(), "model-health-alert", map[string]string{"deployment_id": "d1"}, true)
	if err == nil || !strings.Contains(err.Error(), "instantiate template") {
		t.Fatalf("InstantiateTemplate rand failure: err = %v", err)
	}
}

// The memory/file stores are current-scoped: after a retry Append
// creates attempt N+1, saving the old attempt N fails with
// ErrStateConflict and recordOutcome parks the result (covered by
// TestAgentBEngineRecordOutcomeBackoffCancelled). The Postgres store
// is attempt-scoped instead: SaveNodeExecution addresses the old
// attempt's row directly, so the save succeeds and recordOutcome
// reaches the current-map update at the end of the retry branch
// (engine.go recordOutcome, the two lines after the successful
// SaveNodeExecution). The fake PG driver keeps this in the canonical
// (no live PG) profile.
func TestAgentBEngineRecordOutcomeRetryPostgresSaveSucceeds(t *testing.T) {
	ctx := context.Background()
	db := newAgentBPGFakeDB()
	store := agentBPGOpenStore(t, db)
	var parked []AuditEvent
	eng := mustEngine(t, store, testRegistry(t), func(o *EngineOptions) {
		o.Backoff = func(int) time.Duration { return 0 }
		o.Audit = func(ev AuditEvent) { parked = append(parked, ev) }
	})
	spec := testSpec("test.flaky", []Field{{Name: "payload", Type: "object"}},
		func(context.Context, *RunContext) (map[string]any, error) { return nil, nil })
	spec.Retry = RetryPolicy{MaxAttempts: 3, RetryableCodes: []string{"backend_unavailable"}}
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"f", "test.flaky", nil})
	run := agentBSeedRun(t, store, def, RunRunning, map[string]NodeState{"f": NodeRunning})
	current := nodeStates(t, store, run.ID)
	job := engineJob{
		node: def.Nodes[1], spec: spec, exec: current["f"], input: runInput(),
	}
	eng.recordOutcome(ctx, run, job, nil, NewNodeError("backend_unavailable", true, "transient"), current, map[string]map[string]any{})
	// The retry row is current: recordOutcome updated the map.
	if current["f"].State != NodePending {
		t.Fatalf("current[f] state = %q, want pending (retry row is current)", current["f"].State)
	}
	execs, err := store.GetNodeExecutions(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetNodeExecutions: %v", err)
	}
	var oldAttempt, retryAttempt *NodeExecution
	for i := range execs {
		if execs[i].NodeID != "f" {
			continue
		}
		switch execs[i].Attempt {
		case 0:
			oldAttempt = &execs[i]
		case 1:
			retryAttempt = &execs[i]
		}
	}
	if oldAttempt == nil || oldAttempt.State != NodeFailed {
		t.Fatalf("old attempt = %+v, want attempt 0 failed", oldAttempt)
	}
	if retryAttempt == nil || retryAttempt.State != NodePending {
		t.Fatalf("retry attempt = %+v, want attempt 1 pending", retryAttempt)
	}
	for _, ev := range parked {
		if ev.Kind == "parked" && ev.ErrorCode == "result_not_persisted" {
			t.Fatalf("parked events = %+v, want none (the PG save succeeded, unlike the memory store)", parked)
		}
	}
}

// planMigration's inner ReadDir fails only when the version directory
// cannot be read; root bypasses permission bits, so this runs only
// as a non-root user (the CI runner), mirroring the existing
// root-skipping file-lock test.
func TestAgentBMigrateVersionsReadDirError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs a non-root user: root bypasses directory permissions")
	}
	ctx := context.Background()
	src, _ := Continue95NexaflowMigSrc(t)
	dir := filepath.Join(src, "versions", "wf-mig")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil {
		t.Fatal("MigratePhase8Dir with an unreadable versions directory succeeded")
	}
}

// A legacy run without an input must migrate with an empty (non-nil)
// input map. The empty map comes from deepCopyValue (a nil map still
// matches its map case and yields a fresh empty map), not from a nil
// guard — that guard was provably dead and removed — so this test
// pins the behavior.
func TestAgentBMigrateRunNilInput(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	src, _ := Continue95NexaflowMigSrc(t)
	fin := now
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "runs", "r1.json"), legacyRun{
		ID: "r1", DefinitionID: "wf-mig", Revision: 1, Actor: "alice",
		Status: "succeeded", CreatedAt: now, UpdatedAt: now, FinishedAt: &fin,
	})
	dst := NewMemoryStore()
	if _, err := MigratePhase8Dir(ctx, src, dst, DefaultRegistry()); err != nil {
		t.Fatalf("MigratePhase8Dir: %v", err)
	}
	got, err := dst.GetRun(ctx, "r1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Input == nil {
		t.Fatal("migrated run input is nil, want an empty map")
	}
	if len(got.Input) != 0 {
		t.Fatalf("migrated run input = %v, want empty", got.Input)
	}
}
