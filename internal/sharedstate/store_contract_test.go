package sharedstate

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
)

func memoryBackend(t *testing.T) Store {
	t.Helper()
	store, err := NewControlPlaneStore(controlplane.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func fileBackend(t *testing.T) Store {
	t.Helper()
	file, err := controlplane.NewFileStore(filepath.Join(t.TempDir(), "controlplane.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewControlPlaneStore(file)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestBreakerStoreContractMemory(t *testing.T) {
	runBreakerStoreContract(t, memoryBackend)
}

func TestBreakerStoreContractFile(t *testing.T) {
	runBreakerStoreContract(t, fileBackend)
}

func runBreakerStoreContract(t *testing.T, newStore func(*testing.T) Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("publish load and idempotency", func(t *testing.T) {
		now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
		counting := &countingStore{Store: newStore(t)}
		store := mustBreakerStore(t, counting, Options{ReplicaID: "replica-a", Now: func() time.Time { return now }})
		local := map[string]Entry{
			"p/m": {Status: StatusCooldown, CooldownUntil: now.Add(5 * time.Minute), ReasonClass: "rate_limit"},
		}
		first, err := store.Publish(ctx, local)
		if err != nil {
			t.Fatal(err)
		}
		if first.Revision != 1 {
			t.Fatalf("revision=%d want 1", first.Revision)
		}
		if first.Entries["p/m"].OriginReplica != "replica-a" {
			t.Fatalf("origin=%q want replica-a", first.Entries["p/m"].OriginReplica)
		}
		if !first.Entries["p/m"].ObservedAt.Equal(now) {
			t.Fatalf("observed_at=%v want %v", first.Entries["p/m"].ObservedAt, now)
		}
		second, err := store.Publish(ctx, local)
		if err != nil {
			t.Fatal(err)
		}
		if second.Revision != first.Revision {
			t.Fatalf("idempotent publish advanced revision %d -> %d", first.Revision, second.Revision)
		}
		if got := counting.cas.Load(); got != 1 {
			t.Fatalf("compare-and-swap calls=%d want 1 (second publish must not write)", got)
		}
		loaded, err := store.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Revision != first.Revision || !entriesEqual(loaded.Entries, first.Entries) {
			t.Fatalf("load=%+v want %+v", loaded, first)
		}
	})

	t.Run("concurrent publishes merge without lost update", func(t *testing.T) {
		now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
		barrier := &loadBarrierStore{
			Store:   newStore(t),
			target:  2,
			ready:   make(chan struct{}),
			release: make(chan struct{}),
		}
		first := mustBreakerStore(t, barrier, Options{ReplicaID: "replica-a", Now: func() time.Time { return now }})
		second := mustBreakerStore(t, barrier, Options{ReplicaID: "replica-b", Now: func() time.Time { return now }})

		errs := make(chan error, 2)
		go func() {
			_, err := first.Publish(ctx, map[string]Entry{
				"p/a": {Status: StatusCooldown, CooldownUntil: now.Add(5 * time.Minute), OriginReplica: "replica-a"},
			})
			errs <- err
		}()
		go func() {
			_, err := second.Publish(ctx, map[string]Entry{
				"p/b": {Status: StatusCooldown, CooldownUntil: now.Add(5 * time.Minute), OriginReplica: "replica-b"},
			})
			errs <- err
		}()

		select {
		case <-barrier.ready:
		case <-time.After(5 * time.Second):
			t.Fatal("both publishers did not reach their initial load")
		}
		close(barrier.release)
		for i := 0; i < 2; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("concurrent publish: %v", err)
			}
		}

		view, err := mustBreakerStore(t, barrier.Store, Options{Now: func() time.Time { return now }}).Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := view.Entries["p/a"]; !ok {
			t.Fatal("lost p/a marker")
		}
		if _, ok := view.Entries["p/b"]; !ok {
			t.Fatal("lost p/b marker")
		}
		if loads := barrier.loadCount(); loads < 2 {
			t.Fatalf("initial loads=%d want at least 2", loads)
		}
	})

	t.Run("expiration is filtered and pruned", func(t *testing.T) {
		now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
		backend := newStore(t)
		store := mustBreakerStore(t, backend, Options{Now: func() time.Time { return now }})
		if _, err := store.Publish(ctx, map[string]Entry{
			"expired": {Status: StatusCooldown, CooldownUntil: now.Add(5 * time.Minute)},
		}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(6 * time.Minute)
		view, err := store.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := view.Entries["expired"]; ok {
			t.Fatal("expired marker returned by Load")
		}
		if _, err := store.Publish(ctx, map[string]Entry{
			"active": {Status: StatusCooldown, CooldownUntil: now.Add(5 * time.Minute)},
		}); err != nil {
			t.Fatal(err)
		}
		raw, err := backend.Load(ctx, BreakerNamespace)
		if err != nil {
			t.Fatal(err)
		}
		persisted, err := decodeDocument(raw.Payload, DefaultMaxEntries, DefaultMaxDocumentBytes)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := persisted["expired"]; ok {
			t.Fatal("expired marker was not pruned by the next publish")
		}
		if _, ok := persisted["active"]; !ok {
			t.Fatal("active marker missing after publish")
		}
	})

	t.Run("outage does not clear existing markers", func(t *testing.T) {
		now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
		toggled := &toggleStore{Store: newStore(t)}
		store := mustBreakerStore(t, toggled, Options{Now: func() time.Time { return now }})
		if _, err := store.Publish(ctx, map[string]Entry{
			"p/held": {Status: StatusCooldown, CooldownUntil: now.Add(10 * time.Minute)},
		}); err != nil {
			t.Fatal(err)
		}
		toggled.setUnavailable(true)
		if _, err := store.Load(ctx); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Load outage err=%v want ErrUnavailable", err)
		}
		if _, err := store.Publish(ctx, map[string]Entry{
			"p/new": {Status: StatusCooldown, CooldownUntil: now.Add(10 * time.Minute)},
		}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Publish outage err=%v want ErrUnavailable", err)
		}
		toggled.setUnavailable(false)
		view, err := store.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := view.Entries["p/held"]; !ok {
			t.Fatal("outage cleared an existing cooldown")
		}
		if _, ok := view.Entries["p/new"]; ok {
			t.Fatal("failed publish was persisted")
		}
	})

	t.Run("malformed document is rejected not treated as empty", func(t *testing.T) {
		now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
		backend := newStore(t)
		if _, err := backend.CompareAndSwap(ctx, BreakerNamespace, 0, []byte(`{"schema":2,"entries":{}}`)); err != nil {
			t.Fatal(err)
		}
		store := mustBreakerStore(t, backend, Options{Now: func() time.Time { return now }})
		if _, err := store.Load(ctx); !errors.Is(err, ErrMalformed) {
			t.Fatalf("Load err=%v want ErrMalformed", err)
		}
		if _, err := store.Publish(ctx, map[string]Entry{
			"p/m": {Status: StatusCooldown, CooldownUntil: now.Add(time.Minute)},
		}); !errors.Is(err, ErrMalformed) {
			t.Fatalf("Publish err=%v want ErrMalformed", err)
		}
		raw, err := backend.Load(ctx, BreakerNamespace)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw.Payload) != `{"schema":2,"entries":{}}` {
			t.Fatalf("malformed document was overwritten: %s", raw.Payload)
		}
	})

	t.Run("conflict retries are bounded", func(t *testing.T) {
		now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
		conflicting := &conflictStore{Store: newStore(t)}
		store := mustBreakerStore(t, conflicting, Options{Now: func() time.Time { return now }, Retries: 2})
		_, err := store.Publish(ctx, map[string]Entry{
			"p/m": {Status: StatusCooldown, CooldownUntil: now.Add(time.Minute)},
		})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("err=%v want ErrConflict", err)
		}
		if got := conflicting.cas.Load(); got != 3 {
			t.Fatalf("compare-and-swap attempts=%d want 3 (initial + 2 retries)", got)
		}
	})
}

func TestBreakerStoreRestartRecoveryFileStore(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "controlplane.json")

	firstFile, err := controlplane.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	firstBackend, err := NewControlPlaneStore(firstFile)
	if err != nil {
		t.Fatal(err)
	}
	first := mustBreakerStore(t, firstBackend, Options{ReplicaID: "replica-a", Now: func() time.Time { return now }})
	if _, err := first.Publish(ctx, map[string]Entry{
		"p/a": {Status: StatusCooldown, CooldownUntil: now.Add(7 * time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}

	restartedFile, err := controlplane.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	restartedBackend, err := NewControlPlaneStore(restartedFile)
	if err != nil {
		t.Fatal(err)
	}
	restarted := mustBreakerStore(t, restartedBackend, Options{ReplicaID: "replica-b", Now: func() time.Time { return now }})
	view, err := restarted.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := view.Entries["p/a"]; !ok {
		t.Fatal("restart lost the persisted marker")
	}
	if _, err := restarted.Publish(ctx, map[string]Entry{
		"p/b": {Status: StatusCooldown, CooldownUntil: now.Add(7 * time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	view, err = first.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Entries) != 2 {
		t.Fatalf("entries=%+v want both markers after restart and publish", view.Entries)
	}
}

func TestBreakerStoreRejectsInvalidOptionsAndEntries(t *testing.T) {
	if _, err := NewBreakerStore(nil, Options{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil store err=%v", err)
	}
	if _, err := NewBreakerStore(memoryBackend(t), Options{Retries: -1}); err == nil {
		t.Fatal("negative retries accepted")
	}
	store := mustBreakerStore(t, memoryBackend(t), Options{})
	if _, err := store.Publish(context.Background(), map[string]Entry{"": {Status: StatusRetired}}); !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("empty deployment err=%v", err)
	}
	if _, err := store.Publish(context.Background(), map[string]Entry{"p/m": {Status: "healthy"}}); !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("invalid status err=%v", err)
	}
}

func TestControlPlaneStoreErrorMapping(t *testing.T) {
	if _, err := NewControlPlaneStore(nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil control-plane store err=%v", err)
	}
	ctx := context.Background()
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"not found", controlplane.ErrNotFound, ErrNotFound},
		{"conflict", controlplane.ErrConflict, ErrConflict},
		{"unavailable", controlplane.ErrUnavailable, ErrUnavailable},
		{"malformed", controlplane.ErrMalformedSnapshot, ErrMalformed},
		{"unknown", errors.New("boom"), ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend, err := NewControlPlaneStore(scriptedControlPlane{loadErr: tc.err})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backend.Load(ctx, "ns"); !errors.Is(err, tc.want) {
				t.Fatalf("Load err=%v want %v", err, tc.want)
			}
			backend, err = NewControlPlaneStore(scriptedControlPlane{casErr: tc.err})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backend.CompareAndSwap(ctx, "ns", 0, []byte("x")); !errors.Is(err, tc.want) {
				t.Fatalf("CompareAndSwap err=%v want %v", err, tc.want)
			}
		})
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	backend, _ := NewControlPlaneStore(scriptedControlPlane{loadErr: context.Canceled})
	if _, err := backend.Load(canceled, "ns"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled err=%v want context.Canceled", err)
	}
}

func mustBreakerStore(t *testing.T, store Store, options Options) *BreakerStore {
	t.Helper()
	breaker, err := NewBreakerStore(store, options)
	if err != nil {
		t.Fatal(err)
	}
	return breaker
}

type countingStore struct {
	Store
	cas atomic.Int64
}

func (s *countingStore) CompareAndSwap(ctx context.Context, namespace string, expectedRevision uint64, payload []byte) (Snapshot, error) {
	s.cas.Add(1)
	return s.Store.CompareAndSwap(ctx, namespace, expectedRevision, payload)
}

type conflictStore struct {
	Store
	cas atomic.Int64
}

func (s *conflictStore) CompareAndSwap(context.Context, string, uint64, []byte) (Snapshot, error) {
	s.cas.Add(1)
	return Snapshot{}, ErrConflict
}

type toggleStore struct {
	Store
	mu          sync.RWMutex
	unavailable bool
}

func (s *toggleStore) setUnavailable(unavailable bool) {
	s.mu.Lock()
	s.unavailable = unavailable
	s.mu.Unlock()
}

func (s *toggleStore) Load(ctx context.Context, namespace string) (Snapshot, error) {
	s.mu.RLock()
	unavailable := s.unavailable
	s.mu.RUnlock()
	if unavailable {
		return Snapshot{}, ErrUnavailable
	}
	return s.Store.Load(ctx, namespace)
}

func (s *toggleStore) CompareAndSwap(ctx context.Context, namespace string, expectedRevision uint64, payload []byte) (Snapshot, error) {
	s.mu.RLock()
	unavailable := s.unavailable
	s.mu.RUnlock()
	if unavailable {
		return Snapshot{}, ErrUnavailable
	}
	return s.Store.CompareAndSwap(ctx, namespace, expectedRevision, payload)
}

type loadBarrierStore struct {
	Store
	target  int
	mu      sync.Mutex
	count   int
	ready   chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *loadBarrierStore) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func (s *loadBarrierStore) Load(ctx context.Context, namespace string) (Snapshot, error) {
	snapshot, err := s.Store.Load(ctx, namespace)
	s.mu.Lock()
	s.count++
	count := s.count
	if count == s.target {
		s.once.Do(func() { close(s.ready) })
	}
	s.mu.Unlock()
	if count <= s.target {
		<-s.release
	}
	return snapshot, err
}

type scriptedControlPlane struct {
	loadErr error
	casErr  error
}

func (s scriptedControlPlane) Get(context.Context, string) (controlplane.Snapshot, error) {
	return controlplane.Snapshot{}, s.loadErr
}

func (s scriptedControlPlane) Put(context.Context, string, uint64, []byte) (controlplane.Snapshot, error) {
	return controlplane.Snapshot{}, s.casErr
}

func (s scriptedControlPlane) Delete(context.Context, string, uint64) error { return nil }
