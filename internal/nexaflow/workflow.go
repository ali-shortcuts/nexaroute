// Workflow definitions: the single execution contract for NexaFlow v1.
//
// A Definition is a versioned, immutable snapshot of a workflow: an
// ordered set of node instances, the edges between them, the triggers
// that may start a run, and run-level settings. It is deliberately NOT
// a runtime: it has no store, no scheduler, no HTTP API, and no
// execution engine. The runtime (a later phase) consumes validated
// definitions and must implement exactly the semantics documented in
// docs/adr/0008-nexaflow-foundation.md and docs/nexaflow-architecture.md.
//
// One registry, one contract: node references resolve against the same
// Registry that Execute uses (DefaultRegistry for v1 nodes). Node
// configs are validated with Registry.ValidateConfig, so a definition
// can never reference a node version or a config key the runtime
// would reject. There is no second registry and no parallel schema.
//
// Graph model (v1):
//   - Nodes are instances: the same registry node may appear several
//     times under distinct instance IDs. This differs from templates,
//     whose edges reference registry node IDs directly because a
//     template never contains two instances of one node.
//   - Nodes are single-input / single-output maps. There are no ports
//     in v1: an edge carries the whole output map of its source node
//     to its target node. Port-level typing is a future extension.
//   - Data flow: a node's input is the merge of its direct
//     predecessors' outputs, in edge-declaration order, with later
//     keys overwriting earlier ones on conflict. The control.merge
//     node exists for explicit conflict policies (first / last /
//     error); the default is last-wins.
//   - Branching: only nexaroute.control.branch selects between named
//     branches. A branch edge (Branch non-empty) is valid only from a
//     branch node, and its label must equal one of that node's
//     configured true_branch / false_branch values. An unlabelled edge
//     from a branch node is unconditional (always taken). A node
//     downstream of an unselected branch edge is skipped, not failed.
//   - Cycles: rejected in v1. There is no loop-execution primitive yet
//     (control.loop_plan only plans iterations); a bounded-loop
//     construct is a later phase, so any cycle is a definition error.
//   - Roots: every trigger-category node is an entry point and must
//     have no incoming edge. Every non-trigger node must have at least
//     one incoming edge and must be reachable from a trigger node.
//     Orphan subgraphs are rejected.
//
// Triggers: a DefinitionTrigger names its entry node by instance ID.
// There is exactly one declaration per trigger node and one trigger
// node per declaration (a bijection, enforced by validation). The
// declaration's Config is the trigger node's runtime config, validated
// against that node's ConfigSchema — so a webhook declaration carries
// secret_ref (a reference, never a value) and a schedule declaration
// carries interval or daily. Disabling a trigger is a new published
// revision without that trigger; there is no Enabled flag, because
// published definitions are immutable snapshots.
//
// Versioning: a DefinitionVersion carries the content hash of its
// definition snapshot. The hash is sha256 over the JSON encoding of
// the Definition (Go encoding/json: struct field order is fixed and
// map keys sort, so the encoding is deterministic within this
// implementation). Hashes are comparable only within this
// implementation, not across languages: this is not a JCS-style
// canonicalization. Rollback is republishing an older snapshot as a
// new revision; revisions are never mutated. Status transitions are
// draft -> published -> superseded, with superseded terminal; drafts
// are never "superseded", they are simply never published.
//
// Validation is a pure function: ValidateDefinition never mutates its
// input. All failures are fail-closed with a "nexaflow: definition"
// prefix so callers can distinguish definition errors from node
// execution errors.
package nexaflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// DefinitionSchemaVersion is the only definition schema version
// accepted by ValidateDefinition. It is a registry generation ("v1"),
// not a semver triple, mirroring SupportedVersion for nodes.
const DefinitionSchemaVersion = "v1"

// DefinitionStatus values for DefinitionVersion. See the transition
// rules on DefinitionVersion.CanTransitionTo.
const (
	DefinitionDraft      = "draft"
	DefinitionPublished  = "published"
	DefinitionSuperseded = "superseded"
)

// TriggerKind values for DefinitionTrigger.Kind.
const (
	TriggerManual   = "manual"
	TriggerWebhook  = "webhook"
	TriggerSchedule = "schedule"
)

// triggerNodeID maps a trigger kind to the registry node that
// implements it. The mapping is fixed in v1: no other trigger kinds
// exist.
var triggerNodeID = map[string]string{
	TriggerManual:   "nexaroute.trigger.manual",
	TriggerWebhook:  "nexaroute.trigger.webhook",
	TriggerSchedule: "nexaroute.trigger.schedule",
}

// branchNodeID is the only branching primitive in v1. Branch edges
// (DefinitionEdge.Branch non-empty) are valid only from this node.
const branchNodeID = "nexaroute.control.branch"

// Definition is one versioned workflow snapshot.
type Definition struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	// TenantScope is an admin-plane scope label only. It is NOT an
	// isolation boundary: v1 tenancy is a label checked against an
	// operator ceiling, not a sandbox. Empty means unscoped.
	TenantScope string              `json:"tenant_scope,omitempty"`
	Nodes       []DefinitionNode    `json:"nodes"`
	Edges       []DefinitionEdge    `json:"edges,omitempty"`
	Triggers    []DefinitionTrigger `json:"triggers"`
	Settings    DefinitionSettings  `json:"settings,omitempty"`
}

// DefinitionNode is one instance of a registry node inside a
// definition. Config is validated against the node's ConfigSchema;
// secretRef fields hold references only, never secret values.
type DefinitionNode struct {
	ID      string         `json:"id"`
	NodeID  string         `json:"node_id"`
	Version string         `json:"version"`
	Config  map[string]any `json:"config,omitempty"`
	// Input, when non-nil, maps node input fields to literals or to
	// references ({"$ref": "path"} or "$.path" strings) into the run
	// context (workflow input plus succeeded node outputs). It replaces
	// the predecessor merge for this node, so the node's input is
	// exactly the mapped keys (projection onto the declared input).
	// Contract adopted from the #691 definition model (same JSON
	// shape) by Task 8 to close the condition->branch composition gap.
	// Trigger-category nodes must not declare Input.
	Input map[string]any `json:"input,omitempty"`
}

// DefinitionEdge connects two node instances. Branch is empty for an
// unconditional edge; non-empty only from a branch node, where it
// must equal one of that node's configured branch names.
type DefinitionEdge struct {
	ID     string `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Branch string `json:"branch,omitempty"`
}

// DefinitionTrigger declares one entry point. Name is the instance ID
// of a trigger-category node in the same definition; Config is that
// node's runtime config.
type DefinitionTrigger struct {
	Name   string         `json:"name"`
	Kind   string         `json:"kind"`
	Config map[string]any `json:"config,omitempty"`
}

// DefinitionSettings carries run-level settings. Zero values mean
// defaults, documented per field; negative values are rejected.
type DefinitionSettings struct {
	// RunTimeoutSeconds bounds a whole run. 0 means no run-level
	// deadline beyond the per-node timeouts.
	RunTimeoutSeconds int64 `json:"run_timeout_seconds,omitempty"`
	// MaxConcurrentRuns caps concurrent runs of this definition.
	// 0 means 1 (the v1 default: one run at a time per definition).
	MaxConcurrentRuns int `json:"max_concurrent_runs,omitempty"`
	// RetentionDays bounds how long run history is kept. 0 means
	// the default of 90 days; when set, it must be in [7, 365].
	RetentionDays int `json:"retention_days,omitempty"`
}

// DefinitionVersion is the version record for one published snapshot.
// The definition itself is not stored here: a store (later phase)
// keeps Definition + DefinitionVersion together, keyed by
// DefinitionID + Revision.
type DefinitionVersion struct {
	DefinitionID string `json:"definition_id"`
	Revision     uint64 `json:"revision"`
	ContentHash  string `json:"content_hash"`
	Status       string `json:"status"`
}

// CanTransitionTo reports whether a status change is allowed under
// the v1 lifecycle: draft -> published -> superseded, superseded
// terminal. A draft is never superseded (it is simply never
// published); a published version is never re-published (rollback
// publishes an older snapshot as a NEW revision instead).
func (v DefinitionVersion) CanTransitionTo(next string) bool {
	switch v.Status {
	case DefinitionDraft:
		return next == DefinitionPublished
	case DefinitionPublished:
		return next == DefinitionSuperseded
	default:
		return false
	}
}

// ContentHash returns the hex sha256 of the JSON encoding of def.
// The encoding is deterministic within this implementation (fixed
// struct field order, sorted map keys). Two definitions that differ
// in any field hash differently; a definition and its JSON round-trip
// hash identically.
func ContentHash(def *Definition) (string, error) {
	if def == nil {
		return "", fmt.Errorf("nexaflow: definition is nil")
	}
	raw, err := json.Marshal(def)
	if err != nil {
		return "", fmt.Errorf("nexaflow: definition is not JSON-encodable: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ValidateDefinition checks a definition against the registry and
// the v1 graph rules. It never mutates def. A nil registry is
// rejected fail-closed: callers pass DefaultRegistry explicitly.
func ValidateDefinition(def *Definition, reg *Registry) error {
	if def == nil {
		return fmt.Errorf("nexaflow: definition is nil")
	}
	if reg == nil {
		return fmt.Errorf("nexaflow: definition %q: registry is nil", def.ID)
	}
	if def.SchemaVersion != DefinitionSchemaVersion {
		return fmt.Errorf("nexaflow: definition %q: schema_version %q, want %q", def.ID, def.SchemaVersion, DefinitionSchemaVersion)
	}
	if def.ID == "" {
		return fmt.Errorf("nexaflow: definition id is empty")
	}
	if def.Name == "" {
		return fmt.Errorf("nexaflow: definition %q: name is empty", def.ID)
	}
	if err := validateSettings(def); err != nil {
		return err
	}

	nodes := make(map[string]*NodeSpec, len(def.Nodes))
	instanceIsTrigger := make(map[string]bool, len(def.Nodes))
	if len(def.Nodes) == 0 {
		return fmt.Errorf("nexaflow: definition %q: no nodes", def.ID)
	}
	for i := range def.Nodes {
		n := &def.Nodes[i]
		if n.ID == "" {
			return fmt.Errorf("nexaflow: definition %q: node instance id is empty", def.ID)
		}
		if _, dup := nodes[n.ID]; dup {
			return fmt.Errorf("nexaflow: definition %q: duplicate node instance id %q", def.ID, n.ID)
		}
		if n.Version != SupportedVersion {
			return fmt.Errorf("nexaflow: definition %q: node instance %q: version %q, want %q", def.ID, n.ID, n.Version, SupportedVersion)
		}
		spec, err := reg.Lookup(n.NodeID, n.Version)
		if err != nil {
			return fmt.Errorf("nexaflow: definition %q: node instance %q: %w", def.ID, n.ID, err)
		}
		if err := reg.ValidateConfig(spec, n.Config); err != nil {
			return fmt.Errorf("nexaflow: definition %q: node instance %q (%s): %w", def.ID, n.ID, n.NodeID, err)
		}
		nodes[n.ID] = spec
		instanceIsTrigger[n.ID] = spec.Category == "trigger"
	}
	if err := validateInputMappings(def, nodes, instanceIsTrigger); err != nil {
		return err
	}

	// Triggers: bijection with trigger-category nodes, config
	// validated against the mapped trigger node's ConfigSchema.
	triggerDecls := make(map[string]bool, len(def.Triggers))
	if len(def.Triggers) == 0 {
		return fmt.Errorf("nexaflow: definition %q: no triggers: a definition with no entry point can never run", def.ID)
	}
	for i := range def.Triggers {
		tr := &def.Triggers[i]
		if tr.Name == "" {
			return fmt.Errorf("nexaflow: definition %q: trigger name is empty", def.ID)
		}
		if triggerDecls[tr.Name] {
			return fmt.Errorf("nexaflow: definition %q: duplicate trigger %q", def.ID, tr.Name)
		}
		triggerDecls[tr.Name] = true
		nodeID, ok := triggerNodeID[tr.Kind]
		if !ok {
			return fmt.Errorf("nexaflow: definition %q: trigger %q: unknown kind %q", def.ID, tr.Name, tr.Kind)
		}
		spec, ok := nodes[tr.Name]
		if !ok {
			return fmt.Errorf("nexaflow: definition %q: trigger %q: no node instance with that id", def.ID, tr.Name)
		}
		if spec.ID != nodeID {
			return fmt.Errorf("nexaflow: definition %q: trigger %q: kind %q maps to node %s but instance is %s", def.ID, tr.Name, tr.Kind, nodeID, spec.ID)
		}
		if err := reg.ValidateConfig(spec, tr.Config); err != nil {
			return fmt.Errorf("nexaflow: definition %q: trigger %q: %w", def.ID, tr.Name, err)
		}
	}
	for id, isTrigger := range instanceIsTrigger {
		if isTrigger && !triggerDecls[id] {
			return fmt.Errorf("nexaflow: definition %q: trigger node instance %q has no trigger declaration", def.ID, id)
		}
	}

	// Edges: referential integrity, branch-label rule, root rule.
	incoming := make(map[string]int, len(def.Nodes))
	adj := make(map[string][]string, len(def.Nodes))
	edgeIDs := make(map[string]bool, len(def.Edges))
	edgeTriples := make(map[string]bool, len(def.Edges))
	for i := range def.Edges {
		e := &def.Edges[i]
		if e.ID == "" {
			return fmt.Errorf("nexaflow: definition %q: edge id is empty", def.ID)
		}
		if edgeIDs[e.ID] {
			return fmt.Errorf("nexaflow: definition %q: duplicate edge id %q", def.ID, e.ID)
		}
		edgeIDs[e.ID] = true
		from, ok := nodes[e.From]
		if !ok {
			return fmt.Errorf("nexaflow: definition %q: edge %q: unknown source node %q", def.ID, e.ID, e.From)
		}
		if _, ok := nodes[e.To]; !ok {
			return fmt.Errorf("nexaflow: definition %q: edge %q: unknown target node %q", def.ID, e.ID, e.To)
		}
		if e.From == e.To {
			return fmt.Errorf("nexaflow: definition %q: edge %q: self-edge on node %q", def.ID, e.ID, e.From)
		}
		triple := e.From + "\x00" + e.To + "\x00" + e.Branch
		if edgeTriples[triple] {
			return fmt.Errorf("nexaflow: definition %q: duplicate edge %q -> %q (branch %q)", def.ID, e.From, e.To, e.Branch)
		}
		edgeTriples[triple] = true
		if e.Branch != "" {
			if err := validateBranchEdge(def, e, from); err != nil {
				return err
			}
		}
		incoming[e.To]++
		adj[e.From] = append(adj[e.From], e.To)
	}

	// Root rule: trigger nodes take no input; every other node takes
	// at least one.
	for id, isTrigger := range instanceIsTrigger {
		if isTrigger {
			if incoming[id] > 0 {
				return fmt.Errorf("nexaflow: definition %q: trigger node instance %q has an incoming edge", def.ID, id)
			}
		} else if incoming[id] == 0 {
			return fmt.Errorf("nexaflow: definition %q: node instance %q has no incoming edge and is not a trigger", def.ID, id)
		}
	}

	// Reachability: every node reachable from a trigger node.
	reached := make(map[string]bool, len(def.Nodes))
	var visit func(id string)
	visit = func(id string) {
		if reached[id] {
			return
		}
		reached[id] = true
		for _, next := range adj[id] {
			visit(next)
		}
	}
	for id, isTrigger := range instanceIsTrigger {
		if isTrigger {
			visit(id)
		}
	}
	for id := range nodes {
		if !reached[id] {
			return fmt.Errorf("nexaflow: definition %q: node instance %q is not reachable from any trigger", def.ID, id)
		}
	}

	// Cycle rejection (v1 has no loop-execution primitive).
	if err := checkAcyclic(def, adj); err != nil {
		return err
	}
	return nil
}

// validateBranchEdge enforces the v1 branching rule: a labelled edge
// must come from a branch node, and its label must be one of that
// node's configured branch names.
func validateBranchEdge(def *Definition, e *DefinitionEdge, from *NodeSpec) error {
	if from.ID != branchNodeID {
		return fmt.Errorf("nexaflow: definition %q: edge %q: branch label %q on an edge from non-branch node %s", def.ID, e.ID, e.Branch, from.ID)
	}
	for i := range def.Nodes {
		if def.Nodes[i].ID != e.From {
			continue
		}
		for _, key := range []string{"true_branch", "false_branch"} {
			if v, ok := def.Nodes[i].Config[key].(string); ok && v == e.Branch {
				return nil
			}
		}
		return fmt.Errorf("nexaflow: definition %q: edge %q: branch label %q is not one of node %q configured branches", def.ID, e.ID, e.Branch, e.From)
	}
	return fmt.Errorf("nexaflow: definition %q: edge %q: branch source node %q not found", def.ID, e.ID, e.From)
}

// checkAcyclic reports any cycle via iterative DFS colouring. The
// visit order is sorted for deterministic error messages.
func checkAcyclic(def *Definition, adj map[string][]string) error {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(adj))
	ids := make([]string, 0, len(def.Nodes))
	for i := range def.Nodes {
		ids = append(ids, def.Nodes[i].ID)
	}
	sort.Strings(ids)
	var dfs func(id string) error
	dfs = func(id string) error {
		color[id] = gray
		next := append([]string(nil), adj[id]...)
		sort.Strings(next)
		for _, n := range next {
			switch color[n] {
			case gray:
				return fmt.Errorf("nexaflow: definition %q: cycle detected through node %q (v1 has no loop-execution primitive)", def.ID, n)
			case white:
				if err := dfs(n); err != nil {
					return err
				}
			}
		}
		color[id] = black
		return nil
	}
	for _, id := range ids {
		if color[id] == white {
			if err := dfs(id); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSettings(def *Definition) error {
	s := def.Settings
	if s.RunTimeoutSeconds < 0 {
		return fmt.Errorf("nexaflow: definition %q: run_timeout_seconds %d is negative", def.ID, s.RunTimeoutSeconds)
	}
	if s.MaxConcurrentRuns < 0 {
		return fmt.Errorf("nexaflow: definition %q: max_concurrent_runs %d is negative", def.ID, s.MaxConcurrentRuns)
	}
	if s.RetentionDays != 0 && (s.RetentionDays < 7 || s.RetentionDays > 365) {
		return fmt.Errorf("nexaflow: definition %q: retention_days %d outside [7, 365] (0 means the 90-day default)", def.ID, s.RetentionDays)
	}
	return nil
}
