package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

type memSink struct {
	fail bool
	put  int
}

func (m *memSink) Put(_ context.Context, a video.Asset, _ []byte) (video.Asset, error) {
	if m.fail {
		return video.Asset{}, fmt.Errorf("sink exploded")
	}
	m.put++
	return a, nil
}

// adversarialComfyUI serves failure shapes: mode selects the /prompt
// behavior, history the /history response, queue the /queue response.
func adversarialComfyUI(t *testing.T, promptMode string, historyBody string, queueBody string, viewBody []byte, viewStatus int) *ComfyUI {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		switch promptMode {
		case "error":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "no-id":
			fmt.Fprint(w, `{}`)
		case "oversize":
			w.Write(make([]byte, (8<<20)+1))
		default:
			fmt.Fprint(w, `{"prompt_id":"p1"}`)
		}
	})
	mux.HandleFunc("/history/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, historyBody)
	})
	mux.HandleFunc("/queue", func(w http.ResponseWriter, r *http.Request) {
		if queueBody == "ERROR" {
			http.Error(w, "boom", 500)
			return
		}
		fmt.Fprint(w, queueBody)
	})
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(viewStatus)
		w.Write(viewBody)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	path := comfyWorkflowFile(t)
	p, err := NewComfyUI(comfyTestConfig(path, server.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func queueJSON(running, pending []string) string {
	pair := func(ids []string) string {
		out := "["
		for i, id := range ids {
			if i > 0 {
				out += ","
			}
			out += fmt.Sprintf(`[0,"%s"]`, id)
		}
		return out + "]"
	}
	return fmt.Sprintf(`{"queue_running":%s,"queue_pending":%s}`, pair(running), pair(pending))
}

func TestComfyUIAdversarialServer(t *testing.T) {
	ctx := context.Background()
	req := validComfyRequest()

	// CreateJob: upstream 500.
	p := adversarialComfyUI(t, "error", `{}`, queueJSON(nil, nil), nil, 200)
	if _, err := p.CreateJob(ctx, req); err == nil || !strings.Contains(err.Error(), "submit ComfyUI workflow") {
		t.Errorf("create 500: %v", err)
	}
	// CreateJob: 200 but no prompt_id.
	p = adversarialComfyUI(t, "no-id", `{}`, queueJSON(nil, nil), nil, 200)
	if _, err := p.CreateJob(ctx, req); err == nil || !strings.Contains(err.Error(), "prompt_id") {
		t.Errorf("create no-id: %v", err)
	}
	// doJSON: oversize JSON response.
	p = adversarialComfyUI(t, "oversize", `{}`, queueJSON(nil, nil), nil, 200)
	if _, err := p.CreateJob(ctx, req); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("create oversize: %v", err)
	}
	// doJSON: unencodable payload.
	if err := p.doJSON(ctx, http.MethodPost, "/prompt", func() {}, nil); err == nil {
		t.Error("doJSON unencodable payload accepted")
	}
	// doJSON: undecodable response body.
	p = adversarialComfyUI(t, "ok", `not json`, queueJSON(nil, nil), nil, 200)
	if _, err := p.GetJob(ctx, "p1"); err == nil {
		t.Error("getjob bad history accepted")
	}

	// GetJob: prompt running in queue.
	p = adversarialComfyUI(t, "ok", `{}`, queueJSON([]string{"p1"}, nil), nil, 200)
	st, err := p.GetJob(ctx, "p1")
	if err != nil || st.State != video.StateProcessing || st.Progress != 0.25 {
		t.Errorf("running: %+v %v", st, err)
	}
	// GetJob: prompt pending in queue.
	p = adversarialComfyUI(t, "ok", `{}`, queueJSON(nil, []string{"p1"}), nil, 200)
	st, err = p.GetJob(ctx, "p1")
	if err != nil || st.State != video.StateQueued {
		t.Errorf("pending: %+v %v", st, err)
	}
	// GetJob: absent everywhere.
	p = adversarialComfyUI(t, "ok", `{}`, queueJSON(nil, nil), nil, 200)
	if _, err := p.GetJob(ctx, "p1"); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Errorf("absent: %v", err)
	}
	// GetJob: queue unreadable.
	p = adversarialComfyUI(t, "ok", `{}`, "ERROR", nil, 200)
	if _, err := p.GetJob(ctx, "p1"); err == nil || !strings.Contains(err.Error(), "read ComfyUI queue") {
		t.Errorf("queue error: %v", err)
	}
	// GetJob: execution error in history.
	p = adversarialComfyUI(t, "ok", `{"p1":{"status":{"status_str":"error","completed":false},"outputs":{}}}`, queueJSON(nil, nil), nil, 200)
	st, err = p.GetJob(ctx, "p1")
	if err != nil || st.State != video.StateFailed || st.Error == "" {
		t.Errorf("history error: %+v %v", st, err)
	}
	// GetJob: completed without MP4 output.
	p = adversarialComfyUI(t, "ok", `{"p1":{"status":{"status_str":"success","completed":true},"outputs":{"9":{"gifs":[{"filename":"a.gif","subfolder":"","type":"output"}]}}}}`, queueJSON(nil, nil), nil, 200)
	st, err = p.GetJob(ctx, "p1")
	if err != nil || st.State != video.StateFailed || !strings.Contains(st.Error, "without an MP4") {
		t.Errorf("no mp4: %+v %v", st, err)
	}

	// CancelJob: running prompts refuse the global interrupt.
	p = adversarialComfyUI(t, "ok", `{}`, queueJSON([]string{"p1"}, nil), nil, 200)
	if err := p.CancelJob(ctx, "p1"); err == nil || !strings.Contains(err.Error(), "refusing global interrupt") {
		t.Errorf("cancel running: %v", err)
	}
	// CancelJob: absent prompts are not marked cancelled.
	p = adversarialComfyUI(t, "ok", `{}`, queueJSON(nil, nil), nil, 200)
	if err := p.CancelJob(ctx, "p1"); err == nil || !strings.Contains(err.Error(), "not pending") {
		t.Errorf("cancel absent: %v", err)
	}
	// CancelJob: pending prompts are deleted.
	p = adversarialComfyUI(t, "ok", `{}`, queueJSON(nil, []string{"p1"}), nil, 200)
	if err := p.CancelJob(ctx, "p1"); err != nil {
		t.Errorf("cancel pending: %v", err)
	}
	// CancelJob: empty id.
	if err := p.CancelJob(ctx, "  "); err == nil {
		t.Error("cancel empty id accepted")
	}

	// DownloadOutputs guards.
	p = adversarialComfyUI(t, "ok", `{}`, queueJSON(nil, nil), []byte("vid"), 200)
	asset := video.Asset{ID: "a", Filename: "o.mp4", Metadata: map[string]string{"filename": "o.mp4", "subfolder": "", "type": "output"}}
	status := video.ProviderJobStatus{ProviderJobID: "p1", State: video.StateCompleted, Outputs: []video.Asset{asset}}
	if _, err := p.DownloadOutputs(ctx, status, nil); err == nil {
		t.Error("nil sink accepted")
	}
	if _, err := p.DownloadOutputs(ctx, video.ProviderJobStatus{}, &memSink{}); err == nil {
		t.Error("no outputs accepted")
	}
	got, err := p.DownloadOutputs(ctx, status, &memSink{})
	if err != nil || len(got) != 1 || got[0].ContentType != "video/mp4" {
		t.Errorf("download: %+v %v", got, err)
	}
	// Sink failure surfaces as a persist error.
	if _, err := p.DownloadOutputs(ctx, status, &memSink{fail: true}); err == nil || !strings.Contains(err.Error(), "persist") {
		t.Errorf("sink failure: %v", err)
	}
	// Oversize asset refused.
	big := make([]byte, 2048)
	p = adversarialComfyUI(t, "ok", `{}`, queueJSON(nil, nil), big, 200)
	if _, err := p.DownloadOutputs(ctx, status, &memSink{}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversize download: %v", err)
	}
	// Empty body refused.
	p = adversarialComfyUI(t, "ok", `{}`, queueJSON(nil, nil), nil, 200)
	if _, err := p.DownloadOutputs(ctx, status, &memSink{}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty download: %v", err)
	}
	// HTTP error refused.
	p = adversarialComfyUI(t, "ok", `{}`, queueJSON(nil, nil), nil, 404)
	if _, err := p.DownloadOutputs(ctx, status, &memSink{}); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("404 download: %v", err)
	}
}

func TestComfyUIDownloadRejectsNonVideoContentType(t *testing.T) {
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>"))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	path := comfyWorkflowFile(t)
	p, err := NewComfyUI(comfyTestConfig(path, server.URL), 1024)
	if err != nil {
		t.Fatal(err)
	}
	asset := video.Asset{ID: "a", Filename: "o.mp4", Metadata: map[string]string{"filename": "o.mp4"}}
	status := video.ProviderJobStatus{Outputs: []video.Asset{asset}}
	if _, err := p.DownloadOutputs(ctx, status, &memSink{}); err == nil || !strings.Contains(err.Error(), "non-video") {
		t.Errorf("non-video: %v", err)
	}
}

func TestComfyUIHistoryCompletedWithMP4(t *testing.T) {
	ctx := context.Background()
	history := `{"p1":{"status":{"status_str":"success","completed":true},"outputs":{"9":{"gifs":[{"filename":"out.mp4","subfolder":"","type":"output"}]}}}}`
	p := adversarialComfyUI(t, "ok", history, queueJSON(nil, nil), nil, 200)
	st, err := p.GetJob(ctx, "p1")
	if err != nil || st.State != video.StateCompleted || len(st.Outputs) != 1 {
		t.Fatalf("completed: %+v %v", st, err)
	}
}
