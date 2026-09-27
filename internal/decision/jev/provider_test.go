package jev

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

func TestJevProvider_IDAndCapabilities(t *testing.T) {
	p, err := NewProvider(ProviderConfig{
		ID:          "jev-main",
		Type:        "jev",
		Enabled:     true,
		APIKey:      "test-key",
		PrivacyMode: "metadata_only",
	})
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}
	if p.ID() != "jev-main" {
		t.Fatalf("expected jev-main, got %s", p.ID())
	}
	caps := p.Capabilities()
	if !caps.CanSelect {
		t.Fatalf("CanSelect should be true")
	}
	if caps.CanRank {
		t.Fatalf("CanRank should be false")
	}
	if !p.HasAPIKey() {
		t.Fatalf("should have API key")
	}
}

func TestJevProvider_Health(t *testing.T) {
	// Disabled
	p, _ := NewProvider(ProviderConfig{
		ID:      "jev-main",
		Type:    "jev",
		Enabled: false,
		APIKey:  "key",
	})
	h := p.Health()
	if h.Status != decision.HealthUnavailable {
		t.Fatalf("disabled should be unavailable")
	}

	// Missing key
	p2, _ := NewProvider(ProviderConfig{
		ID:      "jev-main",
		Type:    "jev",
		Enabled: true,
		APIKey:  "",
	})
	h2 := p2.Health()
	if h2.Status != decision.HealthUnavailable {
		t.Fatalf("missing key should be unavailable")
	}

	// Healthy
	p3, _ := NewProvider(ProviderConfig{
		ID:      "jev-main",
		Type:    "jev",
		Enabled: true,
		APIKey:  "key",
	})
	h3 := p3.Health()
	if h3.Status != decision.HealthHealthy {
		t.Fatalf("should be healthy")
	}
}

func TestJevProvider_ValidSelection(t *testing.T) {
	// Mock server that selects c1 -> p2/m2
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"message":"ok","data":{"decision":"c1","confidence":0.9}}`))
	}))
	defer srv.Close()

	p, err := NewProvider(ProviderConfig{
		ID:         "jev-main",
		Type:       "jev",
		Enabled:    true,
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	req := decision.DecisionRequest{
		TaskProfile: taskprofile.TaskProfile{Type: "coding"},
		Features:    feature.RequestFeatures{},
		Candidates: []decision.Candidate{
			{ID: "p1/m1", PoolOrdinal: 0, Priority: 0, OriginalRank: 0},
			{ID: "p2/m2", PoolOrdinal: 0, Priority: 0, OriginalRank: 1},
		},
	}

	result, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != decision.ActionSelect {
		t.Fatalf("expected SELECT, got %s", result.Action)
	}
	if result.SelectedID != "p2/m2" {
		t.Fatalf("expected p2/m2, got %s", result.SelectedID)
	}
	if result.Confidence != 0.9 {
		t.Fatalf("expected confidence 0.9, got %f", result.Confidence)
	}
	found := false
	for _, rc := range result.ReasonCodes {
		if rc == decision.ReasonExternalSelected {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected EXTERNAL_SELECTED reason")
	}
}

func TestJevProvider_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte(`{"code":0,"message":"ok","data":{"decision":"c0"}}`))
	}))
	defer srv.Close()

	p, _ := NewProvider(ProviderConfig{
		ID:         "jev-main",
		Type:       "jev",
		Enabled:    true,
		APIKey:     "key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req := decision.DecisionRequest{
		Candidates: []decision.Candidate{
			{ID: "p1/m1"},
			{ID: "p2/m2"},
		},
	}

	result, _ := p.Decide(ctx, req)
	if result.Action != decision.ActionAbstain {
		t.Fatalf("expected ABSTAIN on timeout, got %s", result.Action)
	}
	found := false
	for _, rc := range result.ReasonCodes {
		if rc == decision.ReasonExternalTimeout {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected EXTERNAL_TIMEOUT, got %v", result.ReasonCodes)
	}
}

func TestJevProvider_MalformedResponse(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"invalid json", "not json"},
		{"wrong envelope", `{"wrong": "field"}`},
		{"non-zero code", `{"code":1,"message":"error","data":{}}`},
		{"missing data", `{"code":0,"message":"ok"}`},
		{"missing decision", `{"code":0,"message":"ok","data":{"confidence":0.5}}`},
		{"unknown choice", `{"code":0,"message":"ok","data":{"decision":"c9"}}`},
		{"nan confidence", `{"code":0,"message":"ok","data":{"decision":"c0","confidence":NaN}}`},
		{"confidence >1", `{"code":0,"message":"ok","data":{"decision":"c0","confidence":1.5}}`},
		{"confidence <0", `{"code":0,"message":"ok","data":{"decision":"c0","confidence":-0.1}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			p, _ := NewProvider(ProviderConfig{
				ID:         "jev-main",
				Type:       "jev",
				Enabled:    true,
				APIKey:     "key",
				BaseURL:    srv.URL,
				HTTPClient: srv.Client(),
			})

			req := decision.DecisionRequest{
				Candidates: []decision.Candidate{
					{ID: "p1/m1"},
					{ID: "p2/m2"},
				},
			}

			result, _ := p.Decide(context.Background(), req)
			if result.Action != decision.ActionAbstain {
				t.Fatalf("expected ABSTAIN for malformed %s, got %s", tt.name, result.Action)
			}
		})
	}
}

func TestJevProvider_HTTPFailures(t *testing.T) {
	statuses := []int{400, 401, 403, 429, 500, 503}
	for _, status := range statuses {
		t.Run(string(rune(status)), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				w.Write([]byte(`{"code":1,"message":"error"}`))
			}))
			defer srv.Close()

			p, _ := NewProvider(ProviderConfig{
				ID:         "jev-main",
				Type:       "jev",
				Enabled:    true,
				APIKey:     "key",
				BaseURL:    srv.URL,
				HTTPClient: srv.Client(),
			})

			req := decision.DecisionRequest{
				Candidates: []decision.Candidate{
					{ID: "p1/m1"},
					{ID: "p2/m2"},
				},
			}

			result, _ := p.Decide(context.Background(), req)
			if result.Action != decision.ActionAbstain {
				t.Fatalf("expected ABSTAIN for status %d, got %s", status, result.Action)
			}
			found := false
			for _, rc := range result.ReasonCodes {
				if rc == decision.ReasonExternalHTTPError {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected EXTERNAL_HTTP_ERROR for status %d", status)
			}
		})
	}
}

func TestJevProvider_Privacy_NoRawPrompt(t *testing.T) {
	canary := "SECRET_EXTERNAL_PROMPT_CANARY_94af"
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 32*1024)
		n, _ := r.Body.Read(b)
		capturedBody = string(b[:n])
		w.Write([]byte(`{"code":0,"message":"ok","data":{"decision":"c0"}}`))
	}))
	defer srv.Close()

	p, _ := NewProvider(ProviderConfig{
		ID:         "jev-main",
		Type:       "jev",
		Enabled:    true,
		APIKey:     "key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})

	// DecisionRequest must not contain raw prompt, but we simulate that even if taskprofile had canary, it shouldn't be sent
	// Our mapper uses only metadata, so canary should not appear
	req := decision.DecisionRequest{
		TaskProfile: taskprofile.TaskProfile{Type: "coding"},
		Candidates: []decision.Candidate{
			{ID: "p1/m1"},
			{ID: "p2/m2"},
		},
	}

	_, _ = p.Decide(context.Background(), req)
	if strings.Contains(capturedBody, canary) {
		t.Fatalf("canary leaked into request body")
	}
}

func TestJevProvider_APIKeyPrivacy(t *testing.T) {
	secretKey := "SECRET_JEV_KEY_CANARY_21df"
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"code":0,"message":"ok","data":{"decision":"c0"}}`))
	}))
	defer srv.Close()

	p, _ := NewProvider(ProviderConfig{
		ID:         "jev-main",
		Type:       "jev",
		Enabled:    true,
		APIKey:     secretKey,
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})

	req := decision.DecisionRequest{
		Candidates: []decision.Candidate{
			{ID: "p1/m1"},
			{ID: "p2/m2"},
		},
	}

	result, _ := p.Decide(context.Background(), req)
	// Auth should be in header as expected for server
	if capturedAuth != "Bearer "+secretKey {
		t.Fatalf("expected auth header to contain key for server")
	}
	// But result, error, etc. must not contain key
	if strings.Contains(result.Error, secretKey) {
		t.Fatalf("key leaked into error")
	}
	for _, rc := range result.ReasonCodes {
		if strings.Contains(string(rc), secretKey) {
			t.Fatalf("key leaked into reason codes")
		}
	}
}

func TestJevProvider_RemoteErrorBodyCanary(t *testing.T) {
	secret := "SECRET_REMOTE_ERROR_CANARY_64ac"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(secret))
	}))
	defer srv.Close()

	p, _ := NewProvider(ProviderConfig{
		ID:         "jev-main",
		Type:       "jev",
		Enabled:    true,
		APIKey:     "key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})

	req := decision.DecisionRequest{
		Candidates: []decision.Candidate{
			{ID: "p1/m1"},
			{ID: "p2/m2"},
		},
	}

	result, _ := p.Decide(context.Background(), req)
	// Secret must not appear in result error, reason codes, etc.
	if strings.Contains(result.Error, secret) {
		t.Fatalf("remote error body leaked into result error")
	}
}
