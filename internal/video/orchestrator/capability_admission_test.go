package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// Phase 6 (issue #398): centralized capability admission. Requests that
// exercise capabilities a provider does not declare must be rejected at
// admission — never silently downgraded (reference frames dropped, a
// callback URL accepted that can never fire, sound requirements ignored).

func admissionCaps() video.Capabilities {
	return video.Capabilities{
		Modes: []video.Mode{video.ModeTextToVideo}, Models: []string{"model-a"},
		MaxDurationSeconds: 10, AspectRatios: []video.AspectRatio{video.Aspect16x9},
		ProviderKind: video.ProviderKindTest,
	}
}

func admissionRequest() video.VideoRequest {
	return video.VideoRequest{ProjectID: "project", Prompt: "a scene", DurationSeconds: 2, Mode: video.ModeTextToVideo}.Normalized()
}

func TestCapabilityAdmissionRejectsUndeclaredCapabilities(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*video.VideoRequest)
		caps   func(*video.Capabilities)
		want   string
	}{
		{
			name:   "reference assets",
			mutate: func(r *video.VideoRequest) { r.ReferenceAssets = []video.Asset{{ID: "ref"}} },
			caps:   func(*video.Capabilities) {},
			want:   "reference/first/last-frame",
		},
		{
			name:   "first frame asset",
			mutate: func(r *video.VideoRequest) { r.FirstFrameAsset = &video.Asset{ID: "first"} },
			caps:   func(*video.Capabilities) {},
			want:   "reference/first/last-frame",
		},
		{
			name:   "last frame asset",
			mutate: func(r *video.VideoRequest) { r.LastFrameAsset = &video.Asset{ID: "last"} },
			caps:   func(*video.Capabilities) {},
			want:   "reference/first/last-frame",
		},
		{
			name:   "character references",
			mutate: func(r *video.VideoRequest) { r.CharacterReferences = []video.Asset{{ID: "char"}} },
			caps:   func(*video.Capabilities) {},
			want:   "reference/first/last-frame",
		},
		{
			name:   "continuity context",
			mutate: func(r *video.VideoRequest) { r.ContinuityContext = &video.ContinuityContext{} },
			caps:   func(*video.Capabilities) {},
			want:   "reference/first/last-frame",
		},
		{
			name: "shot required references",
			mutate: func(r *video.VideoRequest) {
				r.ShotPlan = []video.Shot{{ShotID: "s", Prompt: "x", DurationSeconds: 2, RequiredReferences: []video.Asset{{ID: "ref"}}}}
			},
			caps: func(*video.Capabilities) {},
			want: "reference/first/last-frame",
		},
		{
			name:   "callback without webhook support",
			mutate: func(r *video.VideoRequest) { r.CallbackURL = "https://example.test/hook" },
			caps:   func(*video.Capabilities) {},
			want:   "webhooks/callbacks",
		},
		{
			name:   "sound requirements without music/audio support",
			mutate: func(r *video.VideoRequest) { r.SoundRequirements = []string{"wind"} },
			caps:   func(c *video.Capabilities) { c.SupportsAudio = false },
			want:   "music/sound",
		},
		{
			name:   "dialogue without dialogue/audio support",
			mutate: func(r *video.VideoRequest) { r.DialogueRequested = true },
			caps:   func(c *video.Capabilities) { c.SupportsAudio = false },
			want:   "dialogue",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := admissionRequest()
			tc.mutate(&r)
			caps := admissionCaps()
			tc.caps(&caps)
			if _, err := validateProviderRequest(r, caps); err == nil {
				t.Fatal("request exercising an undeclared capability was accepted")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not explain the missing capability %q", err, tc.want)
			}
		})
	}
}

func TestCapabilityAdmissionAcceptsDeclaredCapabilities(t *testing.T) {
	t.Run("reference frames declared", func(t *testing.T) {
		r := admissionRequest()
		r.ReferenceAssets = []video.Asset{{ID: "ref"}}
		caps := admissionCaps()
		caps.SupportsReferenceFrames = true
		if _, err := validateProviderRequest(r, caps); err != nil {
			t.Fatalf("declared reference support rejected: %v", err)
		}
	})
	t.Run("callback declared", func(t *testing.T) {
		r := admissionRequest()
		r.CallbackURL = "https://example.test/hook"
		caps := admissionCaps()
		caps.SupportsWebhooks = true
		if _, err := validateProviderRequest(r, caps); err != nil {
			t.Fatalf("declared webhook support rejected: %v", err)
		}
	})
	t.Run("dialogue via granular flag", func(t *testing.T) {
		r := admissionRequest()
		r.DialogueRequested = true
		caps := admissionCaps()
		caps.SupportsAudio = false
		caps.SupportsDialogueGeneration = true
		if _, err := validateProviderRequest(r, caps); err != nil {
			t.Fatalf("declared dialogue support rejected: %v", err)
		}
	})
	t.Run("sound via granular flag", func(t *testing.T) {
		r := admissionRequest()
		r.SoundRequirements = []string{"wind"}
		caps := admissionCaps()
		caps.SupportsAudio = false
		caps.SupportsMusicGeneration = true
		if _, err := validateProviderRequest(r, caps); err != nil {
			t.Fatalf("declared music support rejected: %v", err)
		}
	})
}

// End-to-end through Orchestrator.Create: a reference-frame request
// against the fake provider (which declares no reference support) must
// be refused before any provider submission happens.
func TestCreateRejectsReferenceInputsAgainstFakeProvider(t *testing.T) {
	ctx := context.Background()
	p := providers.NewFake(1)
	o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": p}}
	r := video.VideoRequest{
		ProjectID: "project", Prompt: "a scene", DurationSeconds: 2,
		Mode: video.ModeTextToVideo, ProviderPreference: "fake",
		ReferenceAssets: []video.Asset{{ID: "ref"}},
	}
	if _, err := o.Create(ctx, r); err == nil {
		t.Fatal("Create accepted reference assets against a provider without reference support")
	} else if !strings.Contains(err.Error(), "reference/first/last-frame") {
		t.Fatalf("error does not explain the missing capability: %v", err)
	}
	if p.NextID != 0 {
		t.Fatalf("provider was submitted to despite admission rejection (jobs=%d)", p.NextID)
	}
}

// A callback URL against the fake provider (no webhook support) must be
// refused: accepting it would promise delivery that can never happen.
func TestCreateRejectsCallbackAgainstFakeProvider(t *testing.T) {
	ctx := context.Background()
	p := providers.NewFake(1)
	o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": p}}
	r := video.VideoRequest{
		ProjectID: "project", Prompt: "a scene", DurationSeconds: 2,
		Mode: video.ModeTextToVideo, ProviderPreference: "fake",
		CallbackURL: "https://example.test/hook",
	}
	if _, err := o.Create(ctx, r); err == nil {
		t.Fatal("Create accepted a callback URL against a provider without webhook support")
	} else if !strings.Contains(err.Error(), "webhooks/callbacks") {
		t.Fatalf("error does not explain the missing capability: %v", err)
	}
	if p.NextID != 0 {
		t.Fatalf("provider was submitted to despite admission rejection (jobs=%d)", p.NextID)
	}
}
