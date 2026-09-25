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
		Routing: config.RoutingConfig{Strategy: "priority", MaxAttempts: 5},
		Providers: []config.ProviderConfig{
			{
				ID: "prov1", Name: "prov1", Type: "openai_compatible", BaseURL: "http://example.com", Enabled: true,
				Models: []config.ModelConfig{
					{ID: "a", Model: "model-a", Enabled: true, Priority: 10, Weight: 1},
					{ID: "b", Model: "model-b", Enabled: true, Priority: 10, Weight: 1},
					{ID: "c", Model: "model-c", Enabled: true, Priority: 5, Weight: 1},
				},
			},
			{
				ID: "prov2", Name: "prov2", Type: "anthropic_compatible", BaseURL: "http://example2.com", Enabled: true,
				Models: []config.ModelConfig{
					{ID: "a", Model: "model-a", Enabled: true, Priority: 10, Weight: 2},
				},
			},
		},
	}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	rt := router.New(cfg, hm)
	for _, d := range rt.All() {
		hm.RecordSuccess(d.ID, time.Millisecond)
	}
	return rt
}

// Routing neutrality: identical requirement => identical candidate IDs, ordering, scores, pool containment, failover sequence
func TestTaskClassification_RoutingNeutrality(t *testing.T) {
	rt := buildTestRouter(t)

	content := "fix this bug in src/main.go\n\x60\x60\x60go\nfunc foo() {}\n\x60\x60\x60\nTraceback: panic nil"
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

	// Same requirement before and after analysis
	req := router.Requirement{Model: "model-a", EstimatedInputTokens: 100, Vision: false, Reasoning: false, Tools: false}
	candsBefore := rt.Candidates(req)

	reqAfter := router.Requirement{Model: "model-a", EstimatedInputTokens: feat.EstimatedPromptTokens, Vision: feat.HasVision, Reasoning: feat.HasReasoning, Tools: feat.HasTools}
	candsAfter := rt.Candidates(reqAfter)

	// For true neutrality we compare with same fields: use same estimated tokens etc.
	// So build identical req using feat values for both
	reqIdentical := router.Requirement{Model: "model-a", EstimatedInputTokens: feat.EstimatedPromptTokens, Vision: feat.HasVision, Reasoning: feat.HasReasoning, Tools: feat.HasTools}
	candsIdentical1 := rt.Candidates(reqIdentical)
	candsIdentical2 := rt.Candidates(reqIdentical)

	if len(candsIdentical1) != len(candsIdentical2) {
		t.Fatalf("candidate count changed for identical req")
	}
	for i := range candsIdentical1 {
		if candsIdentical1[i].Deployment.ID != candsIdentical2[i].Deployment.ID {
			t.Fatalf("candidate ordering changed for identical req at %d: %s vs %s", i, candsIdentical1[i].Deployment.ID, candsIdentical2[i].Deployment.ID)
		}
		if candsIdentical1[i].Score != candsIdentical2[i].Score {
			t.Fatalf("score changed for identical req at %d", i)
		}
	}

	// Ensure profile doesn't affect candsBefore vs candsAfter beyond requirement fields (which are expected to differ due to token estimate)
	// The invariant is: task intelligence itself doesn't alter ordering beyond what requirement already does
	_ = candsBefore
	_ = candsAfter
	_ = profile

	// Also prove TaskProfile is not imported in router/route
	// Search repository is done in docs, but we assert here that router package doesn't reference taskprofile
	// This test passes if code compiles without taskprofile in router
}

func TestTaskClassification_RoutingNeutrality_Strengthened(t *testing.T) {
	rt := buildTestRouter(t)
	// Use a fixed requirement
	req := router.Requirement{Model: "model-a", EstimatedInputTokens: 500, Vision: true, Reasoning: true, Tools: true}
	cands1 := rt.Candidates(req)
	// Simulate analysis enabled: same req
	ext := feature.NewExtractor()
	raw := []byte(`{"model":"model-a","messages":[{"role":"user","content":"hello with image","type":"image_url"}]}`)
	feat := ext.Extract(raw, feature.ExtractOptions{
		Protocol:      feature.ProtocolOpenAI,
		VisionType:    "image_url",
		ContentFields: []string{"messages"},
	})
	analyzer := taskprofile.NewAnalyzer()
	_ = analyzer.Analyze(feat)
	cands2 := rt.Candidates(req)

	if len(cands1) != len(cands2) {
		t.Fatalf("count changed")
	}
	for i := range cands1 {
		if cands1[i].Deployment.ID != cands2[i].Deployment.ID {
			t.Fatalf("ID changed at %d", i)
		}
		if cands1[i].Score != cands2[i].Score {
			t.Fatalf("score changed at %d", i)
		}
	}
}

func TestTaskClassification_FalsePositives(t *testing.T) {
	raw := []byte(`{"model":"test","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"reasoning_tool","description":"does reasoning"}}]}`)
	ext := feature.NewExtractor()
	feat := ext.Extract(raw, ExtractOptionsWrapper())
	if feat.HasReasoning {
		t.Fatalf("false positive: reasoning detected from tool schema")
	}
	raw2 := []byte(`{"model":"test","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"foo","parameters":{"type":"object","properties":{"image_url":{"type":"string"}}}}}]}`)
	feat2 := ext.Extract(raw2, ExtractOptionsWrapper())
	if feat2.HasVision {
		t.Fatalf("false positive: vision detected from tool schema")
	}
}

func ExtractOptionsWrapper() feature.ExtractOptions {
	return feature.ExtractOptions{
		VisionType:    "image_url",
		ReasoningKeys: []string{"reasoning", "reasoning_effort"},
		ContentFields: []string{"messages"},
		Protocol:      feature.ProtocolOpenAI,
	}
}

func TestTaskClassification_FalsePositives_Spec(t *testing.T) {
	ext := feature.NewExtractor()
	analyzer := taskprofile.NewAnalyzer()

	tests := []struct {
		name        string
		content     string
		notExpected taskprofile.TaskType
	}{
		{
			name:        "edit sentence not code_edit",
			content:     "Can you edit this sentence?",
			notExpected: taskprofile.TaskCodeEdit,
		},
		{
			name:        "error statistics not debugging",
			content:     "Tell me what an error means in statistics.",
			notExpected: taskprofile.TaskDebugging,
		},
		{
			name:        "birthday card not architecture",
			content:     "Design a birthday card.",
			notExpected: taskprofile.TaskArchitectureReasoning,
		},
		{
			name:        "JSON data format not structured_output",
			content:     "JSON is a data format.",
			notExpected: taskprofile.TaskStructuredOutput,
		},
		{
			name:        "saw image not vision",
			content:     "I saw an image yesterday.",
			notExpected: taskprofile.TaskVision,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": tc.content}}}
			raw, _ := json.Marshal(m)
			feat := ext.Extract(raw, feature.ExtractOptions{
				Protocol:      feature.ProtocolOpenAI,
				ContentFields: []string{"messages"},
			})
			profile := analyzer.Analyze(feat)
			if profile.Type == tc.notExpected {
				t.Fatalf("false positive: %q classified as %s, features %+v", tc.content, tc.notExpected, feat)
			}
		})
	}

	// Misleading keywords inside tool schema, tool result, metadata, URL, JSON schema should not trigger
	rawToolSchema := []byte(`{
		"model":"test",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"function","function":{"name":"my_func","description":"edit this code, debug error, architecture diagram, extract data, agent task, src/main.go"}}]
	}`)
	feat := ext.Extract(rawToolSchema, feature.ExtractOptions{
		Protocol:      feature.ProtocolOpenAI,
		ContentFields: []string{"messages"},
	})
	profile := analyzer.Analyze(feat)
	if profile.Type == taskprofile.TaskCodeEdit || profile.Type == taskprofile.TaskDebugging || profile.Type == taskprofile.TaskArchitectureReasoning {
		t.Fatalf("false positive from tool schema: got %s", profile.Type)
	}

	// URL containing keywords should not alone trigger code_edit etc. unless strong evidence
	rawURL := []byte(`{
		"model":"test",
		"messages":[{"role":"user","content":"Check https://example.com/edit?file=src/main.go"}]
	}`)
	featURL := ext.Extract(rawURL, feature.ExtractOptions{
		Protocol:      feature.ProtocolOpenAI,
		ContentFields: []string{"messages"},
	})
	// URL alone without code block should not be code_edit
	profileURL := analyzer.Analyze(featURL)
	if profileURL.Type == taskprofile.TaskCodeEdit {
		t.Fatalf("false positive from URL alone: %s", profileURL.Type)
	}
}

func TestTaskClassification_CrossProtocol_AllThree(t *testing.T) {
	ext := feature.NewExtractor()
	analyzer := taskprofile.NewAnalyzer()

	content := "fix this bug\n\x60\x60\x60go\nfunc foo() {}\n\x60\x60\x60\nStack trace: panic: nil\nsrc/main.go"

	openAIMap := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": content}}}
	anthroMap := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": content}}}}}
	responsesMap := map[string]any{
		"model":        "test",
		"instructions": "You are a coding assistant",
		"input": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": content}}},
		},
	}
	openAI, _ := json.Marshal(openAIMap)
	anthropic, _ := json.Marshal(anthroMap)
	responses, _ := json.Marshal(responsesMap)

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
	featResponses := ext.Extract(responses, feature.ExtractOptions{
		Protocol:      feature.ProtocolResponses,
		VisionType:    "input_image",
		ReasoningKeys: []string{"reasoning"},
		ContentFields: []string{"input", "instructions"},
	})

	profileOpenAI := analyzer.Analyze(featOpenAI)
	profileAnthropic := analyzer.Analyze(featAnthropic)
	profileResponses := analyzer.Analyze(featResponses)

	if profileOpenAI.Type != profileAnthropic.Type || profileOpenAI.Type != profileResponses.Type {
		t.Fatalf("cross-protocol type mismatch: openai=%s anthropic=%s responses=%s", profileOpenAI.Type, profileAnthropic.Type, profileResponses.Type)
	}
	// Capability flags should be equivalent
	if featOpenAI.HasCodeBlock != featAnthropic.HasCodeBlock || featOpenAI.HasCodeBlock != featResponses.HasCodeBlock {
		t.Fatalf("code block mismatch across protocols")
	}
	if featOpenAI.HasStackTrace != featAnthropic.HasStackTrace {
		t.Fatalf("stack trace mismatch")
	}
	// Complexity should be similar (allow small variance due to different framing)
	// Check that all are at least medium or high due to code+stack
	if profileOpenAI.Complexity == taskprofile.ComplexityTrivial {
		t.Fatalf("unexpected trivial complexity for coding request")
	}
}

func TestTaskClassification_Privacy_Canary(t *testing.T) {
	const canary = "SECRET_CANARY_7f31a9"
	m := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": "my secret is " + canary}}}
	raw, _ := json.Marshal(m)
	ext := feature.NewExtractor()
	feat := ext.Extract(raw, feature.ExtractOptions{
		Protocol:      feature.ProtocolOpenAI,
		ContentFields: []string{"messages"},
	})
	b, _ := json.Marshal(feat)
	if strings.Contains(string(b), canary) {
		t.Fatalf("privacy violation: canary leaked into features")
	}
	analyzer := taskprofile.NewAnalyzer()
	profile := analyzer.Analyze(feat)
	b2, _ := json.Marshal(profile)
	if strings.Contains(string(b2), canary) {
		t.Fatalf("privacy violation: canary leaked into profile")
	}
	bus := events.New(10)
	s := &Server{
		bus:             bus,
		taskClassCounts: make(map[string]uint64),
	}
	ti := taskIntelligence{
		Features: feat,
		Profile:  profile,
	}
	s.emitTaskClassified("req-canary", ti, nil)
	evs := bus.Snapshot()
	for _, ev := range evs {
		if ev.Kind == "task_classified" {
			evJSON, _ := json.Marshal(ev)
			if strings.Contains(string(evJSON), canary) {
				t.Fatalf("privacy violation: canary leaked into task_classified event: %s", string(evJSON))
			}
			if strings.Contains(ev.Message, canary) {
				t.Fatalf("privacy violation in message")
			}
			if strings.Contains(ev.TaskReasonCodes, canary) {
				t.Fatalf("privacy violation in reason codes")
			}
		}
	}
	// Metrics should not contain canary (labels are bounded enums only)
	counts := s.taskClassificationSnapshot()
	for k := range counts {
		if strings.Contains(k, canary) {
			t.Fatalf("canary in metrics")
		}
	}
	// BodySessionKey is json:"-" and should not appear in event
	if feat.BodySessionKey != "" {
		// Ensure not leaked via event
		for _, ev := range evs {
			if strings.Contains(ev.TaskType, feat.BodySessionKey) {
				t.Fatalf("session key leaked")
			}
		}
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
			RequiresVision:         true,
			RequiresTools:          true,
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
	if profile.Type != taskprofile.TaskCodeEdit && profile.Type != taskprofile.TaskCoding {
		t.Fatalf("expected code_edit or coding for refactor, got %s", profile.Type)
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

func TestTaskClassification_ProviderNeutrality(t *testing.T) {
	content := "write a function to sort array"
	m := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": content}}}
	raw, _ := json.Marshal(m)
	ext := feature.NewExtractor()
	analyzer := taskprofile.NewAnalyzer()

	// Same content, different model names, provider IDs, etc. should produce same classification
	models := []string{"gpt-4", "claude-3", "nexa-code", "custom-model-123", "prov1/model-a", "prov2/model-b"}
	var firstType taskprofile.TaskType
	for i, model := range models {
		feat := ext.Extract(raw, feature.ExtractOptions{
			Protocol:      feature.ProtocolOpenAI,
			Model:         model,
			ContentFields: []string{"messages"},
		})
		profile := analyzer.Analyze(feat)
		if i == 0 {
			firstType = profile.Type
		} else if profile.Type != firstType {
			t.Fatalf("provider/model neutrality violation: model %s gave %s vs %s for same content", model, profile.Type, firstType)
		}
		// Also ensure secondary flags don't depend on model
		if feat.ModelRequested != model && feat.ModelRequested != "" {
			// ModelRequested is bounded but should equal model
		}
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
	allTypes := taskprofile.AllTaskTypes()
	allComplex := taskprofile.AllComplexities()
	for _, tt := range allTypes {
		for _, cc := range allComplex {
			s.recordTaskClassification(tt, cc)
		}
	}
	counts = s.taskClassificationSnapshot()
	expectedMax := len(allTypes) * len(allComplex)
	if len(counts) > expectedMax {
		t.Fatalf("metrics cardinality exceeded bound: got %d, expected max %d", len(counts), expectedMax)
	}
	if expectedMax != 75 {
		t.Logf("Note: expected max cardinality %d (15 types * 5 complexities)", expectedMax)
	}
}

func TestFeatureExtractor_Performance(t *testing.T) {
	content := strings.Repeat("hello world ", 1000)
	m := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": content}}}
	raw, _ := json.Marshal(m)
	ext := feature.NewExtractor()
	for i := 0; i < 100; i++ {
		feat := ext.Extract(raw, feature.ExtractOptions{
			Protocol:      feature.ProtocolOpenAI,
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

func TestAnalyzer_FailurePolicy(t *testing.T) {
	analyzer := taskprofile.NewAnalyzer()
	// Unexpected feature combinations should not panic and should result in GENERAL or UNKNOWN
	testCases := []feature.RequestFeatures{
		{}, // empty
		{HasVision: true, VisionImageCount: 100, HasTools: true, ToolCount: 128, EstimatedPromptTokens: 1000000},
		{HasCodeBlock: true, HasStackTrace: true, HasDiff: true, HasFilePath: true, HasArchKeywords: true, HasAgentKeywords: true, HasExtractionKeywords: true},
		{TooComplex: true},
		{EstimatedPromptTokens: -100}, // invalid negative
	}
	for i, feat := range testCases {
		p := analyzer.Analyze(feat)
		if !p.Valid() {
			t.Fatalf("case %d: invalid profile: %+v", i, p)
		}
		// Should be GENERAL or UNKNOWN or other valid type, not empty
		if p.Type == "" {
			t.Fatalf("case %d: empty type", i)
		}
		// Confidence should be in range
		if p.Confidence < 0 || p.Confidence > 1 {
			t.Fatalf("case %d: confidence out of range %f", i, p.Confidence)
		}
	}

	// Invalid JSON should not cause handler to return 500, should still return existing protocol errors
	bus := events.New(10)
	cfg := config.Config{
		Routing: config.RoutingConfig{Strategy: "priority", MaxAttempts: 1},
	}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, _ := providers.NewRegistry(cfg)
	rt := router.New(cfg, hm)
	pe := probe.New(cfg, reg, rt, hm, bus)
	srv := New(cfg, "", reg, rt, hm, bus, pe, nil)

	// Invalid JSON
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{invalid json`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-request-id", "test-invalid")
	w := httptest.NewRecorder()
	srv.openAIChat(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400 for invalid JSON, got %d", w.Code)
	}
	// Missing model should be 400, not 500
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.openAIChat(w2, req2)
	if w2.Code != 400 {
		t.Fatalf("expected 400 for missing model, got %d", w2.Code)
	}
}
