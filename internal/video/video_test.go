package video_test

import (
	"context"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
)

func TestVideoRequestValidation(t *testing.T) {
	r := video.VideoRequest{ProjectID: "p", Prompt: "a scene", DurationSeconds: 4, Mode: video.ModeTextToVideo}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := r
	bad.DurationSeconds = 0
	if err := bad.Validate(); err == nil {
		t.Fatal("expected duration validation error")
	}
}

func TestFakeProviderAsyncLifecycle(t *testing.T) {
	p := providers.NewFake(2)
	r := video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 4, Mode: video.ModeTextToVideo}
	job, err := p.CreateJob(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.GetJob(context.Background(), job.ProviderJobID)
	if err != nil || first.State != video.StateProcessing {
		t.Fatalf("first poll: %#v %v", first, err)
	}
	second, err := p.GetJob(context.Background(), job.ProviderJobID)
	if err != nil || second.State != video.StateCompleted {
		t.Fatalf("second poll: %#v %v", second, err)
	}
	if len(second.Outputs) != 1 {
		t.Fatalf("outputs=%d", len(second.Outputs))
	}
}
