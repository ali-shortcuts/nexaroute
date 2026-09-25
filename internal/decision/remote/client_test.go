package remote

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

func TestPostJSONSuccessAndHeaders(t *testing.T) {
	var gotAuth, gotCT, gotUA string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotCT, gotUA = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Header.Get("User-Agent")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	c := NewClient()
	out, err := c.PostJSON(context.Background(), srv.URL, "SECRET_JEV_KEY_CANARY_21df", []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"code":0}` {
		t.Fatalf("body=%q", out)
	}
	if gotAuth != "Bearer SECRET_JEV_KEY_CANARY_21df" {
		t.Fatalf("auth=%q", gotAuth)
	}
	if gotCT != "application/json" || gotUA != UserAgent {
		t.Fatalf("ct=%q ua=%q", gotCT, gotUA)
	}
	if string(gotBody) != `{"a":1}` {
		t.Fatalf("sent=%q", gotBody)
	}
}

func TestPostJSONMissingKeyMakesNoCall(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	_, err := NewClient().PostJSON(context.Background(), srv.URL, "", []byte(`{}`))
	if err == nil {
		t.Fatal("missing key must fail")
	}
	var re *Error
	if !errors.As(err, &re) || re.DecisionReason() != decision.ReasonExternalProviderUnavailable {
		t.Fatalf("err=%v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("no network call may happen without a key")
	}
}

func TestPostJSONRequestTooLargeMakesNoCall(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	big := make([]byte, MaxRequestBytes+1)
	_, err := NewClient().PostJSON(context.Background(), srv.URL, "k", big)
	if err == nil {
		t.Fatal("oversize request must fail")
	}
	var re *Error
	if !errors.As(err, &re) || re.DecisionReason() != decision.ReasonExternalRequestTooLarge {
		t.Fatalf("err=%v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("oversize request must never be sent")
	}
}

func TestPostJSONResponseTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, MaxResponseBytes+16))
	}))
	defer srv.Close()
	_, err := NewClient().PostJSON(context.Background(), srv.URL, "k", []byte(`{}`))
	var re *Error
	if !errors.As(err, &re) || re.DecisionReason() != decision.ReasonExternalResponseTooLarge {
		t.Fatalf("err=%v", err)
	}
}

func TestPostJSONHTTPErrorHidesBody(t *testing.T) {
	const canary = "SECRET_REMOTE_ERROR_CANARY_64ac"
	for _, code := range []int{400, 401, 403, 429, 500, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"` + canary + `"}`))
		}))
		_, err := NewClient().PostJSON(context.Background(), srv.URL, "k", []byte(`{}`))
		srv.Close()
		var re *Error
		if !errors.As(err, &re) || re.DecisionReason() != decision.ReasonExternalHTTPError {
			t.Fatalf("code=%d err=%v", code, err)
		}
		if re.StatusCode() != code {
			t.Fatalf("status=%d", re.StatusCode())
		}
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("remote body leaked into error: %v", err)
		}
	}
}

func TestPostJSONNoRetryOnFailure(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	_, _ = NewClient().PostJSON(context.Background(), srv.URL, "k", []byte(`{}`))
	if hits.Load() != 1 {
		t.Fatalf("hits=%d, want exactly 1 (no retry)", hits.Load())
	}
}

func TestPostJSONRejectsRedirectWithoutForwardingAuth(t *testing.T) {
	var secondHits atomic.Int64
	var secondAuth atomic.Value
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		secondAuth.Store(r.Header.Get("Authorization"))
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/other", http.StatusFound)
	}))
	defer first.Close()
	_, err := NewClient().PostJSON(context.Background(), first.URL, "SECRET_JEV_KEY_CANARY_21df", []byte(`{}`))
	if err == nil {
		t.Fatal("redirect must be rejected")
	}
	if secondHits.Load() != 0 {
		t.Fatal("redirect target must never be contacted")
	}
	if v, _ := secondAuth.Load().(string); v != "" {
		t.Fatalf("auth forwarded across redirect: %q", v)
	}
}

func TestPostJSONCanceledContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewClient().PostJSON(ctx, srv.URL, "k", []byte(`{}`))
	var re *Error
	if !errors.As(err, &re) || !re.Timeout() {
		t.Fatalf("err=%v", err)
	}
	if re.DecisionReason() != decision.ReasonExternalTimeout {
		t.Fatalf("reason=%q", re.DecisionReason())
	}
}

func TestPostJSONMidflightCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-release:
			return
		case <-time.After(5 * time.Second):
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := NewClient().PostJSON(ctx, srv.URL, "k", []byte(`{}`))
	if time.Since(start) > 4*time.Second {
		t.Fatal("mid-flight cancellation did not abort the request")
	}
	if err == nil {
		t.Fatal("canceled request must fail")
	}
}

func TestPostJSONConnectionRefusedIsTyped(t *testing.T) {
	// Port 1 is unroutable: connection refused, fast, no network needed.
	_, err := NewClient().PostJSON(context.Background(), "http://127.0.0.1:1/x", "k", []byte(`{}`))
	var re *Error
	if !errors.As(err, &re) {
		t.Fatalf("err=%v (%T), want typed remote error", err, err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("transport detail leaked: %v", err)
	}
}

func TestRemoteErrorNeverCarriesSecrets(t *testing.T) {
	e := NewHTTPError(500)
	if strings.Contains(e.Error(), "SECRET") {
		t.Fatal("template must be static")
	}
	var nilErr *Error
	if nilErr.Error() == "" || nilErr.DecisionReason() != decision.ReasonProviderError {
		t.Fatal("nil error must degrade safely")
	}
}
