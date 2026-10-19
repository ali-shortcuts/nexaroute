// Package providers: adapter contract conformance fixtures.
//
// Phase 6 (issue #398) requires an explicit adapter contract plus
// conformance fixtures for every supported provider: auth, request
// encoding, async submit/poll, progress, cancellation semantics, output
// download, errors, idempotency and provider job IDs. This file is the
// fixture harness: every registered provider runs the same assertions,
// so a capability a provider does not declare can never be silently
// accepted — ValidateRequest must reject it, and the capability surface
// must stay internally coherent (for example a provider that does not
// claim webhook support must fail RegisterWebhook instead of pretending).
package providers

import (
	"context"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
)

// contractNarrator is a no-op Piper stand-in for capability assertions.
type contractNarrator struct{}

func (contractNarrator) Synthesize(context.Context, string, string) error { return nil }

type contractProvider struct {
	name string
	id   string
	// build constructs the provider under test. It must not require
	// network access or external services.
	build func(t *testing.T) video.VideoProvider
	// valid returns a minimal request the provider must accept.
	valid func() video.VideoRequest
}

func contractProviders(t *testing.T) []contractProvider {
	t.Helper()
	return []contractProvider{
		{
			name: "fake",
			id:   "fake",
			build: func(t *testing.T) video.VideoProvider {
				t.Helper()
				return NewFake(1)
			},
			valid: func() video.VideoRequest {
				return video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5, Mode: video.ModeTextToVideo}
			},
		},
		{
			name: "comfyui",
			id:   "comfyui",
			build: func(t *testing.T) video.VideoProvider {
				t.Helper()
				path := comfyWorkflowFile(t)
				p, err := NewComfyUI(comfyTestConfig(path, "http://127.0.0.1:8188"), 1024)
				if err != nil {
					t.Fatal(err)
				}
				return p
			},
			valid: func() video.VideoRequest { return validComfyRequest() },
		},
		{
			name: "aggregator",
			id:   "aggregator",
			build: func(t *testing.T) video.VideoProvider {
				t.Helper()
				t.Setenv("AGGREGATOR_CONTRACT_KEY", "contract-key")
				p, err := NewAggregator(video.Config{
					AggregatorBaseURL: "http://127.0.0.1:9", AggregatorModel: "test/model-v1",
					AggregatorAPIKeyEnv: "AGGREGATOR_CONTRACT_KEY", AggregatorCostPerSecondUSD: 0.02,
				}, 1024)
				if err != nil {
					t.Fatal(err)
				}
				return p
			},
			valid: func() video.VideoRequest {
				return video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5, Mode: video.ModeTextToVideo}
			},
		},
		{
			name: "code-render",
			id:   CodeRenderID,
			build: func(t *testing.T) video.VideoProvider {
				t.Helper()
				return NewCodeRender(nil, &render.Renderer{}, nil, t.TempDir(), []string{"deepseek-chat"}, 120)
			},
			valid: func() video.VideoRequest {
				return video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5,
					Mode: video.ModeTextToVideo, CreationMode: "programmatic", ModelPreference: "deepseek-chat"}
			},
		},
		{
			name: "code-render-with-narrator",
			id:   CodeRenderID,
			build: func(t *testing.T) video.VideoProvider {
				t.Helper()
				return NewCodeRender(nil, &render.Renderer{}, contractNarrator{}, t.TempDir(), []string{"deepseek-chat"}, 120)
			},
			valid: func() video.VideoRequest {
				return video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5,
					Mode: video.ModeTextToVideo, CreationMode: "programmatic", ModelPreference: "deepseek-chat",
					NarrationText: "hello", SubtitleLanguage: "en"}
			},
		},
	}
}

// firstUnadvertisedMode returns a known mode the provider does not list,
// or "" when it advertises every known mode.
func firstUnadvertisedMode(caps video.Capabilities) video.Mode {
	for _, m := range []video.Mode{video.ModeTextToVideo, video.ModeImageToVideo, video.ModeVideoToVideo} {
		found := false
		for _, advertised := range caps.Modes {
			if advertised == m {
				found = true
				break
			}
		}
		if !found {
			return m
		}
	}
	return ""
}

func TestProviderContract(t *testing.T) {
	ctx := context.Background()
	for _, tc := range contractProviders(t) {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.build(t)

			t.Run("id", func(t *testing.T) {
				if p.ID() == "" {
					t.Fatal("empty provider ID")
				}
				if p.ID() != tc.id {
					t.Fatalf("ID=%q, want %q", p.ID(), tc.id)
				}
			})

			caps, err := p.Capabilities(ctx)
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}

			t.Run("capabilities/deterministic", func(t *testing.T) {
				again, err := p.Capabilities(ctx)
				if err != nil {
					t.Fatalf("second Capabilities call: %v", err)
				}
				if !reflect.DeepEqual(caps, again) {
					t.Fatalf("capabilities not deterministic:\n%#v\n%#v", caps, again)
				}
			})

			t.Run("capabilities/sane", func(t *testing.T) {
				if len(caps.Modes) == 0 {
					t.Error("no modes advertised")
				}
				for _, m := range caps.Modes {
					switch m {
					case video.ModeTextToVideo, video.ModeImageToVideo, video.ModeVideoToVideo:
					default:
						t.Errorf("unknown mode %q", m)
					}
				}
				if caps.MaxDurationSeconds <= 0 || math.IsNaN(caps.MaxDurationSeconds) || math.IsInf(caps.MaxDurationSeconds, 0) {
					t.Errorf("MaxDurationSeconds=%v is not a positive finite bound", caps.MaxDurationSeconds)
				}
				if len(caps.Models) == 0 {
					t.Error("no models advertised")
				}
				if len(caps.AspectRatios) == 0 {
					t.Error("no aspect ratios advertised")
				}
				if !video.KnownProviderKind(caps.ProviderKind) {
					t.Errorf("unknown provider_kind %q", caps.ProviderKind)
				}
				// A test double must never present itself as a
				// production backend.
				if caps.ProviderKind == video.ProviderKindTest && caps.ProductionReady {
					t.Error("test-kind provider claims ProductionReady")
				}
			})

			t.Run("validate/rejects-empty", func(t *testing.T) {
				if err := p.ValidateRequest(video.VideoRequest{}); err == nil {
					t.Error("empty request accepted")
				}
			})

			t.Run("validate/accepts-minimal", func(t *testing.T) {
				if err := p.ValidateRequest(tc.valid()); err != nil {
					t.Fatalf("minimal valid request rejected: %v", err)
				}
			})

			// Capability/validation coherence: every capability the
			// provider does NOT declare must be rejected by
			// ValidateRequest. This is the "never silently ignored"
			// proof for each adapter.
			t.Run("validate/mode-coherence", func(t *testing.T) {
				mode := firstUnadvertisedMode(caps)
				if mode == "" {
					t.Skip("provider advertises every known mode")
				}
				r := tc.valid()
				r.Mode = mode
				if err := p.ValidateRequest(r); err == nil {
					t.Errorf("unadvertised mode %q accepted", mode)
				}
			})

			t.Run("validate/reference-coherence", func(t *testing.T) {
				if caps.SupportsReferenceFrames {
					t.Skip("provider declares reference-frame support")
				}
				r := tc.valid()
				r.ReferenceAssets = []video.Asset{{ID: "ref"}}
				if err := p.ValidateRequest(r); err == nil {
					t.Error("reference assets accepted without SupportsReferenceFrames")
				}
				r = tc.valid()
				r.FirstFrameAsset = &video.Asset{ID: "first"}
				if err := p.ValidateRequest(r); err == nil {
					t.Error("first-frame asset accepted without SupportsReferenceFrames")
				}
			})

			t.Run("validate/webhook-coherence", func(t *testing.T) {
				if caps.SupportsWebhooks {
					t.Skip("provider declares webhook support")
				}
				r := tc.valid()
				r.CallbackURL = "https://example.test/hook"
				if err := p.ValidateRequest(r); err == nil {
					t.Error("callback URL accepted without SupportsWebhooks")
				}
				// A provider that does not claim webhook support must
				// fail registration loudly instead of pretending.
				if err := p.RegisterWebhook(ctx, video.WebhookRegistration{}); err == nil {
					t.Error("RegisterWebhook accepted without SupportsWebhooks")
				}
			})

			t.Run("validate/sound-coherence", func(t *testing.T) {
				if caps.SupportsMusicGeneration || caps.SupportsAudio {
					t.Skip("provider declares music/audio support")
				}
				r := tc.valid()
				r.SoundRequirements = []string{"wind"}
				if err := p.ValidateRequest(r); err == nil {
					t.Error("sound requirements accepted without music/audio support")
				}
			})

			t.Run("validate/dialogue-coherence", func(t *testing.T) {
				if caps.SupportsDialogueGeneration || caps.SupportsAudio {
					t.Skip("provider declares dialogue/audio support")
				}
				r := tc.valid()
				r.DialogueRequested = true
				if err := p.ValidateRequest(r); err == nil {
					t.Error("dialogue accepted without dialogue/audio support")
				}
			})

			t.Run("jobs/unknown-ids-fail-closed", func(t *testing.T) {
				if _, err := p.GetJob(ctx, "contract-no-such-job"); err == nil {
					t.Error("GetJob accepted an unknown provider job ID")
				}
				if err := p.CancelJob(ctx, "contract-no-such-job"); err == nil {
					t.Error("CancelJob accepted an unknown provider job ID")
				}
			})

			t.Run("estimate/sane", func(t *testing.T) {
				est, err := p.EstimateCost(tc.valid())
				if err != nil {
					t.Fatalf("EstimateCost: %v", err)
				}
				if !est.PriceKnown {
					t.Error("estimate price not known")
				}
				if !strings.EqualFold(strings.TrimSpace(est.Currency), "USD") {
					t.Errorf("estimate currency=%q, want USD", est.Currency)
				}
				for _, v := range []float64{est.EstimatedUSD, est.UpperBoundUSD} {
					if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
						t.Errorf("estimate value %v is not a finite non-negative amount", v)
					}
				}
				if est.UpperBoundUSD < est.EstimatedUSD {
					t.Errorf("upper bound %v below estimate %v", est.UpperBoundUSD, est.EstimatedUSD)
				}
			})

			t.Run("create/rejects-invalid", func(t *testing.T) {
				// Invalid submissions must fail in ValidateRequest
				// before any provider state is touched.
				if _, err := p.CreateJob(ctx, video.VideoRequest{}); err == nil {
					t.Error("CreateJob accepted an invalid request")
				}
			})

			t.Run("webhook/parse-unconfigured-fails-closed", func(t *testing.T) {
				req, _ := http.NewRequest(http.MethodPost, "https://example.test/hook", nil)
				if _, err := p.ParseWebhook(req); err == nil {
					t.Error("ParseWebhook accepted without a configured parser")
				}
			})
		})
	}
}
