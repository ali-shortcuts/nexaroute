package httpapi

// A9: isolated SSE seq/id/Last-Event-ID/?since resume contract.
//
// Scope: this file only adds tests. The snapshot endpoint
// (/admin/api/snapshot) is intentionally untouched. The resume contract
// under test lives in admin_events.go (adminEventStream) on top of
// events.Bus.SubscribeSnapshotSince:
//
//   - every data frame is wired exactly as
//     "id: <seq>\nevent: event\ndata: <event JSON>\n\n"
//     where <seq> is the monotonic bus sequence and id == payload seq.
//   - resume cursor is ?since=<seq> (takes precedence) else Last-Event-ID.
//   - only events with Seq > cursor are replayed: strictly increasing,
//     no duplicates, no reordering.
//   - bad cursors yield 400; missing/wrong admin key yields 401.
//
// All tests in this file match `go test -run SSE.*Resume` per issue #79.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
)

func sseResumeTestGateway(t *testing.T, key string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = key
	return testGateway(t, cfg)
}

// sseResumeOpen opens an authenticated stream and returns the live response.
// Caller must close resp.Body and cancel ctx.
func sseResumeOpen(t *testing.T, server *httptest.Server, key, target string, headers map[string]string) (context.Context, context.CancelFunc, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+target, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("x-admin-key", key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open event stream %s: %v", target, err)
	}
	return ctx, cancel, resp
}

// sseResumeReadFrames collects n data frames, skipping SSE comments.
func sseResumeReadFrames(t *testing.T, body io.Reader, n int) []sseEvent {
	t.Helper()
	reader := newSSEReader(body)
	out := make([]sseEvent, 0, n)
	deadline := time.Now().Add(10 * time.Second)
	for len(out) < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d/%d SSE data frames", len(out), n)
		}
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

// sseResumeReadRawFrame reads one raw SSE frame (bytes through blank line).
func sseResumeReadRawFrame(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	var sb strings.Builder
	for {
		if sb.Len() > 256*1024 {
			t.Fatalf("raw frame exceeded sane bound: %q...", sb.String()[:200])
		}
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("raw frame read: %v", err)
		}
		sb.WriteString(line)
		if line == "\n" || strings.HasSuffix(sb.String(), "\n\n") {
			return sb.String()
		}
	}
}

func sseResumeSeqs(t *testing.T, evs []sseEvent) []uint64 {
	t.Helper()
	seqs := make([]uint64, 0, len(evs))
	for i, ev := range evs {
		if ev.name != "event" {
			t.Fatalf("frame %d: event name=%q want %q (data=%q)", i, ev.name, "event", ev.data)
		}
		seq, err := strconv.ParseUint(strings.TrimSpace(ev.id), 10, 64)
		if err != nil || seq == 0 {
			t.Fatalf("frame %d: id=%q is not a positive sequence: %v", i, ev.id, err)
		}
		var e events.Event
		if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
			t.Fatalf("frame %d: data is not event JSON: %v", i, err)
		}
		if e.Seq != seq {
			t.Fatalf("frame %d: id=%d disagrees with payload seq=%d", i, seq, e.Seq)
		}
		seqs = append(seqs, seq)
	}
	return seqs
}

func sseResumeAssertStrictlyIncreasing(t *testing.T, seqs []uint64) {
	t.Helper()
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("seqs not strictly increasing (dup/reorder): %v", seqs)
		}
	}
}

// TestSSEWireFormatResume asserts the exact on-the-wire framing.
func TestSSEWireFormatResume(t *testing.T) {
	s := sseResumeTestGateway(t, "a9-wire-secret")
	s.bus.Add(events.Event{Kind: "wire-1", Message: "hello"})
	server := httptest.NewServer(s.Handler())
	defer server.Close()

	_, cancel, resp := sseResumeOpen(t, server, "a9-wire-secret", "/admin/api/events/stream?limit=4", nil)
	defer cancel()
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type=%q want text/event-stream", ct)
	}
	br := bufio.NewReader(resp.Body)
	// Preamble comment keeps idle streams committed without waiting for data.
	preamble := sseResumeReadRawFrame(t, br)
	if preamble != ": connected\n\n" {
		t.Fatalf("preamble=%q want %q", preamble, ": connected\n\n")
	}
	raw := sseResumeReadRawFrame(t, br)
	lines := strings.Split(strings.TrimSuffix(raw, "\n\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("wire frame lines=%q want 3 lines (id/event/data)", raw)
	}
	if !strings.HasPrefix(lines[0], "id: ") {
		t.Fatalf("wire line0=%q want prefix %q", lines[0], "id: ")
	}
	if lines[1] != "event: event" {
		t.Fatalf("wire line1=%q want %q", lines[1], "event: event")
	}
	if !strings.HasPrefix(lines[2], "data: ") {
		t.Fatalf("wire line2=%q want prefix %q", lines[2], "data: ")
	}
	idSeq, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(lines[0], "id: ")), 10, 64)
	if err != nil || idSeq == 0 {
		t.Fatalf("wire id=%q not a positive seq: %v", lines[0], err)
	}
	var payload events.Event
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[2], "data: ")), &payload); err != nil {
		t.Fatalf("wire data is not event JSON: %v", err)
	}
	if payload.Seq != idSeq {
		t.Fatalf("wire id=%d disagrees with payload seq=%d", idSeq, payload.Seq)
	}
	if payload.Kind != "wire-1" {
		t.Fatalf("wire payload kind=%q want wire-1", payload.Kind)
	}
	t.Logf("wire-format evidence: raw=%q", raw)
}

// TestSSESinceQueryResume covers ?since=<seq> replay.
func TestSSESinceQueryResume(t *testing.T) {
	s := sseResumeTestGateway(t, "a9-since-secret")
	s.bus.Add(events.Event{Kind: "e1"})
	s.bus.Add(events.Event{Kind: "e2"})
	s.bus.Add(events.Event{Kind: "e3"})
	first := s.bus.Snapshot()[0].Seq

	server := httptest.NewServer(s.Handler())
	defer server.Close()

	_, cancel, resp := sseResumeOpen(t, server, "a9-since-secret",
		"/admin/api/events/stream?since="+strconv.FormatUint(first, 10)+"&limit=8", nil)
	defer cancel()
	defer resp.Body.Close()

	evs := sseResumeReadFrames(t, resp.Body, 2)
	seqs := sseResumeSeqs(t, evs)
	if len(seqs) != 2 || seqs[0] != first+1 || seqs[1] != first+2 {
		t.Fatalf("since replay seqs=%v want [%d %d]", seqs, first+1, first+2)
	}
	sseResumeAssertStrictlyIncreasing(t, seqs)
}

// TestSSELastEventIDResume covers Last-Event-ID header replay.
func TestSSELastEventIDResume(t *testing.T) {
	s := sseResumeTestGateway(t, "a9-leid-secret")
	s.bus.Add(events.Event{Kind: "h1"})
	s.bus.Add(events.Event{Kind: "h2"})
	first := s.bus.Snapshot()[0].Seq

	server := httptest.NewServer(s.Handler())
	defer server.Close()

	_, cancel, resp := sseResumeOpen(t, server, "a9-leid-secret",
		"/admin/api/events/stream?limit=8",
		map[string]string{"Last-Event-ID": strconv.FormatUint(first, 10)})
	defer cancel()
	defer resp.Body.Close()

	evs := sseResumeReadFrames(t, resp.Body, 1)
	seqs := sseResumeSeqs(t, evs)
	if seqs[0] != first+1 {
		t.Fatalf("Last-Event-ID replay seq=%d want %d", seqs[0], first+1)
	}
}

// TestSSESincePrecedenceResume asserts ?since wins over Last-Event-ID.
func TestSSESincePrecedenceResume(t *testing.T) {
	s := sseResumeTestGateway(t, "a9-precedence-secret")
	s.bus.Add(events.Event{Kind: "p1"})
	s.bus.Add(events.Event{Kind: "p2"})
	all := s.bus.Snapshot()

	server := httptest.NewServer(s.Handler())
	defer server.Close()

	_, cancel, resp := sseResumeOpen(t, server, "a9-precedence-secret",
		"/admin/api/events/stream?since="+strconv.FormatUint(all[0].Seq, 10)+"&limit=8",
		map[string]string{"Last-Event-ID": strconv.FormatUint(all[1].Seq, 10)})
	defer cancel()
	defer resp.Body.Close()

	evs := sseResumeReadFrames(t, resp.Body, 1)
	if evs[0].id != strconv.FormatUint(all[1].Seq, 10) {
		t.Fatalf("query since did not take precedence: id=%q want %d", evs[0].id, all[1].Seq)
	}
}

// TestSSENoDuplicateReorderResume disconnects, publishes, resumes.
func TestSSENoDuplicateReorderResume(t *testing.T) {
	s := sseResumeTestGateway(t, "a9-nodup-secret")
	s.bus.Add(events.Event{Kind: "before"})

	server := httptest.NewServer(s.Handler())
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/admin/api/events/stream?limit=8", nil)
	req.Header.Set("x-admin-key", "a9-nodup-secret")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first, done, err := newSSEReader(resp.Body).Next()
	if err != nil || done {
		_ = resp.Body.Close()
		t.Fatalf("first frame err=%v done=%t", err, done)
	}
	lastSeen := strings.TrimSpace(first.id)
	cancel()
	_ = resp.Body.Close()

	s.bus.Add(events.Event{Kind: "during-1"})
	s.bus.Add(events.Event{Kind: "during-2"})

	_, cancel2, resp2 := sseResumeOpen(t, server, "a9-nodup-secret",
		"/admin/api/events/stream?limit=8",
		map[string]string{"Last-Event-ID": lastSeen})
	defer cancel2()
	defer resp2.Body.Close()

	evs := sseResumeReadFrames(t, resp2.Body, 2)
	seqs := sseResumeSeqs(t, evs)
	sseResumeAssertStrictlyIncreasing(t, seqs)
	lastSeenSeq, _ := strconv.ParseUint(lastSeen, 10, 64)
	seen := map[uint64]bool{}
	for _, q := range seqs {
		if q <= lastSeenSeq {
			t.Fatalf("resume replayed already-seen seq=%d lastSeen=%d", q, lastSeenSeq)
		}
		if seen[q] {
			t.Fatalf("duplicate seq=%d in resume: %v", q, seqs)
		}
		seen[q] = true
	}
	var kinds []string
	for _, ev := range evs {
		var e events.Event
		_ = json.Unmarshal([]byte(ev.data), &e)
		kinds = append(kinds, e.Kind)
	}
	if len(kinds) != 2 || kinds[0] != "during-1" || kinds[1] != "during-2" {
		t.Fatalf("resume kinds=%v want [during-1 during-2]", kinds)
	}
}

// TestSSEAuthEnforcedResume covers authenticated + unauthenticated cases.
func TestSSEAuthEnforcedResume(t *testing.T) {
	s := sseResumeTestGateway(t, "a9-auth-secret")
	s.bus.Add(events.Event{Kind: "guarded"})

	// Unauthenticated resume attempts must fail before any SSE bytes.
	unauth := []struct {
		name   string
		key    string
		remote string
		target string
		header map[string]string
	}{
		{"missing key plain", "", "127.0.0.31:1", "/admin/api/events/stream?limit=1", nil},
		{"wrong key plain", "nope", "127.0.0.32:1", "/admin/api/events/stream?limit=1", nil},
		{"missing key since", "", "127.0.0.33:1", "/admin/api/events/stream?since=0&limit=1", nil},
		{"wrong key leid", "nope", "127.0.0.34:1", "/admin/api/events/stream?limit=1", map[string]string{"Last-Event-ID": "0"}},
	}
	for _, tc := range unauth {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://gateway"+tc.target, nil)
			req.RemoteAddr = tc.remote
			if tc.key != "" {
				req.Header.Set("x-admin-key", tc.key)
			}
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.Handler().ServeHTTP(rr, req)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("unauthenticated request hung instead of returning 401")
			}
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d want 401 body=%s", rr.Code, rr.Body.String())
			}
		})
	}

	// Authenticated resume succeeds (both cursor styles).
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	for _, tc := range []struct {
		name   string
		target string
		header map[string]string
	}{
		{"auth since", "/admin/api/events/stream?since=0&limit=4", nil},
		{"auth leid", "/admin/api/events/stream?limit=4", map[string]string{"Last-Event-ID": "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cancel, resp := sseResumeOpen(t, server, "a9-auth-secret", tc.target, tc.header)
			defer cancel()
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("authenticated resume status=%d", resp.StatusCode)
			}
			evs := sseResumeReadFrames(t, resp.Body, 1)
			sseResumeSeqs(t, evs)
		})
	}
}

// TestSSEInvalidCursorResume asserts 400 on malformed cursors.
func TestSSEInvalidCursorResume(t *testing.T) {
	s := sseResumeTestGateway(t, "a9-cursor-secret")
	bad := []struct {
		name   string
		target string
		header map[string]string
	}{
		{"since text", "/admin/api/events/stream?since=not-a-number", nil},
		{"since negative", "/admin/api/events/stream?since=-5", nil},
		{"leid text", "/admin/api/events/stream", map[string]string{"Last-Event-ID": "bogus"}},
	}
	for i, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://gateway"+tc.target, nil)
			req.RemoteAddr = fmt.Sprintf("127.0.0.41:%d", 2000+i)
			req.Header.Set("x-admin-key", "a9-cursor-secret")
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400 body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestSSESnapshotUnchangedResume proves /admin/api/snapshot is untouched:
// JSON snapshot still serves bounded events with monotonic seq and is not
// reframed as SSE.
func TestSSESnapshotUnchangedResume(t *testing.T) {
	s := sseResumeTestGateway(t, "a9-snapshot-secret")
	s.bus.Add(events.Event{Kind: "snap-1"})
	s.bus.Add(events.Event{Kind: "snap-2"})

	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot?events=10", nil)
	req.RemoteAddr = "127.0.0.51:1"
	req.Header.Set("x-admin-key", "a9-snapshot-secret")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("snapshot Content-Type=%q want application/json", ct)
	}
	var snap struct {
		Events []events.Event `json:"events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatalf("snapshot is not JSON: %v", err)
	}
	if len(snap.Events) < 2 {
		t.Fatalf("snapshot events=%d want >=2", len(snap.Events))
	}
	for i := 1; i < len(snap.Events); i++ {
		if snap.Events[i].Seq <= snap.Events[i-1].Seq {
			t.Fatalf("snapshot seqs not increasing: %+v", snap.Events)
		}
	}
	if strings.Contains(rr.Body.String(), "text/event-stream") || strings.Contains(rr.Body.String(), "Last-Event-ID") {
		t.Fatalf("snapshot leaked SSE framing")
	}
}
