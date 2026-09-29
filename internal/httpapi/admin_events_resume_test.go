package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
)

// readStreamEvents opens an authenticated admin event stream and collects n
// data events (skipping keepalive comments), failing on timeout.
func readStreamEvents(t *testing.T, url, adminKey string, headers map[string]string, n int) []sseEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-admin-key", adminKey)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream status=%d body=%s", resp.StatusCode, body)
	}
	reader := newSSEReader(resp.Body)
	out := make([]sseEvent, 0, n)
	for len(out) < n {
		ev, done, err := reader.Next()
		if err != nil {
			t.Fatalf("stream read: %v", err)
		}
		if done {
			t.Fatalf("stream closed after %d/%d events", len(out), n)
		}
		out = append(out, ev)
	}
	return out
}

func streamSeqs(t *testing.T, evs []sseEvent) []uint64 {
	t.Helper()
	seqs := make([]uint64, 0, len(evs))
	for _, ev := range evs {
		if ev.name != "event" {
			t.Fatalf("expected data event, got name=%q data=%q", ev.name, ev.data)
		}
		seq, err := strconv.ParseUint(ev.id, 10, 64)
		if err != nil {
			t.Fatalf("event id=%q is not a sequence number: %v", ev.id, err)
		}
		var e events.Event
		if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
			t.Fatalf("event data is not JSON: %v", err)
		}
		if e.Seq != seq {
			t.Fatalf("frame id=%d disagrees with payload seq=%d", seq, e.Seq)
		}
		seqs = append(seqs, seq)
	}
	return seqs
}

func TestAdminEventStreamSinceQueryResumes(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "since-query-secret"
	s := testGateway(t, cfg)
	s.bus.Add(events.Event{Kind: "e1"})
	s.bus.Add(events.Event{Kind: "e2"})
	s.bus.Add(events.Event{Kind: "e3"})
	first := s.bus.Snapshot()[0].Seq

	server := httptest.NewServer(s.Handler())
	defer server.Close()

	evs := readStreamEvents(t, server.URL+"/admin/api/events/stream?since="+strconv.FormatUint(first, 10)+"&limit=8", cfg.Admin.APIKey, nil, 2)
	seqs := streamSeqs(t, evs)
	if len(seqs) != 2 || seqs[0] != first+1 || seqs[1] != first+2 {
		t.Fatalf("since-query resume seqs=%v want [%d %d]", seqs, first+1, first+2)
	}
	var kinds []string
	for _, ev := range evs {
		var e events.Event
		_ = json.Unmarshal([]byte(ev.data), &e)
		kinds = append(kinds, e.Kind)
	}
	if kinds[0] != "e2" || kinds[1] != "e3" {
		t.Fatalf("since-query resume kinds=%v want [e2 e3]", kinds)
	}
}

func TestAdminEventStreamQuerySinceBeatsHeader(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "since-precedence-secret"
	s := testGateway(t, cfg)
	s.bus.Add(events.Event{Kind: "e1"})
	s.bus.Add(events.Event{Kind: "e2"})
	all := s.bus.Snapshot()

	server := httptest.NewServer(s.Handler())
	defer server.Close()

	// Header says "already saw everything", query says "saw only the first".
	// The explicit query parameter must win, so e2 is replayed.
	evs := readStreamEvents(t,
		server.URL+"/admin/api/events/stream?since="+strconv.FormatUint(all[0].Seq, 10)+"&limit=8",
		cfg.Admin.APIKey,
		map[string]string{"Last-Event-ID": strconv.FormatUint(all[1].Seq, 10)},
		1)
	if evs[0].id != strconv.FormatUint(all[1].Seq, 10) {
		t.Fatalf("query since did not take precedence: id=%q want %d", evs[0].id, all[1].Seq)
	}
}

func TestAdminEventStreamOrderingIsMonotonic(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "ordering-secret"
	s := testGateway(t, cfg)
	const total = 20
	for i := 0; i < total; i++ {
		s.bus.Add(events.Event{Kind: fmt.Sprintf("ordered-%d", i)})
	}
	server := httptest.NewServer(s.Handler())
	defer server.Close()

	evs := readStreamEvents(t, server.URL+"/admin/api/events/stream?limit=64", cfg.Admin.APIKey, nil, total)
	seqs := streamSeqs(t, evs)
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("event seqs not strictly monotonic: %v", seqs)
		}
	}
}

func TestAdminEventStreamResumeAfterDisconnect(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "resume-disconnect-secret"
	s := testGateway(t, cfg)
	s.bus.Add(events.Event{Kind: "before"})

	server := httptest.NewServer(s.Handler())
	defer server.Close()

	// First connection: read the pre-existing event, then disconnect.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/admin/api/events/stream?limit=8", nil)
	req.Header.Set("x-admin-key", cfg.Admin.APIKey)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first, done, err := newSSEReader(resp.Body).Next()
	if err != nil || done {
		t.Fatalf("first frame err=%v done=%t", err, done)
	}
	lastSeen := first.id
	cancel()
	_ = resp.Body.Close()

	// Publish while disconnected, then resume from the last seen id.
	s.bus.Add(events.Event{Kind: "during-1"})
	s.bus.Add(events.Event{Kind: "during-2"})
	evs := readStreamEvents(t, server.URL+"/admin/api/events/stream?limit=8", cfg.Admin.APIKey,
		map[string]string{"Last-Event-ID": lastSeen}, 2)
	var kinds []string
	for _, ev := range evs {
		var e events.Event
		_ = json.Unmarshal([]byte(ev.data), &e)
		kinds = append(kinds, e.Kind)
	}
	if len(kinds) != 2 || kinds[0] != "during-1" || kinds[1] != "during-2" {
		t.Fatalf("resume after disconnect replayed %v want [during-1 during-2]", kinds)
	}
	for _, ev := range evs {
		if ev.id <= lastSeen {
			t.Fatalf("resume replayed already-seen event id=%q lastSeen=%q", ev.id, lastSeen)
		}
	}
}

func TestAdminEventStreamSlowClientDropsInsteadOfBlocking(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "slow-client-secret"
	s := testGateway(t, cfg)

	// Occupy one live subscription and never read from it: this is the slow
	// client. Producers must never block on it.
	_, slow, cancel, ok := s.bus.SubscribeSnapshot(1)
	if !ok {
		t.Fatal("could not subscribe slow client")
	}
	defer cancel()

	overflow := 4 * 32 // 4x the documented per-client buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < overflow; i++ {
			s.bus.Add(events.Event{Kind: "burst", Message: fmt.Sprintf("%d", i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("producer blocked on a slow subscriber: events were not dropped")
	}
	// The slow client's bounded channel must not have grown without bound.
	if got := len(slow); got > 32 {
		t.Fatalf("slow client buffered %d events, want <= per-client bound 32", got)
	}
}

func TestAdminEventStreamSubscriberExhaustionReturns503(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "subscriber-cap-secret"
	s := testGateway(t, cfg)

	// Fill the fixed subscriber budget directly on the bus.
	cancels := make([]func(), 0, 64)
	for i := 0; i < 64; i++ {
		_, _, cancel, ok := s.bus.SubscribeSnapshot(1)
		if !ok {
			t.Fatalf("subscription %d unexpectedly rejected", i)
		}
		cancels = append(cancels, cancel)
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/events/stream", nil)
	req.RemoteAddr = "127.0.0.9:1234"
	req.Header.Set("x-admin-key", cfg.Admin.APIKey)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("exhausted stream status=%d want 503 body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminEventStreamAuthRequired(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "stream-auth-secret"
	s := testGateway(t, cfg)

	cases := []struct {
		name   string
		key    string
		remote string
		want   int
	}{
		{"missing key", "", "127.0.0.11:1", http.StatusUnauthorized},
		{"wrong key", "nope", "127.0.0.12:1", http.StatusUnauthorized},
		{"correct key", "stream-auth-secret", "127.0.0.13:1", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// httptest.ResponseRecorder never closes the stream body, so the
			// handler blocks on the live channel. Run it detached and only
			// assert the committed status code.
			req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/events/stream?limit=1", nil)
			req.RemoteAddr = tc.remote
			if tc.key != "" {
				req.Header.Set("x-admin-key", tc.key)
			}
			rr := httptest.NewRecorder()
			statusCh := make(chan int, 1)
			go func() {
				s.Handler().ServeHTTP(rr, req)
				statusCh <- rr.Code
			}()
			select {
			case code := <-statusCh:
				if code != tc.want {
					t.Fatalf("status=%d want %d", code, tc.want)
				}
			case <-time.After(5 * time.Second):
				// A correct key keeps the SSE stream open; a timeout here
				// means the handler is streaming, i.e. authorized.
				if tc.want != http.StatusOK {
					t.Fatalf("request hung, want status %d", tc.want)
				}
			}
		})
	}
}

func TestAdminEventStreamRejectsInvalidSince(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "since-validation-secret"
	s := testGateway(t, cfg)

	for i, target := range []string{
		"http://gateway/admin/api/events/stream?since=not-a-number",
		"http://gateway/admin/api/events/stream?since=-5",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.RemoteAddr = fmt.Sprintf("127.0.0.21:%d", 1000+i)
		req.Header.Set("x-admin-key", cfg.Admin.APIKey)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("target %q status=%d want 400 body=%s", target, rr.Code, rr.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/events/stream", nil)
	req.RemoteAddr = "127.0.0.21:1009"
	req.Header.Set("x-admin-key", cfg.Admin.APIKey)
	req.Header.Set("Last-Event-ID", "bogus")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad Last-Event-ID status=%d want 400 body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminEventStreamEmitsKeepaliveCommentAndHeaders(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "keepalive-secret"
	s := testGateway(t, cfg)
	server := httptest.NewServer(s.Handler())
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/admin/api/events/stream", nil)
	req.Header.Set("x-admin-key", cfg.Admin.APIKey)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type=%q want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control=%q want no-cache", cc)
	}
	buf := make([]byte, 16)
	type readResult struct {
		n   int
		err error
	}
	readCh := make(chan readResult, 1)
	go func() {
		n, err := resp.Body.Read(buf)
		readCh <- readResult{n: n, err: err}
	}()
	select {
	case res := <-readCh:
		if res.err != nil {
			t.Fatalf("reading stream preamble: %v", res.err)
		}
		if !strings.HasPrefix(string(buf[:res.n]), ": connected") {
			t.Fatalf("stream did not open with a keepalive comment: %q", string(buf[:res.n]))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stream preamble comment")
	}
}

func TestAdminEventStreamGoroutineLeak(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "leak-check-secret"
	s := testGateway(t, cfg)
	server := httptest.NewServer(s.Handler())
	defer server.Close()

	// Warm up, then measure a baseline after GC settles.
	openClose := func() {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/admin/api/events/stream?limit=1", nil)
		req.Header.Set("x-admin-key", cfg.Admin.APIKey)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		cancel()
		_ = resp.Body.Close()
	}
	openClose()
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()

	const cycles = 20
	for i := 0; i < cycles; i++ {
		openClose()
	}
	// Every disconnect must release its subscriber: the bus must accept a
	// fresh subscriber immediately, and goroutines must settle back.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _, release, ok := s.bus.SubscribeSnapshot(1)
		if ok {
			release()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("subscriber capacity not released after connect/disconnect cycles")
		}
		time.Sleep(5 * time.Millisecond)
	}
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after-before > 5 {
		t.Fatalf("possible goroutine leak: before=%d after=%d (delta=%d) over %d open/close cycles",
			before, after, after-before, cycles)
	}
}
