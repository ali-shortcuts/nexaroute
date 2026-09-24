package sanitizer

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/compat/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/compat/capabilities"
	"github.com/ali-shortcuts/nexaroute/internal/compat/quirks"
)

func temp(v float64) *float64 { return &v }

func TestDropsOptionalUnsupported(t *testing.T) {
	req := canonical.Request{
		Model: "m", Temperature: temp(0.7),
		Messages: []canonical.Message{{Role: "user", Parts: []canonical.ContentPart{{Kind: canonical.ContentText, Text: "hi"}}}},
	}
	caps := capabilities.DefaultContract()
	caps.Set("temperature", capabilities.Cell{Value: capabilities.Unsupported})
	out, res := Sanitize(req, caps, quirks.Get("generic_openai"), DefaultPolicy())
	if res.Ineligible != "" {
		t.Fatalf("temperature is optional, must not be ineligible: %+v", res)
	}
	if out.Temperature != nil || !res.Modified || len(res.Dropped) != 1 {
		t.Fatalf("temperature should be dropped: %+v", res)
	}
	if req.Temperature == nil {
		t.Fatalf("input must not be mutated")
	}
}

func TestRequiredCapabilityIsIneligible(t *testing.T) {
	req := canonical.Request{
		Model:    "m",
		Messages: []canonical.Message{{Role: "user", Parts: []canonical.ContentPart{{Kind: canonical.ContentText, Text: "hi"}}}},
		Tools:    []canonical.ToolDef{{Name: "t"}},
	}
	caps := capabilities.DefaultContract()
	caps.Set("tools", capabilities.Cell{Value: capabilities.Unsupported})
	_, res := Sanitize(req, caps, quirks.Get("generic_openai"), DefaultPolicy())
	if res.Ineligible != "tools" {
		t.Fatalf("expected tools ineligible, got %+v", res)
	}
}

func TestParallelToolsDegrades(t *testing.T) {
	parallel := true
	req := canonical.Request{
		Model: "m", ParallelTools: &parallel,
		Messages: []canonical.Message{{Role: "user", Parts: []canonical.ContentPart{{Kind: canonical.ContentText, Text: "hi"}}}},
		Tools:    []canonical.ToolDef{{Name: "t"}},
	}
	caps := capabilities.DefaultContract()
	caps.Set("tools", capabilities.Cell{Value: capabilities.Supported})
	caps.Set("parallel_tools", capabilities.Cell{Value: capabilities.Unsupported})
	out, res := Sanitize(req, caps, quirks.Get("generic_openai"), DefaultPolicy())
	if res.Ineligible != "" {
		t.Fatalf("parallel tools degrade, must not be ineligible: %+v", res)
	}
	if out.ParallelTools == nil || *out.ParallelTools {
		t.Fatalf("parallel_tools should degrade to false")
	}
}

func TestPolicyCanDisableSanitizing(t *testing.T) {
	req := canonical.Request{
		Model: "m", Temperature: temp(0.7),
		Messages: []canonical.Message{{Role: "user", Parts: []canonical.ContentPart{{Kind: canonical.ContentText, Text: "hi"}}}},
	}
	caps := capabilities.DefaultContract()
	caps.Set("temperature", capabilities.Cell{Value: capabilities.Unsupported})
	out, res := Sanitize(req, caps, quirks.Get("generic_openai"), Policy{})
	if out.Temperature == nil || res.Modified {
		t.Fatalf("disabled policy must not drop: %+v", res)
	}
}
