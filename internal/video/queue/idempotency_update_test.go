package queue

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func checkIdempotencyUpdate(t *testing.T, store JobStore) {
	t.Helper()
	ctx := context.Background()
	first := queueJob("first", "old-key")
	other := queueJob("other", "occupied")
	if err := store.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	first.IdempotencyKey = "occupied"
	if err := store.Update(ctx, first); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("duplicate key update: %v", err)
	}
	if got, err := store.GetByIdempotency(ctx, "old-key"); err != nil || got.JobID != "first" || got.IdempotencyKey != "old-key" {
		t.Fatalf("failed update changed original index: %+v %v", got, err)
	}
	first.IdempotencyKey = "new-key"
	if err := store.Update(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByIdempotency(ctx, "old-key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old key still indexed: %v", err)
	}
	if got, err := store.GetByIdempotency(ctx, "new-key"); err != nil || got.JobID != "first" {
		t.Fatalf("new key missing: %+v %v", got, err)
	}
	if err := store.Create(ctx, queueJob("replacement", "old-key")); err != nil {
		t.Fatalf("old key not reusable: %v", err)
	}
	if err := store.Delete(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByIdempotency(ctx, "new-key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted key still indexed: %v", err)
	}
}

func TestMemoryFileIdempotencyUpdate(t *testing.T) {
	t.Run("memory", func(t *testing.T) { checkIdempotencyUpdate(t, NewMemoryStore()) })
	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "jobs.json")
		s, err := NewFileStore(path)
		if err != nil {
			t.Fatal(err)
		}
		checkIdempotencyUpdate(t, s)
		reopened, err := NewFileStore(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if got, err := reopened.GetByIdempotency(context.Background(), "old-key"); err != nil || got.JobID != "replacement" {
			t.Fatalf("reopened index: %+v %v", got, err)
		}
	})
}

func TestFileIdempotencyUpdateRollback(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.json")
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	j := queueJob("a", "old")
	if err := s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	j.IdempotencyKey = "new"
	j.Progress = math.NaN() // JSON encoding fails before rename.
	if err := s.Update(ctx, j); err == nil {
		t.Fatal("invalid update succeeded")
	}
	for _, current := range []*FileStore{s, mustOpenJobStore(t, path)} {
		if got, err := current.GetByIdempotency(ctx, "old"); err != nil || got.JobID != "a" {
			t.Fatalf("old key lost: %+v %v", got, err)
		}
		if _, err := current.GetByIdempotency(ctx, "new"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("failed update retained new key: %v", err)
		}
	}
}

func mustOpenJobStore(t *testing.T, path string) *FileStore {
	t.Helper()
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMemoryFileIdempotencyAddAndRemove(t *testing.T) {
	factories := map[string]func(t *testing.T) JobStore{
		"memory": func(t *testing.T) JobStore { return NewMemoryStore() },
		"file":   func(t *testing.T) JobStore { return mustOpenJobStore(t, filepath.Join(t.TempDir(), "jobs.json")) },
	}
	for name, makeStore := range factories {
		t.Run(name, func(t *testing.T) {
			s := makeStore(t)
			ctx := context.Background()
			j := queueJob("a", "")
			if err := s.Create(ctx, j); err != nil {
				t.Fatal(err)
			}
			j.IdempotencyKey = "added"
			if err := s.Update(ctx, j); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetByIdempotency(ctx, "added"); err != nil {
				t.Fatal(err)
			}
			j.IdempotencyKey = ""
			if err := s.Update(ctx, j); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetByIdempotency(ctx, "added"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("removed key: %v", err)
			}
		})
	}
}

func TestFileIdempotencyRecoveryRebuildsStaleIndex(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.json")
	s := mustOpenJobStore(t, path)
	if err := s.Create(ctx, queueJob("a", "new")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state fileState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state.Idempotency = map[string]string{"stale": "a", "dangling": "missing"}
	data, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	reopened := mustOpenJobStore(t, path)
	for _, key := range []string{"stale", "dangling"} {
		if _, err := reopened.GetByIdempotency(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Errorf("stale key %s: %v", key, err)
		}
	}
	if got, err := reopened.GetByIdempotency(ctx, "new"); err != nil || got.JobID != "a" {
		t.Fatalf("authoritative key missing: %+v %v", got, err)
	}
}

func TestFileJobStoreRejectsNullDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := os.WriteFile(path, []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(path); err == nil {
		t.Fatal("null job store accepted")
	}
}
