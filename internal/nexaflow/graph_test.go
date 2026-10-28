package nexaflow

import (
	"testing"
)

func validGraph() Graph {
	return Graph{
		Inputs: []string{"operations"},
		Nodes: []GraphNode{
			{ID: "t1", Type: "nexaroute.trigger.manual", Pos: Position{X: 10, Y: 10}},
			{ID: "x1", Type: "nexaroute.control.json_transform", Pos: Position{X: 200, Y: 10}},
			{ID: "c1", Type: "nexaroute.control.condition", Pos: Position{X: 400, Y: 10}, Config: map[string]any{"field": "a", "operator": "eq", "expected": "1"}},
		},
		Edges: []GraphEdge{
			{ID: "e1", From: "t1", FromPort: "payload", To: "x1", ToPort: "value"},
			{ID: "e2", From: "x1", FromPort: "value", To: "c1", ToPort: "value"},
		},
	}
}

func TestValidateGraphValid(t *testing.T) {
	reg := DefaultRegistry()
	if issues := ValidateGraph(reg, validGraph()); HasErrors(issues) {
		t.Fatalf("valid graph reported errors: %+v", issues)
	}
}

func TestValidateGraphCycle(t *testing.T) {
	reg := DefaultRegistry()
	g := validGraph()
	g.Edges = append(g.Edges, GraphEdge{ID: "e3", From: "c1", FromPort: "result", To: "x1", ToPort: "value"})
	// result is boolean, value is object: port names exist, so the only
	// error must be the cycle (type mismatch is a runtime concern).
	found := false
	for _, iss := range ValidateGraph(reg, g) {
		if iss.Code == "cycle" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected cycle error, got %+v", ValidateGraph(reg, g))
	}
}

func TestValidateGraphMissingInputAndUnknownPort(t *testing.T) {
	reg := DefaultRegistry()
	g := Graph{
		Nodes: []GraphNode{
			{ID: "t1", Type: "nexaroute.trigger.manual"},
			{ID: "m1", Type: "nexaroute.control.merge"},
		},
		Edges: []GraphEdge{
			{ID: "e1", From: "t1", FromPort: "payload", To: "m1", ToPort: "first"},
			{ID: "e2", From: "t1", FromPort: "nope", To: "m1", ToPort: "second"},
		},
	}
	issues := ValidateGraph(reg, g)
	var missing, unknown bool
	for _, iss := range issues {
		if iss.Code == "missing_input" {
			missing = true
		}
		if iss.Code == "unknown_port" {
			unknown = true
		}
	}
	if !missing || !unknown {
		t.Fatalf("expected missing_input and unknown_port, got %+v", issues)
	}
}

func TestValidateGraphNoTriggerAndBadConfig(t *testing.T) {
	reg := DefaultRegistry()
	g := Graph{
		Nodes: []GraphNode{
			{ID: "c1", Type: "nexaroute.control.condition", Config: map[string]any{"operator": "eq"}},
		},
	}
	issues := ValidateGraph(reg, g)
	var noTrigger, badConfig, missing bool
	for _, iss := range issues {
		switch iss.Code {
		case "no_trigger":
			noTrigger = true
		case "invalid_config":
			badConfig = true
		case "missing_input":
			missing = true
		}
	}
	if !noTrigger || !badConfig || !missing {
		t.Fatalf("expected no_trigger, invalid_config, missing_input, got %+v", issues)
	}
}

func TestCloneGraphIndependent(t *testing.T) {
	g := validGraph()
	c := CloneGraph(g)
	c.Nodes[2].Config["expected"] = "changed"
	if g.Nodes[2].Config["expected"] != "1" {
		t.Fatalf("clone mutation leaked into original: %v", g.Nodes[2].Config)
	}
}

func TestTopologicalOrderDeterministic(t *testing.T) {
	g := validGraph()
	order, err := TopologicalOrder(g)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[0] != "t1" || order[1] != "x1" || order[2] != "c1" {
		t.Fatalf("unexpected order %v", order)
	}
}
