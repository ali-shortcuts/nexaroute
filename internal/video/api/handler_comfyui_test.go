package api

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

// Retrieval through NexaRoute for a ComfyUI-produced asset: the asset
// is stored by the ComfyUI provider path (provider/node/filename
// metadata, MP4 bytes), then served by the gateway video handler.
// This is the gateway-path check the direct-provider harness never
// exercises: authorized retrieval must return byte-identical content,
// missing or wrong tokens must be rejected, traversal asset names
// must not resolve, and a same-size tampered file must fail the
// recorded SHA-256 check instead of being served.
func TestHandlerServesComfyUIAssetThroughGateway(t *testing.T) {
	h := newTestHandler(true)
	root := t.TempDir()
	local, err := storage.NewLocal(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	h.Assets = local
	ctx := context.Background()
	asset, err := local.Put(ctx, video.Asset{
		ID: "prompt-ok-10-accept.mp4", Kind: "video", Filename: "accept.mp4",
		ContentType: "video/mp4",
		Metadata: map[string]string{
			"project_id": "comfyui-acceptance", "job_id": "job-comfyui",
			"provider": "comfyui", "node_id": "10", "filename": "accept.mp4",
			"subfolder": "renders", "type": "output",
		},
	}, []byte("comfyui video bytes"))
	if err != nil {
		t.Fatal(err)
	}
	job := video.NewJob("job-comfyui", validAPIRequest())
	job.State = video.StateCompleted
	job.OutputAssets = []video.Asset{asset}
	if err := h.Store.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	route := "/v1/video/jobs/job-comfyui/assets/prompt-ok-10-accept.mp4"

	authorized := performJSON(h, http.MethodGet, route, nil, "Bearer test-token")
	if authorized.Code != http.StatusOK || authorized.Body.String() != "comfyui video bytes" {
		t.Fatalf("authorized download status=%d body=%q", authorized.Code, authorized.Body.String())
	}
	if got := authorized.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("Content-Type=%q, want video/mp4", got)
	}
	if got := authorized.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options=%q, want nosniff", got)
	}
	if got := authorized.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", got)
	}

	missing := performJSON(h, http.MethodGet, route, nil, "")
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status=%d, want 401", missing.Code)
	}
	wrong := performJSON(h, http.MethodGet, route, nil, "Bearer wrong-token")
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status=%d, want 401", wrong.Code)
	}

	for _, traversal := range []string{
		"..", "%2e%2e", "..%2f..%2fsecret",
		"prompt-ok-10-accept.mp4%2f..%2f..%2fsecret",
	} {
		got := performJSON(h, http.MethodGet, "/v1/video/jobs/job-comfyui/assets/"+traversal, nil, "Bearer test-token")
		if got.Code != http.StatusNotFound {
			t.Fatalf("traversal asset %q status=%d, want 404", traversal, got.Code)
		}
	}
	unknownJob := performJSON(h, http.MethodGet, "/v1/video/jobs/no-such-job/assets/prompt-ok-10-accept.mp4", nil, "Bearer test-token")
	if unknownJob.Code != http.StatusNotFound {
		t.Fatalf("unknown job status=%d, want 404", unknownJob.Code)
	}

	// Same-size tamper: the stored bytes change but the recorded
	// size and SHA-256 do not, so serving must fail closed.
	raw, err := os.ReadFile(asset.URI)
	if err != nil {
		t.Fatal(err)
	}
	tampered := make([]byte, len(raw))
	for i, b := range raw {
		if b >= 'a' && b <= 'z' {
			tampered[i] = b - 'a' + 'A'
		} else {
			tampered[i] = b
		}
	}
	if string(tampered) == string(raw) {
		t.Fatal("tamper fixture did not change the stored bytes")
	}
	if err := os.WriteFile(asset.URI, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	afterTamper := performJSON(h, http.MethodGet, route, nil, "Bearer test-token")
	if afterTamper.Code != http.StatusInternalServerError {
		t.Fatalf("tampered asset status=%d, want 500 integrity failure", afterTamper.Code)
	}
	if !strings.Contains(afterTamper.Body.String(), "asset_integrity_error") {
		t.Fatalf("tampered asset body=%s, want asset_integrity_error", afterTamper.Body.String())
	}
}

// A stored asset record whose file is gone must not be served and
// must not read as a transient outage: the file can never come back
// by retrying, so the gateway answers 404, not 503. Fixture only.
func TestHandlerComfyUIAssetMissingFileReturnsNotFound(t *testing.T) {
	h := newTestHandler(true)
	root := t.TempDir()
	local, err := storage.NewLocal(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	h.Assets = local
	ctx := context.Background()
	asset, err := local.Put(ctx, video.Asset{
		ID: "prompt-ok-10-gone.mp4", Kind: "video", Filename: "gone.mp4",
		ContentType: "video/mp4",
		Metadata: map[string]string{
			"project_id": "comfyui-acceptance", "job_id": "job-comfyui",
			"provider": "comfyui", "node_id": "10", "filename": "gone.mp4",
			"subfolder": "renders", "type": "output",
		},
	}, []byte("comfyui video bytes"))
	if err != nil {
		t.Fatal(err)
	}
	job := video.NewJob("job-comfyui", validAPIRequest())
	job.State = video.StateCompleted
	job.OutputAssets = []video.Asset{asset}
	if err := h.Store.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(asset.URI); err != nil {
		t.Fatal(err)
	}
	got := performJSON(h, http.MethodGet, "/v1/video/jobs/job-comfyui/assets/prompt-ok-10-gone.mp4", nil, "Bearer test-token")
	if got.Code != http.StatusNotFound {
		t.Fatalf("missing-file asset status=%d, want 404", got.Code)
	}
	if !strings.Contains(got.Body.String(), "asset_not_found") {
		t.Fatalf("missing-file asset body=%s, want asset_not_found", got.Body.String())
	}
}
