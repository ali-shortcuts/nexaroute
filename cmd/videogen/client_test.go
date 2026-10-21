package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestAPIClientDownloadsAssetToPrivateFileWithoutOverwriting(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/video/jobs/job-123/assets/asset-1" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("authorization=%q", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("video bytes"))
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, "secret-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "episode.mp4")
	if err := client.DownloadAsset(context.Background(), "job-123", "asset-1", out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != "video bytes" {
		t.Fatalf("download=%q err=%v", data, err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("download file mode=%o, want 0600", info.Mode().Perm())
	}
	if err := client.DownloadAsset(context.Background(), "job-123", "asset-1", out); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second download error=%v, want refusal to overwrite", err)
	}
	if requests != 1 {
		t.Fatalf("requests=%d, existing output should fail before network request", requests)
	}
}

func TestAPIClientDownloadAssetReportsGatewayErrorsAndValidatesPaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"asset_not_found","message":"asset not found"}`))
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, "secret-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "missing.mp4")
	if err := client.DownloadAsset(context.Background(), "job-123", "missing", out); err == nil || !strings.Contains(err.Error(), "asset not found") {
		t.Fatalf("download error=%v, want asset-not-found message", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output file unexpectedly exists, stat error=%v", err)
	}
	if err := client.DownloadAsset(context.Background(), " ", "asset", out); err == nil {
		t.Fatal("empty job ID accepted")
	}
	if err := client.DownloadAsset(context.Background(), "job", "asset", " "); err == nil {
		t.Fatal("empty output path accepted")
	}
}

func TestAPIClientRefusesCrossOriginRedirects(t *testing.T) {
	var destinationHits atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"providers":[]}`))
	}))
	defer destination.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+r.URL.Path, http.StatusFound)
	}))
	defer source.Close()

	client, err := newAPIClient(source.URL, "secret-token", source.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Providers(context.Background()); err == nil || !strings.Contains(err.Error(), "cross-origin") {
		t.Fatalf("providers redirect error=%v, want cross-origin refusal", err)
	}
	if hits := destinationHits.Load(); hits != 0 {
		t.Fatalf("cross-origin redirect reached destination %d time(s)", hits)
	}

	out := filepath.Join(t.TempDir(), "asset.mp4")
	if err := client.DownloadAsset(context.Background(), "job-123", "asset-1", out); err == nil || !strings.Contains(err.Error(), "cross-origin") {
		t.Fatalf("asset redirect error=%v, want cross-origin refusal", err)
	}
	if hits := destinationHits.Load(); hits != 0 {
		t.Fatalf("asset redirect reached destination %d time(s)", hits)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused download left an output file, stat error=%v", err)
	}
}

func TestAPIClientFollowsSameOriginRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/video/providers" {
			http.Redirect(w, r, "/providers-final", http.StatusFound)
			return
		}
		if r.URL.Path != "/providers-final" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"providers":[{"id":"fake"}]}`))
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, "secret-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	providers, err := client.Providers(context.Background())
	if err != nil {
		t.Fatalf("same-origin redirect was refused: %v", err)
	}
	if len(providers.Providers) != 1 || providers.Providers[0].ID != "fake" {
		t.Fatalf("providers=%+v", providers)
	}
}

func TestAPIClientPreservesCallerRedirectPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer server.Close()

	httpClient := server.Client()
	httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return errors.New("caller policy stopped redirect")
	}
	client, err := newAPIClient(server.URL, "secret-token", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Providers(context.Background()); err == nil || !strings.Contains(err.Error(), "caller policy stopped redirect") {
		t.Fatalf("caller redirect policy was not preserved: %v", err)
	}
}
