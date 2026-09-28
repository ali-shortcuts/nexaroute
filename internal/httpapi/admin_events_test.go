package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
)

func TestAdminEventStreamRequiresAuthAndStreamsSnapshotAndLive(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "admin-event-secret"
	s := testGateway(t, cfg)
	s.bus.Add(events.Event{Kind: "historical_event", Message: "before connect"})

	unauthorized := httptest.NewRecorder()
	unauthorizedRequest := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/events/stream", nil)
	// Keep the intentional failed auth attempt in a distinct loopback bucket so
	// it cannot consume the live client's admin-rate-limit allowance.
	unauthorizedRequest.RemoteAddr = "127.0.0.2:1234"
	s.Handler().ServeHTTP(unauthorized, unauthorizedRequest)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	server := httptest.NewServer(s.Handler())
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/admin/api/events/stream?limit=8", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-admin-key", cfg.Admin.APIKey)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status=%d content_type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	reader := newSSEReader(resp.Body)
	first, done, err := reader.Next()
	if err != nil || done || first.name != "event" {
		t.Fatalf("snapshot frame=%+v done=%t err=%v", first, done, err)
	}
	var historical events.Event
	if err := json.Unmarshal([]byte(first.data), &historical); err != nil || historical.Kind != "historical_event" {
		t.Fatalf("snapshot event=%+v err=%v", historical, err)
	}

	s.bus.Add(events.Event{Kind: "live_event", Message: "after connect"})
	second, done, err := reader.Next()
	if err != nil || done || second.name != "event" {
		t.Fatalf("live frame=%+v done=%t err=%v", second, done, err)
	}
	var live events.Event
	if err := json.Unmarshal([]byte(second.data), &live); err != nil || live.Kind != "live_event" {
		t.Fatalf("live event=%+v err=%v", live, err)
	}
	cancel()
	_ = resp.Body.Close()
}

func TestAdminEventStreamRejectsInvalidLimit(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "admin-event-secret"
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/events/stream?limit=0", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("x-admin-key", cfg.Admin.APIKey)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminEventStreamDisconnectReleasesSubscriber(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "admin-event-secret"
	s := testGateway(t, cfg)
	server := httptest.NewServer(s.Handler())
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/admin/api/events/stream", nil)
	req.Header.Set("x-admin-key", cfg.Admin.APIKey)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = resp.Body.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, _, release, ok := s.bus.SubscribeSnapshot(1)
		if ok {
			release()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("stream disconnect did not release subscriber")
}
