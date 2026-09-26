// Package eval implements the Phase H evaluation plane: deterministic, bounded,
// provenance-first evaluation of recorded model behavior.
//
// Design rules:
//
//   - Deterministic evaluators outrank any judge. A judge verdict is recorded but
//     can never override a deterministic verdict, and the judge is disabled unless
//     an operator injects one and enables it explicitly.
//   - Evidence in, scores out. The runner replays recorded artifacts (or a
//     caller-supplied Executor); a missing artifact produces a "missing" case
//     result, never a synthetic score.
//   - Isolation. This package has no dependency on the router, the health manager,
//     the provider registry or the routing config. Evaluation can therefore not
//     change data-plane health, candidate ordering or routing state. A guard test
//     enforces the dependency direction.
//   - Bounded. Suites, case counts, artifact sizes, runs kept in memory and stored
//     runs on disk are all bounded by constants that are cheaper to reason about
//     than to configure.
package eval

import (
	"fmt"
	"sort"

	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// Expectation kinds. Each kind maps to deterministic evaluators.
const (
	ExpectExactMatch     = "exact_match"
	ExpectJSONValid      = "json_valid"
	ExpectJSONSchema     = "json_schema"
	ExpectToolCall       = "expected_tool_call"
	ExpectRegex          = "regex_match"
	ExpectUnitTests      = "unit_tests"
	ExpectNumericTol     = "numeric_tolerance"
	ExpectStreamTerminat = "stream_terminated"
)

// AllExpectations is the canonical, bounded set used by validation.
var AllExpectations = []string{
	ExpectExactMatch,
	ExpectJSONValid,
	ExpectJSONSchema,
	ExpectToolCall,
	ExpectRegex,
	ExpectUnitTests,
	ExpectNumericTol,
	ExpectStreamTerminat,
}

// Expectation is a declarative, deterministic scoring rule for one case.
type Expectation struct {
	Kind      string  `json:"kind"`
	Expected  string  `json:"expected,omitempty"`
	Schema    string  `json:"schema,omitempty"`
	ToolName  string  `json:"tool_name,omitempty"`
	Pattern   string  `json:"pattern,omitempty"`
	Tolerance float64 `json:"tolerance,omitempty"`
}

// Valid reports whether the expectation is well formed.
func (e Expectation) Valid() bool {
	known := false
	for _, k := range AllExpectations {
		if e.Kind == k {
			known = true
			break
		}
	}
	if !known {
		return false
	}
	switch e.Kind {
	case ExpectJSONSchema:
		return e.Schema != ""
	case ExpectToolCall:
		return e.ToolName != ""
	case ExpectRegex:
		return e.Pattern != ""
	}
	return true
}

// Case is one deterministic evaluation case inside a suite. Cases carry no
// prompts: the model output being judged lives in a recorded artifact keyed by
// CaseID.
type Case struct {
	ID          string      `json:"id"`
	Weight      float64     `json:"weight"`
	Expectation Expectation `json:"expectation"`
	Note        string      `json:"note,omitempty"`
}

// Suite is a versioned evaluation suite bound to one scorecard dimension.
type Suite struct {
	ID         string `json:"id"`
	Version    string `json:"version"`
	Dimension  scorecards.Dimension
	MinSamples int
	MaxCases   int
	Cases      []Case
}

// CatalogEntry is the bounded admin/metrics view of a suite.
type CatalogEntry struct {
	ID         string               `json:"id"`
	Version    string               `json:"version"`
	Dimension  scorecards.Dimension `json:"dimension"`
	MinSamples int                  `json:"min_samples"`
	Cases      int                  `json:"cases"`
}

// Bounds for suites and runs.
const (
	MaxCasesPerSuite = 128
	MaxExpectBytes   = 4096
)

func c(id string, weight float64, exp Expectation) Case {
	return Case{ID: id, Weight: weight, Expectation: exp}
}

// builtinSuites returns the Phase H suite catalog. Suites are declarative: cases
// describe the deterministic rule, artifacts describe what the model actually
// produced, and nothing in between is invented.
func builtinSuites() []Suite {
	return []Suite{
		{
			ID: "coding", Version: "1", Dimension: scorecards.DimCoding, MinSamples: 3, MaxCases: MaxCasesPerSuite,
			Cases: []Case{
				c("coding-function-implementation", 1, Expectation{Kind: ExpectUnitTests, Expected: "compiled-and-tests-pass"}),
				c("coding-bugfix", 1, Expectation{Kind: ExpectUnitTests, Expected: "compiled-and-tests-pass"}),
				c("coding-refactor-behavior-preserved", 1, Expectation{Kind: ExpectUnitTests, Expected: "compiled-and-tests-pass"}),
				c("coding-edge-case-handling", 0.5, Expectation{Kind: ExpectUnitTests, Expected: "compiled-and-tests-pass"}),
			},
		},
		{
			ID: "debugging", Version: "1", Dimension: scorecards.DimDebugging, MinSamples: 3, MaxCases: MaxCasesPerSuite,
			Cases: []Case{
				c("debugging-root-cause", 1, Expectation{Kind: ExpectUnitTests, Expected: "regression-tests-pass"}),
				c("debugging-null-deref", 1, Expectation{Kind: ExpectUnitTests, Expected: "regression-tests-pass"}),
				c("debugging-concurrency-race", 1, Expectation{Kind: ExpectUnitTests, Expected: "regression-tests-pass"}),
			},
		},
		{
			ID: "reasoning", Version: "1", Dimension: scorecards.DimReasoning, MinSamples: 3, MaxCases: MaxCasesPerSuite,
			Cases: []Case{
				c("reasoning-multi-step-arithmetic", 1, Expectation{Kind: ExpectExactMatch, Expected: "42"}),
				c("reasoning-logic-deduction", 1, Expectation{Kind: ExpectExactMatch, Expected: "carol"}),
				c("reasoning-numeric-estimate", 1, Expectation{Kind: ExpectNumericTol, Expected: "3.14", Tolerance: 0.01}),
			},
		},
		{
			ID: "tool_calling", Version: "1", Dimension: scorecards.DimToolCalling, MinSamples: 3, MaxCases: MaxCasesPerSuite,
			Cases: []Case{
				c("tools-single-call", 1, Expectation{Kind: ExpectToolCall, ToolName: "search_files"}),
				c("tools-parallel-calls", 1, Expectation{Kind: ExpectToolCall, ToolName: "read_file"}),
				c("tools-no-call-when-unnecessary", 1, Expectation{Kind: ExpectExactMatch, Expected: "no_tool"}),
			},
		},
		{
			ID: "structured_output", Version: "1", Dimension: scorecards.DimStructuredOutput, MinSamples: 3, MaxCases: MaxCasesPerSuite,
			Cases: []Case{
				c("structured-issue-summary", 1, Expectation{Kind: ExpectJSONSchema, Schema: `{"required":["title","severity"],"types":{"title":"string","severity":"string"}}`}),
				c("structured-tool-arguments", 1, Expectation{Kind: ExpectJSONSchema, Schema: `{"required":["path"],"types":{"path":"string"}}`}),
				c("structured-no-prose-wrapping", 1, Expectation{Kind: ExpectJSONValid}),
			},
		},
		{
			ID: "instruction_following", Version: "1", Dimension: scorecards.DimInstructionFollow, MinSamples: 3, MaxCases: MaxCasesPerSuite,
			Cases: []Case{
				c("instruction-max-words", 1, Expectation{Kind: ExpectRegex, Pattern: `^(\S+\s+){0,19}\S+$`}),
				c("instruction-language", 1, Expectation{Kind: ExpectRegex, Pattern: `(?i)^\s*(ok|بله)\s*$`}),
				c("instruction-no-preamble", 1, Expectation{Kind: ExpectExactMatch, Expected: "concise"}),
			},
		},
		{
			ID: "long_context", Version: "1", Dimension: scorecards.DimLongContext, MinSamples: 3, MaxCases: MaxCasesPerSuite,
			Cases: []Case{
				c("long-context-needle-early", 1, Expectation{Kind: ExpectExactMatch, Expected: "NEEDLE-A"}),
				c("long-context-needle-late", 1, Expectation{Kind: ExpectExactMatch, Expected: "NEEDLE-B"}),
				c("long-context-multi-hop", 1, Expectation{Kind: ExpectExactMatch, Expected: "NEEDLE-C"}),
			},
		},
		{
			ID: "protocol_compat", Version: "1", Dimension: scorecards.DimProtocolCompat, MinSamples: 3, MaxCases: MaxCasesPerSuite,
			Cases: []Case{
				c("protocol-stream-terminates", 1, Expectation{Kind: ExpectStreamTerminat, Expected: "done"}),
				c("protocol-tool-result-roundtrip", 1, Expectation{Kind: ExpectJSONValid}),
				c("protocol-usage-reported", 1, Expectation{Kind: ExpectRegex, Pattern: `"usage"\s*:`}),
			},
		},
	}
}

// Catalog returns the suite catalog in deterministic order.
func Catalog() []CatalogEntry {
	suites := builtinSuites()
	out := make([]CatalogEntry, 0, len(suites))
	for _, s := range suites {
		out = append(out, CatalogEntry{ID: s.ID, Version: s.Version, Dimension: s.Dimension, MinSamples: s.MinSamples, Cases: len(s.Cases)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// LookupSuite returns a built-in suite by id.
func LookupSuite(id string) (Suite, bool) {
	for _, s := range builtinSuites() {
		if s.ID == id {
			return s, true
		}
	}
	return Suite{}, false
}

// ValidateSuite checks suite internal consistency (used by tests and future
// operator-defined suites).
func ValidateSuite(s Suite) error {
	if s.ID == "" || s.Version == "" {
		return fmt.Errorf("suite id and version are required")
	}
	if !s.Dimension.Valid() {
		return fmt.Errorf("suite %q has unknown dimension %q", s.ID, string(s.Dimension))
	}
	if len(s.Cases) == 0 {
		return fmt.Errorf("suite %q has no cases", s.ID)
	}
	if len(s.Cases) > MaxCasesPerSuite {
		return fmt.Errorf("suite %q exceeds %d cases", s.ID, MaxCasesPerSuite)
	}
	if s.MinSamples < 1 {
		return fmt.Errorf("suite %q min_samples must be >= 1", s.ID)
	}
	seen := map[string]struct{}{}
	for _, cs := range s.Cases {
		if cs.ID == "" {
			return fmt.Errorf("suite %q has a case without id", s.ID)
		}
		if _, dup := seen[cs.ID]; dup {
			return fmt.Errorf("suite %q has duplicate case %q", s.ID, cs.ID)
		}
		seen[cs.ID] = struct{}{}
		if cs.Weight <= 0 || cs.Weight > 100 {
			return fmt.Errorf("case %q weight must be in (0,100]", cs.ID)
		}
		if !cs.Expectation.Valid() {
			return fmt.Errorf("case %q has invalid expectation", cs.ID)
		}
	}
	return nil
}
