package nexaflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// SupportedVersion is the only node schema version this registry accepts.
// Unknown versions are rejected fail-closed by Lookup and Execute.
const SupportedVersion = "v1"

// SideEffect names what a node can do to the world. It is documentation
// with teeth: nodes that bill or mutate require operator policy approval
// (RequiresOperatorPolicy) and the approval is checked in Execute.
type SideEffect string

const (
	SideEffectNone         SideEffect = "none"
	SideEffectRead         SideEffect = "read"
	SideEffectExternal     SideEffect = "external"
	SideEffectBillable     SideEffect = "billable"
	SideEffectIrreversible SideEffect = "irreversible"
)

// RetryPolicy bounds retries for a node. MaxAttempts is clamped to [1,3]
// at registration; RetryableCodes names the NodeError codes that may be
// retried by a runtime. This package itself never retries: retry is the
// runtime's job, and the policy only declares what is safe to retry.
type RetryPolicy struct {
	MaxAttempts    int
	RetryableCodes []string
}

// Field is one versioned schema field. Type is one of
// "string", "number", "boolean", "object", "array", "secretRef".
// A secretRef field holds a reference to a secret (never the secret value);
// the backend resolves it, and it is rejected if no resolver is wired.
type Field struct {
	Name        string
	Type        string
	Required    bool
	Description string
}

// RunContext is what a node's Run function receives. Input and Config are
// already validated against the node's schemas by Execute; Run functions
// must still treat them as untrusted input and re-check their own bounds.
type RunContext struct {
	Principal      authz.Identity
	Input          map[string]any
	Config         map[string]any
	Backend        Backend
	PolicyApproved bool
}

// NodeSpec is the versioned contract for one node. Run is the adapter: it
// must call the Backend port (or nothing, for pure nodes), must never log
// or store prompts, secrets, or filesystem paths, and must return
// secret-safe errors (Execute re-sanitizes regardless).
type NodeSpec struct {
	ID                     string
	Version                string
	Category               string
	Title                  string
	Description            string
	InputSchema            []Field
	OutputSchema           []Field
	ConfigSchema           []Field
	Permission             authz.Permission // "" = authenticated principal only
	SideEffect             SideEffect
	Retry                  RetryPolicy
	DefaultTimeout         time.Duration
	MaxTimeout             time.Duration
	MaxInputBytes          int
	MaxOutputBytes         int
	RequiresOperatorPolicy bool
	Run                    func(ctx context.Context, rc *RunContext) (map[string]any, error)
}

// NodeError is the only error shape nodes should surface. Message is
// always passed through RedactError by Execute before it escapes.
type NodeError struct {
	Code      string
	Retryable bool
	Message   string
}

func (e *NodeError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + RedactError(errors.New(e.Message))
}

// NewNodeError builds a NodeError with a pre-sanitized message.
func NewNodeError(code string, retryable bool, format string, args ...any) *NodeError {
	return &NodeError{Code: code, Retryable: retryable, Message: RedactError(fmt.Errorf(format, args...))}
}

func asNodeError(err error) *NodeError {
	if err == nil {
		return nil
	}
	var ne *NodeError
	if errors.As(err, &ne) {
		return &NodeError{Code: ne.Code, Retryable: ne.Retryable, Message: RedactError(errors.New(ne.Message))}
	}
	return &NodeError{Code: "backend_error", Retryable: false, Message: RedactError(err)}
}

// Registry maps node id + version to specs.
type Registry struct {
	byKey map[string]*NodeSpec
}

// NewRegistry builds a registry from specs, rejecting duplicate keys,
// unsupported versions, and retry policies outside [1,3].
func NewRegistry(specs []*NodeSpec) (*Registry, error) {
	r := &Registry{byKey: make(map[string]*NodeSpec, len(specs))}
	for _, spec := range specs {
		if spec == nil {
			return nil, errors.New("nexaflow: nil node spec")
		}
		if spec.Version != SupportedVersion {
			return nil, fmt.Errorf("nexaflow: node %q has unsupported version %q (only %q is supported)", spec.ID, spec.Version, SupportedVersion)
		}
		if spec.ID == "" || spec.Run == nil {
			return nil, fmt.Errorf("nexaflow: node spec missing id or Run function")
		}
		if spec.Retry.MaxAttempts < 1 || spec.Retry.MaxAttempts > 3 {
			return nil, fmt.Errorf("nexaflow: node %q retry MaxAttempts %d outside [1,3]", spec.ID, spec.Retry.MaxAttempts)
		}
		if spec.DefaultTimeout <= 0 || spec.MaxTimeout < spec.DefaultTimeout {
			return nil, fmt.Errorf("nexaflow: node %q has invalid timeouts", spec.ID)
		}
		if spec.MaxInputBytes <= 0 || spec.MaxOutputBytes <= 0 {
			return nil, fmt.Errorf("nexaflow: node %q has invalid byte bounds", spec.ID)
		}
		key := spec.ID + "@" + spec.Version
		if _, dup := r.byKey[key]; dup {
			return nil, fmt.Errorf("nexaflow: duplicate node %q", key)
		}
		r.byKey[key] = spec
	}
	return r, nil
}

// DefaultRegistry returns the registry of all adapter nodes.
func DefaultRegistry() *Registry {
	r, err := NewRegistry(allNodeSpecs())
	if err != nil {
		panic(err)
	}
	return r
}

// Lookup returns the spec for id@version, rejecting unknown ids and any
// version other than SupportedVersion fail-closed.
func (r *Registry) Lookup(id, version string) (*NodeSpec, error) {
	if version != SupportedVersion {
		return nil, NewNodeError("unsupported_version", false, "node version %q is not supported; only %q is supported", version, SupportedVersion)
	}
	spec, ok := r.byKey[id+"@"+version]
	if !ok {
		return nil, NewNodeError("node_not_found", false, "node %q not found", id)
	}
	return spec, nil
}

// All returns every registered spec in deterministic (registration) order.
func (r *Registry) All() []*NodeSpec {
	out := make([]*NodeSpec, 0, len(r.byKey))
	for _, spec := range allNodeSpecs() {
		if found, ok := r.byKey[spec.ID+"@"+spec.Version]; ok {
			out = append(out, found)
		}
	}
	return out
}

var fieldTypes = map[string]bool{
	"string": true, "number": true, "boolean": true,
	"object": true, "array": true, "secretRef": true,
}

func validateAgainstSchema(values map[string]any, schema []Field, kind string) error {
	known := make(map[string]Field, len(schema))
	for _, f := range schema {
		if !fieldTypes[f.Type] {
			return NewNodeError("invalid_schema", false, "field %q has unknown type %q", f.Name, f.Type)
		}
		known[f.Name] = f
		if f.Required {
			if _, ok := values[f.Name]; !ok {
				return NewNodeError("validation_error", false, "missing required %s field %q", kind, f.Name)
			}
		}
	}
	for key, val := range values {
		f, ok := known[key]
		if !ok {
			// Fail-closed: unknown keys are rejected, never ignored.
			return NewNodeError("validation_error", false, "unknown %s field %q", kind, key)
		}
		if err := checkFieldType(f, val); err != nil {
			return err
		}
	}
	return nil
}

func checkFieldType(f Field, val any) error {
	switch f.Type {
	case "string", "secretRef":
		if _, ok := val.(string); !ok {
			return NewNodeError("validation_error", false, "field %q must be a string", f.Name)
		}
	case "number":
		switch val.(type) {
		case float64, float32, int, int64, json.Number:
		default:
			return NewNodeError("validation_error", false, "field %q must be a number", f.Name)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return NewNodeError("validation_error", false, "field %q must be a boolean", f.Name)
		}
	case "object":
		if _, ok := val.(map[string]any); !ok {
			return NewNodeError("validation_error", false, "field %q must be an object", f.Name)
		}
	case "array":
		if _, ok := val.([]any); !ok {
			return NewNodeError("validation_error", false, "field %q must be an array", f.Name)
		}
	}
	return nil
}

// ValidateConfig validates config against the spec's config schema,
// rejecting unknown keys fail-closed.
func (r *Registry) ValidateConfig(spec *NodeSpec, config map[string]any) error {
	if config == nil {
		config = map[string]any{}
	}
	return validateAgainstSchema(config, spec.ConfigSchema, "config")
}

func jsonByteLen(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return 1 << 30 // unmarshalable input is over any bound
	}
	return len(raw)
}

// Execute runs one node with all cross-cutting guards applied, in order:
// authentication, permission, operator policy, config/input validation,
// input byte bound, per-node timeout, output byte bound, secret-safe
// output and errors. Cancellation is honoured via the caller's context
// and the per-node timeout context; a cancelled run reports code
// "cancelled" and never a partial success.
func (r *Registry) Execute(ctx context.Context, spec *NodeSpec, principal authz.Identity, input, config map[string]any, backend Backend, policyApproved bool) (map[string]any, error) {
	if spec == nil {
		return nil, NewNodeError("node_not_found", false, "nil node spec")
	}
	if !principal.Authenticated() {
		return nil, NewNodeError("permission_denied", false, "anonymous principal cannot execute node %q", spec.ID)
	}
	if spec.Permission != "" && !principal.Authorize(spec.Permission) {
		return nil, NewNodeError("permission_denied", false, "principal %q lacks permission %q for node %q", principal.Subject, string(spec.Permission), spec.ID)
	}
	if spec.RequiresOperatorPolicy && !policyApproved {
		return nil, NewNodeError("policy_required", false, "node %q requires explicit operator policy approval", spec.ID)
	}
	if input == nil {
		input = map[string]any{}
	}
	if config == nil {
		config = map[string]any{}
	}
	if err := r.ValidateConfig(spec, config); err != nil {
		return nil, err
	}
	if err := validateAgainstSchema(input, spec.InputSchema, "input"); err != nil {
		return nil, err
	}
	if jsonByteLen(input) > spec.MaxInputBytes {
		return nil, NewNodeError("input_too_large", false, "input exceeds %d bytes", spec.MaxInputBytes)
	}
	timeout := spec.DefaultTimeout
	if timeout > spec.MaxTimeout {
		timeout = spec.MaxTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := spec.Run(runCtx, &RunContext{
		Principal: principal, Input: input, Config: config,
		Backend: backend, PolicyApproved: policyApproved,
	})
	if err != nil {
		if errors.Is(runCtx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			return nil, NewNodeError("cancelled", false, "node %q execution cancelled", spec.ID)
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return nil, NewNodeError("timeout", true, "node %q timed out after %s", spec.ID, timeout)
		}
		return nil, asNodeError(err)
	}
	if runCtx.Err() != nil {
		return nil, NewNodeError("cancelled", false, "node %q execution cancelled", spec.ID)
	}
	if out == nil {
		out = map[string]any{}
	}
	redacted := redactJSONValue(out)
	outMap, ok := redacted.(map[string]any)
	if !ok {
		return nil, NewNodeError("internal_error", false, "node %q returned a non-object output", spec.ID)
	}
	if jsonByteLen(outMap) > spec.MaxOutputBytes {
		return nil, NewNodeError("output_too_large", false, "output exceeds %d bytes", spec.MaxOutputBytes)
	}
	return outMap, nil
}

// Helpers for node implementations: typed, bound-checked accessors over
// the already schema-validated input/config maps.

func getString(m map[string]any, key string) (string, bool) {
	v, ok := m[key].(string)
	return v, ok
}

func getStringOr(m map[string]any, key, def string) string {
	if v, ok := getString(m, key); ok {
		return v
	}
	return def
}

func getNumber(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

func getBool(m map[string]any, key string) (bool, bool) {
	v, ok := m[key].(bool)
	return v, ok
}

func getArray(m map[string]any, key string) ([]any, bool) {
	v, ok := m[key].([]any)
	return v, ok
}

func getObject(m map[string]any, key string) (map[string]any, bool) {
	v, ok := m[key].(map[string]any)
	return v, ok
}

func getStringSlice(m map[string]any, key string) ([]string, bool) {
	arr, ok := getArray(m, key)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func requireBackend(rc *RunContext) error {
	if rc.Backend == nil {
		return NewNodeError("backend_unavailable", true, "no backend is wired for this node")
	}
	return nil
}

func truncateForOutput(s string, max int) string {
	return truncateText(s, max)
}
