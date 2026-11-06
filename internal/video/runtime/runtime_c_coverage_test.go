package runtime

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// Agent C coverage: the postgres backend open paths, the unknown-host
// fallback, the cancelled Start, and the hourly prune ticker.

func postgresConfig() video.Config {
	cfg := issue662Config()
	cfg.JobStoreBackend = "postgres"
	cfg.JobStoreURL = "postgres://127.0.0.1:1/nexaroute"
	return cfg
}

func TestOpenPostgresStoreSuccess(t *testing.T) {
	old := openPostgresStoreFunc
	openPostgresStoreFunc = func(context.Context, string) (queue.JobStore, error) {
		return queue.NewMemoryStore(), nil
	}
	t.Cleanup(func() { openPostgresStoreFunc = old })

	store, err := openJobStore(postgresConfig(), t.TempDir())
	if err != nil || store == nil {
		t.Fatalf("openJobStore postgres err = %v store = %v", err, store)
	}
}

func TestOpenPostgresLedgerSuccess(t *testing.T) {
	old := openPostgresLedgerFunc
	openPostgresLedgerFunc = func(context.Context, string) (orchestrator.CostLedger, io.Closer, error) {
		return &cost.Ledger{}, nil, nil
	}
	t.Cleanup(func() { openPostgresLedgerFunc = old })

	ledger, closer, err := openCostLedger(postgresConfig())
	if err != nil || ledger == nil {
		t.Fatalf("openCostLedger postgres err = %v ledger = %v closer = %v", err, ledger, closer)
	}
}

func TestNewPostgresLedgerFailureAfterStoreSuccess(t *testing.T) {
	oldStore := openPostgresStoreFunc
	openPostgresStoreFunc = func(context.Context, string) (queue.JobStore, error) {
		return queue.NewMemoryStore(), nil
	}
	t.Cleanup(func() { openPostgresStoreFunc = oldStore })
	oldLedger := openPostgresLedgerFunc
	openPostgresLedgerFunc = func(context.Context, string) (orchestrator.CostLedger, io.Closer, error) {
		return nil, nil, errors.New("synthetic ledger failure")
	}
	t.Cleanup(func() { openPostgresLedgerFunc = oldLedger })

	t.Setenv(issue662TokenEnv, "issue662-token")
	if _, err := New(postgresConfig(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "synthetic ledger failure") {
		t.Fatalf("New with failing ledger err = %v, want the ledger failure", err)
	}
}

func TestInstanceIDUnknownHost(t *testing.T) {
	old := osHostname
	t.Cleanup(func() { osHostname = old })
	for _, tc := range []struct {
		name string
		host string
		err  error
	}{
		{"hostname error", "", errors.New("boom")},
		{"empty hostname", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			osHostname = func() (string, error) { return tc.host, tc.err }
			id := instanceID()
			if !strings.HasPrefix(id, "unknown-host/") {
				t.Fatalf("instanceID = %q, want the unknown-host fallback", id)
			}
		})
	}
}

func TestStartCancelledContext(t *testing.T) {
	// The select in Start races the worker goroutine's Ready close
	// against the already-cancelled context. The context is closed
	// before Start runs, so the Done branch wins in practice; retry a
	// few times so a scheduling fluke cannot fail the test.
	for attempt := 0; attempt < 10; attempt++ {
		r := issue662New(t, issue662Config())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := r.Start(ctx)
		r.Close()
		if errors.Is(err, context.Canceled) {
			return
		}
	}
	t.Fatal("Start with a cancelled context never took the worker-done branch")
}

func TestStartHourlyPruneTicker(t *testing.T) {
	old := pruneInterval
	pruneInterval = 5 * time.Millisecond
	t.Cleanup(func() { pruneInterval = old })

	cfg := issue662Config()
	cfg.JobRetentionDays = 1
	r := issue662New(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Create the expired job after startup, so only the ticker sweep
	// (not the startup sweep) can remove it.
	oldJob := video.VideoJob{
		JobID: "ticker-old-terminal", State: video.StateCompleted,
		CreatedAt: time.Now().Add(-72 * time.Hour),
	}
	if err := r.Store.Create(context.Background(), oldJob); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := r.Store.Get(context.Background(), oldJob.JobID); errors.Is(err, queue.ErrNotFound) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("hourly prune ticker did not remove the expired terminal job")
}
