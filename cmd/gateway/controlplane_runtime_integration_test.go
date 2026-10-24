//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
)

// bootstrapStartBarrierStore makes both independent PostgreSQL clients observe
// the initial missing namespace before either caller can attempt revision 0.
// This deterministically exercises the real CAS race instead of relying on
// scheduler timing.
type bootstrapStartBarrierStore struct {
	controlplane.Store
	namespace string
	arrivals  *atomic.Int32
	released  chan struct{}
	observed  atomic.Bool
}

func (s *bootstrapStartBarrierStore) Get(ctx context.Context, namespace string) (controlplane.Snapshot, error) {
	snapshot, err := s.Store.Get(ctx, namespace)
	if namespace == s.namespace && errors.Is(err, controlplane.ErrNotFound) && s.observed.CompareAndSwap(false, true) {
		if s.arrivals.Add(1) == 2 {
			close(s.released)
		}
		select {
		case <-s.released:
		case <-ctx.Done():
			return controlplane.Snapshot{}, ctx.Err()
		}
	}
	return snapshot, err
}

func TestConcurrentPostgresControlPlaneBootstrapConverges(t *testing.T) {
	const dsnEnv = "NEXAROUTE_TEST_POSTGRES_DSN"
	if os.Getenv(dsnEnv) == "" {
		t.Skip("NEXAROUTE_TEST_POSTGRES_DSN is not configured")
	}

	cfg := config.ControlPlaneConfig{PostgresDSNEnv: dsnEnv}
	storeA, dbA, err := openPostgresControlPlane(cfg)
	if err != nil {
		t.Fatalf("open first PostgreSQL control-plane store: %v", err)
	}
	defer dbA.Close()
	storeB, dbB, err := openPostgresControlPlane(cfg)
	if err != nil {
		t.Fatalf("open second PostgreSQL control-plane store: %v", err)
	}
	defer dbB.Close()

	namespace := fmt.Sprintf("test/concurrent-bootstrap/%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cleanupCancel()
		if current, getErr := storeA.Get(cleanupCtx, namespace); getErr == nil {
			_ = storeA.Delete(cleanupCtx, namespace, current.Revision)
		}
	}()

	payload := []byte(`{"schema":1,"backend":"postgres"}`)
	released := make(chan struct{})
	arrivals := &atomic.Int32{}
	gatedA := &bootstrapStartBarrierStore{Store: storeA, namespace: namespace, arrivals: arrivals, released: released}
	gatedB := &bootstrapStartBarrierStore{Store: storeB, namespace: namespace, arrivals: arrivals, released: released}
	managerA, err := controlplane.NewManager(gatedA, controlplane.FailClosed)
	if err != nil {
		t.Fatal(err)
	}
	managerB, err := controlplane.NewManager(gatedB, controlplane.FailClosed)
	if err != nil {
		t.Fatal(err)
	}

	results := make(chan error, 2)
	go func() { results <- ensureControlPlaneSnapshot(ctx, managerA, namespace, payload) }()
	go func() { results <- ensureControlPlaneSnapshot(ctx, managerB, namespace, payload) }()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent initialization should converge on the same marker: %v", err)
		}
	}

	got, err := storeA.Get(ctx, namespace)
	if err != nil {
		t.Fatalf("read winner marker: %v", err)
	}
	if string(got.Payload) != string(payload) || got.Revision != 1 {
		t.Fatalf("unexpected bootstrap state: revision=%d payload=%q", got.Revision, got.Payload)
	}
}
