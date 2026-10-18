// Package taskintel resolves how signal-based task classification is applied
// to routing for a single request (Phase 19, issue #419).
//
// It is pure and deterministic: no I/O, no locks, no global state. The HTTP
// layer supplies the classifier output, the route profile's configured mode,
// the explicit client pin (if any) and the operator override (if any); Resolve
// returns exactly one Outcome describing what happened and whether the caller
// must switch the candidate pool.
//
// Precedence (highest first):
//  1. Mode off (or unparseable) — the intelligence layer never touches routing.
//  2. Operator override — always wins over the client pin and the classifier.
//  3. Client pin — explicit client intent; classification must not fight it.
//  4. Classifier result — applied only when confidence >= min confidence.
//     Low confidence (or unknown type) degrades to the default profile:
//     never a guess dressed as certainty.
//
// Privacy: inputs are bounded enums/booleans/counts only. No raw prompts,
// headers, or secrets ever enter this package.
package taskintel

import (
	"fmt"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// Mode is the per-route task-intelligence application mode.
type Mode string

const (
	// ModeOff disables routing effects; classification stays observational.
	ModeOff Mode = "off"
	// ModeSuggest records the suggestion in the trace/UI but never changes routing.
	ModeSuggest Mode = "suggest"
	// ModeApply routes high-confidence classifications to the mapped task pool.
	// Operator opt-in per route profile.
	ModeApply Mode = "apply"
)

// ParseMode normalizes a configured mode string. Empty means off.
func ParseMode(s string) (Mode, bool) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case "", ModeOff:
		return ModeOff, true
	case ModeSuggest:
		return ModeSuggest, true
	case ModeApply:
		return ModeApply, true
	default:
		return ModeOff, false
	}
}

// Outcome describes the single decision Resolve reached.
type Outcome string

const (
	// OutcomeOff: task intelligence disabled for this route (observational only).
	OutcomeOff Outcome = "off"
	// OutcomeSuggested: a task was suggested; routing unchanged.
	OutcomeSuggested Outcome = "suggested"
	// OutcomeApplied: the task pool was applied as the primary pool.
	OutcomeApplied Outcome = "applied"
	// OutcomePinned: the explicit client pin was honored (beats the classifier).
	OutcomePinned Outcome = "pinned"
	// OutcomeOverridden: the operator override won (beats pin and classifier).
	OutcomeOverridden Outcome = "overridden"
	// OutcomeLowConfidence: classifier unsure — default profile, no pool change.
	OutcomeLowConfidence Outcome = "low_confidence"
	// OutcomeUnmapped: an effective task was chosen but no pool is mapped for it.
	OutcomeUnmapped Outcome = "unmapped"
	// OutcomeEvalGated: an eval gate is configured for the effective task
	// and no deployment in the target pool has passing eval evidence for
	// the gated suite; routing unchanged. Stamped by the HTTP layer —
	// evidence is I/O, and Resolve stays pure (Phase 19 eval gate, #510).
	OutcomeEvalGated Outcome = "eval_gated"
)

// DefaultMinConfidence is the classifier confidence required before a task
// may steer routing. Used when the route profile does not set its own.
const DefaultMinConfidence = 0.60

// DefaultTask is the fail-safe profile applied on low confidence.
const DefaultTask = taskprofile.TaskGeneral

// Input is everything Resolve needs. Pin and Override must already be
// normalized via NormalizeTaskType; "" means absent.
type Input struct {
	Mode          Mode
	Profile       taskprofile.TaskProfile
	Pin           taskprofile.TaskType
	Override      taskprofile.TaskType
	MinConfidence float64
	TaskPools     map[string]string // task type -> candidate pool ID
}

// Resolution is the single decision for the request.
type Resolution struct {
	// Mode is the effective application mode (off when disabled/invalid).
	Mode          Mode
	Outcome       Outcome
	EffectiveTask taskprofile.TaskType
	// Pin is the validated client pin that was honored ("" when none).
	Pin taskprofile.TaskType
	// TargetPool is the pool the effective task maps to ("" when unmapped).
	// It names the pool that WOULD be used in suggest mode too, so the trace
	// and UI can show the suggestion precisely.
	TargetPool string
	// Applied is true only when the caller must switch routing to TargetPool
	// (mode=apply with a mapped pool).
	Applied bool
	// Summary is one human sentence describing the decision, for the trace/UI.
	Summary string
}

// NormalizeTaskType validates an operator- or client-supplied task type
// (config task_override, X-NexaRoute-Task-Pin header). Invalid values are
// rejected so a typo can never silently steer routing.
func NormalizeTaskType(s string) (taskprofile.TaskType, bool) {
	t := taskprofile.TaskType(strings.ToLower(strings.TrimSpace(s)))
	if t == "" || !t.Valid() {
		return "", false
	}
	return t, true
}

// Resolve computes the single task-intelligence decision for a request.
// Deterministic: same input => same resolution.
func Resolve(in Input) Resolution {
	mode := in.Mode
	if mode != ModeSuggest && mode != ModeApply {
		mode = ModeOff
	}
	base := Resolution{Mode: mode, Pin: in.Pin}
	if mode == ModeOff {
		base.Outcome = OutcomeOff
		base.EffectiveTask = DefaultTask
		base.Summary = "task intelligence off for this route; classification is observational only"
		return base
	}
	minConf := in.MinConfidence
	if minConf <= 0 {
		minConf = DefaultMinConfidence
	}

	// 1. Operator override always wins.
	if in.Override != "" {
		return finish(base, in, OutcomeOverridden, in.Override,
			fmt.Sprintf("operator override forces task %q; pin and classifier ignored", string(in.Override)))
	}
	// 2. Client pin: explicit intent the classifier must not fight.
	if in.Pin != "" {
		return finish(base, in, OutcomePinned, in.Pin,
			fmt.Sprintf("client pinned task %q; classification not applied", string(in.Pin)))
	}
	// 3. Classifier, gated on confidence. Low confidence degrades to the
	// default profile — never a guess dressed as certainty.
	p := in.Profile
	if p.Type == "" || p.Type == taskprofile.TaskUnknown || p.Confidence < minConf {
		base.Outcome = OutcomeLowConfidence
		base.EffectiveTask = DefaultTask
		base.Summary = fmt.Sprintf("low classification confidence (%.2f < %.2f); default profile, routing unchanged",
			p.Confidence, minConf)
		return base
	}
	outcome := OutcomeSuggested
	verb := "suggested"
	if mode == ModeApply {
		outcome = OutcomeApplied
		verb = "applied"
	}
	return finish(base, in, outcome, p.Type,
		fmt.Sprintf("task %q %s (confidence %.2f: %s)", string(p.Type), verb, p.Confidence, reasonSummary(p)))
}

// finish attaches the pool mapping and the Applied flag.
func finish(base Resolution, in Input, outcome Outcome, task taskprofile.TaskType, summary string) Resolution {
	pool := ""
	if in.TaskPools != nil {
		pool = strings.TrimSpace(in.TaskPools[string(task)])
	}
	r := base
	r.Outcome = outcome
	r.EffectiveTask = task
	r.TargetPool = pool
	r.Summary = summary
	if pool == "" {
		r.Outcome = OutcomeUnmapped
		r.Summary = summary + "; no candidate pool mapped for this task, routing unchanged"
		return r
	}
	// In apply mode every chosen task steers routing — whether it came from
	// the classifier, a client pin, or the operator override. Suggest mode
	// only records.
	if in.Mode == ModeApply {
		r.Applied = true
	}
	return r
}

// reasonSummary renders the fired signals compactly for the human summary.
func reasonSummary(p taskprofile.TaskProfile) string {
	codes := make([]string, 0, len(p.ReasonCodes))
	for _, rc := range p.ReasonCodes {
		codes = append(codes, string(rc))
	}
	s := strings.Join(codes, "+")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	if s == "" {
		return "no signals"
	}
	return s
}
