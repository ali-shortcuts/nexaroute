package canonical

import "testing"

// #436: DetectRequirements must surface the dialect-sensitive
// demands the router's conformance filter consumes — a non-text
// response_format is a structured-output requirement, and an
// explicitly enabled parallel_tool_calls is a parallel-tools
// requirement. Plain text format and an explicit parallel=false
// are NOT requirements.
func TestDetectRequirementsConformanceDimensions(t *testing.T) {
	yes := true
	no := false
	cases := []struct {
		name       string
		req        Request
		structured bool
		parallel   bool
	}{
		{"plain", Request{}, false, false},
		{"text format", Request{ResponseFormat: &ResponseFormat{Kind: FormatText}}, false, false},
		{"json object", Request{ResponseFormat: &ResponseFormat{Kind: FormatJSONObject}}, true, false},
		{"json schema", Request{ResponseFormat: &ResponseFormat{Kind: FormatJSONSchema, Name: "x"}}, true, false},
		{"parallel on", Request{ParallelToolCalls: &yes}, false, true},
		{"parallel off", Request{ParallelToolCalls: &no}, false, false},
		{"both", Request{ResponseFormat: &ResponseFormat{Kind: FormatJSONObject}, ParallelToolCalls: &yes}, true, true},
	}
	for _, tc := range cases {
		got := tc.req.DetectRequirements()
		if got.StructuredOutput != tc.structured || got.ParallelTools != tc.parallel {
			t.Errorf("%s: DetectRequirements = structured %v parallel %v, want %v/%v", tc.name, got.StructuredOutput, got.ParallelTools, tc.structured, tc.parallel)
		}
	}
}
