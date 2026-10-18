package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Recording fake aggregator (fal.ai queue class) for #420: it
// counts submits, walks a scripted status sequence, and serves a
// result payload + output bytes, so lifecycle, restart-resume,
// and malformed-output behaviour are proven without any paid
// call or real key.
type fakeAggregator struct {
	mu          sync.Mutex
	submits     int
	cancels     int
	statusStep  map[string]int
	failIDs     map[string]bool
	badOutput   bool
	spoofOutput bool
	authSeen    string
	lastBody    map[string]any
}

func newFakeAggregator() *fakeAggregator {
	return &fakeAggregator{statusStep: map[string]int{}, failIDs: map[string]bool{}}
}

func (f *fakeAggregator) handler(model string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/"+model, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.submits++
		f.authSeen = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&f.lastBody)
		id := "req-1"
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"request_id": id, "status": "IN_QUEUE"})
	})
	prefix := "/" + model + "/requests/"
	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		parts := strings.Split(rest, "/")
		id := parts[0]
		if id != "req-1" {
			http.Error(w, `{"detail":"not found"}`, http.StatusNotFound)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPut && len(parts) == 2 && parts[1] == "cancel":
			f.cancels++
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "status":
			if f.failIDs[id] {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "ERROR", "error": "provider exploded"})
				return
			}
			step := f.statusStep[id]
			f.statusStep[id] = step + 1
			statuses := []string{"IN_QUEUE", "IN_PROGRESS", "COMPLETED"}
			if step >= len(statuses) {
				step = len(statuses) - 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": statuses[step]})
		case r.Method == http.MethodGet && len(parts) == 1:
			ct := "video/mp4"
			if f.badOutput {
				ct = "text/plain"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"video": map[string]any{
				"url": f.fileURL(r), "content_type": ct,
			}})
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/files/out.mp4", func(w http.ResponseWriter, r *http.Request) {
		if f.spoofOutput {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("<html><body>definitely not a video</body></html>"))
			return
		}
		if f.badOutput {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("this is not a video"))
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("\x00\x00\x00\x18ftypmp42-fake-video-bytes"))
	})
	return mux
}

func (f *fakeAggregator) fileURL(r *http.Request) string {
	return "http://" + r.Host + "/files/out.mp4"
}

type aggSink struct{ saved []video.Asset }

func (s *aggSink) Put(_ context.Context, asset video.Asset, content []byte) (video.Asset, error) {
	asset.SizeBytes = int64(len(content))
	s.saved = append(s.saved, asset)
	return asset, nil
}

func aggConfig(baseURL string) video.Config {
	return video.Config{
		AggregatorBaseURL:          baseURL,
		AggregatorModel:            "test/model-v1",
		AggregatorAPIKeyEnv:        "AGGREGATOR_TEST_KEY",
		AggregatorCostPerSecondUSD: 0.02,
	}
}

func aggProvider(t *testing.T, baseURL string) *Aggregator {
	t.Helper()
	t.Setenv("AGGREGATOR_TEST_KEY", "test-key-123")
	p, err := NewAggregator(aggConfig(baseURL), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAggregatorLifecycleAndResume(t *testing.T) {
	fake := newFakeAggregator()
	srv := httptest.NewServer(fake.handler("test/model-v1"))
	defer srv.Close()
	ctx := context.Background()
	p := aggProvider(t, srv.URL)

	job, err := p.CreateJob(ctx, video.VideoRequest{ProjectID: "p", Prompt: "a lighthouse at dawn", DurationSeconds: 5, Mode: video.ModeTextToVideo})
	if err != nil {
		t.Fatal(err)
	}
	if job.ProviderJobID != "req-1" || !job.Accepted {
		t.Fatalf("job = %+v", job)
	}
	if fake.authSeen != "Key test-key-123" {
		t.Fatalf("auth header = %q, want the configured Key scheme", fake.authSeen)
	}
	if fake.lastBody["prompt"] != "a lighthouse at dawn" {
		t.Fatalf("submit body = %v", fake.lastBody)
	}

	st, err := p.GetJob(ctx, job.ProviderJobID)
	if err != nil || st.State != video.StateQueued {
		t.Fatalf("poll 1 = %+v, %v", st, err)
	}
	st, err = p.GetJob(ctx, job.ProviderJobID)
	if err != nil || st.State != video.StateProcessing {
		t.Fatalf("poll 2 = %+v, %v", st, err)
	}

	// Restart-resume: a brand-new adapter instance polls the same
	// provider job ID to completion. No resubmission may occur.
	p2 := aggProvider(t, srv.URL)
	st, err = p2.GetJob(ctx, job.ProviderJobID)
	if err != nil || st.State != video.StateCompleted || len(st.Outputs) != 1 {
		t.Fatalf("resumed poll = %+v, %v", st, err)
	}
	if fake.submits != 1 {
		t.Fatalf("submits = %d, want exactly 1 (resume must not resubmit)", fake.submits)
	}

	sink := &aggSink{}
	saved, err := p2.DownloadOutputs(ctx, st, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].SizeBytes == 0 || saved[0].ContentType != "video/mp4" {
		t.Fatalf("saved = %+v", saved)
	}

	// Terminal polls are idempotent.
	st, err = p2.GetJob(ctx, job.ProviderJobID)
	if err != nil || st.State != video.StateCompleted {
		t.Fatalf("repeat terminal poll = %+v, %v", st, err)
	}

	if err := p2.CancelJob(ctx, job.ProviderJobID); err != nil {
		t.Fatal(err)
	}
	if fake.cancels != 1 {
		t.Fatalf("cancels = %d, want 1", fake.cancels)
	}
	if err := p2.CancelJob(ctx, "nope"); err == nil {
		t.Fatal("cancel of unknown job accepted")
	}
	if _, err := p2.GetJob(ctx, "nope"); err == nil {
		t.Fatal("poll of unknown job accepted")
	}
}

func TestAggregatorFailedAndMalformed(t *testing.T) {
	fake := newFakeAggregator()
	fake.failIDs["req-1"] = true
	srv := httptest.NewServer(fake.handler("test/model-v1"))
	defer srv.Close()
	ctx := context.Background()
	p := aggProvider(t, srv.URL)

	job, err := p.CreateJob(ctx, video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5, Mode: video.ModeTextToVideo})
	if err != nil {
		t.Fatal(err)
	}
	st, err := p.GetJob(ctx, job.ProviderJobID)
	if err != nil || st.State != video.StateFailed || !strings.Contains(st.Error, "provider exploded") {
		t.Fatalf("failed poll = %+v, %v", st, err)
	}

	// Malformed output: the provider completes but serves a
	// non-video payload. The adapter must refuse it, and nothing
	// may reach the sink.
	fake2 := newFakeAggregator()
	fake2.badOutput = true
	srv2 := httptest.NewServer(fake2.handler("test/model-v1"))
	defer srv2.Close()
	p2 := aggProvider(t, srv2.URL)
	job2, err := p2.CreateJob(ctx, video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5, Mode: video.ModeTextToVideo})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		st, err = p2.GetJob(ctx, job2.ProviderJobID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if st.State != video.StateCompleted {
		t.Fatalf("state = %v, want completed at provider level", st.State)
	}
	sink := &aggSink{}
	if _, err := p2.DownloadOutputs(ctx, st, sink); err == nil {
		t.Fatal("non-video output accepted")
	}
	if len(sink.saved) != 0 {
		t.Fatalf("sink received %d assets from a malformed output", len(sink.saved))
	}
}

func TestAggregatorValidationAndPricing(t *testing.T) {
	t.Setenv("AGGREGATOR_TEST_KEY", "k")
	srv := httptest.NewServer(newFakeAggregator().handler("test/model-v1"))
	defer srv.Close()
	p := aggProvider(t, srv.URL)

	if err := p.ValidateRequest(video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5, Mode: video.ModeImageToVideo}); err == nil {
		t.Fatal("image_to_video accepted without a configured image field")
	}
	if err := p.ValidateRequest(video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 9999, Mode: video.ModeTextToVideo}); err == nil {
		t.Fatal("over-duration request accepted")
	}
	est, err := p.EstimateCost(video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5, Mode: video.ModeTextToVideo})
	if err != nil || !est.PriceKnown || est.EstimatedUSD != 0.1 {
		t.Fatalf("estimate = %+v, %v", est, err)
	}

	// Unpriced configuration stays unpriced — never invented.
	cfg := aggConfig(srv.URL)
	cfg.AggregatorCostPerSecondUSD = 0
	free, err := NewAggregator(cfg, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	est, err = free.EstimateCost(video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5, Mode: video.ModeTextToVideo})
	if err != nil || est.PriceKnown {
		t.Fatalf("unpriced estimate = %+v, %v", est, err)
	}

	// Image mode unlocks only with the image field configured,
	// and then requires a fetchable image URI.
	cfg.AggregatorImageField = "image_url"
	img, err := NewAggregator(cfg, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := img.ValidateRequest(video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5, Mode: video.ModeImageToVideo}); err == nil {
		t.Fatal("image_to_video accepted without an image asset")
	}
	frame := video.Asset{ID: "f", Kind: "image", URI: "https://example.test/frame.png"}
	if err := img.ValidateRequest(video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5, Mode: video.ModeImageToVideo, FirstFrameAsset: &frame}); err != nil {
		t.Fatalf("image_to_video with frame rejected: %v", err)
	}
}

func TestAggregatorConstructorFailsClosed(t *testing.T) {
	base := video.Config{AggregatorBaseURL: "https://queue.example.test", AggregatorModel: "m", AggregatorAPIKeyEnv: "AGGREGATOR_TEST_KEY"}
	t.Setenv("AGGREGATOR_TEST_KEY", "")
	if _, err := NewAggregator(base, 0); err == nil {
		t.Fatal("empty API key accepted")
	}
	t.Setenv("AGGREGATOR_TEST_KEY", "k")
	bad := base
	bad.AggregatorBaseURL = "http://203.0.113.10"
	if _, err := NewAggregator(bad, 0); err == nil {
		t.Fatal("plain-HTTP non-loopback base URL accepted")
	}
	bad = base
	bad.AggregatorModel = ""
	if _, err := NewAggregator(bad, 0); err == nil {
		t.Fatal("missing model accepted")
	}
}

// #398 asset-security remainder: a served Content-Type header is
// a claim, not proof. When the output host labels HTML bytes as
// video/mp4, the download must be refused on the byte signature
// and nothing may reach the sink.
func TestAggregatorSpoofedContentTypeRefused(t *testing.T) {
	fake := newFakeAggregator()
	fake.spoofOutput = true
	srv := httptest.NewServer(fake.handler("test/model-v1"))
	defer srv.Close()
	ctx := context.Background()
	p := aggProvider(t, srv.URL)
	job, err := p.CreateJob(ctx, video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 5, Mode: video.ModeTextToVideo})
	if err != nil {
		t.Fatal(err)
	}
	var st video.ProviderJobStatus
	for i := 0; i < 3; i++ {
		st, err = p.GetJob(ctx, job.ProviderJobID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if st.State != video.StateCompleted || len(st.Outputs) != 1 {
		t.Fatalf("status = %+v", st)
	}
	sink := &aggSink{}
	if _, err := p.DownloadOutputs(ctx, st, sink); err == nil {
		t.Fatal("spoofed output (HTML bytes, video/mp4 header) was accepted")
	} else if !strings.Contains(err.Error(), "do not look like a video container") {
		t.Fatalf("error = %v, want the container-signature refusal", err)
	}
	if len(sink.saved) != 0 {
		t.Fatalf("sink received %d assets from a spoofed output", len(sink.saved))
	}
}
