package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

func TestMission95DecisionWiringPure(t *testing.T) {
	// jsonMarshalBounded: fits, too-large falls back to {}, marshal error.
	if got, err := jsonMarshalBounded(map[string]int{"a": 1}, 100); err != nil || got != `{"a":1}` {
		t.Fatalf("bounded fit = %q, %v", got, err)
	}
	if got, err := jsonMarshalBounded(map[string]int{"a": 1}, 2); err != nil || got != `{}` {
		t.Fatalf("bounded overflow = %q, %v", got, err)
	}
	if _, err := jsonMarshalBounded(make(chan int), 100); err == nil {
		t.Error("unmarshalable value accepted")
	}
	if got, err := jsonMarshalBounded(nil, 100); err != nil || got != `null` {
		t.Fatalf("bounded nil = %q, %v", got, err)
	}
	// decisionCandidates: empty in, nil out; fields mapped.
	if got := decisionCandidates(nil, nil); got != nil {
		t.Errorf("empty candidates = %v", got)
	}
	scored := []router.Scored{{
		Deployment: router.Deployment{ID: "d1", ProviderID: "p1", Model: "m1", Priority: 2, Weight: 1.5, ContextWindow: 8000,
			Capabilities: config.Capabilities{Streaming: true, Vision: true}},
		Score: 0.9, CapacityPressure: 0.3, EstimatedCostUSD: 0.02, PriceKnown: true,
		Health: health.State{Status: health.Healthy, EWMALatencyMS: 12, Successes: 5, Failures: 1},
	}}
	cands := decisionCandidates(scored, nil)
	if len(cands) != 1 || cands[0].ID != "d1" || cands[0].ProviderID != "p1" || cands[0].Model != "m1" ||
		cands[0].Priority != 2 || cands[0].Weight != 1.5 || cands[0].RouterScore != 0.9 || cands[0].OriginalRank != 0 ||
		!cands[0].Capabilities.Streaming || !cands[0].Capabilities.Vision || cands[0].Capabilities.Tools {
		t.Fatalf("candidates = %+v", cands)
	}
	// poolInfoForDeployment branches.
	if pool, ord := poolInfoForDeployment("d1", nil); pool != "" || ord != 0 {
		t.Errorf("nil resolved = %q, %d", pool, ord)
	}
	primary := &route.ResolvedRoute{PrimaryPoolID: "pool-a", AllowedDeployments: map[string]struct{}{"d1": {}}}
	if pool, ord := poolInfoForDeployment("d1", primary); pool != "pool-a" || ord != 0 {
		t.Errorf("primary allowed = %q, %d", pool, ord)
	}
	allMode := &route.ResolvedRoute{PrimaryPoolID: "pool-a", PrimaryMode: "all", AllAllowed: map[string]struct{}{"d9": {}}}
	if pool, ord := poolInfoForDeployment("d9", allMode); pool != "pool-a" || ord != 0 {
		t.Errorf("all-mode union = %q, %d", pool, ord)
	}
	allModeNoUnion := &route.ResolvedRoute{PrimaryPoolID: "pool-a", PrimaryMode: "all"}
	if pool, ord := poolInfoForDeployment("d9", allModeNoUnion); pool != "pool-a" || ord != 0 {
		t.Errorf("all-mode no union = %q, %d", pool, ord)
	}
	fallbacks := &route.ResolvedRoute{
		PrimaryPoolID: "pool-a", PrimaryMode: "explicit",
		OrderedPoolIDs:  []string{"pool-a", "pool-b", "pool-c"},
		FallbackAllowed: []map[string]struct{}{nil, {"d2": {}}},
	}
	if pool, ord := poolInfoForDeployment("d2", fallbacks); pool != "pool-c" || ord != 2 {
		t.Errorf("fallback = %q, %d", pool, ord)
	}
	// Fallback ordinal beyond OrderedPoolIDs yields an empty pool id.
	short := &route.ResolvedRoute{
		PrimaryPoolID: "pool-a", PrimaryMode: "explicit",
		OrderedPoolIDs:  []string{"pool-a"},
		FallbackAllowed: []map[string]struct{}{{"d3": {}}},
	}
	if pool, ord := poolInfoForDeployment("d3", short); pool != "" || ord != 1 {
		t.Errorf("short fallback = %q, %d", pool, ord)
	}
	if pool, ord := poolInfoForDeployment("ghost", fallbacks); pool != "pool-a" || ord != 0 {
		t.Errorf("unfound deployment = %q, %d", pool, ord)
	}
	// reorderScoredByDecision: length mismatch preserves original order;
	// a full reorder preserves each deployment's score; unknown ordered ids
	// are dropped and the missing originals appended in original order.
	orig := []router.Scored{
		{Deployment: router.Deployment{ID: "a"}, Score: 1},
		{Deployment: router.Deployment{ID: "b"}, Score: 2},
		{Deployment: router.Deployment{ID: "c"}, Score: 3},
	}
	if got := reorderScoredByDecision(orig, []decision.Candidate{{ID: "a"}}); len(got) != 3 || got[0].Deployment.ID != "a" || got[1].Deployment.ID != "b" {
		t.Errorf("mismatch reorder = %v", got)
	}
	reordered := reorderScoredByDecision(orig, []decision.Candidate{{ID: "c"}, {ID: "a"}, {ID: "b"}})
	if len(reordered) != 3 || reordered[0].Deployment.ID != "c" || reordered[0].Score != 3 ||
		reordered[1].Deployment.ID != "a" || reordered[2].Deployment.ID != "b" {
		t.Errorf("reordered = %+v", reordered)
	}
	partial := reorderScoredByDecision(orig, []decision.Candidate{{ID: "c"}, {ID: "ghost"}, {ID: "a"}})
	if len(partial) != 3 || partial[0].Deployment.ID != "c" || partial[1].Deployment.ID != "a" || partial[2].Deployment.ID != "b" {
		t.Errorf("partial reorder = %+v", partial)
	}
	// taskDimensionFor mapping incl. unknown.
	if got := taskDimensionFor("coding"); got != scorecards.DimCoding {
		t.Errorf("coding dim = %q", got)
	}
	if got := taskDimensionFor("deep_reasoning"); got != scorecards.DimReasoning {
		t.Errorf("reasoning dim = %q", got)
	}
	if got := taskDimensionFor("tool_use"); got != scorecards.DimToolCalling {
		t.Errorf("tool dim = %q", got)
	}
	if got := taskDimensionFor("something-else"); got != "" {
		t.Errorf("unknown dim = %q", got)
	}
	// decisionQualityEnabled: global weight, profile override, task override.
	cfg := config.Config{
		Decision: config.DecisionConfig{Policy: "default"},
		DecisionPolicies: []config.DecisionPolicyConfig{
			{ID: "default", Weights: config.DecisionPolicyWeights{Quality: 0}, TaskOverrides: map[string]config.DecisionPolicyWeights{"coding": {Quality: 2}}},
			{ID: "q", Weights: config.DecisionPolicyWeights{Quality: 1}},
		},
	}
	if !decisionQualityEnabled(cfg, nil, "coding") {
		t.Error("task override quality not enabled")
	}
	if decisionQualityEnabled(cfg, nil, "other") {
		t.Error("zero-weight policy reported quality enabled")
	}
	cfg2 := cfg
	cfg2.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", DecisionPolicy: "q"}}
	resolved := &route.ResolvedRoute{RouteProfileID: "rp1"}
	if !decisionQualityEnabled(cfg2, resolved, "other") {
		t.Error("profile policy quality not enabled")
	}
	cfg2.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", DecisionPolicy: "missing"}}
	if decisionQualityEnabled(cfg2, resolved, "other") {
		t.Error("missing profile policy reported quality enabled")
	}
	// taskQualityEvidence without a plane is a clean miss.
	if _, _, ok := (&Server{}).taskQualityEvidence("coding", "d1"); ok {
		t.Error("nil-plane evidence reported ok")
	}
	planeServer := &Server{evaluation: newEvaluationPlane(config.EvaluationConfig{Enabled: true}, nil)}
	if _, _, ok := planeServer.taskQualityEvidence("unknown-task", "d1"); ok {
		t.Error("unknown-task evidence reported ok")
	}
}

func TestMission95CompatPure(t *testing.T) {
	// credentialScope is a stable 16-hex fingerprint, order-sensitive.
	p1 := config.ProviderConfig{APIKey: "k1"}
	p2 := config.ProviderConfig{APIKey: "k1"}
	if credentialScope(p1) != credentialScope(p2) || len(credentialScope(p1)) != 16 {
		t.Errorf("scope fingerprint = %q", credentialScope(p1))
	}
	if credentialScope(p1) == credentialScope(config.ProviderConfig{APIKey: "k2"}) {
		t.Error("distinct credentials share a fingerprint")
	}
	if credentialScope(config.ProviderConfig{}) == credentialScope(config.ProviderConfig{APIKey: "x"}) {
		t.Error("empty credential set collides with a keyed set")
	}
	// staticCapsFromConfig respects configured false as unsupported.
	caps := staticCapsFromConfig(config.ModelConfig{Capabilities: config.Capabilities{Streaming: true, Tools: false, Vision: true, Reasoning: false}})
	if caps.Streaming != compat.Supported || caps.Tools != compat.Unsupported || caps.Vision != compat.Supported || caps.Reasoning != compat.Unsupported {
		t.Fatalf("static caps = %+v", caps)
	}
	// profileFromRequirement merges router flags with canonical detection.
	req := router.Requirement{Tools: true, Vision: false, Streaming: false, Reasoning: true, StructuredOutput: false}
	prof := profileFromRequirement(req, nil)
	if !prof.Tools || prof.Vision || !prof.Reasoning {
		t.Fatalf("nil-canonical profile = %+v", prof)
	}
	canReq := &canonical.Request{
		Stream:         true,
		Tools:          []canonical.ToolDef{{Name: "t"}},
		Messages:       []canonical.Message{{Parts: []canonical.Part{{Type: canonical.PartImage}}}},
		ResponseFormat: &canonical.ResponseFormat{Kind: "json_object"},
	}
	prof2 := profileFromRequirement(req, canReq)
	if !prof2.Tools || !prof2.Vision || !prof2.Streaming || !prof2.StructuredOutput {
		t.Fatalf("canonical profile = %+v", prof2)
	}
	textReq := &canonical.Request{ResponseFormat: &canonical.ResponseFormat{Kind: canonical.FormatText}}
	if prof3 := profileFromRequirement(router.Requirement{}, textReq); prof3.StructuredOutput {
		t.Errorf("text format reported as structured: %+v", prof3)
	}
	// classifyFailure maps statuses through the shared classifier.
	for _, code := range []int{400, 401, 429, 500, 200} {
		cls, pol := classifyFailure(code, []byte(`{"error":"x"}`))
		if pol.ErrorType == "" {
			t.Errorf("code %d: empty policy", code)
		}
		_ = cls
	}
	// applyRepairToCanonical drops exactly the named controls; nil is safe.
	applyRepairToCanonical(nil, compat.RepairPlan{})
	temp, topP, topK := 0.7, 0.9, 40.0
	parallel := true
	cr := &canonical.Request{
		Temperature: &temp, TopP: &topP, TopK: &topK,
		Reasoning: &canonical.Reasoning{}, ResponseFormat: &canonical.ResponseFormat{Kind: "json_object"},
		ParallelToolCalls: &parallel, Stop: []string{"END"},
	}
	applyRepairToCanonical(cr, compat.RepairPlan{Rules: []compat.RepairRule{
		{Name: "drop:temperature"}, {Name: "drop:top_p"}, {Name: "drop:top_k"},
		{Name: "drop:reasoning-controls"}, {Name: "drop:response_format"},
		{Name: "drop:parallel_tool_calls"}, {Name: "drop:stop"}, {Name: "drop:unknown"},
	}})
	if cr.Temperature != nil || cr.TopP != nil || cr.TopK != nil || cr.Reasoning != nil ||
		cr.ResponseFormat != nil || cr.ParallelToolCalls != nil || cr.Stop != nil {
		t.Fatalf("repair left controls: %+v", cr)
	}
	// dialectFor/providerConfigFor read the server's current config.
	srv := &Server{}
	srv.cfg.Providers = []config.ProviderConfig{{ID: "p1", Type: "openai", BaseURL: "https://api.example.com"}}
	if _, ok := srv.providerConfigFor("p1"); !ok {
		t.Error("providerConfigFor missed a configured provider")
	}
	if _, ok := srv.providerConfigFor("ghost"); ok {
		t.Error("providerConfigFor found a ghost provider")
	}
	dialect := srv.dialectFor("p1", "openai", "https://api.example.com")
	if dialect.Name == "" {
		t.Errorf("dialect empty: %+v", dialect)
	}
	// capabilityIneligible with an empty store is not ineligible; a store
	// seeded with an unsupported capability reports exactly that capability.
	srv.capStore = compat.NewStore()
	srv.bus = events.New(10)
	if _, ineligible := srv.capabilityIneligible("r1", "d1", compat.RequirementProfile{Tools: true}); ineligible {
		t.Error("empty capability store reported ineligible")
	}
	srv.capStore.Seed("d1", compat.ModelCapabilities{Tools: compat.Unsupported}, compat.SourceStatic, "test seed", "k1")
	if capName, ineligible := srv.capabilityIneligible("r1", "d1", compat.RequirementProfile{Tools: true}); !ineligible || capName != compat.CapTools {
		t.Errorf("seeded ineligibility = %q, %v", capName, ineligible)
	}
}

func TestMission95MCPMediaNodeActivity(t *testing.T) {
	// writeRPC: missing id becomes null; error vs result envelopes.
	rec := httptest.NewRecorder()
	writeRPC(rec, nil, json.RawMessage(`{"ok":true}`), nil)
	var resp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.JSONRPC != "2.0" || string(resp.ID) != "null" || string(resp.Result) != `{"ok":true}` || resp.Error != nil {
		t.Fatalf("rpc result envelope = %+v", resp)
	}
	rec = httptest.NewRecorder()
	writeRPC(rec, json.RawMessage(`7`), nil, rpcErr(-32601, "method not found"))
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.ID) != "7" || resp.Error == nil || resp.Error.Code != -32601 || resp.Error.Message != "method not found" {
		t.Fatalf("rpc error envelope = %+v", resp)
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Code != http.StatusOK {
		t.Errorf("rpc headers code=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	// writeJSONBody: marshal failure propagates; success writes bytes.
	if err := writeJSONBody(httptest.NewRecorder(), make(chan int)); err == nil {
		t.Error("unmarshalable RPC body accepted")
	}
	rec = httptest.NewRecorder()
	if err := writeJSONBody(rec, map[string]int{"a": 1}); err != nil || rec.Body.String() != `{"a":1}` {
		t.Fatalf("json body = %q, %v", rec.Body.String(), err)
	}
	// mcpTenantAllowed: empty allowlist allows all; otherwise exact match.
	srvCfg := config.MCPServerConfig{ID: "s1"}
	if !mcpTenantAllowed(srvCfg, "anyone") {
		t.Error("empty tenant allowlist denied")
	}
	srvCfg.AllowedTenants = []string{"t1", "t2"}
	if !mcpTenantAllowed(srvCfg, "t2") || mcpTenantAllowed(srvCfg, "t3") {
		t.Error("tenant allowlist wrong")
	}
	if got := orEmptyObject(nil); string(got) != `{}` {
		t.Errorf("empty object = %q", got)
	}
	if got := orEmptyObject(json.RawMessage(`[1]`)); string(got) != `[1]` {
		t.Errorf("passthrough object = %q", got)
	}
	// Broker: lazy creation, state identity, URL re-point drops the session.
	s := &Server{}
	b := s.broker()
	if b == nil || s.broker() != b {
		t.Error("broker not memoized")
	}
	st := b.stateFor(config.MCPServerConfig{ID: "s1", URL: "https://mcp.example.com"})
	st.sessionID = "sess"
	st.ready = true
	if b.stateFor(config.MCPServerConfig{ID: "s1", URL: "https://mcp.example.com"}) != st || st.sessionID != "sess" {
		t.Error("same-URL state not preserved")
	}
	b.stateFor(config.MCPServerConfig{ID: "s1", URL: "https://mcp2.example.com"})
	if st.sessionID != "" || st.ready {
		t.Error("re-pointed server kept its session")
	}
	// newRPCID: 16 hex chars, unique across calls.
	id1, id2 := newRPCID(), newRPCID()
	if len(id1) != 16 || id1 == id2 {
		t.Errorf("rpc ids = %q, %q", id1, id2)
	}
	// parseRPCBody: JSON form, SSE form (last message wins), failures.
	msg, err := parseRPCBody([]byte(`{"jsonrpc":"2.0","id":1,"result":{"a":1}}`), "application/json", "1")
	if err != nil || string(msg.Result) != `{"a":1}` {
		t.Fatalf("json rpc body = %+v, %v", msg, err)
	}
	if _, err := parseRPCBody([]byte(`{bad`), "application/json", "1"); err == nil {
		t.Error("invalid rpc body accepted")
	}
	sse := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"n\":1}}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"error\":{\"code\":-1,\"message\":\"late\"}}\n\n"
	msg, err = parseRPCBody([]byte(sse), "text/event-stream", "2")
	if err != nil || msg.Error == nil || msg.Error.Code != -1 {
		t.Fatalf("sse rpc body = %+v, %v", msg, err)
	}
	if _, err := parseRPCBody([]byte("data: not-json\n\n"), "text/event-stream", "2"); err == nil {
		t.Error("sse stream without a message accepted")
	}
	if _, err := parseRPCBody([]byte(""), "text/event-stream", "2"); err == nil {
		t.Error("empty sse stream accepted")
	}
	// mcpClientAllowed: auth disabled allows anonymous; enabled requires a key.
	anonServer := &Server{}
	rec = httptest.NewRecorder()
	id, _, ok := anonServer.mcpClientAllowed(rec, httptest.NewRequest(http.MethodPost, "/", nil), config.Config{})
	if !ok || id.ID != "anonymous" {
		t.Fatalf("anonymous client = %+v, %v", id, ok)
	}
	authServer := &Server{}
	rec = httptest.NewRecorder()
	if _, _, ok := authServer.mcpClientAllowed(rec, httptest.NewRequest(http.MethodPost, "/", nil), config.Config{ClientAuth: config.ClientAuthConfig{Enabled: true}}); ok || rec.Code != http.StatusUnauthorized {
		t.Errorf("keyless client allowed, code=%d", rec.Code)
	}
	// Media kinds and upstream addressing.
	if !mediaSpeech.binaryResponse() || mediaEmbeddings.binaryResponse() {
		t.Error("binaryResponse classification wrong")
	}
	if !mediaTranscriptions.multipart() || mediaSpeech.multipart() {
		t.Error("multipart classification wrong")
	}
	p := config.ProviderConfig{Type: "openai_compatible", EmbeddingsPath: "/v1/embeddings", ImagesPath: "/v1/images/generations", SpeechPath: "/v1/audio/speech", TranscriptionsPath: "/v1/audio/transcriptions", RerankPath: "/v1/rerank"}
	for kind, want := range map[mediaKind]string{
		mediaEmbeddings: "/v1/embeddings", mediaImages: "/v1/images/generations",
		mediaSpeech: "/v1/audio/speech", mediaTranscriptions: "/v1/audio/transcriptions", mediaRerank: "/v1/rerank",
	} {
		if got := mediaUpstreamPath(p, kind); got != want {
			t.Errorf("media path kind %v = %q, want %q", kind, got, want)
		}
		if got, ok := resolveMediaUpstream(p, kind, ""); !ok || got != want {
			t.Errorf("resolve kind %v = %q, %v", kind, got, ok)
		}
	}
	if _, ok := resolveMediaUpstream(config.ProviderConfig{Type: "openai_compatible"}, mediaEmbeddings, ""); ok {
		t.Error("empty openai path resolved")
	}
	if _, ok := resolveMediaUpstream(config.ProviderConfig{Type: "vertex"}, mediaEmbeddings, ""); ok {
		t.Error("vertex media resolved")
	}
	// Azure media paths: default api-version, custom version, rerank refused.
	az := config.ProviderConfig{Type: "azure_openai"}
	got, ok := azureMediaPath(az, mediaEmbeddings, "dep1")
	if !ok || got != "/openai/deployments/dep1/embeddings?api-version=2024-10-21" {
		t.Errorf("azure embeddings = %q, %v", got, ok)
	}
	az.APIVersion = "2025-01-01"
	got, ok = azureMediaPath(az, mediaImages, "my dep")
	if !ok || !strings.Contains(got, "/openai/deployments/my%20dep/images/generations?api-version=2025-01-01") {
		t.Errorf("azure images = %q, %v", got, ok)
	}
	if _, ok := azureMediaPath(az, mediaRerank, "dep1"); ok {
		t.Error("azure rerank resolved")
	}
	if _, ok := azureMediaPath(az, mediaSpeech, "  "); ok {
		t.Error("blank azure deployment resolved")
	}
	if got, ok := resolveMediaUpstream(az, mediaSpeech, "dep1"); !ok || !strings.HasSuffix(got, "/audio/speech?api-version=2025-01-01") {
		t.Errorf("resolve azure speech = %q, %v", got, ok)
	}
	// mediaRequestModel: JSON, multipart, and error paths.
	if m, err := mediaRequestModel(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{"model":"m1"}`), mediaEmbeddings); err != nil || m != "m1" {
		t.Fatalf("json model = %q, %v", m, err)
	}
	if _, err := mediaRequestModel(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{bad`), mediaEmbeddings); err == nil {
		t.Error("invalid json model accepted")
	}
	if _, err := mediaRequestModel(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{}`), mediaEmbeddings); err == nil {
		t.Error("missing model accepted")
	}
	mpReq := httptest.NewRequest(http.MethodPost, "/", nil)
	mpReq.Header.Set("Content-Type", "multipart/form-data; boundary=BOUND")
	mpBody := "--BOUND\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nmp-model\r\n--BOUND--\r\n"
	if m, err := mediaRequestModel(mpReq, []byte(mpBody), mediaTranscriptions); err != nil || m != "mp-model" {
		t.Fatalf("multipart model = %q, %v", m, err)
	}
	badCT := httptest.NewRequest(http.MethodPost, "/", nil)
	badCT.Header.Set("Content-Type", "text/plain")
	if _, err := mediaRequestModel(badCT, []byte(mpBody), mediaTranscriptions); err == nil {
		t.Error("non-multipart transcriptions accepted")
	}
	if _, err := mediaRequestModel(mpReq, []byte("--BOUND\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\n\r\nbytes\r\n--BOUND--\r\n"), mediaTranscriptions); err == nil {
		t.Error("multipart without model accepted")
	}
	// rewriteMultipartModel: model replaced, file bytes preserved, order kept.
	origBody := "--BOUND\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\n\r\nWAVBYTES\r\n--BOUND\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nold\r\n--BOUND--\r\n"
	rewritten, ct, err := rewriteMultipartModel([]byte(origBody), "multipart/form-data; boundary=BOUND", "new-model")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ct, "multipart/form-data") || !strings.Contains(string(rewritten), "WAVBYTES") || !strings.Contains(string(rewritten), "new-model") || strings.Contains(string(rewritten), "\r\n\r\nold\r\n") {
		t.Fatalf("rewritten = %q ct=%q", rewritten, ct)
	}
	if _, _, err := rewriteMultipartModel([]byte(origBody), "text/plain", "x"); err == nil {
		t.Error("rewrite with bad content type accepted")
	}
	if _, _, err := rewriteMultipartModel([]byte("--BOUND\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\n\r\nWAVBYTES\r\n--BOUND--\r\n"), "multipart/form-data; boundary=BOUND", "x"); err == nil {
		t.Error("rewrite without model accepted")
	}
	// embeddingsUsage: prompt_tokens wins, total_tokens fallback, invalid JSON errors.
	if n, err := embeddingsUsage([]byte(`{"usage":{"prompt_tokens":7,"total_tokens":9}}`)); err != nil || n != 7 {
		t.Fatalf("embeddings usage = %d, %v", n, err)
	}
	if n, err := embeddingsUsage([]byte(`{"usage":{"total_tokens":9}}`)); err != nil || n != 9 {
		t.Fatalf("embeddings total fallback = %d, %v", n, err)
	}
	if _, err := embeddingsUsage([]byte(`{bad`)); err == nil {
		t.Error("invalid embeddings usage accepted")
	}
	if n, err := embeddingsUsage([]byte(`{}`)); err != nil || n != 0 {
		t.Fatalf("empty embeddings usage = %d, %v", n, err)
	}
	// Node telemetry: age/observed helpers and row building.
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if telemetryAgeMS(now, time.Time{}) != nil || telemetryAgeMS(time.Time{}, now) != nil {
		t.Error("zero time produced an age")
	}
	if got := telemetryAgeMS(now, now.Add(-1500*time.Millisecond)); got == nil || *got != 1500 {
		t.Fatalf("age = %v", got)
	}
	if got := telemetryAgeMS(now, now.Add(time.Second)); got == nil || *got != 0 {
		t.Fatalf("future age = %v", got)
	}
	if telemetryObservedAt(time.Time{}) != "" || telemetryObservedAt(now) != "2026-10-27T12:00:00Z" {
		t.Errorf("observedAt = %q", telemetryObservedAt(now))
	}
	rows := buildNodeTelemetry(
		[]router.Deployment{{ID: "d1", ProviderID: "p1", ProviderName: "P One"}, {ID: "", ProviderID: "ghost"}},
		[]health.State{
			{Deployment: "d1", Status: health.Healthy, EWMALatencyMS: 11, Successes: 3, Failures: 1, EWMAFailureRate: 0.25, LastChecked: now.Add(-time.Minute), LastSuccess: now.Add(-2 * time.Minute), LastFailure: now.Add(-3 * time.Minute), LastError: "upstream said boom"},
			{Deployment: "orphan", Status: health.Cooldown, CooldownUntil: now.Add(time.Hour), LastChecked: now.Add(-time.Minute)},
			{Deployment: ""},
		}, now)
	if len(rows) != 2 || rows[0].Deployment != "d1" || rows[1].Deployment != "orphan" {
		t.Fatalf("node rows = %+v", rows)
	}
	if rows[0].Provider == nil || rows[0].Provider.Value != "p1" || rows[0].LatencyMS == nil || rows[0].LatencyMS.Value != 11 ||
		rows[0].ErrorRate == nil || rows[0].LastSuccess == nil || rows[0].LastFailure == nil {
		t.Fatalf("d1 row = %+v", rows[0])
	}
	if rows[1].CooldownUntl == nil || rows[1].Provider != nil {
		t.Fatalf("orphan row = %+v", rows[1])
	}
	if len(buildNodeTelemetry(nil, nil, now)) != 0 {
		t.Error("empty telemetry inputs produced rows")
	}
	// Request-activity ledger: dedup consecutive, header join, clear, cap.
	act := &Server{bus: events.New(10)}
	act.recordAttemptSeq("", "d1")
	act.recordAttemptSeq("r1", "")
	if act.attemptsHeaderValue("r1") != "" {
		t.Error("empty ids recorded")
	}
	act.recordAttemptSeq("r1", "d1")
	act.recordAttemptSeq("r1", "d1") // consecutive duplicate dropped
	act.recordAttemptSeq("r1", "d2")
	act.recordAttemptSeq("r1", "d1") // non-consecutive repeat kept
	if got := act.attemptsHeaderValue("r1"); got != "d1>d2>d1" {
		t.Errorf("attempt header = %q", got)
	}
	act.clearAttemptSeq("r1")
	if act.attemptsHeaderValue("r1") != "" {
		t.Error("cleared ledger still renders")
	}
	act.clearAttemptSeq("")
	// Bus-backed emitters are no-ops without a request id.
	busless := &Server{bus: events.New(10)}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	busless.emitRequestAccepted(req, "openai", "m", false)
	req.Header.Set("x-request-id", "rid")
	busless.emitRequestAccepted(req, "openai", "m", true)
	busless.emitRouteResolved(req, "openai", &route.ResolvedRoute{VirtualEndpointID: "ve", PublicModel: "pm", RouteProfileID: "rp", PrimaryPoolID: "pool"}, []router.Scored{{Deployment: router.Deployment{Model: "phys"}}})
	busless.emitRouteAttempt("rid", "openai", "m", 1, router.Scored{Deployment: router.Deployment{ID: "d1", ProviderID: "p1"}}, "initial")
	if got := busless.attemptsHeaderValue("rid"); got != "d1" {
		t.Errorf("emitRouteAttempt ledger = %q", got)
	}
	busless.emitRouteAttempt("", "openai", "m", 2, router.Scored{Deployment: router.Deployment{ID: "d2"}}, "retry")
	busless.emitStreamCommitted("rid", "d1", "p1", "openai", 5*time.Millisecond)
	busless.emitRequestFinished("rid", 200, 10*time.Millisecond, "succeeded")
	// summarizeCounts covers every lifecycle state plus unknown.
	counts := summarizeCounts([]events.RequestLifecycle{
		{State: events.StateActive}, {State: events.StateStale},
		{State: events.TerminalSucceeded}, {State: events.TerminalFailed},
		{State: events.TerminalCancelled}, {State: "bogus"},
	})
	if counts.Total != 6 || counts.Active != 1 || counts.Stale != 1 || counts.Succeeded != 1 || counts.Failed != 1 || counts.Cancelled != 1 {
		t.Fatalf("activity counts = %+v", counts)
	}
	// Privacy 503 envelopes carry exactly the privacy type/message.
	for _, fn := range []struct {
		name string
		f    func(http.ResponseWriter)
	}{
		{"openai", openAIPrivacyUnavailable},
		{"anthropic", anthropicPrivacyUnavailable},
		{"responses", responsesPrivacyUnavailable},
	} {
		rec := httptest.NewRecorder()
		fn.f(rec)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), privacyUnavailableType) || !strings.Contains(rec.Body.String(), privacyUnavailableMessage) {
			t.Errorf("%s privacy envelope = %d %s", fn.name, rec.Code, rec.Body.String())
		}
	}
	// emitPrivacyExclusion: zero/negative is silent; positive emits a bus event.
	priv := &Server{bus: events.New(10)}
	priv.emitPrivacyExclusion("r1", 0, nil)
	priv.emitPrivacyExclusion("r1", 3, &route.ResolvedRoute{VirtualEndpointID: "ve"})
	_ = strings.Contains
	_ = fmt.Sprint
}
