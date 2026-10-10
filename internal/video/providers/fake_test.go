package providers

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

type recordingSink struct {
	assets   []video.Asset
	contents [][]byte
	err      error
}

func (s *recordingSink) Put(_ context.Context, asset video.Asset, content []byte) (video.Asset, error) {
	if s.err != nil {
		return video.Asset{}, s.err
	}
	s.assets = append(s.assets, asset)
	s.contents = append(s.contents, append([]byte(nil), content...))
	asset.URI = "memory://" + asset.ID
	return asset, nil
}

func validProviderRequest() video.VideoRequest {
	return video.VideoRequest{
		ProjectID:       "project-1",
		Prompt:          "a test scene",
		DurationSeconds: 12,
		Mode:            video.ModeTextToVideo,
		ModelPreference: "preferred-model",
	}
}

func TestFakeRequestValidationAndEstimate(t *testing.T) {
	provider := NewFake(0)
	if provider.PollsToComplete != 1 {
		t.Fatalf("polls=%d, want minimum of one", provider.PollsToComplete)
	}
	if provider.ID() != "fake" {
		t.Fatalf("ID=%q, want fake", provider.ID())
	}

	request := validProviderRequest()
	if err := provider.ValidateRequest(request); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	cost, err := provider.EstimateCost(request)
	if err != nil {
		t.Fatalf("estimate cost: %v", err)
	}
	if cost.Provider != "fake" || cost.Model != request.ModelPreference || cost.Currency != "USD" || cost.EstimatedUSD != 0.12 || cost.UpperBoundUSD != 0.12 || cost.BillableSeconds != 12 || !cost.PriceKnown {
		t.Fatalf("unexpected estimate: %+v", cost)
	}

	invalid := request
	invalid.ProjectID = ""
	if err := provider.ValidateRequest(invalid); !errors.Is(err, video.ErrInvalidRequest) {
		t.Fatalf("invalid request error=%v, want video.ErrInvalidRequest", err)
	}
	if _, err := provider.CreateJob(context.Background(), invalid); !errors.Is(err, video.ErrInvalidRequest) {
		t.Fatalf("CreateJob invalid request error=%v, want validation error", err)
	}
	if provider.NextID != 0 {
		t.Fatalf("invalid request consumed job ID: %d", provider.NextID)
	}
}

func TestFakeCapabilitiesAndJobLifecycle(t *testing.T) {
	provider := NewFake(2)
	capabilities, err := provider.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if len(capabilities.Modes) != 2 || capabilities.Modes[0] != video.ModeTextToVideo || capabilities.Modes[1] != video.ModeImageToVideo || len(capabilities.Models) != 1 || capabilities.Models[0] != "fake-video-1" || capabilities.MaxDurationSeconds != 60 || len(capabilities.AspectRatios) != 3 || !capabilities.SupportsAudio || !capabilities.SupportsCancel || !capabilities.SupportsWebhooks {
		t.Fatalf("unexpected capabilities: %+v", capabilities)
	}

	request := validProviderRequest()
	job, err := provider.CreateJob(context.Background(), request)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.ProviderJobID != "fake-1" || job.Provider != "fake" || job.Model != request.ModelPreference || !job.Accepted {
		t.Fatalf("unexpected accepted job: %+v", job)
	}
	secondJob, err := provider.CreateJob(context.Background(), request)
	if err != nil {
		t.Fatalf("second CreateJob: %v", err)
	}
	if secondJob.ProviderJobID != "fake-2" {
		t.Fatalf("second job ID=%q, want fake-2", secondJob.ProviderJobID)
	}

	if _, err := provider.GetJob(context.Background(), "missing"); err == nil {
		t.Fatal("unknown GetJob unexpectedly succeeded")
	}
	processing, err := provider.GetJob(context.Background(), job.ProviderJobID)
	if err != nil {
		t.Fatalf("first GetJob: %v", err)
	}
	if processing.State != video.StateProcessing || processing.Progress != 0.5 {
		t.Fatalf("unexpected processing status: %+v", processing)
	}
	completed, err := provider.GetJob(context.Background(), job.ProviderJobID)
	if err != nil {
		t.Fatalf("second GetJob: %v", err)
	}
	if completed.State != video.StateCompleted || completed.Progress != 1 || len(completed.Outputs) != 1 || completed.Outputs[0].ID != "fake-1-output" || completed.Outputs[0].Kind != "development-placeholder" || completed.Outputs[0].ContentType != "text/plain" {
		t.Fatalf("unexpected completed status: %+v", completed)
	}
}

func TestFakeCancellationAndOutputErrors(t *testing.T) {
	provider := NewFake(3)
	job, err := provider.CreateJob(context.Background(), validProviderRequest())
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := provider.CancelJob(context.Background(), "unknown"); err == nil {
		t.Fatal("unknown CancelJob unexpectedly succeeded")
	}
	if err := provider.CancelJob(context.Background(), job.ProviderJobID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	cancelled, err := provider.GetJob(context.Background(), job.ProviderJobID)
	if err != nil {
		t.Fatalf("GetJob after cancellation: %v", err)
	}
	if cancelled.State != video.StateCancelled || cancelled.ProviderJobID != job.ProviderJobID || cancelled.Progress != 0 {
		t.Fatalf("unexpected cancelled status: %+v", cancelled)
	}

	if _, err := provider.DownloadOutputs(context.Background(), video.ProviderJobStatus{}, nil); err == nil {
		t.Fatal("nil sink unexpectedly accepted")
	}
	if _, err := provider.DownloadOutputs(context.Background(), video.ProviderJobStatus{}, &recordingSink{}); err == nil {
		t.Fatal("status without outputs unexpectedly accepted")
	}

	sink := &recordingSink{}
	status := video.ProviderJobStatus{Outputs: []video.Asset{{ID: "one"}, {ID: "two", Filename: "kept.txt", Kind: "old", ContentType: "old/type"}}}
	outputs, err := provider.DownloadOutputs(context.Background(), status, sink)
	if err != nil {
		t.Fatalf("DownloadOutputs: %v", err)
	}
	if len(outputs) != 2 || len(sink.contents) != 2 || string(sink.contents[0]) != "NexaRoute development fake-provider artifact. This is not a playable video.\n" || outputs[0].URI != "memory://one" || outputs[0].Filename != "fake-output.txt" || outputs[1].Filename != "kept.txt" || outputs[1].Kind != "development-placeholder" || outputs[1].ContentType != "text/plain" {
		t.Fatalf("unexpected downloaded outputs: outputs=%+v sink=%+v", outputs, sink)
	}

	sentinel := errors.New("sink failed")
	failedSink := &recordingSink{err: sentinel}
	if _, err := provider.DownloadOutputs(context.Background(), status, failedSink); !errors.Is(err, sentinel) {
		t.Fatalf("sink error=%v, want sentinel", err)
	}
}

func TestFakeWebhookSurface(t *testing.T) {
	provider := NewFake(1)
	if err := provider.RegisterWebhook(context.Background(), video.WebhookRegistration{CallbackURL: "https://example.test/callback"}); err != nil {
		t.Fatalf("RegisterWebhook: %v", err)
	}
	if err := provider.VerifyWebhook(httptest.NewRequest("POST", "https://example.test/webhook", nil)); err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if _, err := provider.ParseWebhook(httptest.NewRequest("POST", "https://example.test/webhook", nil)); err == nil {
		t.Fatal("ParseWebhook unexpectedly succeeded")
	}
}
