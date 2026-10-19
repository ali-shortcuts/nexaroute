package providers

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// #420 item 4: first/last-frame interpolation on the
// aggregator. A last-frame asset is consumable only when the
// operator configures the model's end-image field; then it
// must land in the submit body under exactly that field. With
// no field configured the request is refused — never silently
// dropped.
func TestAggregatorLastFrame(t *testing.T) {
	t.Setenv("AGGREGATOR_TEST_KEY", "k")
	fake := newFakeAggregator()
	srv := httptest.NewServer(fake.handler("test/model-v1"))
	defer srv.Close()
	ctx := context.Background()
	last := video.Asset{ID: "lf", Kind: "image", URI: "https://example.test/last.png"}
	req := video.VideoRequest{ProjectID: "p", Prompt: "a door opens", DurationSeconds: 5, Mode: video.ModeImageToVideo, LastFrameAsset: &last}

	// No end-image field: refused, nothing submitted.
	p := aggProvider(t, srv.URL)
	if err := p.ValidateRequest(req); err == nil {
		t.Fatal("last-frame request accepted without a configured end-image field")
	}

	// Field configured: valid, and the submit body carries
	// the last frame under the configured field name.
	cfg := aggConfig(srv.URL)
	cfg.AggregatorImageField = "image_url"
	cfg.AggregatorEndImageField = "end_image_url"
	p2, err := NewAggregator(cfg, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	first := video.Asset{ID: "ff", Kind: "image", URI: "https://example.test/first.png"}
	req.FirstFrameAsset = &first
	if err := p2.ValidateRequest(req); err != nil {
		t.Fatalf("first/last-frame request rejected: %v", err)
	}
	if _, err := p2.CreateJob(ctx, req); err != nil {
		t.Fatal(err)
	}
	if fake.lastBody["end_image_url"] != "https://example.test/last.png" {
		t.Fatalf("submit body end_image_url = %v, want the last-frame URI (body %v)", fake.lastBody["end_image_url"], fake.lastBody)
	}
	if fake.lastBody["image_url"] != "https://example.test/first.png" {
		t.Fatalf("submit body image_url = %v, want the first-frame URI", fake.lastBody["image_url"])
	}

	// A non-fetchable last-frame URI is refused even with the
	// field configured.
	bad := req
	badAsset := video.Asset{ID: "lf2", Kind: "image", URI: "file:///tmp/last.png"}
	bad.LastFrameAsset = &badAsset
	if err := p2.ValidateRequest(bad); err == nil {
		t.Fatal("last-frame with a non-http(s) URI accepted")
	}
}
