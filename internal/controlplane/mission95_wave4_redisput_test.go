package controlplane

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

type wave4FakeRedis struct {
	result any
	err    error
	key    string
	args   []any
}

func (f *wave4FakeRedis) HGetAll(_ context.Context, _ string) (map[string]string, error) {
	return nil, nil
}

func (f *wave4FakeRedis) Eval(_ context.Context, _ string, keys []string, args ...any) (any, error) {
	if len(keys) > 0 {
		f.key = keys[0]
	}
	f.args = args
	return f.result, f.err
}

func wave4RedisStore(t *testing.T, result any, err error) (*RedisStore, *wave4FakeRedis) {
	t.Helper()
	fake := &wave4FakeRedis{result: result, err: err}
	store, err := NewRedisStore(fake)
	if err != nil {
		t.Fatal(err)
	}
	return store, fake
}

// A backend Eval failure is wrapped with the write context and the cause.
func TestWave4RedisPutEvalErrorWrapped(t *testing.T) {
	boom := errors.New("redis down")
	store, _ := wave4RedisStore(t, nil, boom)
	_, err := store.Put(context.Background(), "ns", 5, []byte("payload"))
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the wrapped backend error", err)
	}
}

// Every integer script status maps to its documented sentinel or error.
func TestWave4RedisPutScriptStatuses(t *testing.T) {
	cases := []struct {
		status int64
		want   error
		substr string
	}{
		{0, ErrConflict, ""},
		{-2, ErrMalformedSnapshot, ""},
		{-3, ErrInvalidRevision, ""},
		{42, nil, "unexpected script status 42"},
	}
	for _, tc := range cases {
		store, _ := wave4RedisStore(t, tc.status, nil)
		_, err := store.Put(context.Background(), "ns", 5, []byte("payload"))
		if tc.want != nil {
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: err = %v, want %v", tc.status, err, tc.want)
			}
		} else if err == nil || !containsWave4CP(err.Error(), tc.substr) {
			t.Fatalf("status %d: err = %v, want %q", tc.status, err, tc.substr)
		}
	}
}

// Malformed script results are rejected with result-shape errors, never
// accepted as a snapshot.
func TestWave4RedisPutMalformedResults(t *testing.T) {
	cases := []struct {
		name   string
		result any
		substr string
	}{
		{"scalar", "garbage", "unexpected script result string"},
		{"short", []any{int64(1), "1"}, "unexpected script result"},
		{"bad-flag", []any{int64(0), "1", "6"}, "unexpected script result"},
		{"flag-type", []any{"1", "1", "6"}, "unexpected script result"},
		{"schema-type", []any{int64(1), int64(1), "6"}, "unexpected script result fields"},
		{"revision-type", []any{int64(1), "1", int64(6)}, "unexpected script result fields"},
		{"schema-zero", []any{int64(1), "0", "6"}, "invalid script result fields"},
		{"schema-nonnumeric", []any{int64(1), "abc", "6"}, "invalid script result fields"},
		{"revision-mismatch", []any{int64(1), "1", "7"}, "invalid script result fields"},
		{"revision-nonnumeric", []any{int64(1), "1", "x"}, "invalid script result fields"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := wave4RedisStore(t, tc.result, nil)
			if _, err := store.Put(context.Background(), "ns", 5, []byte("payload")); err == nil || !containsWave4CP(err.Error(), tc.substr) {
				t.Fatalf("err = %v, want %q", err, tc.substr)
			}
		})
	}
}

// A well-formed result yields the snapshot: revision is expected+1, the
// payload is a copy (mutating the input afterwards must not corrupt it),
// and the key/args sent to Redis are the namespaced key and revisions.
func TestWave4RedisPutSuccess(t *testing.T) {
	payload := []byte("payload")
	store, fake := wave4RedisStore(t, []any{int64(1), "1", "6"}, nil)
	snap, err := store.Put(context.Background(), "ns", 5, payload)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Revision != 6 || snap.Schema != 1 || !bytes.Equal(snap.Payload, payload) {
		t.Fatalf("snap = %+v, want revision 6 schema 1 payload", snap)
	}
	if snap.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt must be set")
	}
	payload[0] = 'X'
	if snap.Payload[0] == 'X' {
		t.Fatal("snapshot payload must be a copy of the input")
	}
	if fake.key != "nexaroute:control:snapshot:ns" {
		t.Fatalf("key = %q, want the namespaced key", fake.key)
	}
	if len(fake.args) < 1 || fake.args[0] != "5" {
		t.Fatalf("args = %v, want the expected revision first", fake.args)
	}
}

// Byte-slice result fields (as some Redis clients return) are accepted.
func TestWave4RedisPutByteSliceFields(t *testing.T) {
	store, _ := wave4RedisStore(t, []any{int64(1), []byte("1"), []byte("6")}, nil)
	snap, err := store.Put(context.Background(), "ns", 5, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Revision != 6 || snap.Schema != 1 {
		t.Fatalf("snap = %+v, want revision 6 schema 1", snap)
	}
}
