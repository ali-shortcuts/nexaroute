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
	if len(res.Removed) == 0 && len(res.Renamed) == 0 {
		return res, false
	}
	return res, true
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
func planRepair(cls Classified, payload map[string]any, dialect DialectProfile, req RequirementProfile) (RepairPlan, bool) {
	plan := RepairPlan{}
	if !cls.CapabilityFailure {
		return plan, false
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
