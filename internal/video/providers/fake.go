package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

type Fake struct {
	mu              sync.Mutex
	jobs            map[string]int
	NextID          int
	PollsToComplete int
	Cancelled       map[string]bool
	// ActualCostUSD, when non-nil, is reported as the deterministic
	// provider actual on every completed GetJob response (explicit field
	// plus the legacy Usage["actual_cost_usd"] entry). Nil means the
	// provider reports no actual cost, so jobs stay estimate-based and
	// explicitly unreconciled. Additive test hook; defaults to nil to
	// preserve existing behavior.
	ActualCostUSD *float64
}

func NewFake(polls int) *Fake {
	if polls < 1 {
		polls = 1
	}
	return &Fake{jobs: map[string]int{}, PollsToComplete: polls, Cancelled: map[string]bool{}}
}
func (f *Fake) ID() string { return "fake" }
func (f *Fake) Capabilities(context.Context) (video.Capabilities, error) {
	return video.Capabilities{
		Modes: []video.Mode{video.ModeTextToVideo, video.ModeImageToVideo}, Models: []string{"fake-video-1"}, MaxDurationSeconds: 60, AspectRatios: []video.AspectRatio{video.Aspect16x9, video.Aspect9x16, video.Aspect1x1},
		// The fake is a non-production test double: it must never be
		// presented to operators or counted as a production provider.
		ProviderKind: video.ProviderKindTest, ProductionReady: false,
		SupportsAudio: true, SupportsCancel: true,
		// SupportsWebhooks is false: the fake accepts no webhook
		// registration and cannot parse webhook payloads, so claiming
		// support would let callback URLs be accepted and then never
		// fire.
		SupportsWebhooks: false,
	}, nil
}
func (f *Fake) ValidateRequest(r video.VideoRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	// The fake honors its advertised capability surface: modes outside
	// Capabilities.Modes are rejected instead of silently accepted, so
	// the contract fixture can prove capability/validation coherence.
	n := r.Normalized()
	supported := map[video.Mode]bool{video.ModeTextToVideo: true, video.ModeImageToVideo: true}
	if !supported[n.Mode] {
		return fmt.Errorf("fake provider supports text_to_video and image_to_video only, not %q", n.Mode)
	}
	// The fake declares no reference-frame or webhook support, so it
	// rejects such requests instead of silently dropping them.
	if len(n.ReferenceAssets) > 0 || n.FirstFrameAsset != nil || n.LastFrameAsset != nil ||
		len(n.CharacterReferences) > 0 || len(n.LocationReferences) > 0 || len(n.StyleReferences) > 0 ||
		n.ContinuityContext != nil {
		return fmt.Errorf("fake provider does not support reference/first/last-frame inputs")
	}
	if strings.TrimSpace(n.CallbackURL) != "" {
		return fmt.Errorf("fake provider does not support webhooks/callbacks")
	}
	return nil
}
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
		return video.ProviderJobStatus{}, fmt.Errorf("unknown fake job")
	}
	if f.Cancelled[id] {
		return video.ProviderJobStatus{ProviderJobID: id, State: video.StateCancelled}, nil
	}
	n++
	f.jobs[id] = n
	if n >= f.PollsToComplete {
		status := video.ProviderJobStatus{ProviderJobID: id, State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{ID: id + "-output", Kind: "development-placeholder", Filename: "fake-output.txt", ContentType: "text/plain"}}}
		if f.ActualCostUSD != nil {
			actual := *f.ActualCostUSD
			status.ActualCostUSD = &actual
			if status.Usage == nil {
				status.Usage = map[string]string{}
			}
			status.Usage["actual_cost_usd"] = strconv.FormatFloat(actual, 'f', -1, 64)
		}
		return status, nil
	}
	return video.ProviderJobStatus{ProviderJobID: id, State: video.StateProcessing, Progress: float64(n) / float64(f.PollsToComplete)}, nil
}
func (f *Fake) CancelJob(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jobs[id]; !ok {
		return fmt.Errorf("unknown fake job")
	}
	f.Cancelled[id] = true
	return nil
}
func (f *Fake) DownloadOutputs(ctx context.Context, status video.ProviderJobStatus, sink video.AssetSink) ([]video.Asset, error) {
	if sink == nil {
		return nil, fmt.Errorf("asset sink is not configured")
	}
	if len(status.Outputs) == 0 {
		return nil, fmt.Errorf("fake job has no outputs")
	}
	out := make([]video.Asset, 0, len(status.Outputs))
	for _, asset := range status.Outputs {
		if asset.Filename == "" {
			asset.Filename = "fake-output.txt"
		}
		asset.Kind = "development-placeholder"
		asset.ContentType = "text/plain"
		data := []byte("NexaRoute development fake-provider artifact. This is not a playable video.\n")
		saved, err := sink.Put(ctx, asset, data)
		if err != nil {
			return nil, err
		}
		out = append(out, saved)
	}
	return out, nil
}
func (f *Fake) RegisterWebhook(context.Context, video.WebhookRegistration) error {
	return errors.New("fake provider does not support webhooks")
}
func (f *Fake) VerifyWebhook(*http.Request) error {
	return errors.New("fake provider does not support webhooks")
}
func (f *Fake) ParseWebhook(*http.Request) (video.ProviderJobStatus, error) {
	return video.ProviderJobStatus{}, fmt.Errorf("fake webhook parser not configured")
}

var _ video.VideoProvider = (*Fake)(nil)
