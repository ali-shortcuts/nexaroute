package compat

import (
	"encoding/json"
	"strings"
	"testing"
)

func mission95Repair(t *testing.T, cls Classified, payload string, dialect DialectProfile, req RequirementProfile) (map[string]any, RepairPlan, bool) {
	t.Helper()
	out, plan, ok := Repair(cls, []byte(payload), dialect, req)
	if !ok {
		return nil, plan, false
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("repaired payload not JSON: %v", err)
	}
	return m, plan, true
}

func mission95MustRepair(t *testing.T, cls Classified, payload string, dialect DialectProfile, req RequirementProfile) map[string]any {
	t.Helper()
	m, plan, ok := mission95Repair(t, cls, payload, dialect, req)
	if !ok || len(plan.Rules) == 0 {
		t.Fatalf("expected repair for %s on %s, got ok=%v plan=%+v", cls.Parameter, payload, ok, plan)
	}
	return m
}

func mission95MustNotRepair(t *testing.T, cls Classified, payload string, dialect DialectProfile, req RequirementProfile) {
	t.Helper()
	if _, _, ok := mission95Repair(t, cls, payload, dialect, req); ok {
		t.Fatalf("expected no repair for %s on %s", cls.Parameter, payload)
	}
}

func TestMission95RepairTopLevelMatrix(t *testing.T) {
	openai := Dialects()["openai"]
	cls := func(param string) Classified {
		return Classified{Class: ClassUnsupportedParameter, Parameter: param, CapabilityFailure: true}
	}
	// Not a capability failure: no repair at all.
	mission95MustNotRepair(t, Classified{Class: ClassUnsupportedParameter, Parameter: "temperature"}, `{"temperature":0.5}`, openai, RequirementProfile{})
	// Token renames and drops.
	m := mission95MustRepair(t, cls("max_completion_tokens"), `{"max_completion_tokens":100}`, DialectProfile{MaxTokensKey: "max_tokens"}, RequirementProfile{})
	if _, ok := m["max_tokens"]; !ok {
		t.Fatalf("rename target missing: %v", m)
	}
	if _, ok := m["max_completion_tokens"]; ok {
		t.Fatalf("rename source survived: %v", m)
	}
	m = mission95MustRepair(t, cls("max_completion_tokens"), `{"max_completion_tokens":100,"max_tokens":50}`, DialectProfile{MaxTokensKey: "max_tokens"}, RequirementProfile{})
	if m["max_tokens"] != float64(50) || len(m) != 1 {
		t.Fatalf("duplicate token budget should keep existing target: %v", m)
	}
	m = mission95MustRepair(t, cls("max_tokens"), `{"max_tokens":100}`, DialectProfile{}, RequirementProfile{})
	if _, ok := m["max_completion_tokens"]; !ok {
		t.Fatalf("empty dialect key should flip max_tokens: %v", m)
	}
	m = mission95MustRepair(t, cls("max_tokens"), `{"max_tokens":100}`, DialectProfile{MaxTokensKey: "max_tokens"}, RequirementProfile{})
	if _, ok := m["max_completion_tokens"]; !ok {
		t.Fatalf("same-key dialect should flip max_tokens: %v", m)
	}
	mission95MustNotRepair(t, cls("max_tokens"), `{"model":"m"}`, openai, RequirementProfile{})
	// Simple drops, present and absent.
	for _, param := range []string{"stream_options", "parallel_tool_calls", "response_format", "seed", "logprobs", "logit_bias", "user", "n", "temperature", "top_p", "top_k"} {
		mission95MustRepair(t, cls(param), `{"`+param+`":1}`, openai, RequirementProfile{})
		mission95MustNotRepair(t, cls(param), `{"model":"m"}`, openai, RequirementProfile{})
	}
	// tool_choice: required tool use blocks, otherwise drops.
	mission95MustNotRepair(t, cls("tool_choice"), `{"tool_choice":"auto"}`, openai, RequirementProfile{NeedsTool: true})
	mission95MustRepair(t, cls("tool_choice"), `{"tool_choice":"auto"}`, openai, RequirementProfile{})
	mission95MustNotRepair(t, cls("tool_choice"), `{"model":"m"}`, openai, RequirementProfile{})
	// Reasoning: requested reasoning blocks; otherwise all controls drop.
	mission95MustNotRepair(t, cls("reasoning"), `{"reasoning_effort":"low"}`, openai, RequirementProfile{Reasoning: true})
	m = mission95MustRepair(t, cls("reasoning_effort"), `{"reasoning_effort":"low","reasoning":"x","thinking":"y"}`, openai, RequirementProfile{})
	for _, k := range []string{"reasoning_effort", "reasoning", "thinking"} {
		if _, ok := m[k]; ok {
			t.Fatalf("reasoning key %q survived: %v", k, m)
		}
	}
	mission95MustNotRepair(t, cls("reasoning"), `{"model":"m"}`, openai, RequirementProfile{})
	// Stop: "stop" drops; a lone "stop_sequences" does not (only "stop" is checked).
	mission95MustRepair(t, cls("stop"), `{"stop":["x"]}`, openai, RequirementProfile{})
	mission95MustNotRepair(t, cls("stop_sequences"), `{"stop_sequences":["x"]}`, openai, RequirementProfile{})
	// Generic unknown field: drop, semantics-critical refusal, excluded words, absent.
	mission95MustRepair(t, cls("custom_field"), `{"custom_field":1,"model":"m"}`, openai, RequirementProfile{})
	for _, param := range []string{"tools", "messages", "model", "stream", "request"} {
		mission95MustNotRepair(t, cls(param), `{"`+param+`":1}`, openai, RequirementProfile{})
	}
	mission95MustNotRepair(t, cls("custom_field"), `{"model":"m"}`, openai, RequirementProfile{})
	mission95MustNotRepair(t, cls(""), `{"model":"m"}`, openai, RequirementProfile{})
	// Non-parameter classes.
	mission95MustNotRepair(t, Classified{Class: ClassUnsupportedTools, CapabilityFailure: true}, `{"tools":[]}`, openai, RequirementProfile{})
	mission95MustNotRepair(t, Classified{Class: ClassUnsupportedVision, CapabilityFailure: true}, `{"messages":[]}`, openai, RequirementProfile{})
	mission95MustNotRepair(t, Classified{Class: ClassQuotaExhausted, CapabilityFailure: true}, `{"model":"m"}`, openai, RequirementProfile{})
	// Reasoning class.
	mission95MustNotRepair(t, Classified{Class: ClassUnsupportedReasoning, CapabilityFailure: true}, `{"reasoning_effort":"low"}`, openai, RequirementProfile{Reasoning: true})
	mission95MustNotRepair(t, Classified{Class: ClassUnsupportedReasoning, CapabilityFailure: true}, `{"model":"m"}`, openai, RequirementProfile{})
	mission95MustRepair(t, Classified{Class: ClassUnsupportedReasoning, CapabilityFailure: true}, `{"reasoning_content":"x"}`, openai, RequirementProfile{})
	// Repair of invalid JSON returns the original payload, unrepaired.
	if out, _, ok := Repair(cls("temperature"), []byte(`{bad`), openai, RequirementProfile{}); ok || string(out) != `{bad` {
		t.Fatalf("invalid JSON repair: ok=%v out=%s", ok, out)
	}
	// DescribePlan joins rule names.
	plan := RepairPlan{Rules: []RepairRule{{Name: "a"}, {Name: "b"}}}
	if got := DescribePlan(plan); got != "a,b" {
		t.Fatalf("DescribePlan = %q", got)
	}
	if got := DescribePlan(RepairPlan{}); got != "" {
		t.Fatalf("empty DescribePlan = %q", got)
	}
}

func mission95GeminiPayload() string {
	return `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.5,"topP":0.9,"topK":40,"maxOutputTokens":100,"stopSequences":["END"],"responseMimeType":"application/json","responseSchema":{"type":"object"},"candidateCount":2},"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},"safetySettings":[{"category":"HARM","threshold":"BLOCK_NONE"}]}`
}

func TestMission95RepairGeminiMatrix(t *testing.T) {
	openai := Dialects()["openai"]
	cls := func(param string) Classified {
		return Classified{Class: ClassUnsupportedParameter, Parameter: param, CapabilityFailure: true}
	}
	gc := func(m map[string]any) map[string]any {
		g, _ := m["generationConfig"].(map[string]any)
		return g
	}
	// Sampling knobs inside generationConfig.
	for _, tc := range []struct {
		param, gcKey string
	}{
		{"temperature", "temperature"}, {"top_p", "topP"}, {"topp", "topP"}, {"top_k", "topK"}, {"topk", "topK"},
	} {
		m := mission95MustRepair(t, cls(tc.param), mission95GeminiPayload(), openai, RequirementProfile{})
		if _, ok := gc(m)[tc.gcKey]; ok {
			t.Fatalf("gemini %s: gc key %q survived", tc.param, tc.gcKey)
		}
	}
	// Token budget, stop, structured output.
	m := mission95MustRepair(t, cls("max_output_tokens"), mission95GeminiPayload(), openai, RequirementProfile{})
	if _, ok := gc(m)["maxOutputTokens"]; ok {
		t.Fatalf("maxOutputTokens survived")
	}
	m = mission95MustRepair(t, cls("stopsequences"), mission95GeminiPayload(), openai, RequirementProfile{})
	if _, ok := gc(m)["stopSequences"]; ok {
		t.Fatalf("stopSequences survived")
	}
	m = mission95MustRepair(t, cls("response_format"), mission95GeminiPayload(), openai, RequirementProfile{})
	if _, ok := gc(m)["responseMimeType"]; ok {
		t.Fatalf("responseMimeType survived")
	}
	if _, ok := gc(m)["responseSchema"]; ok {
		t.Fatalf("responseSchema survived")
	}
	m = mission95MustRepair(t, cls("json_schema"), mission95GeminiPayload(), openai, RequirementProfile{})
	if _, ok := gc(m)["responseSchema"]; ok {
		t.Fatalf("json_schema should drop responseSchema")
	}
	// toolConfig: required tool use blocks, otherwise drops.
	mission95MustNotRepair(t, cls("tool_choice"), mission95GeminiPayload(), openai, RequirementProfile{NeedsTool: true})
	m = mission95MustRepair(t, cls("tool_choice"), mission95GeminiPayload(), openai, RequirementProfile{})
	if _, ok := m["toolConfig"]; ok {
		t.Fatalf("toolConfig survived")
	}
	// Top-level lingering OpenAI-style keys in a Gemini payload.
	m = mission95MustRepair(t, cls("seed"), `{"contents":[],"seed":42}`, openai, RequirementProfile{})
	if _, ok := m["seed"]; ok {
		t.Fatalf("lingering seed survived")
	}
	m = mission95MustRepair(t, cls("frequency_penalty"), `{"contents":[],"frequency_penalty":0.5}`, openai, RequirementProfile{})
	if _, ok := m["frequency_penalty"]; ok {
		t.Fatalf("lingering frequency_penalty survived")
	}
	mission95MustNotRepair(t, cls("seed"), `{"contents":[]}`, openai, RequirementProfile{})
	// candidateCount inside generationConfig.
	m = mission95MustRepair(t, cls("candidate_count"), mission95GeminiPayload(), openai, RequirementProfile{})
	if _, ok := gc(m)["candidateCount"]; ok {
		t.Fatalf("candidateCount survived")
	}
	// Generic Gemini default: top-level camelCase via mapping, snake_case
	// via lowercased key, and direct key.
	m = mission95MustRepair(t, cls("safety_settings"), mission95GeminiPayload(), openai, RequirementProfile{})
	if _, ok := m["safetySettings"]; ok {
		t.Fatalf("safetySettings survived")
	}
	m = mission95MustRepair(t, cls("SOME_FIELD"), `{"contents":[],"some_field":1}`, openai, RequirementProfile{})
	if _, ok := m["some_field"]; ok {
		t.Fatalf("lowercased generic field survived")
	}
	m = mission95MustRepair(t, cls("custom_field"), `{"contents":[],"custom_field":1}`, openai, RequirementProfile{})
	if _, ok := m["custom_field"]; ok {
		t.Fatalf("generic field survived")
	}
	// Generic Gemini default refusals: semantics-critical and excluded.
	// Note: a Gemini refusal falls through to the shared top-level path,
	// so "contents"/"systeminstruction" are NOT in this list — the
	// top-level generic branch drops them (asserted below as fallthrough).
	for _, param := range []string{"tools", "model", "stream", "messages", "request"} {
		mission95MustNotRepair(t, cls(param), `{"contents":[],"`+param+`":1}`, openai, RequirementProfile{})
	}
	// Fallthrough: Gemini refusal for "contents" still drops it via the
	// shared top-level generic branch.
	m = mission95MustRepair(t, cls("contents"), `{"contents":[{"role":"user"}]}`, openai, RequirementProfile{})
	if _, ok := m["contents"]; ok {
		t.Fatalf("fallthrough contents survived")
	}
	// toolconfig in the generic default with required tool use refuses.
	mission95MustNotRepair(t, cls("toolconfig"), `{"contents":[],"toolConfig":{}}`, openai, RequirementProfile{NeedsTool: true})
	// Generic default via the top-key mapping when tool use is not required.
	m = mission95MustRepair(t, cls("toolconfig"), `{"contents":[],"toolConfig":{}}`, openai, RequirementProfile{})
	if _, ok := m["toolConfig"]; ok {
		t.Fatalf("generic toolconfig survived")
	}
	// Non-parameter class in a Gemini payload: no repair.
	mission95MustNotRepair(t, Classified{Class: ClassUnsupportedTools, CapabilityFailure: true}, mission95GeminiPayload(), openai, RequirementProfile{})
	// Gemini fallthrough: GC temperature absent, top-level temperature present.
	m = mission95MustRepair(t, cls("temperature"), `{"contents":[],"temperature":0.5}`, openai, RequirementProfile{})
	if _, ok := m["temperature"]; ok {
		t.Fatalf("fallthrough top-level temperature survived")
	}
	// Gemini GC key / top key mapping tables, direct.
	for _, tc := range []struct {
		param, want string
	}{
		{"temperature", "temperature"}, {"top_p", "topP"}, {"topp", "topP"}, {"top_k", "topK"}, {"topk", "topK"},
		{"max_tokens", "maxOutputTokens"}, {"max_completion_tokens", "maxOutputTokens"}, {"max_output_tokens", "maxOutputTokens"}, {"maxoutputtokens", "maxOutputTokens"},
		{"stop", "stopSequences"}, {"stop_sequences", "stopSequences"}, {"stopsequences", "stopSequences"},
		{"response_format", "responseMimeType"}, {"responsemimetype", "responseMimeType"},
		{"json_schema", "responseSchema"}, {"response_schema", "responseSchema"}, {"responseschema", "responseSchema"},
		{"candidatecount", "candidateCount"}, {"candidate_count", "candidateCount"},
	} {
		if got, ok := geminiGCKeyForParam(tc.param); !ok || got != tc.want {
			t.Fatalf("geminiGCKeyForParam(%q) = %q,%v want %q,true", tc.param, got, ok, tc.want)
		}
	}
	if _, ok := geminiGCKeyForParam("nope"); ok {
		t.Fatalf("unknown GC param should not map")
	}
	for _, tc := range []struct {
		param, want string
	}{
		{"toolconfig", "toolConfig"}, {"tool_choice", "toolConfig"}, {"functioncallingconfig", "toolConfig"},
		{"safetysettings", "safetySettings"}, {"safety_settings", "safetySettings"},
		{"systeminstruction", "systemInstruction"}, {"system_message", "systemInstruction"}, {"system", "systemInstruction"},
	} {
		if got, ok := geminiTopKeyForParam(tc.param); !ok || got != tc.want {
			t.Fatalf("geminiTopKeyForParam(%q) = %q,%v want %q,true", tc.param, got, ok, tc.want)
		}
	}
	if _, ok := geminiTopKeyForParam("nope"); ok {
		t.Fatalf("unknown top param should not map")
	}
	if got := lowerParam("TopP"); got != "topp" {
		t.Fatalf("lowerParam = %q", got)
	}
}

func TestMission95SanitizeBranches(t *testing.T) {
	openai := Dialects()["openai"]
	// Invalid JSON errors.
	if _, err := Sanitize([]byte(`{bad`), Contract{}, openai, RequirementProfile{}); err == nil {
		t.Fatalf("invalid JSON should error")
	}
	// No unsupported entries: unchanged, no payload.
	res, err := Sanitize([]byte(`{"model":"m","temperature":0.5}`), Contract{}, openai, RequirementProfile{})
	if err != nil || res.Changed || res.Payload != nil {
		t.Fatalf("unchanged sanitize: %+v %v", res, err)
	}
	// Required stop is never removed.
	contract := Contract{}
	contract.Capabilities.Set(CapStop, Unsupported)
	res, err = Sanitize([]byte(`{"model":"m","stop":["END"],"stop_sequences":["X"]}`), contract, openai, RequirementProfile{Stop: true})
	if err != nil || res.Changed {
		t.Fatalf("required stop must be kept: %+v %v", res, err)
	}
	// max_completion_tokens renames via the dialect alias when the
	// dialect prefers max_tokens and no max_tokens is present yet.
	contract2 := Contract{}
	contract2.Capabilities.Set(CapMaxCompletionTokens, Unsupported)
	aliasDialect := DialectProfile{MaxTokensKey: "max_tokens", ParameterAliases: map[string]string{"max_completion_tokens": "max_tokens"}}
	res, err = Sanitize([]byte(`{"model":"m","max_completion_tokens":100}`), contract2, aliasDialect, RequirementProfile{})
	if err != nil || !res.Changed {
		t.Fatalf("rename sanitize: %+v %v", res, err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["max_tokens"]; !ok {
		t.Fatalf("renamed max_tokens missing: %s", res.Payload)
	}
	if got := res.Renamed["max_completion_tokens"]; got != "max_tokens" {
		t.Fatalf("renamed map: %v", res.Renamed)
	}
	// Existing max_tokens: the modern key is deleted from the working
	// map, but neither Removed nor Renamed records it, so Sanitize
	// reports unchanged and the deletion is not surfaced. Observed
	// behavior (the delete line itself still executes for coverage).
	res, err = Sanitize([]byte(`{"model":"m","max_completion_tokens":100,"max_tokens":50}`), contract2, openai, RequirementProfile{})
	if err != nil || res.Changed {
		t.Fatalf("hasMT sanitize should report unchanged: %+v %v", res, err)
	}
	// Self-alias (built-in openai dialect has no max_completion_tokens
	// alias): the rename target is the key itself, so the key is dropped
	// and recorded as renamed-to-itself. Observed behavior, asserted
	// so the branch stays covered if the alias table changes.
	res, err = Sanitize([]byte(`{"model":"m","max_completion_tokens":100}`), contract2, openai, RequirementProfile{})
	if err != nil || !res.Changed {
		t.Fatalf("self-alias sanitize: %+v %v", res, err)
	}
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["max_completion_tokens"]; ok {
		t.Fatalf("self-alias key survived: %v", out)
	}
	if res.Renamed["max_completion_tokens"] != "max_completion_tokens" {
		t.Fatalf("self-alias renamed map: %v", res.Renamed)
	}
	// Gemini: generationConfig removals for every knob.
	gcontract := Contract{}
	for _, cap := range []string{CapTemperature, CapTopP, CapMaxTokens, CapStructuredOutput, CapJSONObject, CapJSONSchema, CapStop} {
		gcontract.Capabilities.Set(cap, Unsupported)
	}
	res, err = Sanitize([]byte(mission95GeminiPayload()), gcontract, Dialects()["gemini"], RequirementProfile{})
	if err != nil || !res.Changed {
		t.Fatalf("gemini sanitize: %+v %v", res, err)
	}
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatal(err)
	}
	gc, _ := out["generationConfig"].(map[string]any)
	for _, k := range []string{"temperature", "topP", "topK", "maxOutputTokens", "stopSequences", "responseMimeType", "responseSchema"} {
		if _, ok := gc[k]; ok {
			t.Fatalf("gemini gc key %q survived: %s", k, res.Payload)
		}
	}
	if _, ok := gc["candidateCount"]; !ok {
		t.Fatalf("candidateCount must survive (not a listed knob): %s", res.Payload)
	}
	// Gemini token-budget alias: top-level budget moves into GC.
	res, err = Sanitize([]byte(`{"contents":[],"max_tokens":100}`), Contract{}, Dialects()["gemini"], RequirementProfile{})
	if err != nil || !res.Changed {
		t.Fatalf("gemini alias move: %+v %v", res, err)
	}
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatal(err)
	}
	gc, _ = out["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(100) {
		t.Fatalf("alias move failed: %v", out)
	}
	if got := res.Renamed["max_tokens"]; got != "generationConfig.maxOutputTokens" {
		t.Fatalf("alias renamed map: %v", res.Renamed)
	}
	// Existing GC budget wins; the top-level key is still dropped and
	// recorded as renamed.
	res, err = Sanitize([]byte(`{"contents":[],"generationConfig":{"maxOutputTokens":7},"max_tokens":100}`), Contract{}, Dialects()["gemini"], RequirementProfile{})
	if err != nil || !res.Changed {
		t.Fatalf("existing gc budget: %+v %v", res, err)
	}
	out = map[string]any{}
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatal(err)
	}
	gc, _ = out["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(7) {
		t.Fatalf("existing gc budget overwritten: %v", gc)
	}
	// Unsupported token capability: top-level budget is dropped, never re-added.
	tcontract := Contract{}
	tcontract.Capabilities.Set(CapMaxTokens, Unsupported)
	res, err = Sanitize([]byte(`{"contents":[],"max_tokens":100}`), tcontract, Dialects()["gemini"], RequirementProfile{})
	if err != nil || !res.Changed {
		t.Fatalf("unsupported token sanitize: %+v %v", res, err)
	}
	out = map[string]any{} // Unmarshal does not clear a reused map.
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["max_tokens"]; ok {
		t.Fatalf("unsupported top-level budget survived")
	}
	found := false
	for _, r := range res.Removed {
		if r == "max_tokens" {
			found = true
		}
	}
	if !found {
		t.Fatalf("removed should include max_tokens: %v", res.Removed)
	}
	// Non-map generationConfig: not a Gemini payload, no GC handling.
	res, err = Sanitize([]byte(`{"generationConfig":"str","temperature":0.5}`), Contract{}, openai, RequirementProfile{})
	if err != nil || res.Changed {
		t.Fatalf("non-map GC should be untouched without unsupported caps: %+v %v", res, err)
	}
	// generationConfigMap without create on a contents-only payload.
	if _, ok := generationConfigMap(map[string]any{"contents": []any{}}, false); ok {
		t.Fatalf("no GC should report absent without create")
	}
	if m, ok := generationConfigMap(map[string]any{}, true); !ok || m == nil {
		t.Fatalf("create should make a GC map")
	}
	if isGeminiPayload(nil) || isGeminiPayload(map[string]any{"generationConfig": "str"}) || isGeminiPayload(map[string]any{"contents": "str"}) {
		t.Fatalf("isGeminiPayload gates")
	}
	if !strings.Contains(DescribePlan(RepairPlan{Rules: []RepairRule{{Name: "drop:x"}}}), "drop:x") {
		t.Fatalf("describe plan")
	}
}
