package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// #414 slice 1: the server-side job history endpoint the Studio
// renders — newest first, filterable, with the store's real
// durability class named.
func TestListJobsEndpoint(t *testing.T) {
	store := queue.NewMemoryStore()
	ctx := context.Background()
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	jobs := []video.VideoJob{
		{JobID: "job-old", State: video.StateCompleted, Provider: "fake", Model: "fake-video-1", CreatedAt: base, Request: video.VideoRequest{Prompt: "ocean sunrise"}},
		{JobID: "job-new", State: video.StateProcessing, Provider: "fake", Model: "fake-video-1", CreatedAt: base.Add(time.Hour), Request: video.VideoRequest{Prompt: "city timelapse"}},
		{JobID: "job-failed", State: video.StateFailed, Provider: "other", Model: "m2", CreatedAt: base.Add(2 * time.Hour), Request: video.VideoRequest{Prompt: "ocean storm"}},
	}
	for _, j := range jobs {
		if err := store.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{Store: store}

	decode := func(path string) map[string]any {
		t.Helper()
		w := performJSON(h, http.MethodGet, path, nil, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return body
	}
	ids := func(body map[string]any) []string {
		t.Helper()
		arr, _ := body["jobs"].([]any)
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			m, _ := item.(map[string]any)
			id, _ := m["job_id"].(string)
			out = append(out, id)
		}
		return out
	}

	all := decode("/v1/video/jobs")
	if got := ids(all); len(got) != 3 || got[0] != "job-failed" || got[2] != "job-old" {
		t.Fatalf("list order = %v, want newest first [job-failed ... job-old]", got)
	}
	if all["durability"] != "process" {
		t.Fatalf("durability = %v, want process for the memory store", all["durability"])
	}
	// Every listed job carries the derived workflow.
	arr, _ := all["jobs"].([]any)
	first, _ := arr[0].(map[string]any)
	wf, _ := first["workflow"].([]any)
	if len(wf) != 9 {
		t.Fatalf("workflow steps = %d, want 9", len(wf))
	}

	byState := decode("/v1/video/jobs?state=failed")
	if got := ids(byState); len(got) != 1 || got[0] != "job-failed" {
		t.Fatalf("state filter = %v, want [job-failed]", got)
	}
	byQuery := decode("/v1/video/jobs?q=ocean")
	if got := ids(byQuery); len(got) != 2 {
		t.Fatalf("q filter = %v, want the two ocean jobs", got)
	}
	limited := decode("/v1/video/jobs?limit=1")
	if got := ids(limited); len(got) != 1 || got[0] != "job-failed" {
		t.Fatalf("limit = %v, want [job-failed]", got)
	}
}

func TestListJobsRequiresStore(t *testing.T) {
	h := &Handler{}
	w := performJSON(h, http.MethodGet, "/v1/video/jobs", nil, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 without a store", w.Code)
	}
}
