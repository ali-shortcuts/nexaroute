package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// createBarrierStore forces both concurrent Create calls past the
// GetByIdempotency fast path before either reaches Create. This makes the
// check-then-act window deterministic instead of timing-dependent.
type createBarrierStore struct {
	*queue.MemoryStore
	mu      sync.Mutex
	creates int
	release chan struct{}
}

func newCreateBarrierStore() *createBarrierStore {
	return &createBarrierStore{MemoryStore: queue.NewMemoryStore(), release: make(chan struct{})}
}

func (s *createBarrierStore) Create(ctx context.Context, j video.VideoJob) error {
	s.mu.Lock()
	s.creates++
	if s.creates == 2 {
		close(s.release)
	}
	s.mu.Unlock()
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.MemoryStore.Create(ctx, j)
}

// Concurrent creates with one idempotency key must produce exactly one
// persisted job, hand the same job back to both callers, and submit to the
// provider only once.
func TestConcurrentCreateSameIdempotencyKeyCreatesOneJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	store := newCreateBarrierStore()
	q := queue.New(4)
	o := &Orchestrator{
		Store: store, Queue: q, Providers: Registry{"fake": providers.NewFake(1)}, Assets: testSink{},
		PollInterval: time.Nanosecond, MaxPolls: 3,
	}

	type outcome struct {
		job video.VideoJob
		err error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := requestWithDefaults()
			r.IdempotencyKey = "parallel-idempotency"
			<-start
			j, err := o.Create(ctx, r)
			results <- outcome{job: j, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var first video.VideoJob
	n := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent Create failed instead of returning the existing job: %v", result.err)
		}
		if n == 0 {
			first = result.job
		} else if result.job.JobID != first.JobID {
			t.Fatalf("callers received different jobs: %q and %q", first.JobID, result.job.JobID)
		}
		n++
	}

	jobs, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("persisted jobs = %d, want exactly 1", len(jobs))
	}
	if q.Len() != 1 {
		t.Fatalf("queued jobs = %d, want exactly 1", q.Len())
	}

	queued, err := q.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.RunJob(ctx, queued); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateCompleted {
		t.Fatalf("job state = %s, want completed (last error %q)", got.State, got.LastError)
	}
}
