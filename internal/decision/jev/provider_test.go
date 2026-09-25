package jev

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/decision/remote"
)

func jevRequest() decision.DecisionRequest {
	cands := []decision.Candidate{
		{ID: "p1/model-a", PoolOrdinal: 0, Priority: 0, ContextWindow: 32000, Streaming: true, Observations: 10},
		{ID: "p2/model-b", PoolOrdinal: 0, Priority: 0, ContextWindow: 200000, Tools: true, Reasoning: true, Streaming: true, EWMALatencyMS: 120, Observations: 100},
		{ID: "p3/model-c", PoolOrdinal: 1, Priority: 0},
	}
	return decision.DecisionRequest{
		RequestID:         "r1",
		Candidates:        cands,
		AllowedPrimaryIDs: []string{"p1/model-a", "p2/model-b"},
		Features:          decision.RequestFeatures{Tools: true, EstimatedContextTokens: 2000, EstimatedInputTokens: 1500, MaxOutputTokens: 500},
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New("", "k", PrivacyMetadataOnly); err == nil {
		t.Fatal("empty ID must fail")
	}
	if _, err := New("x", "k", "full_context"); err == nil {
		t.Fatal("non-metadata_only privacy must fail")
	}
	p, err := New("jev-main", "k", PrivacyMetadataOnly)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID() != "jev-main" || p.Type() != ProviderType {
		t.Fatalf("id=%q type=%q", p.ID(), p.Type())
	}
	if !p.Capabilities().CanSelect || p.Capabilities().CanRank {
		t.Fatalf("caps=%+v (select-only)", p.Capabilities())
	}
}

func TestHealthMissingKey(t *testing.T) {
	p, _ := New("jev-main", "", PrivacyMetadataOnly)
	h := p.Health()
	if h.Available || h.KeyConfigured {
		t.Fatalf("health=%+v", h)
	}
	p2, _ := New("jev-main", "k", PrivacyMetadataOnly)
	if h := p2.Health(); !h.Available || !h.KeyConfigured {
		t.Fatalf("health=%+v", h)
	}
}

func TestDecideValidSelectionOpaque(t *testing.T) {
	const physicalCanary = "p2/model-b"
	var gotBody []byte
	var gotAuth string
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"decision":"c1","confidence":0.9}}`))
	}))
	defer srv.Close()
	p, err := NewWithTransport("jev-main", "test-key", PrivacyMetadataOnly, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Decide(context.Background(), jevRequest())
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d, want 1", hits.Load())
	}
	if res.Action != decision.ActionSelect || res.SelectedID != physicalCanary {
		t.Fatalf("res=%+v", res)
	}
	if res.Confidence != 0.9 || res.ReasonCodes[0] != decision.ReasonExternalSelected {
		t.Fatalf("res=%+v", res)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("auth=%q", gotAuth)
	}
	body := string(gotBody)
	// Opaque IDs only: no physical deployment ID may appear.
	for _, physical := range []string{"p1/model-a", "p2/model-b", "p3/model-c"} {
		if strings.Contains(body, physical) {
			t.Fatalf("request leaks physical ID %q: %s", physical, body)
		}
	}
	// Fallback-pool candidate p3 must not be sent at all (only the band).
	if strings.Contains(body, `"c2"`) {
		t.Fatalf("request includes out-of-band candidate: %s", body)
	}
	if !strings.Contains(body, `"c0"`) || !strings.Contains(body, `"c1"`) {
		t.Fatalf("request missing opaque IDs: %s", body)
	}
	if len(gotBody) > 32*1024 {
		t.Fatalf("len=%d", len(gotBody))
	}
}

func TestDecideMissingKeyFailsWithoutNetwork(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	p, _ := NewWithTransport("jev-main", "", PrivacyMetadataOnly, srv.URL, nil)
	_, err := p.Decide(context.Background(), jevRequest())
	var re *remote.Error
	if !errors.As(err, &re) || re.DecisionReason() != decision.ReasonExternalProviderUnavailable {
		t.Fatalf("err=%v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("missing key must fail before any network call")
	}
}

func TestDecideTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	defer close(release)
	p, _ := NewWithTransport("jev-main", "k", PrivacyMetadataOnly, srv.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.Decide(ctx, jevRequest())
	if time.Since(start) > 4*time.Second {
		t.Fatal("provider ignored context cancellation")
	}
	var re *remote.Error
	if !errors.As(err, &re) || re.DecisionReason() != decision.ReasonExternalTimeout {
		t.Fatalf("err=%v", err)
	}
}

func TestDecideHTTPFailuresTyped(t *testing.T) {
	for _, code := range []int{400, 401, 403, 429, 500, 503} {
		var hits atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"code":1,"message":"SECRET_REMOTE_ERROR_CANARY_64ac"}`))
		}))
		p, _ := NewWithTransport("jev-main", "k", PrivacyMetadataOnly, srv.URL, nil)
		_, err := p.Decide(context.Background(), jevRequest())
		srv.Close()
		var re *remote.Error
		if !errors.As(err, &re) || re.DecisionReason() != decision.ReasonExternalHTTPError {
			t.Fatalf("code=%d err=%v", code, err)
		}
		if strings.Contains(err.Error(), "SECRET_REMOTE_ERROR_CANARY_64ac") {
			t.Fatalf("remote body leaked: %v", err)
		}
		if hits.Load() != 1 {
			t.Fatalf("code=%d hits=%d, want 1 (no retry)", code, hits.Load())
		}
	}
}

func TestDecideNonzeroCodeFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":1001,"message":"bad","data":{"decision":"c0"}}`))
	}))
	defer srv.Close()
	p, _ := NewWithTransport("jev-main", "k", PrivacyMetadataOnly, srv.URL, nil)
	_, err := p.Decide(context.Background(), jevRequest())
	var re *remote.Error
	if !errors.As(err, &re) || re.DecisionReason() != decision.ReasonExternalInvalidResponse {
		t.Fatalf("err=%v", err)
	}
}

func TestDecideUnknownChoiceFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"decision":"c9"}}`))
	}))
	defer srv.Close()
	p, _ := NewWithTransport("jev-main", "k", PrivacyMetadataOnly, srv.URL, nil)
	_, err := p.Decide(context.Background(), jevRequest())
	var re *remote.Error
	if !errors.As(err, &re) || re.DecisionReason() != decision.ReasonExternalUnknownCandidate {
		t.Fatalf("err=%v", err)
	}
}

func TestDecideSingleAllowedAbstains(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	p, _ := NewWithTransport("jev-main", "k", PrivacyMetadataOnly, srv.URL, nil)
	req := jevRequest()
	req.AllowedPrimaryIDs = []string{"p1/model-a"}
	res, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != decision.ActionAbstain {
		t.Fatalf("res=%+v", res)
	}
	if hits.Load() != 0 {
		t.Fatal("single allowed primary must not trigger a network call")
	}
}

func TestDecideOversizeRequestSendsNothing(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	p, _ := NewWithTransport("jev-main", "k", PrivacyMetadataOnly, srv.URL, nil)
	cands := make([]decision.Candidate, 0, MaxCandidates)
	allowed := make([]string, 0, MaxCandidates)
	for i := 0; i < MaxCandidates; i++ {
		id := "p/model"
		cands = append(cands, decision.Candidate{ID: id, ContextWindow: 1000000, Tools: true, Streaming: true})
		allowed = append(allowed, id)
	}
	// Unique IDs (duplicates would collapse in the lookup map).
	for i := range cands {
		cands[i].ID += "-" + strconv.Itoa(i)
		allowed[i] = cands[i].ID
	}
	req := decision.DecisionRequest{Candidates: cands, AllowedPrimaryIDs: allowed}
	_, err := p.Decide(context.Background(), req)
	var re *remote.Error
	if !errors.As(err, &re) || re.DecisionReason() != decision.ReasonExternalRequestTooLarge {
		t.Fatalf("err=%v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("oversize request must fail before any network call")
	}
}

func TestDecideOversizeResponseFailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 70*1024))
	}))
	defer srv.Close()
	p, _ := NewWithTransport("jev-main", "k", PrivacyMetadataOnly, srv.URL, nil)
	_, err := p.Decide(context.Background(), jevRequest())
	var re *remote.Error
	if !errors.As(err, &re) || re.DecisionReason() != decision.ReasonExternalResponseTooLarge {
		t.Fatalf("err=%v", err)
	}
}

func TestDecideConnectionRefused(t *testing.T) {
	p, _ := NewWithTransport("jev-main", "k", PrivacyMetadataOnly, "http://127.0.0.1:1/x", nil)
	_, err := p.Decide(context.Background(), jevRequest())
	var re *remote.Error
	if !errors.As(err, &re) {
		t.Fatalf("err=%v (%T)", err, err)
	}
}
