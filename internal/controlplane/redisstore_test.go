package controlplane

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRedisHash emulates the adapter-visible result of the atomic Lua
// scripts. Real Redis integration tests exercise the actual scripts.
type fakeRedisHash struct {
	mu        sync.Mutex
	data      map[string]map[string]string
	err       error
	evalCalls int
}

func newFakeRedisHash() *fakeRedisHash {
	return &fakeRedisHash{data: make(map[string]map[string]string)}
}

func (f *fakeRedisHash) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]string, len(f.data[key]))
	for k, v := range f.data[key] {
		out[k] = v
	}
	return out, nil
}

func (f *fakeRedisHash) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evalCalls++
	if f.err != nil {
		return nil, f.err
	}
	if len(keys) != 1 {
		return nil, errors.New("expected exactly one key")
	}
	key := keys[0]
	str := func(i int) string { value, _ := args[i].(string); return value }
	switch script {
	case redisPutScript:
		fields, exists := f.data[key]
		current, schema := "0", str(1)
		if exists {
			snapshot, err := parseRedisSnapshot(fields)
			if err != nil {
				return int64(-2), nil
			}
			current = strconv.FormatUint(snapshot.Revision, 10)
			schema = strconv.Itoa(snapshot.Schema)
		}
		if current != str(0) {
			return int64(0), nil
		}
		revision, _ := strconv.ParseUint(current, 10, 64)
		if revision == math.MaxUint64 {
			return int64(-3), nil
		}
		next := strconv.FormatUint(revision+1, 10)
		f.data[key] = map[string]string{
			"revision":   next,
			"schema":     schema,
			"payload":    str(2),
			"updated_at": str(3),
		}
		return []any{int64(1), schema, next}, nil
	case redisDeleteScript:
		fields, exists := f.data[key]
		if !exists {
			return int64(-1), nil
		}
		snapshot, err := parseRedisSnapshot(fields)
		if err != nil {
			// Mirror the script's repair path: a malformed record
			// is deleted and reported with status 2.
			delete(f.data, key)
			return int64(2), nil
		}
		if strconv.FormatUint(snapshot.Revision, 10) != str(0) {
			return int64(0), nil
		}
		delete(f.data, key)
		return int64(1), nil
	default:
		return nil, fmt.Errorf("unknown script")
	}
}

func (f *fakeRedisHash) putRaw(key string, fields map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copyFields := make(map[string]string, len(fields))
	for k, v := range fields {
		copyFields[k] = v
	}
	f.data[key] = copyFields
}

func TestRedisStoreConformanceAgainstFake(t *testing.T) {
	ctx := context.Background()
	fake := newFakeRedisHash()
	store, err := NewRedisStore(fake)
	if err != nil {
		t.Fatal(err)
	}
	const ns = "gateway/runtime"
	if _, err := store.Get(ctx, ns); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
	if err := store.Delete(ctx, ns, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing: %v, want ErrNotFound", err)
	}

	payload := []byte{0, 255, 17, 0}
	created, err := store.Put(ctx, ns, 0, payload)
	if err != nil || created.Revision != 1 || created.Schema != 1 {
		t.Fatalf("create: %+v %v", created, err)
	}
	payload[0] = 77
	created.Payload[1] = 66
	got, err := store.Get(ctx, ns)
	if err != nil || got.Revision != 1 || string(got.Payload) != string([]byte{0, 255, 17, 0}) {
		t.Fatalf("Get after create must preserve a defensive binary copy: %+v %v", got, err)
	}
	got.Payload[0] = 99
	gotAgain, err := store.Get(ctx, ns)
	if err != nil || gotAgain.Payload[0] != 0 {
		t.Fatalf("Get returned a shared payload buffer: %+v %v", gotAgain, err)
	}

	if _, err := store.Put(ctx, ns, 0, []byte("stale")); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-create: %v, want ErrConflict", err)
	}
	updated, err := store.Put(ctx, ns, 1, []byte(`{"v":2}`))
	if err != nil || updated.Revision != 2 {
		t.Fatalf("update: %+v %v", updated, err)
	}
	if _, err := store.Put(ctx, ns, 1, []byte("stale")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update: %v, want ErrConflict", err)
	}
	if err := store.Delete(ctx, ns, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale delete: %v, want ErrConflict", err)
	}
	if err := store.Delete(ctx, ns, 2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, ns); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if _, err := store.Put(ctx, "", 0, []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty namespace: %v", err)
	}
	if _, err := store.Get(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty namespace Get: %v", err)
	}
	if err := store.Delete(ctx, "", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty namespace Delete: %v", err)
	}
	if _, err := store.Put(ctx, ns, 0, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty payload: %v", err)
	}
	if _, err := NewRedisStore(nil); err == nil {
		t.Fatal("nil client accepted")
	}
}

func TestRedisStoreMalformedRecordsFailClosedExceptDeleteRepair(t *testing.T) {
	ctx := context.Background()
	fake := newFakeRedisHash()
	store, _ := NewRedisStore(fake)
	valid := map[string]string{
		"revision": "1", "schema": "1", "payload": "payload",
		"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	cases := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{name: "missing revision", mutate: func(fields map[string]string) { delete(fields, "revision") }},
		{name: "noncanonical revision", mutate: func(fields map[string]string) { fields["revision"] = "01" }},
		{name: "revision over uint64", mutate: func(fields map[string]string) { fields["revision"] = "18446744073709551616" }},
		{name: "invalid schema", mutate: func(fields map[string]string) { fields["schema"] = "0" }},
		{name: "empty payload", mutate: func(fields map[string]string) { fields["payload"] = "" }},
		{name: "invalid timestamp", mutate: func(fields map[string]string) { fields["updated_at"] = "yesterday" }},
		{name: "invalid calendar timestamp", mutate: func(fields map[string]string) { fields["updated_at"] = "2026-02-30T12:00:00Z" }},
		{name: "extra field", mutate: func(fields map[string]string) { fields["unexpected"] = "value" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := make(map[string]string, len(valid)+1)
			for k, v := range valid {
				fields[k] = v
			}
			tc.mutate(fields)
			key := redisSnapshotPrefix + "broken/" + tc.name
			fake.putRaw(key, fields)
			ns := strings.TrimPrefix(key, redisSnapshotPrefix)
			if _, err := store.Get(ctx, ns); !errors.Is(err, ErrMalformedSnapshot) {
				t.Fatalf("Get malformed: %v, want ErrMalformedSnapshot", err)
			}
			if _, err := store.Put(ctx, ns, 0, []byte("replace")); !errors.Is(err, ErrMalformedSnapshot) {
				t.Fatalf("Put malformed: %v, want ErrMalformedSnapshot", err)
			}
			// Delete is the namespace-repair path (audit 2026-10-15):
			// a malformed record can never be read or overwritten,
			// and before this exception it could never be removed
			// either, bricking the namespace until manual Redis
			// surgery. Delete removes it and reports ErrNotFound
			// (no valid snapshot existed).
			if err := store.Delete(ctx, ns, 1); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Delete malformed: %v, want ErrNotFound (repair)", err)
			}
			if _, err := store.Get(ctx, ns); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get after repair delete: %v, want ErrNotFound (record removed)", err)
			}
		})
	}
}

func TestRedisStoreRevisionUint64Boundary(t *testing.T) {
	ctx := context.Background()
	fake := newFakeRedisHash()
	store, _ := NewRedisStore(fake)
	if _, err := store.Put(ctx, "overflow", math.MaxUint64, []byte("x")); !errors.Is(err, ErrInvalidRevision) {
		t.Fatalf("Put at MaxUint64: %v, want ErrInvalidRevision", err)
	}
	if fake.evalCalls != 0 {
		t.Fatal("overflowing Put should fail before reaching Redis")
	}

	maxMinusOne := strconv.FormatUint(math.MaxUint64-1, 10)
	fake.putRaw(redisSnapshotPrefix+"boundary", map[string]string{
		"revision": maxMinusOne, "schema": "1", "payload": "before",
		"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
	updated, err := store.Put(ctx, "boundary", math.MaxUint64-1, []byte("at-max"))
	if err != nil || updated.Revision != math.MaxUint64 {
		t.Fatalf("update to MaxUint64: %+v %v", updated, err)
	}
	if _, err := store.Put(ctx, "boundary", math.MaxUint64, []byte("overflow")); !errors.Is(err, ErrInvalidRevision) {
		t.Fatalf("update past MaxUint64: %v, want ErrInvalidRevision", err)
	}
	if err := store.Delete(ctx, "boundary", math.MaxUint64); err != nil {
		t.Fatalf("delete at MaxUint64: %v", err)
	}
}

func TestRedisStoreRevisionComparisonAboveLuaExactIntegerRange(t *testing.T) {
	ctx := context.Background()
	fake := newFakeRedisHash()
	store, _ := NewRedisStore(fake)
	const current = uint64(9007199254740993)
	fake.putRaw(redisSnapshotPrefix+"precision", map[string]string{
		"revision": strconv.FormatUint(current, 10), "schema": "1", "payload": "x",
		"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
	if _, err := store.Put(ctx, "precision", current-1, []byte("stale")); !errors.Is(err, ErrConflict) {
		t.Fatalf("adjacent stale revision: %v, want ErrConflict", err)
	}
	updated, err := store.Put(ctx, "precision", current, []byte("next"))
	if err != nil || updated.Revision != current+1 {
		t.Fatalf("exact decimal revision increment: %+v %v", updated, err)
	}
}

func TestRedisStoreCancellationAndBackendFailures(t *testing.T) {
	fake := newFakeRedisHash()
	store, _ := NewRedisStore(fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Get(ctx, "cancel"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Get: %v", err)
	}
	if _, err := store.Put(ctx, "cancel", 0, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Put: %v", err)
	}
	if err := store.Delete(ctx, "cancel", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Delete: %v", err)
	}
	if fake.evalCalls != 0 {
		t.Fatal("canceled mutations should not reach Redis")
	}

	fake.mu.Lock()
	fake.err = errors.New("connection refused")
	fake.mu.Unlock()
	if _, err := store.Get(context.Background(), "cancel"); err == nil {
		t.Fatal("backend Get error lost")
	}
	if _, err := store.Put(context.Background(), "cancel", 0, []byte("x")); err == nil {
		t.Fatal("backend Put error lost")
	}
	if err := store.Delete(context.Background(), "cancel", 1); err == nil {
		t.Fatal("backend Delete error lost")
	}
}

func TestRedisStoreConcurrentFirstWriteRace(t *testing.T) {
	fake := newFakeRedisHash()
	store, _ := NewRedisStore(fake)
	const writers = 16
	results := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = store.Put(context.Background(), "race", 0, []byte(fmt.Sprintf("%d", i)))
		}(i)
	}
	wg.Wait()
	successes, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected race error: %v", err)
		}
	}
	if successes != 1 || conflicts != writers-1 {
		t.Fatalf("successes=%d conflicts=%d, want 1 and %d", successes, conflicts, writers-1)
	}
}
