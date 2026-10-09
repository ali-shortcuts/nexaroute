package controlplane

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryStoreDeleteRequiresCurrentRevisionAndHonorsAvailability(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Delete(ctx, "missing", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing delete err=%v", err)
	}
	if _, err := store.Put(ctx, "state", 0, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "state", 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale delete err=%v", err)
	}
	store.SetUnavailable(true)
	if err := store.Delete(ctx, "state", 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable delete err=%v", err)
	}
	store.SetUnavailable(false)
	if err := store.Delete(ctx, "state", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "state"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted snapshot still available: %v", err)
	}
}

func TestMemoryStoreRejectsInvalidWritesAndCanceledContexts(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		want error
	}{
		{"empty namespace", ErrNotFound},
		{"empty payload", ErrNotFound},
	} {
		var namespace string
		payload := []byte("payload")
		if tc.name == "empty namespace" {
			namespace = ""
		} else {
			namespace, payload = "config", nil
		}
		if _, err := store.Put(ctx, namespace, 0, payload); !errors.Is(err, tc.want) {
			t.Errorf("%s err=%v", tc.name, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Get(canceled, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Get err=%v", err)
	}
	if _, err := store.Put(canceled, "x", 0, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Put err=%v", err)
	}
	if err := store.Delete(canceled, "x", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Delete err=%v", err)
	}
}

func TestEqualPayloadAndAvailabilityValidation(t *testing.T) {
	a := Snapshot{Revision: 2, Payload: []byte("same")}
	if !EqualPayload(a, Snapshot{Revision: 2, Payload: []byte("same")}) {
		t.Fatal("equal snapshots did not compare equal")
	}
	if EqualPayload(a, Snapshot{Revision: 3, Payload: []byte("same")}) || EqualPayload(a, Snapshot{Revision: 2, Payload: []byte("different")}) {
		t.Fatal("revision or payload difference was ignored")
	}
	valid := Availability{Config: FailOpen, Identity: FailClosed, Budget: LastKnownGood, RateLimit: FailOpen}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	valid.Budget = "invalid"
	if err := valid.Validate(); err == nil {
		t.Fatal("invalid failure mode accepted")
	}
}

func TestManagerLastKnownReturnsCopyAndUnavailableWithoutCache(t *testing.T) {
	store := NewMemoryStore()
	manager, err := NewManager(store, LastKnownGood)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.LastKnown("config"); ok {
		t.Fatal("unexpected initial last-known value")
	}
	if _, err := manager.Save(context.Background(), "config", 0, []byte("durable")); err != nil {
		t.Fatal(err)
	}
	first, ok := manager.LastKnown("config")
	if !ok {
		t.Fatal("saved value not cached")
	}
	first.Payload[0] = 'X'
	second, ok := manager.LastKnown("config")
	if !ok || string(second.Payload) != "durable" {
		t.Fatalf("last-known snapshot was not isolated: %+v", second)
	}
}

func TestMigrationsRejectNilDatabase(t *testing.T) {
	registry, err := NewMigrationRegistry(Migration{Version: 1, Name: "initial", Up: "SELECT 1"})
	if err != nil {
		t.Fatal(err)
	}
	if err = ApplyMigrations(context.Background(), nil, registry); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil database err=%v", err)
	}
	if _, err = NewMigrationRegistry(Migration{Version: 0, Name: "bad", Up: "SELECT 1"}); err == nil {
		t.Fatal("zero-version migration accepted")
	}
	if _, err = NewMigrationRegistry(Migration{Version: 1, Name: "", Up: "SELECT 1"}); err == nil {
		t.Fatal("nameless migration accepted")
	}
	if _, err = NewMigrationRegistry(Migration{Version: 1, Name: "one", Up: "SELECT 1"}, Migration{Version: 1, Name: "duplicate", Up: "SELECT 2"}); err == nil {
		t.Fatal("duplicate migration version accepted")
	}
}

func TestMigrationRegistryChecksumAndCopies(t *testing.T) {
	registry, err := NewMigrationRegistry(Migration{Version: 2, Name: "two", Up: "CREATE TABLE t2"}, Migration{Version: 1, Name: "one", Up: "CREATE TABLE t1"})
	if err != nil {
		t.Fatal(err)
	}
	all := registry.All()
	all[0].Name = "caller mutation"
	if got := registry.All()[0].Name; got != "one" {
		t.Fatalf("All returned mutable registry storage: %q", got)
	}
	pending, err := registry.Pending(nil)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	applied := map[uint64]string{1: registry.All()[0].Checksum(), 2: "bad-checksum"}
	if _, err := registry.Pending(applied); err == nil {
		t.Fatal("modified applied checksum accepted")
	}
}
