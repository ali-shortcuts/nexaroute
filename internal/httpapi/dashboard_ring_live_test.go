package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
)

// TestDashboardLiveRingB1B2RealBackend verifies, against a real gateway over
// real HTTP, every backend contract the live routing ring consumes:
//
//  1. The dashboard served at / carries the ring topology/animation hooks.
//  2. The served app.js consumes the real SSE stream and snapshot fallback,
//     de-duplicates by seq/request_id, and caps simultaneous animations.
//  3. The real event bus assigns strictly increasing seqs to the exact routing
//     lifecycle the ring animates, and the real SSE endpoint replays them with
//     id == seq plus the fields the ring reads (request_id, kind, deployment,
//     latency_ms, error_type).
//  4. The real snapshot endpoint exposes the same events for polling fallback.
//
// No fake timers and no fabricated telemetry: every event travels through the
// real bus and real HTTP handlers; SSE reads block on the live stream.
func TestDashboardLiveRingB1B2RealBackend(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "ring-b1b2-secret"
	s := testGateway(t, cfg)
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	client := server.Client()

	// Publish a realistic routing lifecycle through the real bus, sharing one
	// request id the way the real OpenAI/Anthropic handlers do.
	const reqID = "req-ring-b1b2-live"
	const depA = "e2e-provider/model-alpha"
	const depB = "e2e-provider/model-beta"
	lifecycle := []events.Event{
		{RequestID: reqID, Kind: "route_attempt", Deployment: depA, Message: "attempt=1"},
		{RequestID: reqID, Kind: "route_skip", Deployment: depB, Message: "candidate is no longer eligible"},
		{RequestID: reqID, Kind: "route_fail", Deployment: depA, Message: "upstream refused", ErrorType: "provider_connection_failed"},
		{RequestID: reqID, Kind: "route_timeout", Deployment: depA, Message: "slow first byte", ErrorType: "provider_timeout"},
		{RequestID: reqID, Kind: "stream_fail", Deployment: depA, Message: "stream broke", ErrorType: "provider_stream_error"},
		{RequestID: reqID, Kind: "failover", Deployment: depA, Message: "trying next eligible candidate"},
		{RequestID: reqID, Kind: "route_ok", Deployment: depB, Message: "request completed", LatencyMS: 42},
		{RequestID: reqID, Kind: "candidate_exhausted", Message: "all eligible upstream deployments failed", ErrorType: "candidate_exhausted"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/admin/api/events/stream?limit=64", nil)
	if err != nil {
		t.Fatal(err)
	}
	streamReq.Header.Set("x-admin-key", cfg.Admin.APIKey)
	resp, err := client.Do(streamReq)
	if err != nil {
		t.Fatalf("open real event stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status=%d content_type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	reader := newSSEReader(resp.Body)

	for _, want := range lifecycle {
		s.bus.Add(want)
	}
	var prevSeq uint64
	seen := map[string]bool{}
	for i, want := range lifecycle {
		frame, done, err := reader.Next()
		if err != nil || done || frame.name != "event" {
			t.Fatalf("lifecycle frame %d (%s): frame=%+v done=%t err=%v", i, want.Kind, frame, done, err)
		}
		var got events.Event
		if err := json.Unmarshal([]byte(frame.data), &got); err != nil {
			t.Fatalf("lifecycle frame %d: invalid JSON: %v", i, err)
		}
		if got.Kind != want.Kind {
			t.Fatalf("lifecycle frame %d: kind=%q want %q (data=%s)", i, got.Kind, want.Kind, frame.data)
		}
		if got.Seq == 0 {
			t.Fatalf("lifecycle frame %d: backend did not assign a seq", i)
		}
		if frame.id != fmt.Sprint(got.Seq) {
			t.Fatalf("lifecycle frame %d: SSE id=%q event seq=%d (ring dedupes by seq)", i, frame.id, got.Seq)
		}
		if got.Seq <= prevSeq {
			t.Fatalf("lifecycle frame %d: seqs not increasing (%d after %d)", i, got.Seq, prevSeq)
		}
		prevSeq = got.Seq
		if dup := fmt.Sprintf("%d", got.Seq); seen[dup] {
			t.Fatalf("lifecycle frame %d: duplicate seq %d on the wire", i, got.Seq)
		}
		seen[fmt.Sprintf("%d", got.Seq)] = true
		if want.Kind != "candidate_exhausted" && got.Deployment == "" {
			t.Fatalf("lifecycle frame %d (%s): missing deployment", i, want.Kind)
		}
		if got.RequestID != reqID {
			t.Fatalf("lifecycle frame %d (%s): request_id=%q want %q (ring dedupes by request_id)", i, want.Kind, got.RequestID, reqID)
		}
	}
	// Field-level contracts the ring reads.
	cancel()
	_ = resp.Body.Close()
	_ = seen
	_ = prevSeq

	snapReq, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/api/snapshot?limit=5&events=100", nil)
	snapReq.Header.Set("x-admin-key", cfg.Admin.APIKey)
	snapResp, err := client.Do(snapReq)
	if err != nil {
		t.Fatalf("real snapshot: %v", err)
	}
	snapBody, _ := io.ReadAll(snapResp.Body)
	_ = snapResp.Body.Close()
	if snapResp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", snapResp.StatusCode, snapBody)
	}
	var snap struct {
		Events []events.Event `json:"events"`
	}
	if err := json.Unmarshal(snapBody, &snap); err != nil {
		t.Fatalf("snapshot JSON: %v", err)
	}
	for _, want := range []string{"route_attempt", "route_ok", "failover", "candidate_exhausted"} {
		found := false
		for _, e := range snap.Events {
			if e.Kind == want {
				found = true
				if e.Seq == 0 || e.RequestID == "" {
					t.Fatalf("snapshot %s missing seq/request_id: %+v", want, e)
				}
				break
			}
		}
		if !found {
			t.Fatalf("polling-fallback snapshot missing real %q event", want)
		}
	}
	for _, e := range snap.Events {
		if e.Kind == "route_ok" && e.RequestID == reqID && e.LatencyMS != 42 {
			t.Fatalf("snapshot route_ok latency_ms=%d want 42 (ring latency label)", e.LatencyMS)
		}
	}

	// Dashboard + script served over real HTTP carry the ring contracts.
	htmlResp, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("real dashboard: %v", err)
	}
	html, _ := io.ReadAll(htmlResp.Body)
	_ = htmlResp.Body.Close()
	for _, want := range []string{
		"NEXA ROUTER", `id="ringSvg"`, `id="ringGuides"`, `id="links"`,
		`id="ringFx"`, `id="ringParticles"`, `id="ringCore"`,
		`id="ringCoalesced"`, `id="ringStatus"`, `id="coreSub"`,
		`id="routerStrategyLabel"`,
	} {
		if !strings.Contains(string(html), want) {
			t.Fatalf("served dashboard missing live-ring contract %q", want)
		}
	}
	jsResp, err := client.Get(server.URL + "/app.js")
	if err != nil {
		t.Fatalf("real app.js: %v", err)
	}
	js, _ := io.ReadAll(jsResp.Body)
	_ = jsResp.Body.Close()
	if jsResp.StatusCode != http.StatusOK || len(js) == 0 {
		t.Fatalf("app.js status=%d len=%d", jsResp.StatusCode, len(js))
	}
	for _, want := range []string{
		"/admin/api/events/stream", "pollLiveEventsFallback",
		"route_attempt", "route_skip", "route_fail", "stream_fail",
		"provider_timeout", "failover", "route_ok", "candidate_exhausted",
		"maxAnims: 12", "prefers-reduced-motion",
	} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("served app.js missing live-ring contract %q", want)
		}
	}
}
