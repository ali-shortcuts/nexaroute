package controlplane

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMemoryStoreOptimisticConcurrencyAndPayloadIsolation(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	first, err := store.Put(ctx, "config", 0, []byte(`{"v":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 {
		t.Fatalf("revision=%d", first.Revision)
	}
	first.Payload[0] = 'X'
	got, err := store.Get(ctx, "config")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != `{"v":1}` {
		t.Fatalf("payload was aliased: %q", got.Payload)
	}
	if _, err := store.Put(ctx, "config", 0, []byte(`{"v":2}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write err=%v", err)
	}
}

func TestConcurrentWritersOnlyOneRevisionWins(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	if _, err := store.Put(ctx, "config", 0, []byte("initial")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, payload := range []string{"a", "b"} {
		wg.Add(1)
		go func(p string) { defer wg.Done(); _, err := store.Put(ctx, "config", 1, []byte(p)); results <- err }(payload)
	}
	wg.Wait()
	close(results)
	var success, conflicts int
	for err := range results {
		if err == nil {
			success++
		}
		if errors.Is(err, ErrConflict) {
			conflicts++
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
}

func TestManagerLastKnownGood(t *testing.T) {
	store := NewMemoryStore()
	manager, err := NewManager(store, LastKnownGood)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	saved, err := manager.Save(ctx, "config", 0, []byte("good"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetUnavailable(true)
	got, err := manager.Load(ctx, "config")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != "good" || got.Revision != saved.Revision {
		t.Fatalf("bad fallback: %+v", got)
	}
	manager.Invalidate("config")
	if _, err := manager.Load(ctx, "config"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestManagerFailClosedDoesNotUseStaleSnapshot(t *testing.T) {
	store := NewMemoryStore()
	manager, err := NewManager(store, FailClosed)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := manager.Save(ctx, "config", 0, []byte("good")); err != nil {
		t.Fatal(err)
	}
	store.SetUnavailable(true)
	if _, err := manager.Load(ctx, "config"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestManagerReconcileReportsDurableAndLastKnownGoodSources(t *testing.T) {
	store := NewMemoryStore()
	manager, err := NewManager(store, LastKnownGood)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := manager.Save(ctx, "config", 0, []byte("good")); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Reconcile(ctx, "config")
	if err != nil || result.Source != "durable" || result.Revision != 1 {
		t.Fatalf("durable reconcile=%+v err=%v", result, err)
	}
	store.SetUnavailable(true)
	result, err = manager.Reconcile(ctx, "config")
	if err != nil || result.Source != "last_known_good" || result.Revision != 1 {
		t.Fatalf("fallback reconcile=%+v err=%v", result, err)
	}
	health := manager.Health()
	if health.Saves != 1 || health.Unavailable != 1 {
		t.Fatalf("health=%+v", health)
	}
}

func TestMigrationRegistryRejectsTamperingAndOrdersVersions(t *testing.T) {
	registry, err := NewMigrationRegistry(Migration{Version: 2, Name: "second", Up: "up2"}, Migration{Version: 1, Name: "first", Up: "up1"})
	if err != nil {
		t.Fatal(err)
	}
	all := registry.All()
	if all[0].Version != 1 || all[1].Version != 2 {
		t.Fatalf("not ordered: %+v", all)
	}
	pending, err := registry.Pending(map[uint64]string{1: all[0].Checksum()})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Version != 2 {
		t.Fatalf("pending=%+v", pending)
	}
	if _, err := registry.Pending(map[uint64]string{1: "tampered"}); err == nil {
		t.Fatal("tampered migration accepted")
	}
}
