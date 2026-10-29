package compat

import (
	"encoding/json"
	"testing"
)

func mission95GapsGeminiPayload(gc map[string]any) []byte {
	m := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}}}
	if gc != nil {
		m["generationConfig"] = gc
	}
	b, _ := json.Marshal(m)
	return b
}

func TestMission95SanitizeGeminiGCEdges(t *testing.T) {
	// Only temperature present: its removal runs, while the absent
	// topP/topK keys hit the remove() early return.
	contract := Contract{Capabilities: ModelCapabilities{Temperature: Unsupported}}
	res, err := Sanitize(mission95GapsGeminiPayload(map[string]any{"temperature": 0.5}), contract, DialectProfile{}, RequirementProfile{})
	if err != nil || !res.Changed {
		t.Fatalf("temp remove: %+v %v", res, err)
	}
	found := false
	for _, r := range res.Removed {
		if r == "generationConfig.temperature" {
			found = true
		}
	}
	if !found {
		t.Fatalf("removed: %v", res.Removed)
	}
	// Contents without generationConfig: the GC sanitizer finds no
	// object (no-create miss) and nothing changes.
	res, err = Sanitize(mission95GapsGeminiPayload(nil), contract, DialectProfile{}, RequirementProfile{})
	if err != nil || res.Changed {
		t.Fatalf("no GC: %+v %v", res, err)
	}
	// Token budget + stop + structured-output removals in one pass.
	contract = Contract{Capabilities: ModelCapabilities{
		MaxTokens: Unsupported, Stop: Unsupported,
		StructuredOutput: Unsupported, JSONSchema: Unsupported,
	}}
	res, err = Sanitize(mission95GapsGeminiPayload(map[string]any{
		"maxOutputTokens": 10, "stopSequences": []string{"END"},
		"responseMimeType": "application/json", "responseSchema": map[string]any{"type": "object"},
	}), contract, DialectProfile{}, RequirementProfile{})
	if err != nil || !res.Changed {
		t.Fatalf("gc bundle: %+v %v", res, err)
	}
	for _, want := range []string{"generationConfig.maxOutputTokens", "generationConfig.stopSequences", "generationConfig.responseMimeType", "generationConfig.responseSchema"} {
		ok := false
		for _, r := range res.Removed {
			if r == want {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("missing removal %q in %v", want, res.Removed)
		}
	}
	// Stop required by the request: stopSequences is kept even when
	// the capability is unsupported.
	res, err = Sanitize(mission95GapsGeminiPayload(map[string]any{"stopSequences": []string{"END"}}), contract, DialectProfile{}, RequirementProfile{Stop: true})
	if err != nil || res.Changed {
		t.Fatalf("required stop kept: %+v %v", res, err)
	}
	// MIME/schema kept when the capabilities are supported.
	contract = Contract{Capabilities: ModelCapabilities{}}
	res, err = Sanitize(mission95GapsGeminiPayload(map[string]any{
		"maxOutputTokens": 10, "responseMimeType": "application/json", "responseSchema": map[string]any{},
	}), contract, DialectProfile{}, RequirementProfile{})
	if err != nil || res.Changed {
		t.Fatalf("supported GC kept: %+v %v", res, err)
	}
}

func TestMission95PlanRepairTokenRename(t *testing.T) {
	// Dialect without a token key: max_completion_tokens renames to
	// max_tokens (the else branch of the target selection).
	cls := Classified{Class: ClassUnsupportedParameter, CapabilityFailure: true, Parameter: "max_completion_tokens"}
	payload := map[string]any{"max_completion_tokens": 10}
	plan, ok := planRepair(cls, payload, DialectProfile{}, RequirementProfile{})
	if !ok || len(plan.Rules) != 1 || plan.Rules[0].Name != "rename:max_completion_tokens->max_tokens" {
		t.Fatalf("rename: %+v %v", plan, ok)
	}
	if payload["max_tokens"] != 10 {
		t.Fatalf("renamed payload: %v", payload)
	}
	// Dialect key equal to the offending parameter: same else branch.
	plan, ok = planRepair(cls, map[string]any{"max_completion_tokens": 10}, DialectProfile{MaxTokensKey: "max_completion_tokens"}, RequirementProfile{})
	if !ok || plan.Rules[0].Name != "rename:max_completion_tokens->max_tokens" {
		t.Fatalf("same-key rename: %+v %v", plan, ok)
	}
	// max_tokens renames to max_completion_tokens when no key is set.
	cls.Parameter = "max_tokens"
	plan, ok = planRepair(cls, map[string]any{"max_tokens": 5}, DialectProfile{}, RequirementProfile{})
	if !ok || plan.Rules[0].Name != "rename:max_tokens->max_completion_tokens" {
		t.Fatalf("max_tokens rename: %+v %v", plan, ok)
	}
	// Both token keys present: the duplicate budget is dropped
	// instead of renamed (later switch case, not the rename branch).
	payload2 := map[string]any{"max_tokens": 5, "max_completion_tokens": 9}
	plan, ok = planRepair(cls, payload2, DialectProfile{}, RequirementProfile{})
	if !ok || len(plan.Rules) != 1 || plan.Rules[0].Name != "drop:max_tokens" {
		t.Fatalf("duplicate budget: %+v %v", plan, ok)
	}
	if _, exists := payload2["max_tokens"]; exists {
		t.Fatalf("max_tokens should be dropped: %v", payload2)
	}
}

func TestMission95PlanGeminiRepairGaps(t *testing.T) {
	unsupported := Classified{Class: ClassUnsupportedParameter, Parameter: "candidatecount"}
	// No generationConfig at all: dropGC fails on the missing object.
	payload := map[string]any{"contents": []any{}}
	if _, ok := planGeminiRepair(unsupported, payload, RequirementProfile{}); ok {
		t.Fatalf("candidatecount without GC should fail")
	}
	// GC present but candidateCount absent: dropGC fails on the key.
	payload = map[string]any{"contents": []any{}, "generationConfig": map[string]any{"temperature": 0.5}}
	if _, ok := planGeminiRepair(unsupported, payload, RequirementProfile{}); ok {
		t.Fatalf("candidatecount absent from GC should fail")
	}
	// GC with candidateCount: dropped with a rule.
	payload = map[string]any{"contents": []any{}, "generationConfig": map[string]any{"candidateCount": 2}}
	plan, ok := planGeminiRepair(unsupported, payload, RequirementProfile{})
	if !ok || len(plan.Rules) != 1 || plan.Rules[0].Name != "drop:generationConfig.candidateCount" {
		t.Fatalf("candidatecount drop: %+v %v", plan, ok)
	}
	// Non-parameter class: immediate refusal.
	if _, ok := planGeminiRepair(Classified{Class: ClassUnsupportedTools, Parameter: "temperature"}, payload, RequirementProfile{}); ok {
		t.Fatalf("non-parameter class should fail")
	}
	// Top-level OpenAI-style key lingering in a Gemini payload.
	seedCls := Classified{Class: ClassUnsupportedParameter, Parameter: "seed"}
	payload = map[string]any{"contents": []any{}, "seed": 42}
	plan, ok = planGeminiRepair(seedCls, payload, RequirementProfile{})
	if !ok || len(plan.Rules) != 1 || plan.Rules[0].Name != "drop:seed" || payload["seed"] != nil {
		t.Fatalf("seed drop: %+v %v %v", plan, ok, payload)
	}
	// Canonical snake_case variant: lowercased key absent, but the
	// original-cased key is present.
	seedCls.Parameter = "Seed"
	payload = map[string]any{"contents": []any{}, "Seed": 7}
	plan, ok = planGeminiRepair(seedCls, payload, RequirementProfile{})
	if !ok || plan.Rules[0].Name != "drop:Seed" {
		t.Fatalf("cased seed drop: %+v %v", plan, ok)
	}
	// Seed absent everywhere: no repair.
	payload = map[string]any{"contents": []any{}}
	if _, ok := planGeminiRepair(seedCls, payload, RequirementProfile{}); ok {
		t.Fatalf("absent seed should fail")
	}
	// toolconfig with tool use required: refusal, toolConfig kept.
	toolCls := Classified{Class: ClassUnsupportedParameter, Parameter: "toolconfig"}
	payload = map[string]any{"contents": []any{}, "toolConfig": map[string]any{"mode": "ANY"}}
	if _, ok := planGeminiRepair(toolCls, payload, RequirementProfile{NeedsTool: true}); ok {
		t.Fatalf("required toolconfig should fail")
	}
	if payload["toolConfig"] == nil {
		t.Fatalf("toolConfig must be kept on refusal")
	}
	// toolconfig not required: toolConfig dropped.
	plan, ok = planGeminiRepair(toolCls, payload, RequirementProfile{})
	if !ok || plan.Rules[0].Name != "drop:toolConfig" {
		t.Fatalf("toolconfig drop: %+v %v", plan, ok)
	}
	// toolconfig not required but no toolConfig present: no repair.
	payload = map[string]any{"contents": []any{}}
	if _, ok := planGeminiRepair(toolCls, payload, RequirementProfile{}); ok {
		t.Fatalf("absent toolconfig should fail")
	}
	// Semantics-critical Gemini fields are never dropped.
	for _, param := range []string{"tools", "contents", "model", "stream", "systeminstruction"} {
		payload = map[string]any{"contents": []any{}, param: "x"}
		if _, ok := planGeminiRepair(Classified{Class: ClassUnsupportedParameter, Parameter: param}, payload, RequirementProfile{}); ok {
			t.Fatalf("critical %q should fail", param)
		}
	}
	// Generic unknown field: top-level camelCase equivalent dropped.
	payload = map[string]any{"contents": []any{}, "safetySettings": []any{}}
	plan, ok = planGeminiRepair(Classified{Class: ClassUnsupportedParameter, Parameter: "safety_settings"}, payload, RequirementProfile{})
	if !ok || plan.Rules[0].Name != "drop:safetySettings" {
		t.Fatalf("safety drop: %+v %v", plan, ok)
	}
	// Generic unknown field with no GC, no top-level, no camelCase: fail.
	payload = map[string]any{"contents": []any{}}
	if _, ok := planGeminiRepair(Classified{Class: ClassUnsupportedParameter, Parameter: "frobnicate"}, payload, RequirementProfile{}); ok {
		t.Fatalf("unknown generic should fail")
	}
	// Generic unknown field present top-level under its lowered name.
	payload = map[string]any{"contents": []any{}, "frobnicate": 1}
	plan, ok = planGeminiRepair(Classified{Class: ClassUnsupportedParameter, Parameter: "frobnicate"}, payload, RequirementProfile{})
	if !ok || plan.Rules[0].Name != "drop:frobnicate" {
		t.Fatalf("generic top drop: %+v %v", plan, ok)
	}
}
