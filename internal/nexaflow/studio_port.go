package nexaflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Studio port: server-side template instantiation and secret-ref
// reporting for the standalone Visual Studio page. These are additive
// service helpers only: they use the existing Template.Instantiate
// inertness contract and the existing SaveDraft validation, and they
// touch no engine, store, or registry behaviour.

// InstantiateTemplate turns a starter template into a saved draft.
// The draft id is generated here (template id + random suffix) so two
// instantiations never collide; the template's own id is never stored
// as a draft id. The draft is always a draft: this method never
// publishes. Template edges reference registry node ids; they are
// mapped to the generated instance ids (base templates never repeat a
// node id, so the mapping is unambiguous). Trigger-category nodes get
// a trigger declaration whose kind is the inverse of the fixed v1
// triggerNodeID map; an unmapped trigger node fails closed.
func (s *Service) InstantiateTemplate(ctx context.Context, templateID string, placeholders map[string]string, policyApproved bool) (Definition, error) {
	templates := DefaultTemplates()
	var tpl *Template
	for i := range templates {
		if templates[i].ID == templateID {
			tpl = &templates[i]
			break
		}
	}
	if tpl == nil {
		return Definition{}, ErrNotFound
	}
	resolved, err := tpl.Instantiate(placeholders, policyApproved)
	if err != nil {
		return Definition{}, err
	}
	instanceByNode := make(map[string]string, len(resolved.Nodes))
	taken := make(map[string]bool, len(resolved.Nodes))
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion,
		Name:          resolved.Title,
		Description:   resolved.Description,
	}
	for _, node := range resolved.Nodes {
		base := node.NodeID
		if idx := strings.LastIndex(base, "."); idx >= 0 {
			base = base[idx+1:]
		}
		id := base
		for n := 2; taken[id]; n++ {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		taken[id] = true
		instanceByNode[node.NodeID] = id
		def.Nodes = append(def.Nodes, DefinitionNode{
			ID:      id,
			NodeID:  node.NodeID,
			Version: node.Version,
			Config:  node.Config,
		})
	}
	// Trigger declarations: bijection with trigger-category nodes is
	// enforced by ValidateDefinition, so every trigger node must get
	// exactly one declaration here.
	kindByNode := make(map[string]string, len(triggerNodeID))
	for kind, nodeID := range triggerNodeID {
		kindByNode[nodeID] = kind
	}
	hasTrigger := false
	for _, node := range def.Nodes {
		spec, err := s.Registry.Lookup(node.NodeID, node.Version)
		if err != nil {
			return Definition{}, err
		}
		if spec.Category == "trigger" {
			hasTrigger = true
			break
		}
	}
	// Entry-point completion (not a catalog change): four of the five
	// shipped templates carry no trigger node, so a draft built from
	// one would fail validation with "no triggers" and the gallery's
	// Use-template button would always error. A manual trigger is the
	// natural entry point (run input verbatim, no config): it is added
	// visibly as the first node, wired to every root node (nodes with
	// no incoming template edge), and the draft shows it like any
	// other node. Templates that already carry a trigger are
	// untouched.
	var completionEdges []DefinitionEdge
	if !hasTrigger {
		triggerID := "manual"
		for n := 2; taken[triggerID]; n++ {
			triggerID = fmt.Sprintf("manual-%d", n)
		}
		taken[triggerID] = true
		def.Nodes = append([]DefinitionNode{
			{ID: triggerID, NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
		}, def.Nodes...)
		hasIncoming := make(map[string]bool, len(resolved.Edges))
		for _, pair := range resolved.Edges {
			hasIncoming[instanceByNode[pair[1]]] = true
		}
		for _, node := range def.Nodes {
			if node.ID != triggerID && !hasIncoming[node.ID] {
				completionEdges = append(completionEdges, DefinitionEdge{From: triggerID, To: node.ID})
			}
		}
	}
	for _, node := range def.Nodes {
		spec, err := s.Registry.Lookup(node.NodeID, node.Version)
		if err != nil {
			return Definition{}, err
		}
		if spec.Category != "trigger" {
			continue
		}
		kind, ok := kindByNode[node.NodeID]
		if !ok {
			return Definition{}, fmt.Errorf("nexaflow: template %q: trigger node %q has no v1 trigger kind", templateID, node.NodeID)
		}
		def.Triggers = append(def.Triggers, DefinitionTrigger{
			Name:   node.ID,
			Kind:   kind,
			Config: node.Config,
		})
	}
	for i, pair := range resolved.Edges {
		from, ok := instanceByNode[pair[0]]
		if !ok {
			return Definition{}, fmt.Errorf("nexaflow: template %q: edge from unknown node %q", templateID, pair[0])
		}
		to, ok := instanceByNode[pair[1]]
		if !ok {
			return Definition{}, fmt.Errorf("nexaflow: template %q: edge to unknown node %q", templateID, pair[1])
		}
		def.Edges = append(def.Edges, DefinitionEdge{
			ID:   fmt.Sprintf("e%d", i+1),
			From: from,
			To:   to,
		})
	}
	for i, e := range completionEdges {
		e.ID = fmt.Sprintf("e%d", len(def.Edges)+i+1)
		def.Edges = append(def.Edges, e)
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return Definition{}, fmt.Errorf("nexaflow: instantiate template: %w", err)
	}
	def.ID = "tpl-" + templateID + "-" + hex.EncodeToString(suffix[:])
	if err := s.SaveDraft(ctx, def); err != nil {
		return Definition{}, err
	}
	return def, nil
}

// DefinitionSecretRefs returns the sorted, deduplicated secret
// references (never secret values) declared in a definition: the
// config values of secretRef-typed ConfigSchema fields, on nodes and
// on trigger declarations. Unknown node specs are skipped; validation
// of the definition itself is the caller's job.
func DefinitionSecretRefs(def *Definition, reg *Registry) []string {
	if def == nil || reg == nil {
		return nil
	}
	seen := make(map[string]bool)
	var refs []string
	add := func(spec *NodeSpec, cfg map[string]any) {
		if spec == nil {
			return
		}
		for _, f := range spec.ConfigSchema {
			if f.Type != "secretRef" {
				continue
			}
			v, ok := cfg[f.Name].(string)
			if !ok || v == "" {
				continue
			}
			if !seen[v] {
				seen[v] = true
				refs = append(refs, v)
			}
		}
	}
	nodeByID := make(map[string]*DefinitionNode, len(def.Nodes))
	for i := range def.Nodes {
		n := &def.Nodes[i]
		nodeByID[n.ID] = n
		spec, err := reg.Lookup(n.NodeID, n.Version)
		if err != nil {
			continue
		}
		add(spec, n.Config)
	}
	for _, tr := range def.Triggers {
		node, ok := nodeByID[tr.Name]
		if !ok {
			continue
		}
		spec, err := reg.Lookup(node.NodeID, node.Version)
		if err != nil {
			continue
		}
		add(spec, tr.Config)
	}
	sort.Strings(refs)
	return refs
}
