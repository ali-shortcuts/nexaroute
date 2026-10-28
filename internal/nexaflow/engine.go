package nexaflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// Workflow engine (Phase 5).
//
// A Workflow is a validated DAG of steps over registry nodes. Steps
// reference node ids, carry per-step config, take static inputs, and
// bind input fields from prior step outputs ("stepID.field",
// "stepID.a.b", or the whole output with just "stepID"), from the
// workflow's initial input ("$input.field"), or, inside a loop
// iteration, from the current item ("loop.item", "loop.index").
// Edges declare precedence; execution order is the topological order
// with declaration order breaking ties.
//
// Branch selection: a nexaroute.control.branch step records its
// selected_branch in the frame. Later steps tagged with Branch run
// only when the selection equals their tag; unselected steps are
// recorded as skipped and never execute (no side effects).
//
// Loops: a step with IterateFrom names an earlier loop_plan step. The
// engine executes the step once per planned iteration, with a hard
// cap (Engine.MaxLoopIterations, default 10, never above 100). A plan
// longer than the cap fails the run before any iteration executes.
//
// Subworkflows: a step with Subworkflow (and no NodeID) runs a nested
// workflow whose $input scope is the step's resolved input. Nesting
// depth is capped (Engine.MaxDepth, default 3, never above 3). A
// subworkflow step's output is {"outputs": <nested step outputs>,
// "status": "completed"}.
//
// Approval pause: an approval step with no decision yet does not
// error — the run pauses. Run returns Status "paused" with a resume
// token; Resume(token, decision) continues from exactly that step.
// Paused state lives in a RunStore. The bundled MemoryRunStore is
// in-memory only: a token is worthless after a process restart, and
// this package never claims cross-restart durability. A future
// PostgreSQL store can implement RunStore without changing the engine.
//
// Error policy per step (OnError): "propagate" (default) fails the
// run with the step's error; "stop" ends the run with status
// "stopped" (terminal, explicitly not success) and the same error;
// "fallback" continues with FallbackOutput as the step's output.
// Fallback does NOT roll back side effects that already happened
// (for example, earlier loop iterations stay done).
//
// Retry: the engine retries a step only when the spec's RetryPolicy
// allows it: the error code must be in RetryableCodes, and for
// billable or irreversible nodes only code "timeout" is ever retried.
// A retry never re-runs an approval decision or a paused step.
//
// Every failure is fail-closed: Run returns a Run whose Status is
// "failed" or "stopped" with Error set, never a partial success
// presented as success. Only validation/setup problems return a Go
// error with no Run; execution outcomes always come back as a Run.

const (
	approvalNodeID = "nexaroute.control.approval"
	branchNodeID   = "nexaroute.control.branch"
	loopPlanNodeID = "nexaroute.control.loop_plan"
)

// RunStatus is the terminal-or-paused state of a workflow run.
type RunStatus string

const (
	RunCompleted RunStatus = "completed"
	RunPaused    RunStatus = "paused"
	RunFailed    RunStatus = "failed"
	RunStopped   RunStatus = "stopped"
)

// Step is one workflow step. Exactly one of NodeID and Subworkflow
// must be set: NodeID names a registry node, Subworkflow nests a
// workflow. Bindings map an input field name to a source reference
// (see the package comment). Branch gates the step on a branch
// selection. IterateFrom names a loop_plan step for per-iteration
// execution. OnError is "", "propagate", "fallback", or "stop".
type Step struct {
	ID             string
	NodeID         string
	Version        string
	Config         map[string]any
	Input          map[string]any
	Bindings       map[string]string
	Branch         string
	IterateFrom    string
	OnError        string
	FallbackOutput map[string]any
	Subworkflow    *Workflow
}

// Workflow is a DAG of steps. Edges are [from, to] step id pairs.
type Workflow struct {
	ID      string
	Version string
	Steps   []Step
	Edges   [][2]string
}

// StepRecord is one history entry. Output is secret-redacted; ErrorCode
// is empty for completed/skipped steps.
type StepRecord struct {
	StepID     string
	NodeID     string
	Status     string // completed|skipped|fallback|failed
	Output     map[string]any
	ErrorCode  string
	Attempts   int
	Iterations int
}

// Run is the outcome of Engine.Run or Engine.Resume.
type Run struct {
	ID          string
	Status      RunStatus
	Outputs     map[string]map[string]any
	History     []StepRecord
	Error       *NodeError
	ResumeToken string
}

// RunStore persists paused runs. Implementations must deep-copy on
// Save/Load so a caller mutating a loaded state cannot corrupt the
// stored one. Tokens are opaque to callers.
type RunStore interface {
	Save(state *RunState) error
	Load(token string) (*RunState, error)
	Delete(token string) error
}

// RunState is the full resumable state of a paused run.
type RunState struct {
	ID             string
	Token          string
	Workflow       *Workflow
	Principal      authz.Identity
	PolicyApproved bool
	Frames         []*runFrame
	History        []StepRecord
	Path           []string
}

// runFrame is one execution frame. All fields are exported so a store
// can serialize the frame; the type itself is unexported because
// callers never construct frames directly.
type runFrame struct {
	Workflow        *Workflow
	Order           []string
	Pos             int
	Outputs         map[string]map[string]any
	Input           map[string]any
	BranchSelection string
	ParentStepID    string
	Depth           int
	PendingApproval string
	PendingDecision string
	PendingApprover string
}

// MemoryRunStore is an in-memory RunStore. It is NOT durable: tokens
// die with the process. That limitation is documented on the Engine
// and in the Phase 5 doc; it is not a bug to be worked around by
// pretending otherwise.
type MemoryRunStore struct {
	mu     sync.Mutex
	states map[string]*RunState
}

func NewMemoryRunStore() *MemoryRunStore {
	return &MemoryRunStore{states: make(map[string]*RunState)}
}

func (s *MemoryRunStore) Save(state *RunState) error {
	if state == nil || state.Token == "" {
		return errors.New("nexaflow: cannot save a run state without a token")
	}
	cp, err := deepCopyRunState(state)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[state.Token] = cp
	return nil
}

func (s *MemoryRunStore) Load(token string) (*RunState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.states[token]
	if !ok {
		return nil, NewNodeError("resume_token_not_found", false, "unknown or expired resume token")
	}
	return deepCopyRunState(stored)
}

func (s *MemoryRunStore) Delete(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.states, token)
	return nil
}

func deepCopyRunState(state *RunState) (*RunState, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, NewNodeError("internal_error", false, "run state is not serializable")
	}
	var cp RunState
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, NewNodeError("internal_error", false, "run state could not be restored")
	}
	return &cp, nil
}

func deepCopyWorkflow(wf *Workflow) (*Workflow, error) {
	raw, err := json.Marshal(wf)
	if err != nil {
		return nil, NewNodeError("validation_error", false, "workflow is not serializable")
	}
	var cp Workflow
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, NewNodeError("validation_error", false, "workflow could not be copied")
	}
	return &cp, nil
}

// Engine executes workflows through a Registry and a Backend.
type Engine struct {
	Registry          *Registry
	Backend           Backend
	Store             RunStore
	PolicyApproved    bool
	MaxSteps          int // 0 = 100
	MaxLoopIterations int // 0 = 10, never above 100
	MaxDepth          int // 0 = 3, never above 3
}

func (e *Engine) registry() *Registry {
	if e.Registry != nil {
		return e.Registry
	}
	return DefaultRegistry()
}

// bounds resolves engine limits, failing closed on out-of-range values.
func (e *Engine) bounds() (maxSteps, maxLoop, maxDepth int, err error) {
	maxSteps = e.MaxSteps
	if maxSteps == 0 {
		maxSteps = 100
	}
	if maxSteps < 1 {
		return 0, 0, 0, NewNodeError("validation_error", false, "MaxSteps must be >= 1")
	}
	maxLoop = e.MaxLoopIterations
	if maxLoop == 0 {
		maxLoop = 10
	}
	if maxLoop < 1 || maxLoop > 100 {
		return 0, 0, 0, NewNodeError("validation_error", false, "MaxLoopIterations must be in [1,100]")
	}
	maxDepth = e.MaxDepth
	if maxDepth == 0 {
		maxDepth = 3
	}
	if maxDepth < 1 || maxDepth > 3 {
		return 0, 0, 0, NewNodeError("validation_error", false, "MaxDepth must be in [1,3]")
	}
	return maxSteps, maxLoop, maxDepth, nil
}

// Validate checks a workflow without executing anything. It uses the
// engine's PolicyApproved flag: a workflow containing a node that
// requires operator policy is rejected here, before any step runs,
// when the engine is not policy-approved.
func (e *Engine) Validate(wf *Workflow) error {
	_, _, maxDepth, err := e.bounds()
	if err != nil {
		return err
	}
	maxSteps := e.MaxSteps
	if maxSteps == 0 {
		maxSteps = 100
	}
	return validateWorkflow(e.registry(), wf, 0, maxDepth, maxSteps, e.PolicyApproved)
}

func normalizeVersion(v string) (string, error) {
	if v == "" {
		return SupportedVersion, nil
	}
	if v != SupportedVersion {
		return "", NewNodeError("unsupported_version", false, "version %q is not supported", v)
	}
	return v, nil
}

func validateWorkflow(reg *Registry, wf *Workflow, depth, maxDepth, maxSteps int, policyApproved bool) error {
	if wf == nil {
		return NewNodeError("validation_error", false, "workflow is nil")
	}
	if depth > maxDepth {
		return NewNodeError("validation_error", false, "workflow %q exceeds subworkflow depth cap %d", wf.ID, maxDepth)
	}
	if strings.TrimSpace(wf.ID) == "" {
		return NewNodeError("validation_error", false, "workflow id is required")
	}
	if _, err := normalizeVersion(wf.Version); err != nil {
		return err
	}
	if len(wf.Steps) < 1 || len(wf.Steps) > maxSteps {
		return NewNodeError("validation_error", false, "workflow %q must have 1..%d steps", wf.ID, maxSteps)
	}
	byID := make(map[string]*Step, len(wf.Steps))
	for i := range wf.Steps {
		st := &wf.Steps[i]
		if strings.TrimSpace(st.ID) == "" {
			return NewNodeError("validation_error", false, "workflow %q has a step without an id", wf.ID)
		}
		if _, dup := byID[st.ID]; dup {
			return NewNodeError("validation_error", false, "workflow %q has duplicate step id %q", wf.ID, st.ID)
		}
		byID[st.ID] = st
		if _, err := normalizeVersion(st.Version); err != nil {
			return err
		}
		if (st.NodeID == "") == (st.Subworkflow == nil) {
			return NewNodeError("validation_error", false, "step %q must set exactly one of NodeID and Subworkflow", st.ID)
		}
		if st.IterateFrom != "" && st.Subworkflow != nil {
			return NewNodeError("validation_error", false, "step %q cannot combine a subworkflow with loop iteration", st.ID)
		}
		switch st.OnError {
		case "", "propagate", "fallback", "stop":
		default:
			return NewNodeError("validation_error", false, "step %q has unknown OnError %q", st.ID, st.OnError)
		}
		for target, ref := range st.Bindings {
			if strings.Contains(target, ".") {
				return NewNodeError("validation_error", false, "step %q binding target %q must be a top-level input field", st.ID, target)
			}
			if containsPlaceholder(ref) {
				return NewNodeError("validation_error", false, "step %q has an unresolved placeholder in binding %q", st.ID, target)
			}
		}
		if err := checkNoPlaceholders(st.Config, st.ID, "config"); err != nil {
			return err
		}
		if err := checkNoPlaceholders(st.Input, st.ID, "input"); err != nil {
			return err
		}
	}
	order, index, err := topoOrder(wf)
	if err != nil {
		return err
	}
	_ = order
	for i := range wf.Steps {
		st := &wf.Steps[i]
		if st.Subworkflow != nil {
			if err := validateWorkflow(reg, st.Subworkflow, depth+1, maxDepth, maxSteps, policyApproved); err != nil {
				return err
			}
			continue
		}
		spec, err := reg.Lookup(st.NodeID, normalizeVersionOr(st.Version))
		if err != nil {
			return err
		}
		if spec.RequiresOperatorPolicy && !policyApproved {
			return NewNodeError("policy_required", false, "step %q node %q requires explicit operator policy approval", st.ID, st.NodeID)
		}
		if err := reg.ValidateConfig(spec, st.Config); err != nil {
			return err
		}
		if err := validateStepInputCoverage(reg, spec, st); err != nil {
			return err
		}
		if st.NodeID == approvalNodeID && st.IterateFrom != "" {
			return NewNodeError("validation_error", false, "step %q cannot loop over an approval node (approval pause is per-step, not per-iteration)", st.ID)
		}
		if st.IterateFrom != "" {
			plan, ok := byID[st.IterateFrom]
			if !ok {
				return NewNodeError("validation_error", false, "step %q iterates from unknown step %q", st.ID, st.IterateFrom)
			}
			if plan.NodeID != loopPlanNodeID {
				return NewNodeError("validation_error", false, "step %q iterates from %q, which is not a loop_plan step", st.ID, st.IterateFrom)
			}
			if index[st.IterateFrom] >= index[st.ID] {
				return NewNodeError("validation_error", false, "step %q iterates from %q, which does not precede it", st.ID, st.IterateFrom)
			}
		}
		if st.Branch != "" {
			seen := false
			for _, other := range wf.Steps {
				if other.NodeID == branchNodeID && index[other.ID] < index[st.ID] {
					seen = true
					break
				}
			}
			if !seen {
				return NewNodeError("validation_error", false, "step %q is branch-gated but no branch step precedes it", st.ID)
			}
		}
		for _, ref := range st.Bindings {
			if strings.HasPrefix(ref, "$input") {
				continue
			}
			if strings.HasPrefix(ref, "loop.") {
				if st.IterateFrom == "" {
					return NewNodeError("validation_error", false, "step %q uses a loop binding without IterateFrom", st.ID)
				}
				if ref != "loop.item" && ref != "loop.index" {
					return NewNodeError("validation_error", false, "step %q has unknown loop binding %q", st.ID, ref)
				}
				continue
			}
			src := longestStepPrefix(ref, byID)
			if src == "" {
				return NewNodeError("validation_error", false, "step %q binding %q references an unknown step", st.ID, ref)
			}
			if index[src] >= index[st.ID] {
				return NewNodeError("validation_error", false, "step %q binding %q references step %q, which does not precede it", st.ID, ref, src)
			}
		}
	}
	return nil
}

func normalizeVersionOr(v string) string {
	if v == "" {
		return SupportedVersion
	}
	return v
}

// placeholderPattern matches a "{{name}}" placeholder. A bare
// substring scan for "{{" or "}}" is wrong: ordinary nested JSON
// contains "}}" whenever an object value closes (e.g. {"a":{}}).
var placeholderPattern = regexp.MustCompile(`\{\{[^}]*\}\}`)

func containsPlaceholder(s string) bool {
	return placeholderPattern.MatchString(s)
}

func checkNoPlaceholders(values map[string]any, stepID, kind string) error {
	if len(values) == 0 {
		return nil
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return NewNodeError("validation_error", false, "step %q %s is not JSON-encodable", stepID, kind)
	}
	if containsPlaceholder(string(raw)) {
		return NewNodeError("validation_error", false, "step %q has an unresolved placeholder in %s (instantiate the template before executing)", stepID, kind)
	}
	return nil
}

// validateStepInputCoverage checks that every required input field is
// supplied statically or by binding, and that static keys are known
// and correctly typed. Full validation still happens at execution
// time, when bound values are known.
func validateStepInputCoverage(reg *Registry, spec *NodeSpec, st *Step) error {
	known := make(map[string]Field, len(spec.InputSchema))
	for _, f := range spec.InputSchema {
		known[f.Name] = f
		if f.Required {
			if _, ok := st.Input[f.Name]; !ok {
				if _, ok := st.Bindings[f.Name]; !ok {
					return NewNodeError("validation_error", false, "step %q is missing required input %q for node %q", st.ID, f.Name, spec.ID)
				}
			}
		}
	}
	for key, val := range st.Input {
		f, ok := known[key]
		if !ok {
			return NewNodeError("validation_error", false, "step %q has unknown input field %q for node %q", st.ID, key, spec.ID)
		}
		if err := checkFieldType(f, val); err != nil {
			return err
		}
	}
	return nil
}

// topoOrder returns a deterministic topological order (declaration
// order breaks ties) or an error on unknown endpoints, self-edges,
// or cycles.
func topoOrder(wf *Workflow) ([]string, map[string]int, error) {
	indeg := make(map[string]int, len(wf.Steps))
	next := make(map[string][]string)
	for _, st := range wf.Steps {
		indeg[st.ID] = 0
	}
	for _, e := range wf.Edges {
		from, to := e[0], e[1]
		if _, ok := indeg[from]; !ok {
			return nil, nil, NewNodeError("validation_error", false, "edge references unknown step %q", from)
		}
		if _, ok := indeg[to]; !ok {
			return nil, nil, NewNodeError("validation_error", false, "edge references unknown step %q", to)
		}
		if from == to {
			return nil, nil, NewNodeError("validation_error", false, "edge from %q to itself", from)
		}
		next[from] = append(next[from], to)
		indeg[to]++
	}
	var order []string
	remaining := make(map[string]bool, len(wf.Steps))
	for _, st := range wf.Steps {
		remaining[st.ID] = true
	}
	for len(remaining) > 0 {
		progress := false
		for _, st := range wf.Steps {
			if !remaining[st.ID] || indeg[st.ID] != 0 {
				continue
			}
			delete(remaining, st.ID)
			order = append(order, st.ID)
			for _, to := range next[st.ID] {
				indeg[to]--
			}
			progress = true
			break // restart scan to keep declaration order deterministic
		}
		if !progress {
			return nil, nil, NewNodeError("validation_error", false, "workflow %q has a cycle in its edges", wf.ID)
		}
	}
	index := make(map[string]int, len(order))
	for i, id := range order {
		index[id] = i
	}
	return order, index, nil
}

func longestStepPrefix(ref string, byID map[string]*Step) string {
	parts := strings.Split(ref, ".")
	for i := len(parts); i >= 1; i-- {
		candidate := strings.Join(parts[:i], ".")
		if _, ok := byID[candidate]; ok {
			return candidate
		}
	}
	return ""
}

type loopContext struct {
	item  any
	index int
}

// resolveStepInput merges static input with bindings. Loop bindings
// resolve only when loop is non-nil; with a nil loop they are skipped
// (the caller re-resolves per iteration).
func resolveStepInput(st *Step, outputs map[string]map[string]any, inputScope map[string]any, loop *loopContext) (map[string]any, error) {
	out := make(map[string]any, len(st.Input)+len(st.Bindings))
	for k, v := range st.Input {
		out[k] = v
	}
	for target, ref := range st.Bindings {
		if strings.HasPrefix(ref, "loop.") {
			if loop == nil {
				continue
			}
			switch ref {
			case "loop.item":
				out[target] = loop.item
			case "loop.index":
				out[target] = loop.index
			default:
				return nil, NewNodeError("validation_error", false, "step %q has unknown loop binding %q", st.ID, ref)
			}
			continue
		}
		val, ok := resolveBinding(ref, outputs, inputScope)
		if !ok {
			return nil, NewNodeError("binding_not_found", false, "step %q binding %q could not be resolved", st.ID, ref)
		}
		out[target] = val
	}
	return out, nil
}

func resolveBinding(ref string, outputs map[string]map[string]any, inputScope map[string]any) (any, bool) {
	if ref == "$input" {
		return inputScope, true
	}
	if strings.HasPrefix(ref, "$input.") {
		return lookupPath(inputScope, strings.TrimPrefix(ref, "$input."))
	}
	// Longest-prefix match: step ids themselves contain dots
	// (e.g. "nexaroute.video.submit"), so split greedily.
	parts := strings.Split(ref, ".")
	for i := len(parts); i >= 1; i-- {
		candidate := strings.Join(parts[:i], ".")
		if out, ok := outputs[candidate]; ok {
			if i == len(parts) {
				return out, true
			}
			return lookupPath(out, strings.Join(parts[i:], "."))
		}
	}
	return nil, false
}

func newRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "run-unknown"
	}
	return "run-" + hex.EncodeToString(b[:])
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", NewNodeError("internal_error", false, "could not generate a resume token")
	}
	return hex.EncodeToString(b[:]), nil
}

func (e *Engine) store() RunStore {
	if e.Store != nil {
		return e.Store
	}
	return NewMemoryRunStore()
}

// Run validates and executes a workflow from the start. See the
// package comment for the Run-vs-error contract.
func (e *Engine) Run(ctx context.Context, wf *Workflow, principal authz.Identity, initialInput map[string]any) (*Run, error) {
	if _, _, _, err := e.bounds(); err != nil {
		return nil, err
	}
	cp, err := deepCopyWorkflow(wf)
	if err != nil {
		return nil, err
	}
	normalizeWorkflow(cp)
	if err := e.Validate(cp); err != nil {
		return nil, err
	}
	if initialInput == nil {
		initialInput = map[string]any{}
	}
	order, err := e.topoOrderOrErr(cp)
	if err != nil {
		return nil, err
	}
	root := &runFrame{
		Workflow: cp, Order: order, Outputs: map[string]map[string]any{},
		Input: deepCopyMap(initialInput), Depth: 0,
	}
	exec := &execution{
		engine: e, principal: principal, runID: newRunID(),
		frames: []*runFrame{root}, path: []string{},
	}
	return exec.run(ctx)
}

func (e *Engine) topoOrderOrErr(wf *Workflow) ([]string, error) {
	order, _, err := topoOrder(wf)
	return order, err
}

func normalizeWorkflow(wf *Workflow) {
	if wf.Version == "" {
		wf.Version = SupportedVersion
	}
	for i := range wf.Steps {
		if wf.Steps[i].Version == "" {
			wf.Steps[i].Version = SupportedVersion
		}
		if wf.Steps[i].Subworkflow != nil {
			normalizeWorkflow(wf.Steps[i].Subworkflow)
		}
	}
}

func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var cp map[string]any
	if err := json.Unmarshal(raw, &cp); err != nil {
		return m
	}
	return cp
}

// Resume continues a paused run. The decision must be "approved" or
// "rejected"; the resuming principal becomes the approver. The token
// is one-shot: it is consumed even if the resumed run pauses again
// (a fresh token is issued then).
func (e *Engine) Resume(ctx context.Context, token, decision string, principal authz.Identity) (*Run, error) {
	if decision != "approved" && decision != "rejected" {
		return nil, NewNodeError("validation_error", false, "decision must be approved or rejected")
	}
	if !principal.Authenticated() {
		return nil, NewNodeError("permission_denied", false, "anonymous principal cannot resume a run")
	}
	store := e.store()
	state, err := store.Load(token)
	if err != nil {
		return nil, err
	}
	if len(state.Frames) == 0 {
		return nil, NewNodeError("internal_error", false, "paused run has no frames")
	}
	top := state.Frames[len(state.Frames)-1]
	if top.PendingApproval == "" {
		return nil, NewNodeError("validation_error", false, "paused run has no pending approval")
	}
	top.PendingDecision = decision
	top.PendingApprover = principal.Subject
	if err := store.Delete(token); err != nil {
		return nil, err
	}
	exec := &execution{
		engine: e, principal: state.Principal, runID: state.ID,
		frames: state.Frames, history: state.History, path: state.Path,
	}
	return exec.run(ctx)
}

type execution struct {
	engine    *Engine
	principal authz.Identity
	runID     string
	frames    []*runFrame
	history   []StepRecord
	path      []string
	rootOut   map[string]map[string]any
}

func (ex *execution) record(stepID, nodeID, status string, output map[string]any, errCode string, attempts, iterations int) {
	redacted := output
	if output != nil {
		if r, ok := redactJSONValue(output).(map[string]any); ok {
			redacted = r
		}
	}
	fullID := stepID
	if len(ex.path) > 0 {
		fullID = strings.Join(append(append([]string{}, ex.path...), stepID), "/")
	}
	ex.history = append(ex.history, StepRecord{
		StepID: fullID, NodeID: nodeID, Status: status,
		Output: redacted, ErrorCode: errCode,
		Attempts: attempts, Iterations: iterations,
	})
}

func (ex *execution) failRun(status RunStatus, err *NodeError) *Run {
	out := ex.rootOut
	if out == nil && len(ex.frames) > 0 {
		out = ex.frames[0].Outputs
	}
	return &Run{ID: ex.runID, Status: status, Outputs: out, History: ex.history, Error: err}
}

// run drives the frame stack until completion, pause, or failure.
func (ex *execution) run(ctx context.Context) (*Run, error) {
	e := ex.engine
	_, maxLoop, _, err := e.bounds()
	if err != nil {
		return nil, err
	}
	for len(ex.frames) > 0 {
		fr := ex.frames[len(ex.frames)-1]
		if err := ctx.Err(); err != nil {
			return ex.failRun(RunFailed, NewNodeError("cancelled", false, "workflow execution cancelled")), nil
		}
		if fr.Pos >= len(fr.Order) {
			// Frame complete: pop and deliver to the parent step.
			completed := fr
			ex.frames = ex.frames[:len(ex.frames)-1]
			if len(ex.frames) == 0 {
				ex.rootOut = completed.Outputs
				return &Run{ID: ex.runID, Status: RunCompleted, Outputs: completed.Outputs, History: ex.history}, nil
			}
			parent := ex.frames[len(ex.frames)-1]
			pstep := findStep(parent.Workflow, parent.Order[parent.Pos])
			childOut := map[string]any{"outputs": completed.Outputs, "status": "completed"}
			parent.Outputs[pstep.ID] = childOut
			if len(ex.path) > 0 {
				ex.path = ex.path[:len(ex.path)-1]
			}
			ex.record(pstep.ID, "", "completed", childOut, "", 0, 0)
			parent.Pos++
			continue
		}
		step := findStep(fr.Workflow, fr.Order[fr.Pos])
		if step == nil {
			return ex.failRun(RunFailed, NewNodeError("internal_error", false, "topological order references unknown step")), nil
		}
		// Branch gate: unselected steps never execute.
		if step.Branch != "" && fr.BranchSelection != step.Branch {
			ex.record(step.ID, step.NodeID, "skipped", nil, "", 0, 0)
			fr.Pos++
			continue
		}
		// Subworkflow: push a child frame; the parent step completes
		// when the child frame pops.
		if step.Subworkflow != nil {
			input, err := resolveStepInput(step, fr.Outputs, fr.Input, nil)
			if err != nil {
				if run, done := ex.applyStepError(fr, step, asNodeError(err), 0); done {
					if r, ret := ex.handleDone(run, step); ret {
						return r, nil
					}
					continue
				}
				continue
			}
			childOrder, _, err := topoOrder(step.Subworkflow)
			if err != nil {
				return ex.failRun(RunFailed, asNodeError(err)), nil
			}
			ex.path = append(ex.path, step.ID)
			ex.frames = append(ex.frames, &runFrame{
				Workflow: step.Subworkflow, Order: childOrder,
				Outputs: map[string]map[string]any{},
				Input:   deepCopyMap(input), Depth: fr.Depth + 1,
				ParentStepID: step.ID,
			})
			continue
		}
		spec, err := e.registry().Lookup(step.NodeID, step.Version)
		if err != nil {
			if run, done := ex.applyStepError(fr, step, asNodeError(err), 0); done {
				if r, ret := ex.handleDone(run, step); ret {
					return r, nil
				}
				continue
			}
			continue
		}
		// Approval pause: no decision yet means pause, not error.
		if step.NodeID == approvalNodeID {
			input, err := resolveStepInput(step, fr.Outputs, fr.Input, nil)
			if err != nil {
				if run, done := ex.applyStepError(fr, step, asNodeError(err), 0); done {
					if r, ret := ex.handleDone(run, step); ret {
						return r, nil
					}
					continue
				}
				continue
			}
			if _, hasDecision := input["decision"]; !hasDecision {
				if fr.PendingApproval == step.ID && fr.PendingDecision != "" {
					input["decision"] = fr.PendingDecision
					if fr.PendingApprover != "" {
						input["approver"] = fr.PendingApprover
					}
					fr.PendingApproval, fr.PendingDecision, fr.PendingApprover = "", "", ""
				} else {
					fr.PendingApproval = step.ID
					token, err := newToken()
					if err != nil {
						return nil, err
					}
					state := &RunState{
						ID: ex.runID, Token: token, Workflow: ex.frames[0].Workflow,
						Principal: ex.principal, PolicyApproved: e.PolicyApproved,
						Frames: ex.frames, History: ex.history, Path: ex.path,
					}
					if err := e.store().Save(state); err != nil {
						return nil, err
					}
					out := ex.frames[0].Outputs
					return &Run{ID: ex.runID, Status: RunPaused, Outputs: out, History: ex.history, ResumeToken: token}, nil
				}
			}
			out, attempts, runErr := ex.executeNode(ctx, spec, step, input)
			if runErr != nil {
				if run, done := ex.applyStepError(fr, step, runErr, attempts); done {
					if r, ret := ex.handleDone(run, step); ret {
						return r, nil
					}
					continue
				}
				continue
			}
			fr.Outputs[step.ID] = out
			ex.record(step.ID, step.NodeID, "completed", out, "", attempts, 0)
			fr.Pos++
			continue
		}
		// Loop step: expand the loop_plan output, capped, then run
		// each iteration through the node with retry.
		if step.IterateFrom != "" {
			run, done := ex.executeLoop(ctx, fr, step, spec, maxLoop)
			if done {
				if r, ret := ex.handleDone(run, step); ret {
					return r, nil
				}
			}
			continue
		}
		input, err := resolveStepInput(step, fr.Outputs, fr.Input, nil)
		if err != nil {
			if run, done := ex.applyStepError(fr, step, asNodeError(err), 0); done {
				if r, ret := ex.handleDone(run, step); ret {
					return r, nil
				}
				continue
			}
			continue
		}
		out, attempts, runErr := ex.executeNode(ctx, spec, step, input)
		if runErr != nil {
			if run, done := ex.applyStepError(fr, step, runErr, attempts); done {
				if r, ret := ex.handleDone(run, step); ret {
					return r, nil
				}
				continue
			}
			continue
		}
		fr.Outputs[step.ID] = out
		if step.NodeID == branchNodeID {
			if sel, ok := out["selected_branch"].(string); ok {
				fr.BranchSelection = sel
			}
		}
		ex.record(step.ID, step.NodeID, "completed", out, "", attempts, 0)
		fr.Pos++
	}
	return ex.failRun(RunFailed, NewNodeError("internal_error", false, "execution ended with no frames and no result")), nil
}

// executeLoop runs one loop step. It returns (run, true) when the run
// is over (failure/stop), or (nil, false) when the step completed and
// execution should continue.
func (ex *execution) executeLoop(ctx context.Context, fr *runFrame, step *Step, spec *NodeSpec, maxLoop int) (*Run, bool) {
	plan, ok := fr.Outputs[step.IterateFrom]
	if !ok {
		return ex.applyStepError(fr, step, NewNodeError("binding_not_found", false, "loop plan step %q has no output", step.IterateFrom), 0)
	}
	rawIterations, ok := plan["iterations"].([]any)
	if !ok {
		return ex.applyStepError(fr, step, NewNodeError("validation_error", false, "loop plan step %q output has no iterations array", step.IterateFrom), 0)
	}
	if len(rawIterations) > maxLoop {
		return ex.applyStepError(fr, step, NewNodeError("loop_iteration_cap", false, "loop plan has %d iterations, exceeding the engine cap %d", len(rawIterations), maxLoop), 0)
	}
	base, err := resolveStepInput(step, fr.Outputs, fr.Input, nil)
	if err != nil {
		return ex.applyStepError(fr, step, asNodeError(err), 0)
	}
	collected := make([]map[string]any, 0, len(rawIterations))
	totalAttempts := 0
	for idx, item := range rawIterations {
		iter, ok := item.(map[string]any)
		if !ok {
			return ex.applyStepError(fr, step, NewNodeError("validation_error", false, "loop iteration %d is not an object", idx), 0)
		}
		loop := &loopContext{item: iter["item"], index: idx}
		// Iteration index in the plan object is authoritative when
		// present; fall back to position.
		if n, ok := iter["index"]; ok {
			if f, ok := toNumber(n); ok {
				loop.index = int(f)
			}
		}
		input := deepCopyMap(base)
		iterInput, err := resolveStepInput(step, fr.Outputs, fr.Input, loop)
		if err != nil {
			return ex.applyStepError(fr, step, asNodeError(err), 0)
		}
		for k, v := range iterInput {
			input[k] = v
		}
		out, attempts, runErr := ex.executeNode(ctx, spec, step, input)
		totalAttempts += attempts
		if runErr != nil {
			return ex.applyStepError(fr, step, runErr, totalAttempts)
		}
		collected = append(collected, out)
	}
	out := map[string]any{"iterations": collected, "count": len(collected)}
	fr.Outputs[step.ID] = out
	ex.record(step.ID, step.NodeID, "completed", out, "", totalAttempts, len(collected))
	fr.Pos++
	return nil, false
}

// applyStepError applies the step's OnError policy. It returns
// (run, true) when the run ends, or (nil, false) when execution
// continues: on fallback it records the fallback output and advances
// Pos itself, so the caller must just continue the loop.
//
// A failure inside a subworkflow does not bypass the parent step's
// policy: handleDone unwinds a propagate failure to the enclosing
// subworkflow step and applies that step's OnError there (fallback
// continues the parent, stop ends the run, propagate unwinds
// further). A stop inside a nested frame always ends the whole run;
// it is never converted into a parent fallback.
func (ex *execution) applyStepError(fr *runFrame, step *Step, runErr *NodeError, attempts int) (*Run, bool) {
	switch step.OnError {
	case "fallback":
		fallback := deepCopyMap(step.FallbackOutput)
		fr.Outputs[step.ID] = fallback
		ex.record(step.ID, step.NodeID, "fallback", fallback, runErr.Code, attempts, 0)
		fr.Pos++
		return nil, false
	case "stop":
		ex.record(step.ID, step.NodeID, "failed", nil, runErr.Code, attempts, 0)
		return ex.failRun(RunStopped, runErr), true
	default: // "" and "propagate"
		ex.record(step.ID, step.NodeID, "failed", nil, runErr.Code, attempts, 0)
		return ex.failRun(RunFailed, runErr), true
	}
}

// handleDone routes a finished-or-failed step result. It returns
// (run, true) when the caller should return the run, or (nil, false)
// when execution continues after unwinding a nested propagate
// failure into a parent fallback.
func (ex *execution) handleDone(run *Run, step *Step) (*Run, bool) {
	if run == nil {
		return nil, false
	}
	if len(ex.frames) > 1 && (step.OnError == "" || step.OnError == "propagate") && run.Error != nil {
		if unwound := ex.unwindFailure(run.Error); unwound != nil {
			return unwound, true
		}
		return nil, false
	}
	return run, true
}

// unwindFailure pops frames, applying each enclosing subworkflow
// step's OnError policy, until a fallback continues execution
// (returns nil), a stop ends the run, or the root frame fails.
func (ex *execution) unwindFailure(err *NodeError) *Run {
	for len(ex.frames) > 1 {
		ex.frames = ex.frames[:len(ex.frames)-1]
		if len(ex.path) > 0 {
			ex.path = ex.path[:len(ex.path)-1]
		}
		parent := ex.frames[len(ex.frames)-1]
		pstep := findStep(parent.Workflow, parent.Order[parent.Pos])
		if pstep == nil {
			return ex.failRun(RunFailed, err)
		}
		switch pstep.OnError {
		case "fallback":
			fallback := deepCopyMap(pstep.FallbackOutput)
			parent.Outputs[pstep.ID] = fallback
			ex.record(pstep.ID, "", "fallback", fallback, err.Code, 0, 0)
			parent.Pos++
			return nil
		case "stop":
			ex.record(pstep.ID, "", "failed", nil, err.Code, 0, 0)
			return ex.failRun(RunStopped, err)
		default:
			ex.record(pstep.ID, "", "failed", nil, err.Code, 0, 0)
			if len(ex.frames) == 1 {
				return ex.failRun(RunFailed, err)
			}
		}
	}
	return ex.failRun(RunFailed, err)
}

// executeNode runs one node through the registry with the spec's
// retry policy. Billable and irreversible nodes are retried only on
// timeout, even if the spec names other retryable codes.
func (ex *execution) executeNode(ctx context.Context, spec *NodeSpec, step *Step, input map[string]any) (map[string]any, int, *NodeError) {
	e := ex.engine
	attempts := spec.Retry.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 3 {
		attempts = 3
	}
	retryable := make(map[string]bool, len(spec.Retry.RetryableCodes))
	for _, code := range spec.Retry.RetryableCodes {
		retryable[code] = true
	}
	var lastErr *NodeError
	for attempt := 1; attempt <= attempts; attempt++ {
		out, err := e.registry().Execute(ctx, spec, ex.principal, input, step.Config, e.Backend, e.PolicyApproved)
		if err == nil {
			return out, attempt, nil
		}
		lastErr = asNodeError(err)
		if ctx.Err() != nil {
			return nil, attempt, NewNodeError("cancelled", false, "node %q execution cancelled", spec.ID)
		}
		if attempt >= attempts || !retryable[lastErr.Code] {
			return nil, attempt, lastErr
		}
		if (spec.SideEffect == SideEffectBillable || spec.SideEffect == SideEffectIrreversible) && lastErr.Code != "timeout" {
			return nil, attempt, lastErr
		}
	}
	return nil, attempts, lastErr
}

func findStep(wf *Workflow, id string) *Step {
	for i := range wf.Steps {
		if wf.Steps[i].ID == id {
			return &wf.Steps[i]
		}
	}
	return nil
}
