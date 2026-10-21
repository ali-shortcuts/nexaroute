package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The parameter sanitizer and bounded repair engine (spec sections 9-10).
//
// Sanitizer: proactive, pre-dispatch adaptation driven by the cached
// capability contract. Optional unsupported fields are removed; required
// capabilities that are UNSUPPORTED make the deployment ineligible.
//
// Repair: reactive, deterministic, bounded adaptation after a classified
// upstream rejection. One request performs at most MaxRepairAttempts repairs,
// every successful repair is cached into the capability contract, and a
// repair never masks a genuine model failure.

// RepairRule is one deterministic payload mutation.
type RepairRule struct {
	// Name is a stable identifier surfaced in events and the UI.
	Name string `json:"name"`
	// Capability updated on success ("" when the repair proves nothing).
	Capability string `json:"capability,omitempty"`
	// Description of the mutation for observability.
	Description string `json:"description"`
}

// RepairPlan is the deterministic repair computed for a classified failure.
type RepairPlan struct {
	Rules []RepairRule `json:"rules"`
	// Payload is the repaired JSON.
	Payload []byte `json:"-"`
}

// SanitizeResult reports proactive sanitizer decisions.
type SanitizeResult struct {
	Removed []string          `json:"removed,omitempty"`
	Renamed map[string]string `json:"renamed,omitempty"`
	// Payload carries the sanitized JSON when anything changed.
	Payload []byte `json:"-"`
	Changed bool   `json:"changed"`
}

// MaxRepairAttemptsDefault bounds repair attempts per request.
const MaxRepairAttemptsDefault = 1

// sanitizeMap applies capability-driven removals/renames to a payload map.
// required fields are never removed. Dialect aliases drive renames.
//
// Gemini-native payloads carry sampling/structural knobs inside
// `generationConfig` (temperature, topP/topK, maxOutputTokens,
// stopSequences, responseMimeType/responseSchema). When the payload is
// Gemini-shaped, the same capability-driven removals apply inside that
// object with identical conservative semantics; non-Gemini payloads follow
// the original top-level path byte-identically.
func sanitizeMap(payload map[string]any, contract Contract, dialect DialectProfile, req RequirementProfile) (SanitizeResult, bool) {
	res := SanitizeResult{Renamed: map[string]string{}}
	caps := contract.Capabilities
	removeOptional := func(key, capability string) {
		if _, ok := payload[key]; !ok {
			return
		}
		if caps.Get(capability) != Unsupported {
			return
		}
		// Semantics-critical fields are never silently dropped.
		switch capability {
		case CapTools, CapVision:
			return
		}
		delete(payload, key)
		res.Removed = append(res.Removed, key)
	}
	removeOptional("temperature", CapTemperature)
	removeOptional("top_p", CapTopP)
	removeOptional("top_k", CapTopP)
	removeOptional("seed", CapSeed)
	removeOptional("reasoning_effort", CapReasoningEffort)
	removeOptional("reasoning", CapReasoning)
	removeOptional("reasoning_content", CapReasoning)
	removeOptional("thinking", CapReasoning)
	removeOptional("response_format", CapStructuredOutput)
	removeOptional("parallel_tool_calls", CapParallelToolCalls)
	removeOptional("stream_options", CapStreaming)
	if !req.Stop {
		removeOptional("stop", CapStop)
		removeOptional("stop_sequences", CapStop)
	}
	// max_completion_tokens -> max_tokens rename when the dialect prefers
	// max_tokens and the upstream rejects the modern key.
	if v, ok := payload["max_completion_tokens"]; ok {
		if caps.Get(CapMaxCompletionTokens) == Unsupported || dialect.MaxTokensKey == "max_tokens" {
			if _, hasMT := payload["max_tokens"]; !hasMT {
				payload[dialect.AliasParameter("max_completion_tokens")] = v
				res.Renamed["max_completion_tokens"] = dialect.AliasParameter("max_completion_tokens")
			}
			delete(payload, "max_completion_tokens")
		}
	}
	if isGeminiPayload(payload) {
		sanitizeGeminiGenerationConfig(payload, contract, req, &res)
		// Gemini token-budget alias: a Gemini-shaped payload that still
		// carries an OpenAI-style top-level budget moves it into
		// generationConfig.maxOutputTokens (the only Gemini knob) —
		// unless a token capability is verified Unsupported, mirroring
		// sanitizeGeminiGenerationConfig: then the key is dropped and
		// recorded as removed, never re-added under the Gemini name.
		tokenUnsupported := caps.Get(CapMaxCompletionTokens) == Unsupported || caps.Get(CapMaxTokens) == Unsupported
		for _, topKey := range []string{"max_completion_tokens", "max_tokens", "max_output_tokens"} {
			if v, ok := payload[topKey]; ok {
				if tokenUnsupported {
					delete(payload, topKey)
					res.Removed = append(res.Removed, topKey)
					continue
				}
				if gc, ok := generationConfigMap(payload, true); ok {
					if _, exists := gc["maxOutputTokens"]; !exists {
						gc["maxOutputTokens"] = v
						res.Renamed[topKey] = "generationConfig.maxOutputTokens"
					}
				}
				delete(payload, topKey)
				if _, already := res.Renamed[topKey]; !already {
					res.Renamed[topKey] = "generationConfig.maxOutputTokens"
				}
			}
		}
	}
	if len(res.Removed) == 0 && len(res.Renamed) == 0 {
		return res, false
	}
	return res, true
}

// isGeminiPayload reports whether a decoded request body is Gemini-native:
// a GenerateContent shape with contents and/or generationConfig. OpenAI and
// Anthropic payloads use messages/input and never carry generationConfig,
// so this gate keeps non-Gemini behavior byte-identical.
func isGeminiPayload(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	if gc, ok := payload["generationConfig"]; ok {
		if _, isMap := gc.(map[string]any); isMap {
			return true
		}
	}
	if contents, ok := payload["contents"]; ok {
		if _, isSlice := contents.([]any); isSlice {
			return true
		}
	}
	return false
}

// generationConfigMap returns the mutable generationConfig object, creating
// it when create is true.
func generationConfigMap(payload map[string]any, create bool) (map[string]any, bool) {
	if raw, ok := payload["generationConfig"]; ok {
		if m, isMap := raw.(map[string]any); isMap {
			return m, true
		}
		return nil, false
	}
	if !create {
		return nil, false
	}
	m := map[string]any{}
	payload["generationConfig"] = m
	return m, true
}

// sanitizeGeminiGenerationConfig removes verified-unsupported optional knobs
// from generationConfig. Semantics-critical Gemini fields (contents, tools,
// toolConfig when tool use is required, systemInstruction) are never dropped.
func sanitizeGeminiGenerationConfig(payload map[string]any, contract Contract, req RequirementProfile, res *SanitizeResult) bool {
	gc, ok := generationConfigMap(payload, false)
	if !ok {
		return false
	}
	changed := false
	remove := func(key, capability string) {
		if _, present := gc[key]; !present {
			return
		}
		if contract.Capabilities.Get(capability) != Unsupported {
			return
		}
		delete(gc, key)
		res.Removed = append(res.Removed, "generationConfig."+key)
		changed = true
	}
	remove("temperature", CapTemperature)
	remove("topP", CapTopP)
	remove("topK", CapTopP)
	// Token budget: either token capability being unsupported means the
	// single Gemini knob cannot be sent.
	if _, present := gc["maxOutputTokens"]; present {
		if contract.Capabilities.Get(CapMaxCompletionTokens) == Unsupported || contract.Capabilities.Get(CapMaxTokens) == Unsupported {
			delete(gc, "maxOutputTokens")
			res.Removed = append(res.Removed, "generationConfig.maxOutputTokens")
			changed = true
		}
	}
	if !req.Stop {
		remove("stopSequences", CapStop)
	}
	// Structured output: dropping the MIME type alone is enough to fall back
	// to text; the schema is dropped with it.
	if _, hasMIME := gc["responseMimeType"]; hasMIME {
		if contract.Capabilities.Get(CapStructuredOutput) == Unsupported || contract.Capabilities.Get(CapJSONObject) == Unsupported {
			delete(gc, "responseMimeType")
			res.Removed = append(res.Removed, "generationConfig.responseMimeType")
			changed = true
		}
	}
	if _, hasSchema := gc["responseSchema"]; hasSchema {
		if contract.Capabilities.Get(CapStructuredOutput) == Unsupported || contract.Capabilities.Get(CapJSONSchema) == Unsupported {
			delete(gc, "responseSchema")
			res.Removed = append(res.Removed, "generationConfig.responseSchema")
			changed = true
		}
	}
	return changed
}

// Sanitize proactively adapts a provider payload before dispatch. It is a
// no-op when the contract has no verified UNSUPPORTED entries.
func Sanitize(payload []byte, contract Contract, dialect DialectProfile, req RequirementProfile) (SanitizeResult, error) {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return SanitizeResult{}, fmt.Errorf("sanitizer: payload is not a JSON object: %w", err)
	}
	res, changed := sanitizeMap(m, contract, dialect, req)
	if !changed {
		return res, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return res, err
	}
	res.Payload = b
	res.Changed = true
	return res, nil
}

// planRepair computes the deterministic repair rules for a classified
// capability failure. It returns false when no safe repair exists.
//
// Gemini-native payloads are repaired inside `generationConfig` (and the
// Gemini equivalents of repaired top-level fields) with the same audit and
// event surfacing as existing repairs. Non-Gemini payloads follow the
// original top-level path unchanged.
func planRepair(cls Classified, payload map[string]any, dialect DialectProfile, req RequirementProfile) (RepairPlan, bool) {
	plan := RepairPlan{}
	if !cls.CapabilityFailure {
		return plan, false
	}
	if isGeminiPayload(payload) {
		if geminiPlan, ok := planGeminiRepair(cls, payload, req); ok {
			return geminiPlan, true
		}
		// Fall through to the shared top-level repairs so a Gemini-shaped
		// payload that still carries OpenAI-style top-level keys is repaired
		// identically to today.
	}
	switch cls.Class {
	case ClassUnsupportedParameter:
		param := cls.Parameter
		cap := cls.Capability
		if cap == "" {
			cap = capabilityMapping[param]
		}
		switch param {
		case "max_completion_tokens", "max_output_tokens", "max_tokens":
			// Rename to the dialect's accepted token-limit key.
			target := dialect.MaxTokensKey
			if target == "" || target == param {
				if param == "max_tokens" {
					target = "max_completion_tokens"
				} else {
					target = "max_tokens"
				}
			}
			if v, ok := payload[param]; ok {
				if _, exists := payload[target]; !exists {
					payload[target] = v
					delete(payload, param)
					plan.Rules = append(plan.Rules, RepairRule{
						Name: "rename:" + param + "->" + target, Capability: CapMaxTokens,
						Description: fmt.Sprintf("%s renamed to %s", param, target),
					})
					return plan, true
				}
				delete(payload, param)
				plan.Rules = append(plan.Rules, RepairRule{
					Name: "drop:" + param, Capability: CapMaxCompletionTokens,
					Description: fmt.Sprintf("%s removed (duplicate token budget)", param),
				})
				return plan, true
			}
			return plan, false
		case "stream_options":
			if _, ok := payload["stream_options"]; ok {
				delete(payload, "stream_options")
				plan.Rules = append(plan.Rules, RepairRule{
					Name: "drop:stream_options", Capability: CapStreaming,
					Description: "stream_options removed (usage tail loss tolerated)",
				})
				return plan, true
			}
			return plan, false
		case "parallel_tool_calls":
			if _, ok := payload["parallel_tool_calls"]; ok {
				delete(payload, "parallel_tool_calls")
				plan.Rules = append(plan.Rules, RepairRule{
					Name: "drop:parallel_tool_calls", Capability: CapParallelToolCalls,
					Description: "parallel_tool_calls removed (model decides parallelism)",
				})
				return plan, true
			}
			return plan, false
		case "response_format":
			if _, ok := payload["response_format"]; ok {
				delete(payload, "response_format")
				plan.Rules = append(plan.Rules, RepairRule{
					Name: "drop:response_format", Capability: CapStructuredOutput,
					Description: "response_format removed (structured output not enforced)",
				})
				return plan, true
			}
			return plan, false
		case "tool_choice":
			if req.NeedsTool {
				// A forced tool call cannot be downgraded silently.
				return plan, false
			}
			if _, ok := payload["tool_choice"]; ok {
				delete(payload, "tool_choice")
				plan.Rules = append(plan.Rules, RepairRule{
					Name: "drop:tool_choice", Capability: CapToolChoiceAuto,
					Description: "tool_choice removed (model decides tool usage)",
				})
				return plan, true
			}
			return plan, false
		case "reasoning", "reasoning_effort", "thinking":
			if req.Reasoning {
				// The caller explicitly asked for reasoning; dropping it
				// silently would change semantics.
				return plan, false
			}
			removed := false
			for _, key := range []string{"reasoning_effort", "reasoning", "reasoning_content", "thinking"} {
				if _, ok := payload[key]; ok {
					delete(payload, key)
					removed = true
				}
			}
			if removed {
				plan.Rules = append(plan.Rules, RepairRule{
					Name: "drop:reasoning-controls", Capability: CapReasoning,
					Description: "reasoning controls removed (not requested as hard requirement)",
				})
				return plan, true
			}
			return plan, false
		case "seed", "logprobs", "logit_bias", "user", "n":
			if _, ok := payload[param]; ok {
				delete(payload, param)
				plan.Rules = append(plan.Rules, RepairRule{
					Name:        "drop:" + param,
					Description: param + " removed",
				})
				return plan, true
			}
			return plan, false
		case "temperature", "top_p", "top_k":
			if _, ok := payload[param]; ok {
				delete(payload, param)
				plan.Rules = append(plan.Rules, RepairRule{
					Name: "drop:" + param, Capability: cap,
					Description: fmt.Sprintf("%s removed (sampling default applies)", param),
				})
				return plan, true
			}
			return plan, false
		case "stop", "stop_sequences":
			if _, ok := payload["stop"]; ok {
				delete(payload, "stop")
				plan.Rules = append(plan.Rules, RepairRule{
					Name: "drop:stop", Capability: CapStop,
					Description: "stop sequences removed",
				})
				return plan, true
			}
			return plan, false
		default:
			// Generic unknown top-level field: drop it when it exists and is
			// not semantics-critical.
			if param != "" && !excludeFromParameter[param] {
				if _, ok := payload[param]; ok {
					switch param {
					case "tools", "messages", "model", "stream":
						return plan, false
					}
					delete(payload, param)
					plan.Rules = append(plan.Rules, RepairRule{
						Name:        "drop:" + param,
						Description: param + " removed (unknown field rejection)",
					})
					return plan, true
				}
			}
			return plan, false
		}
	case ClassUnsupportedReasoning:
		if req.Reasoning {
			return plan, false
		}
		removed := false
		for _, key := range []string{"reasoning_effort", "reasoning", "reasoning_content", "thinking"} {
			if _, ok := payload[key]; ok {
				delete(payload, key)
				removed = true
			}
		}
		if removed {
			plan.Rules = append(plan.Rules, RepairRule{
				Name: "drop:reasoning-controls", Capability: CapReasoning,
				Description: "reasoning controls removed",
			})
			return plan, true
		}
		return plan, false
	case ClassUnsupportedTools:
		// Tools are semantics-critical for agent traffic; never stripped.
		return plan, false
	case ClassUnsupportedVision:
		// Images carry user semantics; never stripped silently.
		return plan, false
	default:
		return plan, false
	}
}

// planGeminiRepair repairs Gemini-native payloads. It mirrors the existing
// repair operations: sampling knobs and structural knobs inside
// generationConfig, plus the Gemini equivalents of repaired top-level fields
// (toolConfig for tool_choice, stopSequences for stop, maxOutputTokens for
// token budgets, responseMimeType/responseSchema for response_format).
// Semantics-critical fields (contents, tools, systemInstruction, toolConfig
// when tool use is required, vision parts) are never stripped.
func planGeminiRepair(cls Classified, payload map[string]any, req RequirementProfile) (RepairPlan, bool) {
	plan := RepairPlan{}
	if cls.Class != ClassUnsupportedParameter {
		return plan, false
	}
	paramLower := lowerParam(cls.Parameter)
	cap := cls.Capability
	if cap == "" {
		cap = capabilityMapping[paramLower]
	}
	gc, hasGC := generationConfigMap(payload, false)
	dropGC := func(key, capability, description string) (RepairPlan, bool) {
		if !hasGC {
			return plan, false
		}
		if _, ok := gc[key]; !ok {
			return plan, false
		}
		delete(gc, key)
		plan.Rules = append(plan.Rules, RepairRule{
			Name: "drop:generationConfig." + key, Capability: capability,
			Description: description,
		})
		return plan, true
	}
	switch paramLower {
	case "temperature":
		if p, ok := dropGC("temperature", CapTemperature, "temperature removed (sampling default applies)"); ok {
			return p, true
		}
	case "top_p", "topp":
		if p, ok := dropGC("topP", CapTopP, "topP removed (sampling default applies)"); ok {
			return p, true
		}
	case "top_k", "topk":
		if p, ok := dropGC("topK", CapTopP, "topK removed (sampling default applies)"); ok {
			return p, true
		}
	case "max_completion_tokens", "max_output_tokens", "max_tokens", "maxoutputtokens":
		// Gemini has a single token knob; there is no alias to rename to,
		// so the bounded repair drops it and records the capability fact.
		if hasGC {
			if _, ok := gc["maxOutputTokens"]; ok {
				delete(gc, "maxOutputTokens")
				plan.Rules = append(plan.Rules, RepairRule{
					Name: "drop:generationConfig.maxOutputTokens", Capability: CapMaxCompletionTokens,
					Description: "maxOutputTokens removed (token budget not enforced)",
				})
				return plan, true
			}
		}
	case "stop", "stop_sequences", "stopsequences":
		if p, ok := dropGC("stopSequences", CapStop, "stopSequences removed"); ok {
			return p, true
		}
	case "response_format", "responsemimetype", "json_schema", "response_schema", "responseschema", "json_object", "json":
		if hasGC {
			removed := false
			for _, key := range []string{"responseMimeType", "responseSchema"} {
				if _, ok := gc[key]; ok {
					delete(gc, key)
					removed = true
				}
			}
			if removed {
				plan.Rules = append(plan.Rules, RepairRule{
					Name: "drop:generationConfig.responseMimeType", Capability: CapStructuredOutput,
					Description: "responseMimeType/responseSchema removed (structured output not enforced)",
				})
				return plan, true
			}
		}
	case "tool_choice", "toolconfig", "functioncallingconfig":
		if req.NeedsTool {
			return plan, false
		}
		if _, ok := payload["toolConfig"]; ok {
			delete(payload, "toolConfig")
			plan.Rules = append(plan.Rules, RepairRule{
				Name: "drop:toolConfig", Capability: CapToolChoiceAuto,
				Description: "toolConfig removed (model decides tool usage)",
			})
			return plan, true
		}
	case "seed", "logprobs", "logit_bias", "user", "n", "frequency_penalty", "presence_penalty", "top_logprobs":
		if _, ok := payload[paramLower]; ok {
			// Top-level OpenAI-style key lingering in a Gemini payload.
			delete(payload, paramLower)
			plan.Rules = append(plan.Rules, RepairRule{
				Name:        "drop:" + paramLower,
				Description: paramLower + " removed",
			})
			return plan, true
		}
		// Canonical snake_case variants that may appear top-level.
		if _, ok := payload[cls.Parameter]; ok {
			delete(payload, cls.Parameter)
			plan.Rules = append(plan.Rules, RepairRule{
				Name:        "drop:" + cls.Parameter,
				Description: cls.Parameter + " removed",
			})
			return plan, true
		}
		return plan, false
	case "candidatecount", "candidate_count":
		if p, ok := dropGC("candidateCount", cap, "candidateCount removed"); ok {
			return p, true
		}
	default:
		// Generic Gemini field: try generationConfig first (case-insensitive),
		// then Gemini top-level optionals. Never strip semantics-critical
		// fields.
		if hasGC {
			if actual, ok := geminiGCKeyForParam(paramLower); ok {
				if _, exists := gc[actual]; exists {
					switch actual {
					case "candidateCount":
						delete(gc, actual)
						plan.Rules = append(plan.Rules, RepairRule{
							Name:        "drop:generationConfig." + actual,
							Description: actual + " removed (unknown field rejection)",
						})
						return plan, true
					}
				}
			}
		}
		if paramLower != "" && !excludeFromParameter[paramLower] {
			switch paramLower {
			case "tools", "contents", "model", "stream", "systeminstruction", "system", "system_message":
				return plan, false
			case "toolconfig":
				if req.NeedsTool {
					return plan, false
				}
			}
			if _, ok := payload[cls.Parameter]; ok {
				switch cls.Parameter {
				case "tools", "contents", "model", "stream":
					return plan, false
				}
				delete(payload, cls.Parameter)
				plan.Rules = append(plan.Rules, RepairRule{
					Name:        "drop:" + cls.Parameter,
					Description: cls.Parameter + " removed (unknown field rejection)",
				})
				return plan, true
			}
			if _, ok := payload[paramLower]; ok {
				delete(payload, paramLower)
				plan.Rules = append(plan.Rules, RepairRule{
					Name:        "drop:" + paramLower,
					Description: paramLower + " removed (unknown field rejection)",
				})
				return plan, true
			}
			// Gemini top-level camelCase equivalents.
			if actual, ok := geminiTopKeyForParam(paramLower); ok {
				if _, exists := payload[actual]; exists {
					delete(payload, actual)
					plan.Rules = append(plan.Rules, RepairRule{
						Name:        "drop:" + actual,
						Description: actual + " removed (unknown field rejection)",
					})
					return plan, true
				}
			}
		}
		return plan, false
	}
	return plan, false
}

// lowerParam normalizes a classifier parameter for case-insensitive lookup.
func lowerParam(p string) string {
	lowered := ""
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			lowered += string(r + ('a' - 'A'))
		} else {
			lowered += string(r)
		}
	}
	return lowered
}

// geminiGCKeyForParam maps a lowercased parameter onto its generationConfig
// key for generic unknown-field repairs.
func geminiGCKeyForParam(paramLower string) (string, bool) {
	switch paramLower {
	case "temperature":
		return "temperature", true
	case "top_p", "topp":
		return "topP", true
	case "top_k", "topk":
		return "topK", true
	case "max_tokens", "max_completion_tokens", "max_output_tokens", "maxoutputtokens":
		return "maxOutputTokens", true
	case "stop", "stop_sequences", "stopsequences":
		return "stopSequences", true
	case "response_format", "responsemimetype":
		return "responseMimeType", true
	case "json_schema", "response_schema", "responseschema":
		return "responseSchema", true
	case "candidatecount", "candidate_count":
		return "candidateCount", true
	default:
		return "", false
	}
}

// geminiTopKeyForParam maps a lowercased parameter onto a Gemini top-level
// key for generic unknown-field repairs.
func geminiTopKeyForParam(paramLower string) (string, bool) {
	switch paramLower {
	case "toolconfig", "tool_choice", "functioncallingconfig":
		return "toolConfig", true
	case "safetysettings", "safety_settings":
		return "safetySettings", true
	case "systeminstruction", "system_message", "system":
		return "systemInstruction", true
	default:
		return "", false
	}
}

// Repair applies the deterministic bounded repair to a raw payload.
// It returns the repaired payload, the applied plan, and whether a repair
// was possible at all.
func Repair(cls Classified, payload []byte, dialect DialectProfile, req RequirementProfile) ([]byte, RepairPlan, bool) {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return payload, RepairPlan{}, false
	}
	plan, ok := planRepair(cls, m, dialect, req)
	if !ok || len(plan.Rules) == 0 {
		return payload, plan, false
	}
	b, err := json.Marshal(m)
	if err != nil {
		return payload, plan, false
	}
	plan.Payload = b
	return b, plan, true
}

// DescribePlan renders a repair plan for events/UI.
func DescribePlan(p RepairPlan) string {
	parts := make([]string, 0, len(p.Rules))
	for _, r := range p.Rules {
		parts = append(parts, r.Name)
	}
	return strings.Join(parts, ",")
}
