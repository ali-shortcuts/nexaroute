package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// #415 item 6: the one-click connection test is bounded (fixed
// tiny prompt, max_tokens 8, one request) and reports provider/
// model, endpoint, auth result, protocol match and token use
// against a fake upstream — no live secrets.
func TestConnectionTestAgainstFakeUpstream(t *testing.T) {
	var gotAuth, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"OK"}}],"usage":{"prompt_tokens":6,"completion_tokens":1}}`))
	}))
	defer up.Close()

	prov := config.ProviderConfig{ID: "fake", Type: "openai_compatible", BaseURL: up.URL, APIKey: "test-key"}
	model := config.ModelConfig{ID: "m", Model: "fake-model", InputCostPerMTok: 2, OutputCostPerMTok: 4}
	rep := runConnectionTest(t.Context(), prov, model, "fake/m", "openai")
	if !rep.OK || !rep.AuthOK || !rep.ProtocolMatch {
		t.Fatalf("report = %+v", rep)
	}
	if rep.InputTokens == nil || *rep.InputTokens != 6 || rep.OutputTokens == nil || *rep.OutputTokens != 1 {
		t.Fatalf("usage = %+v", rep)
	}
	if rep.CostBasis != "actual_usage" || rep.EstimatedCost == nil || *rep.EstimatedCost <= 0 {
		t.Fatalf("cost = %v basis %q", rep.EstimatedCost, rep.CostBasis)
	}
	if rep.ExpectedMaxToks != connectionTestPromptEstToks+connectionTestMaxOutTokens {
		t.Fatalf("bound = %d", rep.ExpectedMaxToks)
	}
	if !strings.Contains(rep.Endpoint, "/v1/chat/completions") || rep.Model != "fake-model" || rep.Provider != "fake" {
		t.Fatalf("identity = %+v", rep)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("upstream auth header = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"max_tokens":8`) {
		t.Fatalf("upstream body not bounded: %s", gotBody)
	}
}

func TestConnectionTestAuthFailureAndShapeMismatch(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer up.Close()
	prov := config.ProviderConfig{ID: "fake", Type: "openai_compatible", BaseURL: up.URL, APIKey: "wrong"}
	rep := runConnectionTest(t.Context(), prov, config.ModelConfig{ID: "m", Model: "x"}, "fake/m", "openai")
	if rep.OK || rep.AuthOK || rep.StatusCode != 401 {
		t.Fatalf("401 report = %+v", rep)
	}

	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"unexpected":true}`))
	}))
	defer up2.Close()
	prov.BaseURL = up2.URL
	rep = runConnectionTest(t.Context(), prov, config.ModelConfig{ID: "m", Model: "x"}, "fake/m", "openai")
	if rep.OK || rep.ProtocolMatch {
		t.Fatalf("shape-mismatch report = %+v", rep)
	}
	if rep.CostBasis != "unpriced" || rep.EstimatedCost != nil {
		t.Fatalf("unpriced model must not invent a cost: %+v", rep)
	}
}

func TestConnectionTestHandlerValidation(t *testing.T) {
	s := &Server{cfg: config.Config{Providers: []config.ProviderConfig{
		{ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", Models: []config.ModelConfig{{ID: "m", Model: "x"}}},
	}}}
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/admin/api/connection-test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.adminConnectionTest(rec, req)
		return rec
	}
	for _, body := range []string{`{"deployment_id":"noslash"}`, `{"deployment_id":"p/ghost"}`} {
		rec := post(body)
		if rec.Code != 400 && rec.Code != 404 {
			t.Fatalf("body %s → %d", body, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	s.adminConnectionTest(rec, httptest.NewRequest("GET", "/admin/api/connection-test", nil))
	if rec.Code != 405 {
		t.Fatalf("GET → %d", rec.Code)
	}
	var rep connectionTestReport
	rec = post(`{"deployment_id":"p/m"}`)
	if rec.Code != 200 {
		t.Fatalf("known deployment → %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil || rep.DeploymentID != "p/m" || rep.OK {
		t.Fatalf("unreachable upstream report = %+v err %v", rep, err)
	}
}
