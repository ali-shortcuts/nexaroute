package queue

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

func queueJob(id, idem string) video.VideoJob {
	return video.NewJob(id, video.VideoRequest{ProjectID: "project", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo, IdempotencyKey: idem})
}

func TestQueueEnqueueNextCapacityContextAndClose(t *testing.T) {
	q := New(0)
	if q == nil || q.jobs == nil {
		t.Fatal("New(0) did not create a usable queue")
	}
	ctx := context.Background()
	j := queueJob("one", "")
	if err := q.Enqueue(ctx, j); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("Len=%d want 1", q.Len())
	}
	if err := q.Enqueue(ctx, queueJob("two", "")); !errors.Is(err, ErrFull) {
		t.Fatalf("full enqueue error=%v want %v", err, ErrFull)
	}
	got, err := q.Next(ctx)
	if err != nil || got.JobID != "one" {
		t.Fatalf("Next=%#v, %v", got, err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := q.Enqueue(cancelled, j); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled enqueue error=%v", err)
	}
	if _, err := q.Next(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Next error=%v", err)
	}
	q.Close()
	q.Close()
	if err := q.Enqueue(ctx, j); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed enqueue error=%v", err)
	}
	if _, err := q.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed Next error=%v want context.Canceled", err)
	}
}

func TestWorkerPoolRunsHandlerAndStopsOnContext(t *testing.T) {
	q := New(4)
	seen := make(chan string, 2)
	handler := func(_ context.Context, j video.VideoJob) error {
		seen <- j.JobID
		return errors.New("handler errors are intentionally ignored by pool")
	}
	p := NewWorkerPool(q, 0, handler)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	select {
	case <-p.Ready():
	case <-time.After(time.Second):
		t.Fatal("worker pool did not become ready")
	}
	for _, id := range []string{"a", "b"} {
		if err := q.Enqueue(ctx, queueJob(id, "")); err != nil {
			t.Fatal(err)
		}
	}
	seenIDs := map[string]bool{}
	for range 2 {
		select {
		case id := <-seen:
			seenIDs[id] = true
		case <-time.After(time.Second):
			t.Fatalf("handler did not process both jobs: %#v", seenIDs)
		}
	}
	if len(seenIDs) != 2 {
		t.Fatalf("handler saw jobs %#v", seenIDs)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker pool did not stop after cancellation")
	}
}

func TestMemoryStoreCRUDAndIdempotency(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	j := queueJob("job", "key")
	if err := s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, j); err == nil {
		t.Fatal("duplicate job was accepted")
	}
	if err := s.Create(ctx, queueJob("other", "key")); err == nil {
		t.Fatal("duplicate idempotency key was accepted")
	}
	byID, err := s.Get(ctx, "job")
	if err != nil || byID.IdempotencyKey != "key" {
		t.Fatalf("Get=%#v, %v", byID, err)
	}
	byKey, err := s.GetByIdempotency(ctx, "key")
	if err != nil || byKey.JobID != "job" {
		t.Fatalf("GetByIdempotency=%#v, %v", byKey, err)
	}
	if _, err := s.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get error=%v", err)
	}
	if _, err := s.GetByIdempotency(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing idempotency error=%v", err)
	}
	j.State = video.StateProcessing
	if err := s.Update(ctx, j); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, j.JobID); got.State != video.StateProcessing {
		t.Fatalf("Update state=%s", got.State)
	}
	if err := s.Update(ctx, queueJob("missing", "")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Update error=%v", err)
	}
	if jobs, err := s.List(ctx); err != nil || len(jobs) != 1 {
		t.Fatalf("List=%d, %v", len(jobs), err)
	}
}

func TestFileStoreRecoveryRebuildsIdempotencyAndRejectsCorruption(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "jobs.json")
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	j := queueJob("persisted", "recovered-key")
	if err := s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	// Remove the index from disk: opening the store must reconstruct it from jobs.
	var state fileState
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state.Idempotency = nil
	data, _ = json.Marshal(state)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.GetByIdempotency(ctx, "recovered-key")
	if err != nil || got.JobID != "persisted" {
		t.Fatalf("recovered lookup=%#v, %v", got, err)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(bad); err == nil {
		t.Fatal("malformed store was accepted")
	}
	duplicate := filepath.Join(dir, "duplicate.json")
	dupState := fileState{Jobs: map[string]video.VideoJob{
		"a": queueJob("a", "same"), "b": queueJob("b", "same"),
	}}
	dupData, _ := json.Marshal(dupState)
	if err := os.WriteFile(duplicate, dupData, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(duplicate); err == nil {
		t.Fatal("duplicate persisted idempotency key was accepted")
	}
	if _, err := NewFileStore(""); err == nil {
		t.Fatal("empty file store path was accepted")
	}
}

func TestFileStoreDurabilityFailureRollsBackCreateAndUpdate(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "jobs")
	path := filepath.Join(root, "jobs.json")
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first := queueJob("first", "first-key")
	if err := s.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	updated := first
	updated.State = video.StateProcessing
	if err := s.Update(ctx, updated); err == nil {
		t.Fatal("update unexpectedly persisted through a blocking path")
	}
	got, err := s.Get(ctx, first.JobID)
	if err != nil || got.State != first.State {
		t.Fatalf("update rollback got=%#v err=%v", got, err)
	}
	second := queueJob("second", "second-key")
	if err := s.Create(ctx, second); err == nil {
		t.Fatal("create unexpectedly persisted through a blocking path")
	}
	if _, err := s.Get(ctx, second.JobID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed create leaked job: %v", err)
	}
	if _, err := s.GetByIdempotency(ctx, second.IdempotencyKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed create leaked index: %v", err)
	}
}
