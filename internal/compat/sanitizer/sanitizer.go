package sanitizer

import (
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/compat/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/compat/capabilities"
	"github.com/ali-shortcuts/nexaroute/internal/compat/quirks"
)

// Policy controls which optional unsupported fields may be dropped.
// Semantics-critical fields (required capabilities) are never dropped:
// the deployment is ineligible instead. See spec section 10.
type Policy struct {
	// RemoveOptionalUnsupported permits dropping optional unsupported fields.
	RemoveOptionalUnsupported bool
	// ApplyDialectDefaults permits dialect-driven normalization
	// (e.g. max_completion_tokens -> max_tokens for NIM).
	ApplyDialectDefaults bool
}

// DefaultPolicy is the recommended production policy.
func DefaultPolicy() Policy {
	return Policy{RemoveOptionalUnsupported: true, ApplyDialectDefaults: true}
}

// Result describes what the sanitizer changed.
type Result struct {
	// Dropped lists removed optional field names.
	Dropped []string `json:"dropped"`
	// Ineligible names the missing REQUIRED capability, if any.
	Ineligible string `json:"ineligible,omitempty"`
	// Modified reports whether the request changed.
	Modified bool `json:"modified"`
}

// Sanitize adapts a canonical request to a deployment contract.
// It returns the adapted request and a result describing the changes.
// The input request is never mutated.
func Sanitize(in canonical.Request, caps capabilities.ModelCapabilities, dialect quirks.DialectProfile, policy Policy) (canonical.Request, Result) {
	out := clone(in)
	res := Result{}
	req := out.Requirements()

	// REQUIRED capabilities gate eligibility; never silently drop them.
	for _, need := range req.Required {
		if caps.Get(string(need)).Value == capabilities.Unsupported {
			res.Ineligible = string(need)
			return in, res
		}
	}

	drop := func(field string, apply func()) {
		if !policy.RemoveOptionalUnsupported {
			return
		}
		apply()
		res.Dropped = append(res.Dropped, field)
		res.Modified = true
	}

	if out.Temperature != nil && caps.Get("temperature").Value == capabilities.Unsupported {
		drop("temperature", func() { out.Temperature = nil })
	}
	if out.TopP != nil && caps.Get("top_p").Value == capabilities.Unsupported {
		drop("top_p", func() { out.TopP = nil })
	}
	if len(out.Stop) > 0 && caps.Get("stop").Value == capabilities.Unsupported {
		drop("stop", func() { out.Stop = nil })
	}
	if dialect.DropTemperature && out.Temperature != nil {
		drop("temperature", func() { out.Temperature = nil })
	}
	if out.Reasoning != nil && out.Reasoning.Enabled {
		// Reasoning is required only when the request uses it (handled
		// above); when merely optional, an unsupported signal still allows
		// the request if the caller tolerates dropping it. We drop only
		// when reasoning is NOT in the required set.
		required := false
		for _, need := range req.Required {
			if need == canonical.CapReasoning {
				required = true
				break
			}
		}
		if !required && caps.Get("reasoning").Value == capabilities.Unsupported {
			drop("reasoning", func() { out.Reasoning = nil })
		} else if policy.ApplyDialectDefaults && dialect.ReasoningField == "none" && out.Reasoning.Effort != "" {
			drop("reasoning_effort", func() { out.Reasoning.Effort = "" })
		}
	}
	if policy.ApplyDialectDefaults && dialect.MaxTokensField == "max_tokens" {
		// Canonical form carries MaxTokens already; dialect mapping happens
		// at encode time. Nothing to drop here.
	}
	if out.ResponseFormat != nil && caps.Get("structured_output").Value == capabilities.Unsupported {
		// Structured output is required when used; reaching here means the
		// contract disagrees about required-ness (e.g. stale). Stay safe:
		// mark ineligible rather than silently changing response shape.
		res.Ineligible = "structured_output"
		return in, res
	}
	// Parallel tools degrade gracefully: keep tools, allow serial fallback.
	if out.ParallelTools != nil && *out.ParallelTools && caps.Get("parallel_tools").Value == capabilities.Unsupported {
		drop("parallel_tools", func() {
			v := false
			out.ParallelTools = &v
		})
	}
	return out, res
}

func clone(in canonical.Request) canonical.Request {
	out := in
	out.Messages = append([]canonical.Message(nil), in.Messages...)
	for i := range out.Messages {
		out.Messages[i].Parts = append([]canonical.ContentPart(nil), in.Messages[i].Parts...)
	}
	out.Tools = append([]canonical.ToolDef(nil), in.Tools...)
	out.Stop = append([]string(nil), in.Stop...)
	if in.Temperature != nil {
		v := *in.Temperature
		out.Temperature = &v
	}
	if in.TopP != nil {
		v := *in.TopP
		out.TopP = &v
	}
	if in.ParallelTools != nil {
		v := *in.ParallelTools
		out.ParallelTools = &v
	}
	if in.Reasoning != nil {
		r := *in.Reasoning
		out.Reasoning = &r
	}
	if in.ResponseFormat != nil {
		f := *in.ResponseFormat
		out.ResponseFormat = &f
	}
	return out
}

// FieldName normalizes upstream parameter names for reporting.
func FieldName(p string) string {
	return strings.ToLower(strings.TrimSpace(p))
}
