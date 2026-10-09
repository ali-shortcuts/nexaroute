package providers

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

type Fake struct {
	mu              sync.Mutex
	jobs            map[string]int
	NextID          int
	PollsToComplete int
	Cancelled       map[string]bool
}

func NewFake(polls int) *Fake {
	if polls < 1 {
		polls = 1
	}
	return &Fake{jobs: map[string]int{}, PollsToComplete: polls, Cancelled: map[string]bool{}}
}
func (f *Fake) ID() string { return "fake" }
func (f *Fake) Capabilities(context.Context) (video.Capabilities, error) {
	return video.Capabilities{Modes: []video.Mode{video.ModeTextToVideo, video.ModeImageToVideo}, Models: []string{"fake-video-1"}, MaxDurationSeconds: 60, AspectRatios: []video.AspectRatio{video.Aspect16x9, video.Aspect9x16, video.Aspect1x1}, SupportsAudio: true, SupportsCancel: true, SupportsWebhooks: true}, nil
}
func (f *Fake) ValidateRequest(r video.VideoRequest) error { return r.Validate() }
func (f *Fake) EstimateCost(r video.VideoRequest) (video.CostEstimate, error) {
	return video.CostEstimate{Provider: f.ID(), Model: r.ModelPreference, Currency: "USD", EstimatedUSD: r.DurationSeconds * 0.01, UpperBoundUSD: r.DurationSeconds * 0.01, PriceKnown: true, BillableSeconds: r.DurationSeconds}, nil
}
func (f *Fake) CreateJob(_ context.Context, r video.VideoRequest) (video.ProviderJob, error) {
	if err := f.ValidateRequest(r); err != nil {
		return video.ProviderJob{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.NextID++
	id := fmt.Sprintf("fake-%d", f.NextID)
	f.jobs[id] = 0
	return video.ProviderJob{ProviderJobID: id, Provider: f.ID(), Model: r.ModelPreference, Accepted: true}, nil
}
func (f *Fake) GetJob(_ context.Context, id string) (video.ProviderJobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.jobs[id]
	if !ok {
		return video.ProviderJobStatus{}, fmt.Errorf("unknown fake job %s", id)
	}
	if f.Cancelled[id] {
		return video.ProviderJobStatus{ProviderJobID: id, State: video.StateCancelled}, nil
	}
	n++
	f.jobs[id] = n
	if n >= f.PollsToComplete {
		return video.ProviderJobStatus{ProviderJobID: id, State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{ID: id + "-output", Kind: "video", Filename: "output.mp4", ContentType: "video/mp4"}}}, nil
	}
	return video.ProviderJobStatus{ProviderJobID: id, State: video.StateProcessing, Progress: float64(n) / float64(f.PollsToComplete)}, nil
}
func (f *Fake) CancelJob(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jobs[id]; !ok {
		return fmt.Errorf("unknown fake job %s", id)
	}
	f.Cancelled[id] = true
	return nil
}
func (f *Fake) DownloadOutputs(_ context.Context, s video.ProviderJobStatus, _ video.AssetSink) ([]video.Asset, error) {
	return s.Outputs, nil
}
func (f *Fake) RegisterWebhook(context.Context, video.WebhookRegistration) error { return nil }
func (f *Fake) VerifyWebhook(*http.Request) error                                { return nil }
func (f *Fake) ParseWebhook(*http.Request) (video.ProviderJobStatus, error) {
	return video.ProviderJobStatus{}, fmt.Errorf("fake webhook parser not configured")
}

var _ video.VideoProvider = (*Fake)(nil)
var _ = time.Now
