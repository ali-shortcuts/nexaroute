package queue

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// json.Marshal rejects non-finite floats, so a job carrying NaN
// progress cannot be encoded. Create must surface the encode failure
// without writing a row, and Update must surface it without changing
// the stored job.
func TestPostgresStoreFakeEncodeFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("create encode failure writes nothing", func(t *testing.T) {
		store, _ := fakeQueueStore(t, &fakeQueueDB{})
		bad := queueJob("j1", "k1")
		bad.Progress = math.NaN()
		err := store.Create(ctx, bad)
		if err == nil || !strings.Contains(err.Error(), "encode job") {
			t.Fatalf("Create NaN err = %v, want an \"encode job\" error", err)
		}
		if _, err := store.Get(ctx, "j1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get after failed Create = %v, want %v", err, ErrNotFound)
		}
	})

	t.Run("update encode failure keeps stored job", func(t *testing.T) {
		store, _ := fakeQueueStore(t, &fakeQueueDB{})
		original := queueJob("j1", "k1")
		if err := store.Create(ctx, original); err != nil {
			t.Fatalf("Create: %v", err)
		}
		updated := original
		updated.State = video.StateProcessing
		updated.Progress = math.NaN()
		err := store.Update(ctx, updated)
		if err == nil || !strings.Contains(err.Error(), "encode job") {
			t.Fatalf("Update NaN err = %v, want an \"encode job\" error", err)
		}
		got, err := store.Get(ctx, "j1")
		if err != nil {
			t.Fatal(err)
		}
		if got.State == video.StateProcessing || got.Progress != 0 {
			t.Fatalf("stored job after failed Update = state %q progress %v, want unchanged",
				got.State, got.Progress)
		}
	})
}

// A failure while iterating list rows must surface as a list-rows
// error instead of silently returning the rows scanned so far.
func TestPostgresStoreFakeListRowsFailure(t *testing.T) {
	ctx := context.Background()
	store, _ := fakeQueueStore(t, &fakeQueueDB{failListRows: true})
	if err := store.Create(ctx, queueJob("j1", "k1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	list, err := store.List(ctx)
	if err == nil || !strings.Contains(err.Error(), "list rows") {
		t.Fatalf("List err = %v, list = %v, want a \"list rows\" error", err, list)
	}
	if list != nil {
		t.Fatalf("List = %v, want nil on rows failure", list)
	}
}
