package decision

import (
	"context"
	"testing"
)

type stubProvider struct{ id, typ string }

func (s stubProvider) ID() string   { return s.id }
func (s stubProvider) Type() string { return s.typ }
func (s stubProvider) Capabilities() Capabilities {
	return Capabilities{}
}
func (s stubProvider) Health() Health { return Health{} }
func (s stubProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	return DecisionResult{Action: ActionAbstain}, nil
}

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubProvider{id: "jev-main", typ: "jev"}); err != nil {
		t.Fatal(err)
	}
	p, ok := r.Get("jev-main")
	if !ok || p.Type() != "jev" {
		t.Fatalf("get=%v ok=%v", p, ok)
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("missing ID must not resolve")
	}
}

func TestRegistryRejectsDuplicates(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubProvider{id: "a", typ: "jev"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(stubProvider{id: "a", typ: "jev"}); err == nil {
		t.Fatal("duplicate ID must be rejected")
	}
}

func TestRegistryRejectsEmptyAndNil(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubProvider{}); err == nil {
		t.Fatal("empty ID must be rejected")
	}
	if err := r.Register(nil); err == nil {
		t.Fatal("nil provider must be rejected")
	}
}

func TestRegistryAllSorted(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(stubProvider{id: "z", typ: "jev"})
	_ = r.Register(stubProvider{id: "a", typ: "local"})
	all := r.All()
	if len(all) != 2 || all[0].ID() != "a" || all[1].ID() != "z" {
		t.Fatalf("all=%v", all)
	}
}
