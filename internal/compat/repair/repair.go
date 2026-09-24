package repair

import (
	"encoding/json"
	"strings"

	compaterrors "github.com/ali-shortcuts/nexaroute/internal/compat/errors"
)

// Bounded deterministic request repair. See spec section 9.
// At most MaxAttempts repairs per request; each rule is explicit and tested.
const MaxAttempts = 2

// Rule names for observability.
const (
	RuleDropUnsupportedParam = "drop_unsupported_param"
	RuleMaxCompletionToMax   = "max_completion_tokens_to_max_tokens"
	RuleDropStreamOptions    = "drop_stream_options"
	RuleDropReasoningEffort  = "drop_reasoning_effort"
)

// Attempt describes one applied repair.
type Attempt struct {
	Rule    string `json:"rule"`
	Removed string `json:"removed,omitempty"`
	Mapped  string `json:"mapped,omitempty"`
}

// Apply inspects a classified failure and returns a repaired payload.
// ok=false means no safe repair exists for this failure.
func Apply(payload []byte, classified compaterrors.Result) (repaired []byte, attempt Attempt, ok bool) {
	if !classified.RetryableRepair && classified.Class != compaterrors.UnsupportedParameter {
		return payload, Attempt{}, false
	}
	param := strings.ToLower(strings.TrimSpace(classified.Parameter))
	switch param {
	case "max_completion_tokens":
		if out, good := renameField(payload, "max_completion_tokens", "max_tokens"); good {
			return out, Attempt{Rule: RuleMaxCompletionToMax, Removed: "max_completion_tokens", Mapped: "max_tokens"}, true
		}
		return payload, Attempt{}, false
	case "stream_options":
		if out, good := dropField(payload, "stream_options"); good {
			return out, Attempt{Rule: RuleDropStreamOptions, Removed: "stream_options"}, true
		}
		return payload, Attempt{}, false
	case "reasoning_effort":
		if out, good := dropField(payload, "reasoning_effort"); good {
			return out, Attempt{Rule: RuleDropReasoningEffort, Removed: "reasoning_effort"}, true
		}
		// Some providers nest the control under reasoning; drop that too.
		if out, good := dropField(payload, "reasoning"); good {
			return out, Attempt{Rule: RuleDropReasoningEffort, Removed: "reasoning"}, true
		}
		return payload, Attempt{}, false
	case "reasoning", "thinking":
		if out, good := dropField(payload, param); good {
			return out, Attempt{Rule: RuleDropReasoningEffort, Removed: param}, true
		}
		return payload, Attempt{}, false
	case "temperature", "top_p", "top_k", "stop", "seed",
		"frequency_penalty", "presence_penalty", "logprobs",
		"parallel_tool_calls":
		if out, good := dropField(payload, param); good {
			return out, Attempt{Rule: RuleDropUnsupportedParam, Removed: param}, true
		}
		return payload, Attempt{}, false
	case "response_format", "json_schema", "tools", "tool_choice":
		// Semantics-critical fields are never silently dropped; the request
		// fails as health-neutral instead (consistent with the sanitizer,
		// which marks these ineligible rather than removing them).
		return payload, Attempt{}, false
	case "max_tokens":
		// The provider claims max_tokens itself is unsupported; there is no
		// safe generic mapping (some providers want max_completion_tokens,
		// but that flip-flop risks a repair loop). Decline repair.
		return payload, Attempt{}, false
	default:
		// Unknown parameter name: only stream_options-style known-safe
		// fields are repaired. Anything else is declined to stay bounded.
		return payload, Attempt{}, false
	}
}

func dropField(payload []byte, field string) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return payload, false
	}
	if _, ok := obj[field]; !ok {
		return payload, false
	}
	delete(obj, field)
	out, err := json.Marshal(obj)
	if err != nil {
		return payload, false
	}
	return out, true
}

func renameField(payload []byte, from, to string) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return payload, false
	}
	raw, ok := obj[from]
	if !ok {
		return payload, false
	}
	if _, exists := obj[to]; !exists {
		obj[to] = raw
	}
	delete(obj, from)
	out, err := json.Marshal(obj)
	if err != nil {
		return payload, false
	}
	return out, true
}
