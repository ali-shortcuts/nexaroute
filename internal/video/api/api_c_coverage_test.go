package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

// Agent C coverage: a filename containing a control character cannot be
// quoted, so mime.FormatMediaType serves it RFC 2231-encoded instead of
// failing; the download must still succeed with that disposition.

func TestGetAssetDispositionEncodesUnusualFilename(t *testing.T) {
	local, err := storage.NewLocal(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("clip-data-1234")
	stored := mission95StoredAsset(t, local, data)
	stored.Filename = "a\x01b.mp4"
	stored.SHA256 = "" // no checksum, so the download reaches disposition

	store := queue.NewMemoryStore()
	mission95JobWithAssets(t, store, "job-disp", []video.Asset{stored}, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/video/jobs/job-disp/assets/"+stored.ID, nil)
	(&Handler{Store: store, Assets: local}).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("download status=%d body=%s", w.Code, w.Body.String())
	}
	if disp := w.Header().Get("Content-Disposition"); !strings.Contains(disp, `filename*=utf-8''a%01b.mp4`) {
		t.Fatalf("disposition=%q, want the RFC 2231-encoded filename", disp)
	}
	if w.Body.String() != string(data) {
		t.Fatalf("body=%q, want %q", w.Body.String(), data)
	}
}
