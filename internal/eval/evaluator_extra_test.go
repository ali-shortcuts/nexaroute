package eval

import (
	"context"
	"encoding/json"
	"testing"
)

// TestJSONTypeMatches pins the type vocabulary the schema evaluator accepts.
// JSON integers decode as float64, so "number" must accept them; an unknown type
// name must reject rather than pass silently.
func TestJSONTypeMatches(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"string", `"x"`, "string", true},
		{"number", `1.5`, "number", true},
		{"integer decodes as number", `7`, "number", true},
		{"boolean", `true`, "boolean", true},
		{"array", `[1]`, "array", true},
		{"object", `{"a":1}`, "object", true},
		{"string is not number", `"x"`, "number", false},
		{"number is not string", `1`, "string", false},
		{"null is not string", `null`, "string", false},
		{"unknown type name", `"x"`, "integer", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got any
			if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
				t.Fatalf("decode %s: %v", tc.raw, err)
			}
			if ok := jsonTypeMatches(got, tc.want); ok != tc.ok {
				t.Fatalf("jsonTypeMatches(%s, %q) = %v, want %v", tc.raw, tc.want, ok, tc.ok)
			}
		})
	}
}

// TestJudgeSupportsEveryKind documents the judge contract: a judge is registered
// for every expectation kind, and only the deterministic-first resolver decides
// whether its verdict counts.
func TestJudgeSupportsEveryKind(t *testing.T) {
	judge := NewJudgeEvaluator("", func(context.Context, Case, Outcome) (Verdict, string) {
		return VerdictPass, "judge"
	})
	if judge.ID() != "llm_judge" {
		t.Fatalf("default judge id = %q, want llm_judge", judge.ID())
	}
	for _, kind := range AllExpectations {
		if !judge.Supports(kind) {
			t.Fatalf("judge must support %q", kind)
		}
	}
	if judge.Kind() != KindJudge {
		t.Fatalf("kind = %s, want judge", judge.Kind())
	}
	// A judge with no function records unjudged instead of guessing.
	vr := NewJudgeEvaluator("silent", nil).Evaluate(context.Background(), Case{}, Outcome{})
	if vr.Verdict != VerdictUnjudged || vr.Kind != KindJudge {
		t.Fatalf("unconfigured judge = %+v, want unjudged", vr)
	}
}
