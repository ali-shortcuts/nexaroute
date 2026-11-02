package nexaflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func checkAppendIdempotency(t *testing.T, store Store, id string) {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateRun(ctx, Run{ID: id, DefinitionID: "wf"}); err != nil {
		t.Fatal(err)
	}
	op := NodeExecution{RunID: id, NodeID: "node", State: NodePending, AppendOperationID: "initial"}
	const clients = 20
	results := make(chan NodeExecution, clients)
	errs := make(chan error, clients)
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			node, err := store.AppendNodeExecution(ctx, op)
			if err != nil {
				errs <- err
			} else {
				results <- node
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("append: %v", err)
	}
	for node := range results {
		if node.Attempt != 0 || node.AppendOperationID != "initial" {
			t.Errorf("replay: %+v", node)
		}
	}
	next, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: id, NodeID: "node", State: NodePending, AppendOperationID: "retry:0"})
	if err != nil || next.Attempt != 1 {
		t.Fatalf("legitimate retry: %+v %v", next, err)
	}
	next.State = NodeClaimed
	next.AppendOperationID = "client-must-not-change-token"
	if _, err := store.SaveNodeExecution(ctx, next, NodePending); err != nil {
		t.Fatal(err)
	}
	replay, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: id, NodeID: "node", AppendOperationID: "retry:0"})
	if err != nil || replay.Attempt != 1 || replay.AppendOperationID != "retry:0" || replay.State != NodeClaimed {
		t.Fatalf("updated attempt replay: %+v %v", replay, err)
	}
	nodes, err := store.GetNodeExecutions(ctx, id)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("attempts: %+v %v", nodes, err)
	}
}

func TestAppendIdempotencyMemoryFile(t *testing.T) {
	t.Run("memory", func(t *testing.T) { checkAppendIdempotency(t, NewMemoryStore(), "run") })
	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "store.json")
		store, err := OpenFileStore(path)
		if err != nil {
			t.Fatal(err)
		}
		checkAppendIdempotency(t, store, "run")
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store, err = OpenFileStore(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		node, err := store.AppendNodeExecution(context.Background(), NodeExecution{RunID: "run", NodeID: "node", AppendOperationID: "initial"})
		if err != nil || node.Attempt != 0 {
			t.Fatalf("reopen replay: %+v %v", node, err)
		}
	})
}

func TestAppendIdempotencyPostgres(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx := context.Background()
	store, err := OpenPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	id := Continue95NexaflowPGID("append-idem")
	checkAppendIdempotency(t, store, id)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	defer func() {
		_, _ = store.db.ExecContext(ctx, `DELETE FROM `+wfNodesTable+` WHERE run_id=$1`, id)
		_, _ = store.db.ExecContext(ctx, `DELETE FROM `+wfRunsTable+` WHERE run_id=$1`, id)
	}()
	node, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: id, NodeID: "node", AppendOperationID: "initial"})
	if err != nil || node.Attempt != 0 {
		t.Fatalf("reconnected replay: %+v %v", node, err)
	}
}

// A later retry does not substitute for a missing initial attempt.
func TestInitializationRequiresAttemptZero(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	reg := testRegistry(t, fixedSpec("test.echo", nil, nil))
	eng := mustEngine(t, store, reg)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"work", "test.echo", nil})
	pub := publishDef(t, store, def)
	run, err := eng.CreateRun(ctx, pub, principal(), nil, "key")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AppendNodeExecution(ctx, NodeExecution{RunID: run.ID, NodeID: "work", AppendOperationID: "retry:0"})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	var kept []*NodeExecution
	for _, n := range store.state.nodes[run.ID] {
		if n.NodeID != "work" || n.Attempt != 0 {
			kept = append(kept, n)
		}
	}
	store.state.nodes[run.ID] = kept
	store.mu.Unlock()
	if _, err := eng.CreateRun(ctx, pub, principal(), nil, "key"); !errors.Is(err, ErrRunInitializationIncomplete) {
		t.Fatalf("replay: %v", err)
	}
	if _, err := eng.Execute(ctx, run.ID); !errors.Is(err, ErrRunInitializationIncomplete) {
		t.Fatalf("execute: %v", err)
	}
}

type pausedInitializationStore struct {
	Store
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *pausedInitializationStore) AppendNodeExecution(ctx context.Context, node NodeExecution) (NodeExecution, error) {
	s.once.Do(func() { close(s.started); <-s.release })
	return s.Store.AppendNodeExecution(ctx, node)
}

func TestConcurrentReplayWaitsForInitialization(t *testing.T) {
	ctx := context.Background()
	base := NewMemoryStore()
	store := &pausedInitializationStore{Store: base, started: make(chan struct{}), release: make(chan struct{})}
	reg := testRegistry(t)
	eng := mustEngine(t, store, reg)
	pub := publishDef(t, store, chainDef())
	type result struct {
		run Run
		err error
	}
	first := make(chan result, 1)
	go func() { r, e := eng.CreateRun(ctx, pub, principal(), nil, "shared"); first <- result{r, e} }()
	<-store.started
	run, err := base.GetRunByIdempotency(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Execute(ctx, run.ID); !errors.Is(err, ErrRunInitializationIncomplete) {
		t.Fatalf("incomplete execute: %v", err)
	}
	second := make(chan result, 1)
	go func() { r, e := eng.CreateRun(ctx, pub, principal(), nil, "shared"); second <- result{r, e} }()
	close(store.release)
	a, b := <-first, <-second
	if a.err != nil || b.err != nil || a.run.ID != b.run.ID {
		t.Fatalf("concurrent creates: %+v %+v", a, b)
	}
	nodes, err := base.GetNodeExecutions(ctx, run.ID)
	if err != nil || len(nodes) != 1 || nodes[0].Attempt != 0 {
		t.Fatalf("duplicate initial nodes: %+v %v", nodes, err)
	}
}
