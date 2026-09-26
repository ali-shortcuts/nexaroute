package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Verdict is the canonical case outcome.
type Verdict string

const (
	VerdictPass     Verdict = "pass"
	VerdictFail     Verdict = "fail"
	VerdictError    Verdict = "error"    // evaluator could not decide (bad input)
	VerdictSkip     Verdict = "skip"     // not applicable
	VerdictMissing  Verdict = "missing"  // no recorded artifact for the case
	VerdictUnjudged Verdict = "unjudged" // only a judge could score it and no judge ran
)

// AllVerdicts is the canonical bounded set for metrics and validation.
var AllVerdicts = []Verdict{VerdictPass, VerdictFail, VerdictError, VerdictSkip, VerdictMissing, VerdictUnjudged}

// Decisive reports whether the verdict contributes to a score.
func (v Verdict) Decisive() bool {
	return v == VerdictPass || v == VerdictFail
}

// EvaluatorKind separates deterministic rules from judges. This separation is the
// precedence mechanism: the resolver never lets a judge override a deterministic
// verdict.
type EvaluatorKind string

const (
	KindDeterministic EvaluatorKind = "deterministic"
	KindJudge         EvaluatorKind = "judge"
)

// Evaluator decides one verdict for one recorded model behavior.
type Evaluator interface {
	ID() string
	Kind() EvaluatorKind
	// Supports reports whether the evaluator can judge this expectation kind.
	Supports(expectKind string) bool
	Evaluate(ctx context.Context, c Case, o Outcome) VerdictResult
}

// VerdictResult is a bounded evaluator verdict.
type VerdictResult struct {
	EvaluatorID string        `json:"evaluator_id"`
	Kind        EvaluatorKind `json:"kind"`
	Verdict     Verdict       `json:"verdict"`
	Reason      string        `json:"reason,omitempty"`
}

// Bounds for evaluation
const (
	MaxVerdictsPerCase = 8
	MaxReasonBytes     = 256
	MaxOutputBytes     = 64 << 10
)

func bounded(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// ---------------------------------------------------------------------------
// Deterministic evaluators
// ---------------------------------------------------------------------------

type fnEvaluator struct {
	id       string
	kind     EvaluatorKind
	supports func(string) bool
	fn       func(c Case, o Outcome) (Verdict, string)
}

func (e *fnEvaluator) ID() string          { return e.id }
func (e *fnEvaluator) Kind() EvaluatorKind { return e.kind }
func (e *fnEvaluator) Supports(k string) bool {
	if e.supports == nil {
		return true
	}
	return e.supports(k)
}
func (e *fnEvaluator) Evaluate(_ context.Context, c Case, o Outcome) VerdictResult {
	v, reason := e.fn(c, o)
	if !v.Valid() {
		v = VerdictError
		reason = "evaluator returned an unknown verdict"
	}
	return VerdictResult{EvaluatorID: e.id, Kind: e.kind, Verdict: v, Reason: bounded(reason, MaxReasonBytes)}
}

// Valid reports whether v is canonical.
func (v Verdict) Valid() bool {
	for _, known := range AllVerdicts {
		if v == known {
			return true
		}
	}
	return false
}

func exactMatchEvaluator() Evaluator {
	return &fnEvaluator{
		id: "exact_match", kind: KindDeterministic,
		supports: func(k string) bool { return k == ExpectExactMatch },
		fn: func(c Case, o Outcome) (Verdict, string) {
			if o.Status != "" && o.Status != OutcomeOK {
				return VerdictFail, "upstream status " + o.Status
			}
			if strings.TrimSpace(o.Output) == strings.TrimSpace(c.Expectation.Expected) {
				return VerdictPass, "output matches expected value"
			}
			return VerdictFail, "output does not match expected value"
		},
	}
}

func jsonValidEvaluator() Evaluator {
	return &fnEvaluator{
		id: "json_valid", kind: KindDeterministic,
		supports: func(k string) bool { return k == ExpectJSONValid },
		fn: func(_ Case, o Outcome) (Verdict, string) {
			var v any
			if err := json.Unmarshal([]byte(strings.TrimSpace(o.Output)), &v); err != nil {
				return VerdictFail, "output is not valid JSON"
			}
			return VerdictPass, "output is valid JSON"
		},
	}
}

// schemaSpec is the deterministic subset of JSON Schema used by the suite
// definitions: required top-level keys plus an optional type map. It is
// deliberately small, dependency-free and predictable.
type schemaSpec struct {
	Required []string          `json:"required"`
	Types    map[string]string `json:"types"`
}

func jsonSchemaEvaluator() Evaluator {
	return &fnEvaluator{
		id: "json_schema", kind: KindDeterministic,
		supports: func(k string) bool { return k == ExpectJSONSchema },
		fn: func(c Case, o Outcome) (Verdict, string) {
			var spec schemaSpec
			if err := json.Unmarshal([]byte(c.Expectation.Schema), &spec); err != nil {
				return VerdictError, "case schema is not valid JSON"
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(o.Output)), &got); err != nil {
				return VerdictFail, "output is not a JSON object"
			}
			for _, key := range spec.Required {
				if _, ok := got[key]; !ok {
					return VerdictFail, "missing required key " + key
				}
			}
			for key, want := range spec.Types {
				val, ok := got[key]
				if !ok {
					continue
				}
				if !jsonTypeMatches(val, want) {
					return VerdictFail, "key " + key + " has wrong type"
				}
			}
			return VerdictPass, "output satisfies schema"
		},
	}
}

func jsonTypeMatches(v any, want string) bool {
	switch want {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	default:
		return false
	}
}

func toolCallEvaluator() Evaluator {
	return &fnEvaluator{
		id: "expected_tool_call", kind: KindDeterministic,
		supports: func(k string) bool { return k == ExpectToolCall },
		fn: func(c Case, o Outcome) (Verdict, string) {
			if o.ToolCall == nil {
				return VerdictFail, "no tool call recorded"
			}
			if o.ToolCall.Name != c.Expectation.ToolName {
				return VerdictFail, "tool call name mismatch"
			}
			return VerdictPass, "expected tool call observed"
		},
	}
}

func regexEvaluator() Evaluator {
	return &fnEvaluator{
		id: "regex_match", kind: KindDeterministic,
		supports: func(k string) bool { return k == ExpectRegex },
		fn: func(c Case, o Outcome) (Verdict, string) {
			re, err := regexp.Compile(c.Expectation.Pattern)
			if err != nil {
				return VerdictError, "case pattern does not compile"
			}
			if re.MatchString(o.Output) {
				return VerdictPass, "output matches pattern"
			}
			return VerdictFail, "output does not match pattern"
		},
	}
}

func unitTestsEvaluator() Evaluator {
	return &fnEvaluator{
		id: "unit_tests", kind: KindDeterministic,
		supports: func(k string) bool { return k == ExpectUnitTests },
		fn: func(_ Case, o Outcome) (Verdict, string) {
			if o.UnitTests == nil {
				return VerdictFail, "no test result recorded"
			}
			ut := o.UnitTests
			if !ut.Compiled {
				return VerdictFail, "code did not compile"
			}
			if ut.Failed > 0 {
				return VerdictFail, "tests failed"
			}
			if ut.Passed <= 0 {
				return VerdictFail, "no test executed"
			}
			return VerdictPass, "tests passed"
		},
	}
}

func numericToleranceEvaluator() Evaluator {
	return &fnEvaluator{
		id: "numeric_tolerance", kind: KindDeterministic,
		supports: func(k string) bool { return k == ExpectNumericTol },
		fn: func(c Case, o Outcome) (Verdict, string) {
			want, err := strconv.ParseFloat(strings.TrimSpace(c.Expectation.Expected), 64)
			if err != nil {
				return VerdictError, "case expectation is not numeric"
			}
			got, err := strconv.ParseFloat(strings.TrimSpace(o.Output), 64)
			if err != nil {
				return VerdictFail, "output is not numeric"
			}
			tol := math.Abs(c.Expectation.Tolerance)
			if math.IsNaN(got) || math.IsInf(got, 0) {
				return VerdictFail, "output is not finite"
			}
			if math.Abs(got-want) <= tol {
				return VerdictPass, "output within tolerance"
			}
			return VerdictFail, "output outside tolerance"
		},
	}
}

func streamTerminatedEvaluator() Evaluator {
	return &fnEvaluator{
		id: "stream_terminated", kind: KindDeterministic,
		supports: func(k string) bool { return k == ExpectStreamTerminat },
		fn: func(_ Case, o Outcome) (Verdict, string) {
			if o.Stream == nil {
				return VerdictFail, "no stream result recorded"
			}
			if !o.Stream.Terminated {
				if o.Stream.Error != "" {
					return VerdictFail, "stream ended with error"
				}
				return VerdictFail, "stream did not terminate"
			}
			return VerdictPass, "stream terminated cleanly"
		},
	}
}

// judgeEvaluator is the only non-deterministic evaluator in the registry. It is
// disabled by default: with no judge function injected it returns "unjudged"
// instead of guessing, and its verdict can never override a deterministic one.
type judgeEvaluator struct {
	id string
	fn func(ctx context.Context, c Case, o Outcome) (Verdict, string)
}

func (e *judgeEvaluator) ID() string           { return e.id }
func (e *judgeEvaluator) Kind() EvaluatorKind  { return KindJudge }
func (e *judgeEvaluator) Supports(string) bool { return true }

func (e *judgeEvaluator) Evaluate(ctx context.Context, c Case, o Outcome) VerdictResult {
	if e.fn == nil {
		return VerdictResult{EvaluatorID: e.id, Kind: KindJudge, Verdict: VerdictUnjudged, Reason: "judge not configured"}
	}
	v, reason := e.fn(ctx, c, o)
	if v != VerdictPass && v != VerdictFail {
		v = VerdictUnjudged
	}
	return VerdictResult{EvaluatorID: e.id, Kind: KindJudge, Verdict: v, Reason: bounded(reason, MaxReasonBytes)}
}

// NewJudgeEvaluator constructs an injectable judge. Phase H ships no live judge
// implementation: callers must supply one explicitly and enable it per run.
func NewJudgeEvaluator(id string, fn func(ctx context.Context, c Case, o Outcome) (Verdict, string)) Evaluator {
	if id == "" {
		id = "llm_judge"
	}
	return &judgeEvaluator{id: id, fn: fn}
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry is a bounded, ordered evaluator set. Deterministic evaluators are
// always consulted before judges, and the order inside each class is stable so
// results are reproducible.
type Registry struct {
	mu            sync.RWMutex
	deterministic []Evaluator
	judges        []Evaluator
}

// MaxEvaluators bounds the registry so evaluation cannot turn into an unbounded
// rule engine.
const MaxEvaluators = 32

// NewRegistry builds a registry with the built-in deterministic evaluators.
func NewRegistry() *Registry {
	r := &Registry{}
	for _, e := range []Evaluator{
		exactMatchEvaluator(),
		jsonValidEvaluator(),
		jsonSchemaEvaluator(),
		toolCallEvaluator(),
		regexEvaluator(),
		unitTestsEvaluator(),
		numericToleranceEvaluator(),
		streamTerminatedEvaluator(),
	} {
		_ = r.Register(e)
	}
	return r
}

// Register adds an evaluator. Duplicate ids and unknown kinds are rejected.
func (r *Registry) Register(e Evaluator) error {
	if e == nil {
		return errors.New("nil evaluator")
	}
	if e.ID() == "" {
		return errors.New("evaluator id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	total := len(r.deterministic) + len(r.judges)
	if total >= MaxEvaluators {
		return fmt.Errorf("evaluator registry is full (%d)", MaxEvaluators)
	}
	for _, existing := range append(append([]Evaluator{}, r.deterministic...), r.judges...) {
		if existing.ID() == e.ID() {
			return fmt.Errorf("duplicate evaluator id %q", e.ID())
		}
	}
	switch e.Kind() {
	case KindDeterministic:
		r.deterministic = append(r.deterministic, e)
	case KindJudge:
		r.judges = append(r.judges, e)
	default:
		return fmt.Errorf("evaluator %q has unknown kind %q", e.ID(), string(e.Kind()))
	}
	return nil
}

// Deterministic returns the deterministic evaluators in registration order.
func (r *Registry) Deterministic() []Evaluator {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Evaluator{}, r.deterministic...)
}

// Judges returns the judges in registration order.
func (r *Registry) Judges() []Evaluator {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Evaluator{}, r.judges...)
}

// IDs returns the bounded evaluator id list in deterministic order.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.deterministic)+len(r.judges))
	for _, e := range r.deterministic {
		out = append(out, e.ID())
	}
	for _, e := range r.judges {
		out = append(out, e.ID())
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Precedence
// ---------------------------------------------------------------------------

// Resolve applies the deterministic-first precedence rule.
//
//   - deterministic fail  -> fail (a judge can never rescue it)
//   - deterministic error -> error
//   - deterministic pass  -> pass
//   - no deterministic verdict: judge verdict when judgeEnabled, else unjudged
//
// Judge verdicts are always recorded in the case result, even when overridden, so
// the disagreement stays observable.
//
// The second return value is the resolution *category* — deterministic | judge |
// none — which is what metrics and the admin surface label, so a judge can never
// be reported as the decider of a case a deterministic rule decided.
func Resolve(verdicts []VerdictResult, judgeEnabled bool) (Verdict, string) {
	detSeen := false
	detError := false
	detFail := false
	judgeVerdict := Verdict("")
	for _, v := range verdicts {
		switch v.Kind {
		case KindDeterministic:
			switch v.Verdict {
			case VerdictPass:
				detSeen = true
			case VerdictFail:
				detSeen = true
				detFail = true
			case VerdictError:
				detSeen = true
				detError = true
			case VerdictSkip:
				// skip does not make a deterministic decision
			}
		case KindJudge:
			judgeVerdict = v.Verdict
		}
	}
	switch {
	case detFail:
		return VerdictFail, ResolutionDeterministic
	case detError:
		return VerdictError, ResolutionDeterministic
	case detSeen:
		return VerdictPass, ResolutionDeterministic
	}
	if judgeEnabled && (judgeVerdict == VerdictPass || judgeVerdict == VerdictFail) {
		return judgeVerdict, ResolutionJudge
	}
	return VerdictUnjudged, ResolutionNone
}

// Resolution categories. They are stable, bounded labels: the human-readable
// reason lives on the individual evaluator verdicts.
const (
	ResolutionDeterministic = "deterministic"
	ResolutionJudge         = "judge"
	ResolutionNone          = "none"
)
