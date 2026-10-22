package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/agent"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// storeDurability must name exactly what the configured store
// guarantees — the Studio renders this verbatim, so a wrong label
// would overstate durability to operators.
func TestStoreDurabilityLabels(t *testing.T) {
	dir := t.TempDir()
	fileStore, err := queue.NewFileStore(dir + "/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		store queue.JobStore
		want  string
	}{
		{"file", fileStore, "file"},
		{"memory", queue.NewMemoryStore(), "process"},
		{"postgres", &queue.PostgresStore{}, "postgres"},
	}
	for _, tc := range cases {
		if got := storeDurability(tc.store); got != tc.want {
			t.Fatalf("%s store durability = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestHandlerForceFail(t *testing.T) {
	h := newTestHandler(false)
	ctx := context.Background()

	// Create a job through the public API; it starts non-terminal.
	w := performJSON(h, http.MethodPost, "/v1/video/jobs", validAPIRequest(), "")
	if w.Code != http.StatusCreated && w.Code != http.StatusOK && w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, body %s", w.Code, w.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id, _ := created["job_id"].(string)
	if id == "" {
		// Some shapes nest the job object.
		if job, ok := created["job"].(map[string]any); ok {
			id, _ = job["job_id"].(string)
		}
	}
	if id == "" {
		t.Fatalf("create response has no job id: %s", w.Body.String())
	}

	attempted, confirmed, err := h.ForceFail(ctx, id)
	if err != nil {
		t.Fatalf("force-fail active job: %v", err)
	}
	if attempted || confirmed {
		t.Fatalf("no provider job existed yet: attempted=%v confirmed=%v, want both false", attempted, confirmed)
	}
	job, err := h.Store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != video.StateFailed || !job.OperatorForceFailed {
		t.Fatalf("job after force-fail = %+v, want failed + operator flag", job)
	}
	if !strings.Contains(job.LastError, "Force-failed by operator") {
		t.Fatalf("job LastError = %q, want the operator force-fail wording", job.LastError)
	}
	// A terminal job cannot be force-failed again.
	if _, _, err := h.ForceFail(ctx, id); err == nil {
		t.Fatal("second force-fail of a terminal job succeeded")
	}
	// Unknown job and unconfigured handler both error.
	if _, _, err := h.ForceFail(ctx, "missing"); err == nil {
		t.Fatal("force-fail of unknown job succeeded")
	}
	var bare Handler
	if _, _, err := bare.ForceFail(ctx, id); err == nil {
		t.Fatal("force-fail without orchestrator succeeded")
	}
}

type stubPlanner struct{ out string }

func (s stubPlanner) Complete(context.Context, string, string, string) (string, error) {
	return s.out, nil
}

const canaryPlanJSON = `{"schema_version":"video-plan.v1","title":"T","duration_seconds":4,"width":320,"height":240,"fps":12,"scenes":[{"id":"a","duration_seconds":2,"background":"#101828","elements":[{"type":"text","text":"Hello","x":0.5,"y":0.4,"font_size":40,"color":"#ffffff"}]},{"id":"b","duration_seconds":2,"background":"#ffffff","elements":[{"type":"shape","shape":"rect","x":0.1,"y":0.1,"width":0.5,"height":0.2,"color":"#f79009"}]}]}`

func TestUniversalPlanEndpoint(t *testing.T) {
	// Not configured: the endpoint must say so (503), not fabricate a plan.
	h := newTestHandler(false)
	w := performJSON(h, http.MethodPost, "/v1/video/universal/plan",
		map[string]any{"prompt": "a sunrise", "model": "m"}, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured plan status = %d, want 503 (%s)", w.Code, w.Body.String())
	}

	h.Universal = &agent.Agent{Planner: stubPlanner{out: canaryPlanJSON}, MaxAttempts: 1}
	// Validation: prompt and model are required.
	w = performJSON(h, http.MethodPost, "/v1/video/universal/plan",
		map[string]any{"prompt": "  "}, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty plan request status = %d, want 400", w.Code)
	}
	// Aspect ratio defaults: 9:16 maps to portrait dimensions, visible
	// in the returned plan's canvas via the agent request — here we
	// assert the happy path returns a plan and the honesty notice.
	w = performJSON(h, http.MethodPost, "/v1/video/universal/plan",
		map[string]any{"prompt": "a sunrise", "model": "m", "aspect_ratio": "9:16"}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("plan status = %d, body %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode plan response: %v", err)
	}
	if body["plan"] == nil {
		t.Fatalf("plan response missing plan: %v", body)
	}
	if notice, _ := body["notice"].(string); !strings.Contains(notice, "not native generative video") {
		t.Fatalf("plan response notice = %q, want the deterministic-renderer disclosure", notice)
	}
	if mode, _ := body["creation_mode"].(string); mode != "programmatic" {
		t.Fatalf("creation_mode = %q, want programmatic", mode)
	}
}
