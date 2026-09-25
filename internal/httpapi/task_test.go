package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

func buildTestRouter(t *testing.T) *router.Router {
	t.Helper()
	cfg := config.Config{
		Routing: config.RoutingConfig{Strategy: "priority", MaxAttempts: 2},
		Providers: []config.ProviderConfig{
			{
				ID: "prov1", Name: "prov1", Type: "openai_compatible", BaseURL: "http://example.com", Enabled: true,
				Models: []config.ModelConfig{
					{ID: "a", Model: "model-a", Enabled: true, Priority: 10, Weight: 1},
					{ID: "b", Model: "model-b", Enabled: true, Priority: 10, Weight: 1},
				},
			},
		},
	}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	rt := router.New(cfg, hm)
	// Mark healthy
	for _, d := range rt.All() {
		hm.RecordSuccess(d.ID, time.Millisecond)
	}
	return rt
}

// TestTaskClassification_RoutingNeutrality ensures same request produces same candidate ordering
// with and without task intelligence (observational only invariant).
func TestTaskClassification_RoutingNeutrality(t *testing.T) {
	rt := buildTestRouter(t)

	req := router.Requirement{Model: "model-a", EstimatedInputTokens: 100}
	cands1 := rt.Candidates(req)

	content := "fix this bug in src/main.go\n\x60\x60\x60go\nfunc foo() {}\n\x60\x60\x60\nTraceback..."
	m := map[string]any{"model": "model-a", "messages": []any{map[string]any{"role": "user", "content": content}}}
	raw, _ := json.Marshal(m)

	ext := feature.NewExtractor()
	feat := ext.Extract(raw, feature.ExtractOptions{
		Protocol:      feature.ProtocolOpenAI,
		Model:         "model-a",
		VisionType:    "image_url",
		ReasoningKeys: []string{"reasoning_effort"},
		ContentFields: []string{"messages"},
	})
	analyzer := taskprofile.NewAnalyzer()
	profile := analyzer.Analyze(feat)

	req2 := router.Requirement{Model: "model-a", EstimatedInputTokens: feat.EstimatedPromptTokens, Vision: feat.HasVision, Reasoning: feat.HasReasoning, Tools: feat.HasTools}
	cands2 := rt.Candidates(req2)

	if len(cands1) != len(cands2) {
		t.Fatalf("candidate count changed: %d vs %d", len(cands1), len(cands2))
	}
	if len(cands1) > 0 && cands1[0].Deployment.ID != cands2[0].Deployment.ID {
		t.Fatalf("candidate ordering changed: %s vs %s", cands1[0].Deployment.ID, cands2[0].Deployment.ID)
	}
	_ = profile
}

func TestTaskClassification_FalsePositives(t *testing.T) {
	raw := []byte(`{"model":"test","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"reasoning_tool","description":"does reasoning"}}]}`)
	ext := feature.NewExtractor()
	feat := ext.Extract(raw, feature.ExtractOptions{
		VisionType:    "image_url",
		ReasoningKeys: []string{"reasoning", "reasoning_effort"},
		ContentFields: []string{"messages"},
	})
	if feat.HasReasoning {
		t.Fatalf("false positive: reasoning detected from tool schema")
	}
	raw2 := []byte(`{"model":"test","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"foo","parameters":{"type":"object","properties":{"image_url":{"type":"string"}}}}}]}`)
	feat2 := ext.Extract(raw2, feature.ExtractOptions{
		VisionType:    "image_url",
		ContentFields: []string{"messages"},
	})
	if feat2.HasVision {
		t.Fatalf("false positive: vision detected from tool schema")
	}
}

func TestTaskClassification_CrossProtocol(t *testing.T) {
	ext := feature.NewExtractor()
	analyzer := taskprofile.NewAnalyzer()

	content := "fix this bug\n\x60\x60\x60go\nfunc foo() {}\n\x60\x60\x60\nStack trace: panic: nil"
	openAIMap := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": content}}}
	anthroMap := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": content}}}}}
	openAI, _ := json.Marshal(openAIMap)
	anthropic, _ := json.Marshal(anthroMap)

	featOpenAI := ext.Extract(openAI, feature.ExtractOptions{
		Protocol:      feature.ProtocolOpenAI,
		VisionType:    "image_url",
		ReasoningKeys: []string{"reasoning_effort"},
		ContentFields: []string{"messages"},
	})
	featAnthropic := ext.Extract(anthropic, feature.ExtractOptions{
		Protocol:      feature.ProtocolAnthropic,
		VisionType:    "image",
		ReasoningKeys: []string{"thinking"},
		ContentFields: []string{"messages"},
	})

	profileOpenAI := analyzer.Analyze(featOpenAI)
	profileAnthropic := analyzer.Analyze(featAnthropic)

	if profileOpenAI.Type != profileAnthropic.Type {
		t.Fatalf("cross-protocol task type mismatch: openai=%s anthropic=%s", profileOpenAI.Type, profileAnthropic.Type)
	}
}

func TestTaskClassification_Privacy(t *testing.T) {
	m := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": "my secret is hunter2 and my api key is sk-12345"}}}
	raw, _ := json.Marshal(m)
	ext := feature.NewExtractor()
	feat := ext.Extract(raw, feature.ExtractOptions{
		ContentFields: []string{"messages"},
	})
	b, _ := json.Marshal(feat)
	if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "sk-12345") {
		t.Fatalf("privacy violation: secret leaked into features")
	}
	analyzer := taskprofile.NewAnalyzer()
	profile := analyzer.Analyze(feat)
	b2, _ := json.Marshal(profile)
	if strings.Contains(string(b2), "hunter2") || strings.Contains(string(b2), "sk-12345") {
		t.Fatalf("privacy violation: secret leaked into profile")
	}
}

func TestTaskClassification_EventEmission(t *testing.T) {
	bus := events.New(100)
	s := &Server{
		bus:             bus,
		taskClassCounts: make(map[string]uint64),
	}
	ti := taskIntelligence{
		Features: feature.RequestFeatures{
			HasVision:             true,
			VisionImageCount:      1,
			HasTools:              true,
			ToolCount:             2,
			HasCodeBlock:          true,
			EstimatedPromptTokens: 100,
			MessageCount:          1,
		},
		Profile: taskprofile.TaskProfile{
			Type:                   taskprofile.TaskCoding,
			Complexity:             taskprofile.ComplexityLow,
			Confidence:             0.9,
			ReasonCodes:            []taskprofile.ReasonCode{taskprofile.ReasonCodeBlock},
			EstimatedContextTokens: 100,
			ToolCount:              2,
			ImageCount:             1,
			MessageCount:           1,
			HasVision:              true,
			HasTools:               true,
		},
	}
	s.emitTaskClassified("req-123", ti, nil)
	evs := bus.Snapshot()
	found := false
	for _, ev := range evs {
		if ev.Kind == "task_classified" && ev.RequestID == "req-123" {
			found = true
			if ev.TaskType != "coding" {
				t.Fatalf("expected coding got %s", ev.TaskType)
			}
			if ev.TaskComplexity != "low" {
				t.Fatalf("expected low got %s", ev.TaskComplexity)
			}
			if ev.TaskReasonCodes == "" {
				t.Fatalf("expected reason codes")
			}
			if ev.Message != "request classified" {
				t.Fatalf("unexpected message")
			}
			if strings.Contains(ev.Message, "hunter2") {
				t.Fatalf("privacy leak in event message")
			}
		}
	}
	if !found {
		t.Fatalf("task_classified event not found")
	}
	counts := s.taskClassificationSnapshot()
	if len(counts) == 0 {
		t.Fatalf("expected task counts")
	}
}

func TestTaskClassification_VirtualEndpointNeutrality(t *testing.T) {
	content := "refactor src/main.go\n\x60\x60\x60go\nfunc old() {}\n\x60\x60\x60"
	m := map[string]any{"model": "nexa-code", "messages": []any{map[string]any{"role": "user", "content": content}}}
	raw, _ := json.Marshal(m)
	ext := feature.NewExtractor()
	feat := ext.Extract(raw, feature.ExtractOptions{
		Protocol:      feature.ProtocolOpenAI,
		Model:         "nexa-code",
		VisionType:    "image_url",
		ContentFields: []string{"messages"},
	})
	analyzer := taskprofile.NewAnalyzer()
	profile := analyzer.Analyze(feat)
	if profile.Type != taskprofile.TaskEditing && profile.Type != taskprofile.TaskCoding {
		t.Fatalf("expected editing or coding for refactor, got %s", profile.Type)
	}
	m2 := map[string]any{"model": "different-model", "messages": []any{map[string]any{"role": "user", "content": content}}}
	raw2, _ := json.Marshal(m2)
	feat2 := ext.Extract(raw2, feature.ExtractOptions{
		Protocol:      feature.ProtocolOpenAI,
		Model:         "different-model",
		VisionType:    "image_url",
		ContentFields: []string{"messages"},
	})
	profile2 := analyzer.Analyze(feat2)
	if profile.Type != profile2.Type {
		t.Fatalf("VE neutrality violation: task type changed with model: %s vs %s", profile.Type, profile2.Type)
	}
}

func TestTaskClassification_BoundedMetrics(t *testing.T) {
	s := &Server{
		bus:             events.New(10),
		taskClassCounts: make(map[string]uint64),
	}
	for i := 0; i < 1000; i++ {
		s.recordTaskClassification(taskprofile.TaskCoding, taskprofile.ComplexityMedium)
	}
	counts := s.taskClassificationSnapshot()
	if len(counts) != 1 {
		t.Fatalf("expected 1 key, got %d", len(counts))
	}
	if counts["coding|medium"] != 1000 {
		t.Fatalf("expected 1000 got %d", counts["coding|medium"])
	}
	allTypes := []taskprofile.TaskType{taskprofile.TaskCoding, taskprofile.TaskDebugging, taskprofile.TaskEditing, taskprofile.TaskRepo, taskprofile.TaskArchitecture, taskprofile.TaskExtraction, taskprofile.TaskAgent, taskprofile.TaskVision, taskprofile.TaskReasoning, taskprofile.TaskGeneral}
	allComplex := []taskprofile.Complexity{taskprofile.ComplexityTrivial, taskprofile.ComplexityLow, taskprofile.ComplexityMedium, taskprofile.ComplexityHigh, taskprofile.ComplexityVeryHigh}
	for _, tt := range allTypes {
		for _, cc := range allComplex {
			s.recordTaskClassification(tt, cc)
		}
	}
	counts = s.taskClassificationSnapshot()
	if len(counts) > 50 {
		t.Fatalf("metrics cardinality exceeded bound: %d", len(counts))
	}
}

func TestFeatureExtractor_Performance(t *testing.T) {
	content := strings.Repeat("hello world ", 1000)
	m := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": content}}}
	raw, _ := json.Marshal(m)
	ext := feature.NewExtractor()
	for i := 0; i < 100; i++ {
		feat := ext.Extract(raw, feature.ExtractOptions{
			ContentFields: []string{"messages"},
		})
		if feat.EstimatedPromptTokens == 0 {
			t.Fatalf("no tokens")
		}
	}
}

func TestTaskClassification_HandlerIntegration(t *testing.T) {
	bus := events.New(100)
	cfg := config.Config{
		Routing: config.RoutingConfig{Strategy: "priority", MaxAttempts: 1},
	}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, _ := providers.NewRegistry(cfg)
	rt := router.New(cfg, hm)
	pe := probe.New(cfg, reg, rt, hm, bus)
	srv := New(cfg, "", reg, rt, hm, bus, pe, nil)

	content := "write a function to sort array\n\x60\x60\x60python\ndef sort(arr):\n    pass\n\x60\x60\x60"
	m := map[string]any{"model": "test-model", "messages": []any{map[string]any{"role": "user", "content": content}}}
	raw, _ := json.Marshal(m)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-request-id", "test-req-1")
	w := httptest.NewRecorder()
	srv.openAIChat(w, req)

	evs := bus.Snapshot()
	found := false
	for _, ev := range evs {
		if ev.Kind == "task_classified" && ev.RequestID == "test-req-1" {
			found = true
			if ev.TaskType == "" {
				t.Fatalf("empty task type")
			}
		}
	}
	if !found {
		t.Fatalf("expected task_classified event, got %v", evs)
	}
}
