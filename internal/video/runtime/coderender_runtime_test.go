package runtime_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/runtime"
)

const runtimePlanJSON = `{"schema_version":"video-plan.v1","title":"Runtime universal","duration_seconds":3,"width":320,"height":240,"fps":12,"scenes":[{"id":"a","duration_seconds":1.5,"background":"#101828","elements":[{"type":"text","text":"Planned by a text model","x":0.5,"y":0.4,"font_size":36,"color":"#ffffff"}]},{"id":"b","duration_seconds":1.5,"background":"#ffffff","elements":[{"type":"chart","data":[1,4,2],"x":0.15,"y":0.25,"width":0.7,"height":0.45,"color":"#2e90fa"}]}]}`

// fakePlannerServer is an OpenAI-compatible chat endpoint standing in for
// the gateway data plane (or any configured text/coding model endpoint).
func fakePlannerServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "deepseek-chat" {
			http.Error(w, `{"error":"unknown model"}`, http.StatusBadRequest)
			return
		}
		content, _ := json.Marshal(runtimePlanJSON)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + string(content) + `}}]}`))
	}))
}

// TestRuntimeCodeRenderEndToEnd proves the full gateway path: a configured
// text-only model plans a video through the Universal Video Agent, the job
// runs through the existing Video Gateway lifecycle, and the finished MP4
// is downloadable through the authenticated asset endpoint.
func TestRuntimeCodeRenderEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	planner := fakePlannerServer(t)
	defer planner.Close()
	const tokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN_CR"
	t.Setenv(tokenEnv, "runtime-cr-token")
	r, err := runtime.New(video.Config{
		Enabled: true, StorePath: "jobs.json", StorageRoot: "assets",
		QueueSize: 4, Workers: 1, AuthTokenEnv: tokenEnv,
		CodeRenderEnabled: true, CodeRenderPlannerBaseURL: planner.URL,
		CodeRenderPlannerModel: "deepseek-chat", CodeRenderMaxDurationSeconds: 120,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.Start(ctx); err != nil {
		cancel()
		t.Fatalf("runtime.Start: %v", err)
	}
	defer func() {
		cancel()
		r.Close()
	}()
	auth := "Bearer runtime-cr-token"
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		r.Handler.ServeHTTP(rec, req)
		return rec
	}
	// Providers listing exposes capabilities and planning models honestly.
	rec := do("GET", "/v1/video/providers", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"code-render"`) || !strings.Contains(rec.Body.String(), `"deepseek-chat"`) {
		t.Fatalf("providers = %d %s", rec.Code, rec.Body.String())
	}
	// Unauthenticated planning is refused.
	req := httptest.NewRequest("POST", "/v1/video/universal/plan", strings.NewReader(`{"prompt":"x","model":"deepseek-chat"}`))
	rec = httptest.NewRecorder()
	r.Handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("unauthenticated plan status = %d", rec.Code)
	}
	// Plan-preview endpoint returns a validated plan for review.
	rec = do("POST", "/v1/video/universal/plan", `{"prompt":"Explain routing","model":"deepseek-chat","duration_seconds":3,"aspect_ratio":"16:9"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"video-plan.v1"`) {
		t.Fatalf("plan preview = %d %s", rec.Code, rec.Body.String())
	}
	// Create the job with the text-only planning model.
	rec = do("POST", "/v1/video/jobs", `{"project_id":"universal","prompt":"Explain routing","mode":"text_to_video","provider_preference":"code-render","model_preference":"deepseek-chat","duration_seconds":3,"aspect_ratio":"16:9","width":320,"height":240,"fps":12,"creation_mode":"programmatic"}`)
	if rec.Code != 202 {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var created video.VideoJob
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	// Poll to completion through the existing job API.
	var job video.VideoJob
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		rec = do("GET", "/v1/video/jobs/"+created.JobID, "")
		if rec.Code != 200 {
			t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if job.State == video.StateCompleted || job.State == video.StateFailed {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if job.State != video.StateCompleted {
		t.Fatalf("job state = %s, error = %s", job.State, job.LastError)
	}
	if len(job.OutputAssets) != 1 {
		t.Fatalf("output assets = %+v", job.OutputAssets)
	}
	if job.UsageMetadata["native_video"] != "false" || job.UsageMetadata["planning_model"] != "deepseek-chat" {
		t.Fatalf("job must record honest provenance: %v", job.UsageMetadata)
	}
	// Download the asset through the authenticated endpoint.
	asset := job.OutputAssets[0]
	rec = do("GET", "/v1/video/jobs/"+created.JobID+"/assets/"+asset.ID, "")
	if rec.Code != 200 {
		t.Fatalf("asset download = %d %s", rec.Code, rec.Body.String())
	}
	data, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 1000 || !strings.Contains(string(data[:12]), "ftyp") {
		t.Fatalf("downloaded asset is not a real MP4 (%d bytes)", len(data))
	}
	if int64(len(data)) != asset.SizeBytes {
		t.Fatalf("downloaded size %d != asset size %d", len(data), asset.SizeBytes)
	}
}

func TestRuntimeCodeRenderRequiresPlanningModel(t *testing.T) {
	const tokenEnv = "NEXAROUTE_TEST_VIDEO_TOKEN_CR2"
	t.Setenv(tokenEnv, "x")
	_, err := runtime.New(video.Config{
		Enabled: true, AuthTokenEnv: tokenEnv,
		CodeRenderEnabled: true, CodeRenderPlannerBaseURL: "http://127.0.0.1:9",
	}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "planning model") {
		t.Fatalf("expected planning-model requirement error, got %v", err)
	}
}
