package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// trickleUpstream sends SSE headers immediately, then small chunks forever
// without ever terminating: the shape of a hung provider that idle timeouts
// alone cannot kill.
func trickleUpstream(chunkEvery time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		if fl != nil {
			fl.Flush()
		}
		for {
			select {
			case <-time.After(chunkEvery):
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
				if fl != nil {
					fl.Flush()
				}
			case <-r.Context().Done():
				return
			}
		}
	}))
}

func streamChatBody(model string) string {
	return fmt.Sprintf(`{"model":%q,"stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, model)
}

func TestStreamMaxDurationKillsTricklingStream(t *testing.T) {
	trickle := trickleUpstream(100 * time.Millisecond)
	defer trickle.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, trickle, solid, func(c *config.Config) {
		c.Routing.StreamMaxDurationSeconds = 1
		c.Routing.RequestTimeoutMS = 30000
	})
	rr := postChat(t, s, streamChatBody("up-m1"))
	// The response commits (200 + partial chunks) and then dies by the
	// stream deadline; committed streams cannot fail over.
	if rr.Code != 200 {
		t.Fatalf("trickling stream must commit 200, got %d", rr.Code)
	}
	sawFail := false
	for _, e := range s.bus.Snapshot() {
		if e.Kind == "stream_fail" && e.Deployment == "flaky/m1" {
			sawFail = true
			if e.ErrorType != "provider_timeout" {
				t.Fatalf("expired stream must report provider_timeout, got %q", e.ErrorType)
			}
		}
		if e.Kind == "route_ok" {
			t.Fatalf("killed stream must not record route_ok: %+v", e)
		}
	}
	if !sawFail {
		t.Fatal("expected stream_fail for the trickling deployment")
	}
}

func TestStreamMaxDurationZeroKeepsLegacyBehavior(t *testing.T) {
	trickle := trickleUpstream(100 * time.Millisecond)
	defer trickle.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	// White-box routeContext check: disabled bound means no deadline.
	ctx, cancel := routeContext(context.Background(), true, time.Second, 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("stream_max_duration 0 must leave streams without a deadline")
	}
	ctx2, cancel2 := routeContext(context.Background(), true, time.Second, time.Minute)
	defer cancel2()
	if _, ok := ctx2.Deadline(); !ok {
		t.Fatal("configured stream_max_duration must bound streams")
	}
	_ = trickle
	_ = solid
}

type errWriter struct{ err error }

func (w errWriter) Header() http.Header       { return http.Header{} }
func (w errWriter) Write([]byte) (int, error) { return 0, w.err }
func (w errWriter) WriteHeader(int)           {}

func TestWriteHelpersPropagateErrors(t *testing.T) {
	w := errWriter{err: fmt.Errorf("client gone")}
	if err := writeFlushed(w, []byte("x")); err == nil || !strings.Contains(err.Error(), "client gone") {
		t.Fatalf("writeFlushed must propagate write errors, got %v", err)
	} else if !AsClientWriteError(err) {
		t.Fatalf("write failures must be typed as client write errors, got %T", err)
	}
	if err := writeOnce(w, []byte("x")); err == nil {
		t.Fatal("writeOnce must propagate write errors")
	} else if !AsClientWriteError(err) {
		t.Fatalf("write failures must be typed as client write errors, got %T", err)
	}
	if AsClientWriteError(fmt.Errorf("upstream boom")) {
		t.Fatal("upstream errors must not classify as client write errors")
	}
	if AsClientWriteError(nil) {
		t.Fatal("nil must not classify as a client write error")
	}
}

func TestWriteFlushedOverRealConnection(t *testing.T) {
	// Exercises the ResponseController path (set + clear the write
	// deadline) over a real keep-alive connection: two sequential requests
	// must both succeed, proving a stale deadline never poisons the
	// connection for the next request.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		if err := writeFlushed(w, []byte("data: hi\n\n")); err != nil {
			t.Errorf("writeFlushed over real conn: %v", err)
		}
	}))
	defer srv.Close()
	for i := 0; i < 2; i++ {
		resp, err := srv.Client().Get(srv.URL)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		resp.Body.Close()
		if !strings.Contains(string(buf[:n]), "data: hi") {
			t.Fatalf("request %d: unexpected body %q", i, buf[:n])
		}
	}
}

func TestWriteFlushedFallsBackOnRecorders(t *testing.T) {
	rr := httptest.NewRecorder()
	if err := writeFlushed(rr, []byte("data: x\n\n")); err != nil {
		t.Fatalf("recorder write must succeed: %v", err)
	}
	if !rr.Flushed || !strings.Contains(rr.Body.String(), "data: x") {
		t.Fatalf("recorder must receive flushed bytes: flushed=%v body=%q", rr.Flushed, rr.Body.String())
	}
}

func TestNativeSSETrackerRejectsOversizedLine(t *testing.T) {
	var tr nativeSSETracker
	chunk := bytes.Repeat([]byte("x"), 256<<10)
	var err error
	for i := 0; i < (maxNativeSSELineBytes/len(chunk))+2; i++ {
		err = tr.consume(chunk)
		if err != nil {
			break
		}
	}
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized SSE line must be rejected, got %v", err)
	}
}
