// Package scorecards implements versioned Model Intelligence scorecards with
// mandatory provenance (master spec section 16, Phase H).
//
// A scorecard is a *claim* about a deployment: its quality dimensions, measured
// performance, reliability and cost. The central rule of this package is that a
// claim without a source is not a claim at all:
//
//   - every value carries a Provenance (operator_config | imported | evaluation |
//     production_telemetry), a sample count where the provenance is measured, and
//     an attribution label;
//   - a value without provenance is rejected by Validate, so a score can never be
//     invented by omission;
//   - an unknown dimension is represented by the *absence* of a value. There is no
//     neutral default score. Callers that want "no evidence" must handle the
//     missing case, which is what makes non-fabrication structural instead of a
//     convention.
//
// Phase H deliberately does not route on scorecards. The router, the decision
// plane and the policy engine do not import this package (a guard test enforces
// that), so adding scorecards cannot change real routing until a later phase
// explicitly wires them behind review.
package scorecards

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Provenance describes where a value came from. It is mandatory.
type Provenance string

const (
	// ProvenanceOperatorConfig is an operator declaration: a configuration value,
	// not a measurement. It must not claim samples.
	ProvenanceOperatorConfig Provenance = "operator_config"
	// ProvenanceImported is a value that arrived from an external artifact
	// (benchmark file, another gateway, a vendor report) and must name its source.
	ProvenanceImported Provenance = "imported"
	// ProvenanceEvaluation is a value measured by the evaluation plane and must
	// carry suite version, sample count and evaluation timestamp.
	ProvenanceEvaluation Provenance = "evaluation"
	// ProvenanceProductionTelemetry is a value measured from real traffic and must
	// carry sample count and observation timestamp.
	ProvenanceProductionTelemetry Provenance = "production_telemetry"
)

// AllProvenances is the canonical, bounded set used by validation, docs, metrics
// and tests. Order is deterministic.
var AllProvenances = []Provenance{
	ProvenanceOperatorConfig,
	ProvenanceImported,
	ProvenanceEvaluation,
	ProvenanceProductionTelemetry,
}

// Valid reports whether p is a canonical provenance.
func (p Provenance) Valid() bool {
	for _, known := range AllProvenances {
		if p == known {
			return true
		}
	}
	return false
}

// Measured reports whether the provenance implies an actual measurement (as
// opposed to an operator declaration).
func (p Provenance) Measured() bool {
	switch p {
	case ProvenanceImported, ProvenanceEvaluation, ProvenanceProductionTelemetry:
		return true
	default:
		return false
	}
}

// Dimension is a bounded scorecard dimension.
type Dimension string

const (
	// Quality dimensions (empirical, evaluation-owned in Phase H).
	DimCoding            Dimension = "coding"
	DimDebugging         Dimension = "debugging"
	DimReasoning         Dimension = "reasoning"
	DimToolCalling       Dimension = "tool_calling"
	DimStructuredOutput  Dimension = "structured_output"
	DimInstructionFollow Dimension = "instruction_following"
	DimLongContext       Dimension = "long_context"
	DimProtocolCompat    Dimension = "protocol_compat"
	DimLatencyMS         Dimension = "latency_ms"
	DimTTFTMS            Dimension = "ttft_ms"
	DimThroughputTPS     Dimension = "throughput_tps"
	DimAvailability      Dimension = "availability"
	DimFailureRate       Dimension = "failure_rate"
	DimInputCostPerMTok  Dimension = "input_cost_per_mtok"
	DimOutputCostPerMTok Dimension = "output_cost_per_mtok"
)

// DimensionKind groups dimensions for reporting.
type DimensionKind string

const (
	KindQuality     DimensionKind = "quality"
	KindPerformance DimensionKind = "performance"
	KindReliability DimensionKind = "reliability"
	KindCost        DimensionKind = "cost"
)

// AllDimensions is the canonical bounded set in deterministic order.
var AllDimensions = []Dimension{
	DimCoding,
	DimDebugging,
	DimReasoning,
	DimToolCalling,
	DimStructuredOutput,
	DimInstructionFollow,
	DimLongContext,
	DimProtocolCompat,
	DimLatencyMS,
	DimTTFTMS,
	DimThroughputTPS,
	DimAvailability,
	DimFailureRate,
	DimInputCostPerMTok,
	DimOutputCostPerMTok,
}

// QualityDimensions are the dimensions Phase H evaluates empirically.
var QualityDimensions = []Dimension{
	DimCoding,
	DimDebugging,
	DimReasoning,
	DimToolCalling,
	DimStructuredOutput,
	DimInstructionFollow,
	DimLongContext,
	DimProtocolCompat,
}

// Valid reports whether d is a canonical dimension.
func (d Dimension) Valid() bool {
	for _, known := range AllDimensions {
		if d == known {
			return true
		}
	}
	return false
}

// ValidQuality reports whether d is a quality dimension.
func (d Dimension) ValidQuality() bool {
	for _, known := range QualityDimensions {
		if d == known {
			return true
		}
	}
	return false
}

// Kind classifies the dimension. Unknown dimensions return "".
func (d Dimension) Kind() DimensionKind {
	switch d {
	case DimCoding, DimDebugging, DimReasoning, DimToolCalling, DimStructuredOutput,
		DimInstructionFollow, DimLongContext, DimProtocolCompat:
		return KindQuality
	case DimLatencyMS, DimTTFTMS, DimThroughputTPS:
		return KindPerformance
	case DimAvailability, DimFailureRate:
		return KindReliability
	case DimInputCostPerMTok, DimOutputCostPerMTok:
		return KindCost
	default:
		return ""
	}
}

// HigherIsBetter reports the direction of the raw measurement. Score is always
// normalized so that 1.0 is the best possible outcome; this flag exists so a raw
// measurement can be interpreted correctly by consumers.
func (d Dimension) HigherIsBetter() bool {
	switch d {
	case DimThroughputTPS, DimAvailability:
		return true
	default:
		// latency, ttft, failure rate and cost are all "lower is better".
		return false
	}
}

// Unit returns the native unit for a dimension (bounded, for display only).
func (d Dimension) Unit() string {
	switch d {
	case DimLatencyMS, DimTTFTMS:
		return "ms"
	case DimThroughputTPS:
		return "tokens/s"
	case DimAvailability, DimFailureRate:
		return "ratio"
	case DimInputCostPerMTok, DimOutputCostPerMTok:
		return "usd_per_mtok"
	default:
		return "score"
	}
}

// Scorecard envelope bounds. They keep a single scorecard, and the registry that
// holds them, small enough to be safe on the admin/metrics surface.
const (
	MaxValuesPerScorecard = 32
	MaxStringBytes        = 256
	MaxSampleCount        = 100_000_000
	MaxHistoryPerDeploy   = 16
	MaxDeployments        = 4096
	// MinMeasuredSamples is the minimum evidence behind a measured value. One
	// sample is technically a measurement; callers that want statistical
	// confidence use Coverage/ConfidenceFromSamples.
	MinMeasuredSamples = 1
	// FullConfidenceSamples is the sample count at which ConfidenceFromSamples
	// reaches 1.0. It is a documented calibration, not a statistical claim.
	FullConfidenceSamples = 32
)

// Value is one dimension of a scorecard. Score is normalized to [0,1] with 1.0
// best; Raw/Unit keep the native measurement when one exists.
//
// Provenance is mandatory: an empty Provenance makes the value invalid, which is
// how the package makes fabrication structurally impossible.
type Value struct {
	Score       float64    `json:"score"`
	Raw         float64    `json:"raw,omitempty"`
	Unit        string     `json:"unit,omitempty"`
	Provenance  Provenance `json:"provenance"`
	SampleCount int        `json:"sample_count"`
	Confidence  float64    `json:"confidence"`
	EvaluatedAt time.Time  `json:"evaluated_at,omitempty"`
	// SuiteVersion is required for evaluation provenance.
	SuiteVersion string `json:"suite_version,omitempty"`
	// Source attributes the value: config key, artifact file, suite id or window.
	Source string `json:"source,omitempty"`
}

// EvaluationRef summarizes the evaluation run that produced a scorecard.
type EvaluationRef struct {
	SuiteID      string    `json:"suite_id"`
	SuiteVersion string    `json:"suite_version"`
	RunID        string    `json:"run_id,omitempty"`
	SampleCount  int       `json:"sample_count"`
	EvaluatedAt  time.Time `json:"evaluated_at"`
}

// Scorecard is a versioned claim about one deployment.
type Scorecard struct {
	DeploymentID string              `json:"deployment_id"`
	ProviderID   string              `json:"provider_id,omitempty"`
	Model        string              `json:"model,omitempty"`
	Version      int                 `json:"version"`
	GeneratedAt  time.Time           `json:"generated_at"`
	Values       map[Dimension]Value `json:"values,omitempty"`
	Evaluation   *EvaluationRef      `json:"evaluation,omitempty"`
	Notes        []string            `json:"notes,omitempty"`
}

var (
	// ErrNoProvenance is returned when a value tries to enter the system without a
	// source. It is the package's core invariant, exposed for tests and callers.
	ErrNoProvenance = errors.New("scorecard value has no provenance (fabricated values are rejected)")
	// ErrNotScoreable is returned when a conversion has no usable evidence.
	ErrNotScoreable = errors.New("no usable evidence for scorecard")
)

// ConfidenceFromSamples is a deterministic, documented calibration: it grows from
// 0 samples to FullConfidenceSamples and saturates at 1.0. It is deliberately NOT
// a statistical confidence interval.
func ConfidenceFromSamples(n int) float64 {
	if n <= 0 {
		return 0
	}
	if n >= FullConfidenceSamples {
		return 1
	}
	c := float64(n) / float64(FullConfidenceSamples)
	if c < 0 {
		return 0
	}
	if c > 1 {
		return 1
	}
	return c
}

func validIdentifier(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		switch c {
		case '-', '_', '.', '/':
			continue
		default:
			return false
		}
	}
	return true
}

func finite01(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}

// ValidateValue enforces the provenance contract for one dimension.
func ValidateValue(d Dimension, v Value) error {
	if !d.Valid() {
		return fmt.Errorf("unknown scorecard dimension %q", string(d))
	}
	if !v.Provenance.Valid() {
		if v.Provenance == "" {
			return fmt.Errorf("dimension %q: %w", string(d), ErrNoProvenance)
		}
		return fmt.Errorf("dimension %q has unknown provenance %q", string(d), string(v.Provenance))
	}
	if !finite01(v.Score) {
		return fmt.Errorf("dimension %q score must be finite and between 0 and 1", string(d))
	}
	if !finite01(v.Confidence) {
		return fmt.Errorf("dimension %q confidence must be finite and between 0 and 1", string(d))
	}
	if v.SampleCount < 0 || v.SampleCount > MaxSampleCount {
		return fmt.Errorf("dimension %q sample_count must be between 0 and %d", string(d), MaxSampleCount)
	}
	if len(v.Source) > MaxStringBytes || len(v.SuiteVersion) > MaxStringBytes || len(v.Unit) > 32 {
		return fmt.Errorf("dimension %q attribution fields exceed safe limits", string(d))
	}
	if math.IsNaN(v.Raw) || math.IsInf(v.Raw, 0) {
		return fmt.Errorf("dimension %q raw value must be finite", string(d))
	}
	switch v.Provenance {
	case ProvenanceEvaluation:
		if v.SuiteVersion == "" {
			return fmt.Errorf("dimension %q has evaluation provenance without suite_version", string(d))
		}
		if v.SampleCount < MinMeasuredSamples {
			return fmt.Errorf("dimension %q has evaluation provenance without samples", string(d))
		}
		if v.EvaluatedAt.IsZero() {
			return fmt.Errorf("dimension %q has evaluation provenance without evaluated_at", string(d))
		}
	case ProvenanceProductionTelemetry:
		if v.SampleCount < MinMeasuredSamples {
			return fmt.Errorf("dimension %q has telemetry provenance without samples", string(d))
		}
		if v.EvaluatedAt.IsZero() {
			return fmt.Errorf("dimension %q has telemetry provenance without observed_at", string(d))
		}
	case ProvenanceImported:
		if v.Source == "" {
			return fmt.Errorf("dimension %q has imported provenance without a source", string(d))
		}
		if v.SampleCount < MinMeasuredSamples {
			return fmt.Errorf("dimension %q has imported provenance without samples", string(d))
		}
	case ProvenanceOperatorConfig:
		if v.Source == "" {
			return fmt.Errorf("dimension %q has operator_config provenance without a source key", string(d))
		}
		if v.SampleCount != 0 {
			return fmt.Errorf("dimension %q is an operator declaration and must not claim samples", string(d))
		}
	}
	return nil
}

// Validate enforces the scorecard envelope. It never repairs, it only rejects.
func (s Scorecard) Validate() error {
	if !validIdentifier(s.DeploymentID, MaxStringBytes) {
		return fmt.Errorf("scorecard deployment_id %q is invalid", s.DeploymentID)
	}
	if s.ProviderID != "" && !validIdentifier(s.ProviderID, MaxStringBytes) {
		return fmt.Errorf("scorecard provider_id %q is invalid", s.ProviderID)
	}
	if len(s.Model) > MaxStringBytes {
		return errors.New("scorecard model exceeds safe limit")
	}
	if s.Version < 1 {
		return errors.New("scorecard version must be >= 1")
	}
	if len(s.Values) > MaxValuesPerScorecard {
		return fmt.Errorf("scorecard has more than %d values", MaxValuesPerScorecard)
	}
	if len(s.Values) == 0 {
		// A scorecard is a claim backed by evidence: an empty one is not a claim.
		return errors.New("scorecard has no values (no evidence, no scorecard)")
	}
	if len(s.Notes) > 16 {
		return errors.New("scorecard has too many notes")
	}
	for _, n := range s.Notes {
		if len(n) > MaxStringBytes {
			return errors.New("scorecard note exceeds safe limit")
		}
	}
	for d, v := range s.Values {
		if err := ValidateValue(d, v); err != nil {
			return err
		}
	}
	if s.Evaluation != nil {
		if s.Evaluation.SuiteID == "" || s.Evaluation.SuiteVersion == "" {
			return errors.New("scorecard evaluation reference requires suite id and version")
		}
		if s.Evaluation.SampleCount < MinMeasuredSamples {
			return errors.New("scorecard evaluation reference requires samples")
		}
		if s.Evaluation.EvaluatedAt.IsZero() {
			return errors.New("scorecard evaluation reference requires evaluated_at")
		}
	}
	return nil
}

// Quality returns the value for a quality dimension. ok is false when the
// dimension has no evidence: nothing is invented to fill the gap.
func (s Scorecard) Quality(d Dimension) (Value, bool) {
	if s.Values == nil {
		return Value{}, false
	}
	v, ok := s.Values[d]
	return v, ok
}

// Coverage reports how many of the canonical quality dimensions carry evidence.
func (s Scorecard) Coverage() (present int, total int) {
	total = len(QualityDimensions)
	for _, d := range QualityDimensions {
		if _, ok := s.Values[d]; ok {
			present++
		}
	}
	return present, total
}

// Clone deep-copies a scorecard so registry contents can never be mutated by a
// caller holding a snapshot.
func (s Scorecard) Clone() Scorecard {
	out := s
	if s.Values != nil {
		out.Values = make(map[Dimension]Value, len(s.Values))
		for k, v := range s.Values {
			out.Values[k] = v
		}
	}
	if s.Notes != nil {
		out.Notes = append([]string(nil), s.Notes...)
	}
	if s.Evaluation != nil {
		ref := *s.Evaluation
		out.Evaluation = &ref
	}
	return out
}

// ---------------------------------------------------------------------------
// Conversions. Each conversion sets provenance explicitly; there is no path
// that produces a value without one.
// ---------------------------------------------------------------------------

// EvaluationValue is one measured dimension of an evaluation run.
type EvaluationValue struct {
	Dimension   Dimension
	Score       float64
	Raw         float64
	SampleCount int
}

// EvaluationEvidence is the evidence an evaluation run produces for a deployment.
type EvaluationEvidence struct {
	DeploymentID string
	ProviderID   string
	Model        string
	SuiteID      string
	SuiteVersion string
	RunID        string
	EvaluatedAt  time.Time
	Values       []EvaluationValue
}

// FromEvaluation converts measured evaluation evidence into a scorecard. It
// refuses to produce a scorecard when there is no evidence at all, and it never
// fills missing dimensions.
func FromEvaluation(ev EvaluationEvidence) (Scorecard, error) {
	if ev.EvaluatedAt.IsZero() {
		return Scorecard{}, fmt.Errorf("%w: evaluation timestamp is required", ErrNotScoreable)
	}
	if ev.SuiteID == "" || ev.SuiteVersion == "" {
		return Scorecard{}, fmt.Errorf("%w: suite id and version are required", ErrNotScoreable)
	}
	if len(ev.Values) == 0 {
		return Scorecard{}, fmt.Errorf("%w: evaluation produced no values", ErrNotScoreable)
	}
	sc := Scorecard{
		DeploymentID: ev.DeploymentID,
		ProviderID:   ev.ProviderID,
		Model:        ev.Model,
		Version:      1,
		GeneratedAt:  ev.EvaluatedAt.UTC(),
		Values:       make(map[Dimension]Value, len(ev.Values)),
		Evaluation: &EvaluationRef{
			SuiteID:      ev.SuiteID,
			SuiteVersion: ev.SuiteVersion,
			RunID:        ev.RunID,
			EvaluatedAt:  ev.EvaluatedAt.UTC(),
		},
	}
	for _, v := range ev.Values {
		if v.SampleCount < MinMeasuredSamples {
			// No evidence, no value. Skipping is the honest behaviour.
			continue
		}
		sc.Values[v.Dimension] = Value{
			Score:        v.Score,
			Raw:          v.Raw,
			Unit:         v.Dimension.Unit(),
			Provenance:   ProvenanceEvaluation,
			SampleCount:  v.SampleCount,
			Confidence:   ConfidenceFromSamples(v.SampleCount),
			EvaluatedAt:  ev.EvaluatedAt.UTC(),
			SuiteVersion: ev.SuiteVersion,
			Source:       ev.SuiteID,
		}
	}
	if len(sc.Values) == 0 {
		return Scorecard{}, fmt.Errorf("%w: no dimension had samples", ErrNotScoreable)
	}
	samples := 0
	for _, v := range sc.Values {
		if v.SampleCount > samples {
			samples = v.SampleCount
		}
	}
	sc.Evaluation.SampleCount = samples
	if err := sc.Validate(); err != nil {
		return Scorecard{}, err
	}
	return sc, nil
}

// TelemetryEvidence is measured production traffic for a deployment.
type TelemetryEvidence struct {
	DeploymentID string
	ProviderID   string
	Model        string
	ObservedAt   time.Time
	WindowLabel  string
	SampleCount  int
	// Optional raw measurements. A measurement without a comparison target is
	// omitted rather than scored against an invented target.
	LatencyP50MS    float64
	LatencyP95MS    float64
	LatencyTargetMS float64
	TTFTMS          float64
	TTFTTargetMS    float64
	SuccessRate     float64
	FailureRate     float64
}

// FromTelemetry converts measured traffic into a scorecard. Missing targets mean
// missing dimensions: the function subtracts evidence, it never adds any.
func FromTelemetry(ev TelemetryEvidence) (Scorecard, error) {
	if ev.SampleCount < MinMeasuredSamples {
		return Scorecard{}, fmt.Errorf("%w: telemetry requires samples", ErrNotScoreable)
	}
	if ev.ObservedAt.IsZero() {
		return Scorecard{}, fmt.Errorf("%w: telemetry requires an observation time", ErrNotScoreable)
	}
	sc := Scorecard{
		DeploymentID: ev.DeploymentID,
		ProviderID:   ev.ProviderID,
		Model:        ev.Model,
		Version:      1,
		GeneratedAt:  ev.ObservedAt.UTC(),
		Values:       map[Dimension]Value{},
	}
	source := "telemetry"
	if ev.WindowLabel != "" {
		source = "telemetry:" + ev.WindowLabel
	}
	put := func(d Dimension, score, raw float64) {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return
		}
		if score < 0 {
			score = 0
		}
		if score > 1 {
			score = 1
		}
		sc.Values[d] = Value{
			Score:       score,
			Raw:         raw,
			Unit:        d.Unit(),
			Provenance:  ProvenanceProductionTelemetry,
			SampleCount: ev.SampleCount,
			Confidence:  ConfidenceFromSamples(ev.SampleCount),
			EvaluatedAt: ev.ObservedAt.UTC(),
			Source:      source,
		}
	}
	if ev.LatencyTargetMS > 0 && ev.LatencyP95MS > 0 {
		put(DimLatencyMS, 1-ev.LatencyP95MS/ev.LatencyTargetMS, ev.LatencyP95MS)
	}
	if ev.TTFTTargetMS > 0 && ev.TTFTMS > 0 {
		put(DimTTFTMS, 1-ev.TTFTMS/ev.TTFTTargetMS, ev.TTFTMS)
	}
	if ev.SuccessRate > 0 {
		put(DimAvailability, ev.SuccessRate, ev.SuccessRate)
	}
	if ev.FailureRate > 0 {
		put(DimFailureRate, 1-ev.FailureRate, ev.FailureRate)
	}
	if len(sc.Values) == 0 {
		return Scorecard{}, fmt.Errorf("%w: telemetry carried no measurable dimension (targets required)", ErrNotScoreable)
	}
	if err := sc.Validate(); err != nil {
		return Scorecard{}, err
	}
	return sc, nil
}

// OperatorDeclaration is a declared (not measured) dimension value.
type OperatorDeclaration struct {
	Dimension Dimension
	Score     float64
}

// FromOperatorConfig converts explicit operator declarations into a scorecard.
// Declarations carry provenance=operator_config, zero samples and zero
// confidence, so downstream consumers can always tell them apart from measured
// values.
func FromOperatorConfig(deploymentID, providerID, model, sourceKey string, decls []OperatorDeclaration) (Scorecard, error) {
	if len(decls) == 0 {
		return Scorecard{}, fmt.Errorf("%w: no declared dimensions", ErrNotScoreable)
	}
	if sourceKey == "" {
		return Scorecard{}, fmt.Errorf("%w: operator declarations require a source key", ErrNotScoreable)
	}
	sc := Scorecard{
		DeploymentID: deploymentID,
		ProviderID:   providerID,
		Model:        model,
		Version:      1,
		GeneratedAt:  time.Now().UTC(),
		Values:       make(map[Dimension]Value, len(decls)),
	}
	for _, d := range decls {
		sc.Values[d.Dimension] = Value{
			Score:      d.Score,
			Unit:       d.Dimension.Unit(),
			Provenance: ProvenanceOperatorConfig,
			Source:     sourceKey,
		}
	}
	if err := sc.Validate(); err != nil {
		return Scorecard{}, err
	}
	return sc, nil
}

// importDocument is the on-disk artifact shape for imported scorecards. Unknown
// JSON fields are rejected so a typo cannot silently drop provenance.
type importDocument struct {
	Scorecards []Scorecard `json:"scorecards"`
}

// ImportJSON parses an external scorecard artifact. Everything it returns has
// been validated; invalid entries fail the whole import instead of being
// partially trusted. Values with imported provenance and no source are
// attributed to sourceLabel (attribution, not invention).
func ImportJSON(data []byte, sourceLabel string, maxDeployments int) ([]Scorecard, error) {
	if len(data) == 0 {
		return nil, errors.New("empty scorecard artifact")
	}
	if len(data) > 8<<20 {
		return nil, errors.New("scorecard artifact exceeds safe limit")
	}
	if maxDeployments <= 0 || maxDeployments > MaxDeployments {
		maxDeployments = MaxDeployments
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var doc importDocument
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("invalid scorecard artifact: %w", err)
	}
	if dec.More() {
		return nil, errors.New("invalid scorecard artifact: trailing data")
	}
	if len(doc.Scorecards) == 0 {
		return nil, errors.New("scorecard artifact contains no scorecards")
	}
	if len(doc.Scorecards) > maxDeployments {
		return nil, fmt.Errorf("scorecard artifact exceeds %d scorecards", maxDeployments)
	}
	out := make([]Scorecard, 0, len(doc.Scorecards))
	seen := map[string]struct{}{}
	for _, sc := range doc.Scorecards {
		for d, v := range sc.Values {
			if v.Provenance == ProvenanceImported && v.Source == "" {
				v.Source = sourceLabel
				sc.Values[d] = v
			}
		}
		if sc.Version < 1 {
			sc.Version = 1
		}
		if sc.GeneratedAt.IsZero() {
			return nil, fmt.Errorf("scorecard %q has no generated_at", sc.DeploymentID)
		}
		if err := sc.Validate(); err != nil {
			return nil, fmt.Errorf("scorecard %q: %w", sc.DeploymentID, err)
		}
		if _, dup := seen[sc.DeploymentID]; dup {
			return nil, fmt.Errorf("duplicate scorecard for deployment %q", sc.DeploymentID)
		}
		seen[sc.DeploymentID] = struct{}{}
		out = append(out, sc.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeploymentID < out[j].DeploymentID })
	return out, nil
}

// ---------------------------------------------------------------------------
// Registry: versioned, bounded, concurrency-safe storage.
// ---------------------------------------------------------------------------

// Registry stores the latest scorecard per deployment plus a bounded version
// history. It is safe for concurrent use and never mutates caller data.
type Registry struct {
	mu             sync.RWMutex
	maxDeployments int
	maxVersions    int
	current        map[string]Scorecard
	history        map[string][]Scorecard
	upserts        uint64
	rejected       uint64
}

// NewRegistry creates a bounded registry. Non-positive bounds fall back to safe
// defaults.
func NewRegistry(maxDeployments, maxVersions int) *Registry {
	if maxDeployments <= 0 || maxDeployments > MaxDeployments {
		maxDeployments = MaxDeployments
	}
	if maxVersions <= 0 || maxVersions > MaxHistoryPerDeploy {
		maxVersions = 4
	}
	return &Registry{
		maxDeployments: maxDeployments,
		maxVersions:    maxVersions,
		current:        map[string]Scorecard{},
		history:        map[string][]Scorecard{},
	}
}

// Upsert validates a scorecard, assigns it the next version for its deployment,
// stores it as the current version and appends it to the bounded history.
func (r *Registry) Upsert(sc Scorecard) (Scorecard, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sc = sc.Clone()
	if sc.GeneratedAt.IsZero() {
		sc.GeneratedAt = time.Now().UTC()
	}
	if len(r.current) >= r.maxDeployments {
		if _, exists := r.current[sc.DeploymentID]; !exists {
			r.rejected++
			return Scorecard{}, fmt.Errorf("scorecard registry is full (%d deployments)", r.maxDeployments)
		}
	}
	next := 1
	if prev, ok := r.current[sc.DeploymentID]; ok {
		next = prev.Version + 1
	}
	sc.Version = next
	if err := sc.Validate(); err != nil {
		r.rejected++
		return Scorecard{}, err
	}
	r.current[sc.DeploymentID] = sc
	hist := append(r.history[sc.DeploymentID], sc)
	if len(hist) > r.maxVersions {
		hist = append([]Scorecard(nil), hist[len(hist)-r.maxVersions:]...)
	}
	r.history[sc.DeploymentID] = hist
	r.upserts++
	return sc.Clone(), nil
}

// Get returns the current scorecard for a deployment.
func (r *Registry) Get(deploymentID string) (Scorecard, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sc, ok := r.current[deploymentID]
	if !ok {
		return Scorecard{}, false
	}
	return sc.Clone(), true
}

// History returns the bounded version history, oldest first.
func (r *Registry) History(deploymentID string) []Scorecard {
	r.mu.RLock()
	defer r.mu.RUnlock()
	hist := r.history[deploymentID]
	out := make([]Scorecard, 0, len(hist))
	for _, sc := range hist {
		out = append(out, sc.Clone())
	}
	return out
}

// Snapshot returns every current scorecard, deterministically sorted by
// deployment id, deep-copied.
func (r *Registry) Snapshot() []Scorecard {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Scorecard, 0, len(r.current))
	for _, sc := range r.current {
		out = append(out, sc.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeploymentID < out[j].DeploymentID })
	return out
}

// Quality returns the current value for a dimension. Missing evidence is
// reported as ok=false; nothing is substituted.
func (r *Registry) Quality(deploymentID string, d Dimension) (Value, bool) {
	sc, ok := r.Get(deploymentID)
	if !ok {
		return Value{}, false
	}
	return sc.Quality(d)
}

// Len returns the number of deployments with a current scorecard.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.current)
}

// Stats reports cumulative counters plus provenance distribution.
func (r *Registry) Stats() Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := Stats{
		Deployments:  len(r.current),
		Upserts:      r.upserts,
		Rejected:     r.rejected,
		ByProvenance: map[Provenance]int{},
	}
	for _, sc := range r.current {
		for _, v := range sc.Values {
			out.Values++
			out.ByProvenance[v.Provenance]++
		}
		present, total := sc.Coverage()
		out.QualityCoverage += present
		out.QualitySlots += total
	}
	return out
}

// Stats is the bounded observability view of the registry.
type Stats struct {
	Deployments     int                `json:"deployments"`
	Values          int                `json:"values"`
	Upserts         uint64             `json:"upserts"`
	Rejected        uint64             `json:"rejected"`
	QualityCoverage int                `json:"quality_coverage"`
	QualitySlots    int                `json:"quality_slots"`
	ByProvenance    map[Provenance]int `json:"by_provenance"`
}

// Retain drops scorecards for deployments that no longer exist. It is called on
// config reload so stale claims cannot outlive their deployment.
func (r *Registry) Retain(valid map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.current {
		if _, ok := valid[id]; !ok {
			delete(r.current, id)
			delete(r.history, id)
		}
	}
}

// Reset clears the registry (used by hot reload of the import artifact and by
// tests).
func (r *Registry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = map[string]Scorecard{}
	r.history = map[string][]Scorecard{}
}

// Load replaces the registry contents with validated scorecards (already
// validated by the caller, revalidated here). Partial loads are never applied:
// either every scorecard is accepted or the registry is left untouched.
func (r *Registry) Load(list []Scorecard) error {
	prepared := make([]Scorecard, 0, len(list))
	seen := map[string]struct{}{}
	for _, sc := range list {
		if _, dup := seen[sc.DeploymentID]; dup {
			return fmt.Errorf("duplicate scorecard for deployment %q", sc.DeploymentID)
		}
		seen[sc.DeploymentID] = struct{}{}
		c := sc.Clone()
		if c.GeneratedAt.IsZero() {
			return fmt.Errorf("scorecard %q has no generated_at", c.DeploymentID)
		}
		if c.Version < 1 {
			c.Version = 1
		}
		if err := c.Validate(); err != nil {
			return fmt.Errorf("scorecard %q: %w", c.DeploymentID, err)
		}
		prepared = append(prepared, c)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(prepared) > r.maxDeployments {
		return fmt.Errorf("scorecard set exceeds %d deployments", r.maxDeployments)
	}
	r.current = make(map[string]Scorecard, len(prepared))
	r.history = make(map[string][]Scorecard, len(prepared))
	for _, sc := range prepared {
		r.current[sc.DeploymentID] = sc
		r.history[sc.DeploymentID] = []Scorecard{sc}
	}
	return nil
}
