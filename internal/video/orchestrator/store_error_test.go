package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// failingVideoStore simulates a backend outage at the job-store layer:
// every store operation fails with a backend-shaped error, the way the
// PostgreSQL store wraps a connection failure. It embeds JobStore so
// only the operations under test need overriding.
type failingVideoStore struct {
	queue.JobStore
	err error
}

func (f failingVideoStore) Get(context.Context, string) (video.VideoJob, error) {
	return video.VideoJob{}, f.err
}

func (f failingVideoStore) GetByIdempotency(context.Context, string) (video.VideoJob, error) {
	return video.VideoJob{}, f.err
}

func (f failingVideoStore) Create(context.Context, video.VideoJob) error { return f.err }

func (f failingVideoStore) Update(context.Context, video.VideoJob) error { return f.err }

func backendErr() error { return errors.New("video job store: get: dial tcp: connection refused") }

func TestCancelStoreFailureIsUnavailableNotNotFound(t *testing.T) {
	o := &Orchestrator{
		Store:     failingVideoStore{JobStore: queue.NewMemoryStore(), err: backendErr()},
		Queue:     queue.New(4),
		Providers: Registry{},
	}
	err := o.Cancel(context.Background(), "video-missing")
	if err == nil {
		t.Fatal("Cancel with a failing store: got nil error")
	}
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Cancel store failure: error %v does not wrap ErrStoreUnavailable", err)
	}
	if errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("Cancel store failure: error %v must not look like a missing job", err)
	}
}

func TestCancelMissingJobStillNotFound(t *testing.T) {
	o := &Orchestrator{
		Store:     queue.NewMemoryStore(),
		Queue:     queue.New(4),
		Providers: Registry{},
	}
	err := o.Cancel(context.Background(), "no-such-job")
	if !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("Cancel missing job: error %v, want queue.ErrNotFound", err)
	}
	if errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Cancel missing job: error %v must not wrap ErrStoreUnavailable", err)
	}
}

func TestCreateIdempotencyLookupFailureIsUnavailable(t *testing.T) {
	o := &Orchestrator{
		Store:     failingVideoStore{JobStore: queue.NewMemoryStore(), err: backendErr()},
		Queue:     queue.New(4),
		Providers: Registry{},
	}
	r := video.VideoRequest{
		ProjectID: "project", Prompt: "a scene", DurationSeconds: 2,
		Mode: video.ModeTextToVideo, IdempotencyKey: "idem-key",
	}
	_, err := o.Create(context.Background(), r)
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Create idempotency lookup failure: error %v, want ErrStoreUnavailable", err)
	}
}

func TestCreateStoreCreateFailureIsUnavailable(t *testing.T) {
	o := &Orchestrator{
		Store:     failingVideoStore{JobStore: queue.NewMemoryStore(), err: backendErr()},
		Queue:     queue.New(4),
		Providers: Registry{"fake": providers.NewFake(1)},
	}
	r := video.VideoRequest{
		ProjectID: "project", Prompt: "a scene", DurationSeconds: 2,
		Mode: video.ModeTextToVideo, ProviderPreference: "fake",
		ModelPreference: "fake-video-1",
	}
	_, err := o.Create(context.Background(), r)
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Create store failure: error %v, want ErrStoreUnavailable", err)
	}
}
