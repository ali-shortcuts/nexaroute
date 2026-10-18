package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

func TestCoverageGapPureHelpers(t *testing.T) {
	// shotPrompt combines direction metadata and skips blank continuity entries.
	prompt := shotPrompt(video.Shot{
		Prompt: "Noora enters", CameraMovement: "slow pan", SubjectAction: "walks",
		StartState: "gate closed", EndState: "gate open",
		ContinuityConstraints: []string{"same robe", "  ", ""},
	})
	for _, want := range []string{"Noora enters", "Camera movement: slow pan", "Subject action: walks", "Starting state: gate closed", "Ending state: gate open", "Continuity constraint: same robe"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("shot prompt missing %q: %q", want, prompt)
		}
	}
	if got := shotPrompt(video.Shot{Prompt: "  bare  "}); got != "bare" {
		t.Fatalf("bare prompt=%q", got)
	}

	// buildShotRequest isolates episode-level narration/audio and scopes idempotency.
	parent := episodeRequest()
	parent.IdempotencyKey = "episode-key"
	parent.AudioRequested = true
	parent.DialogueRequested = true
	parent.NarrationText = "narrate"
	parent.SubtitleLanguage = "en"
	parent.ReferenceAssets = []video.Asset{{ID: "parent-ref"}}
	shot := parent.ShotPlan[0]
	shot.Provider = "fake"
	shot.Model = "model-a"
	shot.RequiredReferences = []video.Asset{{ID: "shot-ref"}}
	built := buildShotRequest(parent, shot, 0)
	if built.ShotPlan != nil || built.AudioRequested || built.DialogueRequested || built.NarrationText != "" || built.SubtitleLanguage != "" {
		t.Fatalf("episode fields leaked into provider request: %+v", built)
	}
	if built.IdempotencyKey != "episode-key-shot-001" || built.ProviderPreference != "fake" || built.ModelPreference != "model-a" {
		t.Fatalf("shot request scoping wrong: %+v", built)
	}
	if len(built.ReferenceAssets) != 2 {
		t.Fatalf("reference assets=%d, want parent plus shot", len(built.ReferenceAssets))
	}
	noKey := buildShotRequest(video.VideoRequest{}, video.Shot{}, 1)
	if noKey.IdempotencyKey != "" {
		t.Fatalf("empty parent key should stay empty, got %q", noKey.IdempotencyKey)
	}
	negative := buildShotRequest(video.VideoRequest{NegativePrompt: "parent-neg"}, video.Shot{NegativePrompt: "shot-neg"}, 0)
	if negative.NegativePrompt != "shot-neg" {
		t.Fatalf("shot negative prompt not preferred: %q", negative.NegativePrompt)
	}
	keepParentNeg := buildShotRequest(video.VideoRequest{NegativePrompt: "parent-neg"}, video.Shot{}, 0)
	if keepParentNeg.NegativePrompt != "parent-neg" {
		t.Fatalf("parent negative prompt lost: %q", keepParentNeg.NegativePrompt)
	}

	// episodeDimensions prefers explicit sizes, then aspect defaults.
	if w, h := episodeDimensions(video.VideoRequest{Width: 640, Height: 480}); w != 640 || h != 480 {
		t.Fatalf("explicit dimensions=%d/%d", w, h)
	}
	if w, h := episodeDimensions(video.VideoRequest{AspectRatio: video.Aspect9x16}); w != 720 || h != 1280 {
		t.Fatalf("9x16 dimensions=%d/%d", w, h)
	}
	if w, h := episodeDimensions(video.VideoRequest{AspectRatio: video.Aspect1x1}); w != 1024 || h != 1024 {
		t.Fatalf("1x1 dimensions=%d/%d", w, h)
	}
	if w, h := episodeDimensions(video.VideoRequest{}); w != 1280 || h != 720 {
		t.Fatalf("default dimensions=%d/%d", w, h)
	}

	// maxAssetBytes and pollInterval honor configured values with safe defaults.
	if got := (&Orchestrator{MaxAssetBytes: 123}).maxAssetBytes(); got != 123 {
		t.Fatalf("custom max bytes=%d", got)
	}
	if got := (&Orchestrator{}).maxAssetBytes(); got != 512<<20 {
		t.Fatalf("default max bytes=%d", got)
	}
	if got := (&Orchestrator{PollInterval: time.Second}).pollInterval(); got != time.Second {
		t.Fatalf("custom interval=%v", got)
	}
	if got := (&Orchestrator{}).pollInterval(); got != 100*time.Millisecond {
		t.Fatalf("default interval=%v", got)
	}

	// findEpisodeSourceAsset returns a copy; mutating it must not affect the job.
	assets := []video.Asset{{ID: "a-1", URI: "/tmp/a.mp4"}}
	found := findEpisodeSourceAsset(assets, "a-1")
	if found == nil || found.ID != "a-1" {
		t.Fatalf("source asset lookup=%+v", found)
	}
	found.URI = "mutated"
	if assets[0].URI != "/tmp/a.mp4" {
		t.Fatal("source asset lookup did not return a copy")
	}
	if findEpisodeSourceAsset(assets, "missing") != nil {
		t.Fatal("missing source asset should be nil")
	}

	// checkNarrationDuration rejects over-long narration with an actionable error.
	episode := video.VideoJob{Request: video.VideoRequest{DurationSeconds: 4}}
	if err := checkNarrationDuration(episode, 10*time.Second); err == nil || !strings.Contains(err.Error(), "narration exceeds episode duration") {
		t.Fatalf("over-long narration error=%v", err)
	}
	if err := checkNarrationDuration(episode, time.Second); err != nil {
		t.Fatalf("fitting narration rejected: %v", err)
	}
	if err := checkNarrationDuration(video.VideoJob{}, time.Hour); err != nil {
		t.Fatalf("zero-duration episode should accept narration: %v", err)
	}
}

func writeCoverageGapAsset(t *testing.T, content []byte) (string, video.Asset) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "asset.bin")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return path, video.Asset{
		URI: path, ContentType: "audio/wav", SizeBytes: int64(len(content)),
		SHA256: hex.EncodeToString(sum[:]),
	}
}

func TestCoverageGapVerifyAssetHelpers(t *testing.T) {
	_, good := writeCoverageGapAsset(t, append([]byte("RIFF....WAVE"), make([]byte, 64)...))
	if err := verifyNarrationAsset(good, 1<<20); err != nil {
		t.Fatalf("valid narration rejected: %v", err)
	}
	sub := good
	sub.ContentType = "application/x-subrip"
	if err := verifySubtitleAsset(sub, 1<<20); err != nil {
		t.Fatalf("valid subtitle rejected: %v", err)
	}
	if err := verifyNarrationAsset(video.Asset{}, 1<<20); err == nil {
		t.Fatal("empty narration accepted")
	}
	oversized := good
	oversized.SizeBytes = 2 << 20
	if err := verifyNarrationAsset(oversized, 1<<20); err == nil {
		t.Fatal("oversized narration accepted")
	}
	badHex := good
	badHex.SHA256 = strings.Repeat("z", 64)
	if err := verifyNarrationAsset(badHex, 1<<20); err == nil {
		t.Fatal("malformed narration checksum accepted")
	}
	missing := good
	missing.URI = filepath.Join(t.TempDir(), "absent.bin")
	if err := verifyNarrationAsset(missing, 1<<20); err == nil {
		t.Fatal("missing narration file accepted")
	}
	mismatchPath := filepath.Join(t.TempDir(), "mismatch.bin")
	if err := os.WriteFile(mismatchPath, []byte("different"), 0600); err != nil {
		t.Fatal(err)
	}
	mismatch := good
	mismatch.URI = mismatchPath
	if err := verifyNarrationAsset(mismatch, 1<<20); err == nil {
		t.Fatal("checksum-mismatched narration accepted")
	}
	if err := verifySubtitleAsset(video.Asset{}, 1<<20); err == nil {
		t.Fatal("empty subtitle accepted")
	}
	badSubHex := sub
	badSubHex.SHA256 = strings.Repeat("z", 64)
	if err := verifySubtitleAsset(badSubHex, 1<<20); err == nil {
		t.Fatal("malformed subtitle checksum accepted")
	}
	missingSub := sub
	missingSub.URI = filepath.Join(t.TempDir(), "absent-sub.srt")
	if err := verifySubtitleAsset(missingSub, 1<<20); err == nil {
		t.Fatal("missing subtitle file accepted")
	}
	mismatchSub := sub
	mismatchSub.URI = mismatchPath
	if err := verifySubtitleAsset(mismatchSub, 1<<20); err == nil {
		t.Fatal("checksum-mismatched subtitle accepted")
	}
}

func TestCoverageGapRetryComposition(t *testing.T) {
	ctx := context.Background()
	store := coverageVideoOrchExtraStoreNew()
	job := queueJobForTest("retry-composition")
	if err := store.MemoryStore.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	// Persistence failure surfaces the combined error without scheduling work.
	store.updateErrAt = 1
	if err := (&Orchestrator{Store: store, Queue: queue.New(1)}).retryEpisodeComposition(ctx, &job, "compose failed", errors.New("ffmpeg")); err == nil || !strings.Contains(err.Error(), "checkpoint could not be saved") {
		t.Fatalf("retry persist error=%v", err)
	}
	// The third consecutive failure marks the job failed instead of retrying.
	store2 := coverageVideoOrchExtraStoreNew()
	job2 := queueJobForTest("retry-exhausted")
	job2.CompositionRetryCount = 2
	if err := store2.MemoryStore.Create(ctx, job2); err != nil {
		t.Fatal(err)
	}
	if err := (&Orchestrator{Store: store2, Queue: queue.New(1)}).retryEpisodeComposition(ctx, &job2, "compose failed", errors.New("ffmpeg")); err == nil {
		t.Fatal("exhausted retries should fail")
	}
	got, _ := store2.MemoryStore.Get(ctx, job2.JobID)
	if got.State != video.StateFailed {
		t.Fatalf("exhausted retry state=%s", got.State)
	}
	// A retryable failure without a cause still returns a plain error.
	store3 := coverageVideoOrchExtraStoreNew()
	job3 := queueJobForTest("retry-plain")
	if err := store3.MemoryStore.Create(ctx, job3); err != nil {
		t.Fatal(err)
	}
	if err := (&Orchestrator{Store: store3, Queue: queue.New(1)}).retryEpisodeComposition(ctx, &job3, "compose failed", nil); err == nil || err.Error() != "compose failed" {
		t.Fatalf("plain retry error=%v", err)
	}
}

func TestCoverageGapValidateShotPlan(t *testing.T) {
	baseCaps := providerWithDefaults().caps
	newShotRequest := func() video.VideoRequest {
		r := episodeRequest()
		r.ProviderPreference = "fake"
		return r
	}
	t.Run("too few shots", func(t *testing.T) {
		r := newShotRequest()
		r.ShotPlan = r.ShotPlan[:1]
		if _, _, err := validateShotPlanRequest(r, providerWithDefaults(), baseCaps); err == nil {
			t.Fatal("single-shot plan accepted by shot-plan validator")
		}
	})
	t.Run("duplicate shot id", func(t *testing.T) {
		r := newShotRequest()
		r.ShotPlan[1].ShotID = r.ShotPlan[0].ShotID
		if _, _, err := validateShotPlanRequest(r, providerWithDefaults(), baseCaps); err == nil || !strings.Contains(err.Error(), "duplicate shot_id") {
			t.Fatalf("duplicate error=%v", err)
		}
	})
	t.Run("shot selects another provider", func(t *testing.T) {
		r := newShotRequest()
		r.ShotPlan[0].Provider = "other"
		if _, _, err := validateShotPlanRequest(r, providerWithDefaults(), baseCaps); err == nil || !strings.Contains(err.Error(), "selects provider") {
			t.Fatalf("provider mismatch error=%v", err)
		}
	})
	t.Run("shot selects another model", func(t *testing.T) {
		r := newShotRequest()
		r.ModelPreference = "model-a"
		r.ShotPlan[0].Model = "model-b"
		if _, _, err := validateShotPlanRequest(r, providerWithDefaults(), baseCaps); err == nil || !strings.Contains(err.Error(), "selects model") {
			t.Fatalf("model mismatch error=%v", err)
		}
	})
	t.Run("unsupported shot mode", func(t *testing.T) {
		r := newShotRequest()
		p := providerWithDefaults()
		p.caps.Modes = nil
		if _, _, err := validateShotPlanRequest(r, p, p.caps); err == nil {
			t.Fatal("unsupported mode accepted")
		}
	})
	t.Run("provider rejects shot", func(t *testing.T) {
		r := newShotRequest()
		p := providerWithDefaults()
		p.validateErr = errors.New("nope")
		if _, _, err := validateShotPlanRequest(r, p, baseCaps); err == nil || !strings.Contains(err.Error(), "provider rejected shot") {
			t.Fatalf("provider rejection error=%v", err)
		}
	})
	t.Run("estimate failure", func(t *testing.T) {
		r := newShotRequest()
		p := providerWithDefaults()
		p.estimateErr = errors.New("pricing down")
		if _, _, err := validateShotPlanRequest(r, p, baseCaps); err == nil || !strings.Contains(err.Error(), "cannot safely estimate cost") {
			t.Fatalf("estimate error=%v", err)
		}
	})
	t.Run("invalid estimate", func(t *testing.T) {
		r := newShotRequest()
		p := providerWithDefaults()
		p.estimate.Currency = "EUR"
		if _, _, err := validateShotPlanRequest(r, p, baseCaps); err == nil {
			t.Fatal("invalid estimate accepted")
		}
	})
	t.Run("estimate for another provider", func(t *testing.T) {
		r := newShotRequest()
		p := providerWithDefaults()
		p.estimate.Provider = "other"
		if _, _, err := validateShotPlanRequest(r, p, baseCaps); err == nil || !strings.Contains(err.Error(), "another provider") {
			t.Fatalf("provider estimate error=%v", err)
		}
	})
	t.Run("estimate for another model", func(t *testing.T) {
		r := newShotRequest()
		p := providerWithDefaults()
		p.estimate.Model = "other"
		if _, _, err := validateShotPlanRequest(r, p, baseCaps); err == nil || !strings.Contains(err.Error(), "another model") {
			t.Fatalf("model estimate error=%v", err)
		}
	})
	t.Run("provider model split across shots", func(t *testing.T) {
		r := newShotRequest()
		r.ModelPreference = ""
		r.ShotPlan[0].Model = "model-a"
		r.ShotPlan[1].Model = "model-b"
		p := providerWithDefaults()
		p.caps.Models = []string{"model-a", "model-b"}
		if _, _, err := validateShotPlanRequest(r, p, p.caps); err == nil || !strings.Contains(err.Error(), "selects model") {
			t.Fatalf("split model error=%v", err)
		}
	})
	t.Run("success aggregates cost and clears breakdown when empty", func(t *testing.T) {
		r := newShotRequest()
		r.ModelPreference = ""
		p := providerWithDefaults()
		p.estimate.Breakdown = nil
		normalized, total, err := validateShotPlanRequest(r, p, baseCaps)
		if err != nil {
			t.Fatal(err)
		}
		if total.EstimatedUSD != 4 || total.UpperBoundUSD != 6 || total.Model != "model-a" {
			t.Fatalf("aggregate=%+v", total)
		}
		if total.Breakdown != nil {
			t.Fatalf("empty breakdown should be nil, got %+v", total.Breakdown)
		}
		if normalized.ProviderPreference != "fake" || normalized.ModelPreference != "model-a" {
			t.Fatalf("normalized=%+v", normalized)
		}
	})
	t.Run("success preserves non-empty breakdown", func(t *testing.T) {
		r := newShotRequest()
		p := providerWithDefaults()
		p.estimate.Breakdown = map[string]float64{"render": 1}
		_, total, err := validateShotPlanRequest(r, p, baseCaps)
		if err != nil {
			t.Fatal(err)
		}
		if total.Breakdown["render"] != 2 {
			t.Fatalf("breakdown=%+v", total.Breakdown)
		}
		if total.BillableSeconds != 0 {
			t.Fatalf("billable=%v", total.BillableSeconds)
		}
	})
}

func TestCoverageGapCreateSingleShotValidation(t *testing.T) {
	ctx := context.Background()
	newOrch := func() (*Orchestrator, *fakeProvider) {
		p := providerWithDefaults()
		return &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": p}}, p
	}
	t.Run("missing shot id", func(t *testing.T) {
		o, _ := newOrch()
		r := requestWithDefaults()
		r.ShotPlan = []video.Shot{{Prompt: "x", DurationSeconds: 2}}
		if _, err := o.Create(ctx, r); err == nil || !strings.Contains(err.Error(), "single shot requires shot_id") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("invalid shot duration", func(t *testing.T) {
		for _, d := range []float64{0, math.NaN(), math.Inf(1)} {
			o, _ := newOrch()
			r := requestWithDefaults()
			r.ShotPlan = []video.Shot{{ShotID: "s", Prompt: "x", DurationSeconds: d}}
			if _, err := o.Create(ctx, r); err == nil || !strings.Contains(err.Error(), "invalid duration") {
				t.Fatalf("duration %v error=%v", d, err)
			}
		}
	})
	t.Run("duration mismatch", func(t *testing.T) {
		o, _ := newOrch()
		r := requestWithDefaults()
		r.DurationSeconds = 5
		r.ShotPlan = []video.Shot{{ShotID: "s", Prompt: "x", DurationSeconds: 2}}
		if _, err := o.Create(ctx, r); err == nil || !strings.Contains(err.Error(), "does not match shot duration") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("provider conflict", func(t *testing.T) {
		o, _ := newOrch()
		r := requestWithDefaults()
		r.ProviderPreference = "fake"
		r.ShotPlan = []video.Shot{{ShotID: "s", Prompt: "x", DurationSeconds: 2, Provider: "other"}}
		if _, err := o.Create(ctx, r); err == nil || !strings.Contains(err.Error(), "provider conflicts") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("model conflict", func(t *testing.T) {
		o, _ := newOrch()
		r := requestWithDefaults()
		r.ModelPreference = "model-a"
		r.ShotPlan = []video.Shot{{ShotID: "s", Prompt: "x", DurationSeconds: 2, Model: "other"}}
		if _, err := o.Create(ctx, r); err == nil || !strings.Contains(err.Error(), "model conflicts") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("inherits prompt and preferences", func(t *testing.T) {
		o, _ := newOrch()
		r := requestWithDefaults()
		r.Prompt = ""
		r.DurationSeconds = 0
		r.ShotPlan = []video.Shot{{
			ShotID: "s", CameraMovement: "pan", DurationSeconds: 2,
			Provider: "fake", Model: "model-a",
			// Note: per-shot RequiredReferences are rejected at
			// admission unless the provider declares
			// SupportsReferenceFrames (see capability_admission_test.go);
			// they are intentionally absent from this fixture so the
			// test keeps proving prompt/preference inheritance.
			SoundRequirements: []string{"wind"},
		}}
		job, err := o.Create(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(job.Request.Prompt, "Camera movement: pan") || !job.Request.AudioRequested {
			t.Fatalf("single-shot inheritance wrong: %+v", job.Request)
		}
	})
}

func TestCoverageGapCreateMultiShotValidation(t *testing.T) {
	ctx := context.Background()
	multi := func() video.VideoRequest {
		r := episodeRequest()
		r.DurationSeconds = 4
		return r
	}
	t.Run("invalid multi duration", func(t *testing.T) {
		o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}
		r := multi()
		r.ShotPlan[0].DurationSeconds = 0
		if _, err := o.Create(ctx, r); err == nil || !strings.Contains(err.Error(), "invalid duration") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("invalid total duration", func(t *testing.T) {
		o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}
		r := multi()
		r.DurationSeconds = math.NaN()
		if _, err := o.Create(ctx, r); err == nil || !strings.Contains(err.Error(), "duration_seconds is invalid") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("total mismatch", func(t *testing.T) {
		o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}
		r := multi()
		r.DurationSeconds = 10
		if _, err := o.Create(ctx, r); err == nil || !strings.Contains(err.Error(), "does not match total shot duration") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("subtitle without narration", func(t *testing.T) {
		o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}
		r := multi()
		r.SubtitleLanguage = "en"
		if _, err := o.Create(ctx, r); err == nil || !strings.Contains(err.Error(), "subtitle_language requires narration") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("narration with audio requested", func(t *testing.T) {
		o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}, Narrator: &episodeTestNarrator{}}
		r := multi()
		r.NarrationText = "tell it"
		r.AudioRequested = true
		if _, err := o.Create(ctx, r); err == nil || !strings.Contains(err.Error(), "narration only") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("shot sound with narration", func(t *testing.T) {
		o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}, Narrator: &episodeTestNarrator{}}
		r := multi()
		r.NarrationText = "tell it"
		r.ShotPlan[0].SoundRequirements = []string{"thunder"}
		if _, err := o.Create(ctx, r); err == nil || !strings.Contains(err.Error(), "only narration synthesis is configured") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("odd dimensions rejected", func(t *testing.T) {
		o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}
		r := multi()
		r.Width, r.Height = 641, 360
		if _, err := o.Create(ctx, r); err == nil || !strings.Contains(err.Error(), "must be even") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("multi without composer", func(t *testing.T) {
		o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}
		if _, err := o.Create(ctx, multi()); err == nil || !strings.Contains(err.Error(), "episode composer and asset sink") {
			t.Fatalf("error=%v", err)
		}
	})
}
