package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video/agent"
)

func TestOpenAIPlannerAgainstFakeEndpoint(t *testing.T) {
	var gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"the-plan-text"}}]}`))
	}))
	defer srv.Close()
	t.Setenv("TEST_PLANNER_KEY", "secret-value-must-not-leak")
	p, err := agent.NewOpenAIPlanner(srv.URL, "TEST_PLANNER_KEY", false)
	if err != nil {
		t.Fatalf("NewOpenAIPlanner: %v", err)
	}
	out, err := p.Complete(context.Background(), "some-model", "sys", "user")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "the-plan-text" || gotModel != "some-model" || gotAuth != "Bearer secret-value-must-not-leak" {
		t.Fatalf("out=%q model=%q auth set=%v", out, gotModel, gotAuth != "")
	}
}

func TestOpenAIPlannerRejectsPlainHTTPRemote(t *testing.T) {
	if _, err := agent.NewOpenAIPlanner("http://203.0.113.10:8080", "", false); err == nil {
		t.Fatal("plain HTTP to a non-loopback host must be rejected")
	}
	if _, err := agent.NewOpenAIPlanner("http://127.0.0.1:8080", "", false); err != nil {
		t.Fatalf("loopback HTTP must be accepted: %v", err)
	}
}

func TestOpenAIPlannerErrorDoesNotLeakKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()
	t.Setenv("TEST_PLANNER_KEY2", "topsecret123")
	p, err := agent.NewOpenAIPlanner(srv.URL, "TEST_PLANNER_KEY2", false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Complete(context.Background(), "m", "s", "u")
	if err == nil || strings.Contains(err.Error(), "topsecret123") {
		t.Fatalf("error must exist and must not contain the key: %v", err)
	}
}

func TestOpenAIPlannerMissingKeyEnvFailsClosed(t *testing.T) {
	if _, err := agent.NewOpenAIPlanner("http://127.0.0.1:9", "DEFINITELY_UNSET_ENV_VAR_XYZ", false); err == nil {
		t.Fatal("missing key env must fail closed at construction")
	}
}
