package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

func TestAPIClientGatewayLifecycle(t *testing.T) {
	var seenCreate video.VideoRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("authorization=%q, want bearer token", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/video/providers":
			_ = json.NewEncoder(w).Encode(map[string]any{"providers": []map[string]string{{"id": "fake"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/video/jobs":
			if err := json.NewDecoder(r.Body).Decode(&seenCreate); err != nil {
				t.Errorf("decode create request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(video.NewJob("job-123", seenCreate))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/video/jobs/job-123":
			_ = json.NewEncoder(w).Encode(video.NewJob("job-123", seenCreate))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/video/jobs/job-123/cancel":
			job := video.NewJob("job-123", seenCreate)
			job.State = video.StateCancelled
			_ = json.NewEncoder(w).Encode(job)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, "secret-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	providers, err := client.Providers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers.Providers) != 1 || providers.Providers[0].ID != "fake" {
		t.Fatalf("providers=%+v", providers)
	}

	req := video.VideoRequest{ProjectID: "project-1", Prompt: "A sunrise", DurationSeconds: 4, AspectRatio: video.Aspect16x9}
	created, err := client.CreateJob(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if created.JobID != "job-123" || created.State != video.StateQueued {
		t.Fatalf("created job=%+v", created)
	}
	if seenCreate.ProjectID != req.ProjectID || seenCreate.Prompt != req.Prompt {
		t.Fatalf("request sent to gateway=%+v", seenCreate)
	}

	got, err := client.GetJob(ctx, "job-123")
	if err != nil {
		t.Fatal(err)
	}
	if got.JobID != "job-123" {
		t.Fatalf("got job=%+v", got)
	}
	cancelled, err := client.CancelJob(ctx, "job-123")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != video.StateCancelled {
		t.Fatalf("cancelled job=%+v", cancelled)
	}
}

func TestAPIClientRejectsInsecureRemoteHTTP(t *testing.T) {
	for _, base := range []string{"http://example.com", "http://192.0.2.10:8080"} {
		if _, err := newAPIClient(base, "secret-token", nil); err == nil {
			t.Errorf("newAPIClient(%q) unexpectedly succeeded", base)
		}
	}
	if _, err := newAPIClient("https://example.com", "secret-token", nil); err != nil {
		t.Errorf("HTTPS base URL rejected: %v", err)
	}
	if _, err := newAPIClient("http://127.0.0.1:8080", "secret-token", nil); err != nil {
		t.Errorf("loopback HTTP base URL rejected: %v", err)
	}
}

func TestAPIClientSurfacesGatewayErrorsWithoutToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized","message":"authentication required"}`))
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, "do-not-print-this", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Providers(context.Background())
	if err == nil || !strings.Contains(err.Error(), "authentication required") {
		t.Fatalf("error=%v, want gateway authentication error", err)
	}
	if strings.Contains(err.Error(), "do-not-print-this") {
		t.Fatalf("error leaked bearer token: %v", err)
	}
}

func TestAPIClientValidatesArgumentsAndBaseURL(t *testing.T) {
	if _, err := newAPIClient("file:///tmp/nexaroute", "token", nil); err == nil {
		t.Fatal("non-HTTP URL accepted")
	}
	if _, err := newAPIClient("https://user:pass@example.com", "token", nil); err == nil {
		t.Fatal("URL user info accepted")
	}
	client, err := newAPIClient("http://127.0.0.1:8080", "token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetJob(context.Background(), " "); err == nil {
		t.Fatal("empty job ID accepted")
	}
	if _, err := client.CancelJob(context.Background(), " "); err == nil {
		t.Fatal("empty cancel job ID accepted")
	}
}
