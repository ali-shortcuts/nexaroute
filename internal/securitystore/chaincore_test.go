package securitystore

import (
	"strings"
	"testing"
	"time"
)

var chainCoreNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func chainEvent(action string, ts time.Time) AuditEvent {
	return AuditEvent{Timestamp: ts, Actor: "alice", Action: action, Outcome: "success", Status: 200}
}

// applyPlan folds a plan into a state the way an adapter would persist it.
func applyPlan(st chainState, plan appendPlan) chainState {
	pruned := map[uint64]bool{}
	for _, seq := range plan.PrunedSeqs {
		pruned[seq] = true
	}
	out := chainState{Allocator: plan.Allocator, Head: plan.Head, HeadState: refValid}
	for _, r := range st.Records {
		if !pruned[r.Seq] {
			out.Records = append(out.Records, r)
		}
	}
	if !pruned[plan.Event.Seq] {
		out.Records = append(out.Records, plan.Event)
	}
	if plan.HasAnchor {
		out.Anchor = plan.Anchor
		out.AnchorState = refValid
	}
	return out
}

func growChain(t *testing.T, n int) chainState {
	t.Helper()
	st := chainState{}
	for i := 0; i < n; i++ {
		plan, err := planAuditAppend(st, chainEvent("contract.append", chainCoreNow.Add(time.Duration(i)*time.Second)), 1000, 365*24*time.Hour, chainCoreNow)
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		st = applyPlan(st, plan)
	}
	return st
}

func TestChainCoreGenesisAndLinkage(t *testing.T) {
	st := growChain(t, 3)
	report := verifyChainState(st)
	if !report.Verified || report.ChainedRecords != 3 || report.FirstSeq != 1 || report.LastSeq != 3 {
		t.Fatalf("report = %+v", report)
	}
	if st.Records[0].PrevHash != "" {
		t.Errorf("genesis record must have an empty prev hash, got %q", st.Records[0].PrevHash)
	}
	for i := 1; i < 3; i++ {
		if st.Records[i].PrevHash != st.Records[i-1].Hash {
			t.Errorf("record %d does not link to its predecessor", i)
		}
	}
	if st.Allocator != 3 || st.Head.Seq != 3 {
		t.Errorf("allocator/head = %d/%d, want 3/3", st.Allocator, st.Head.Seq)
	}
}

func TestChainCoreRejectsTampering(t *testing.T) {
	base := growChain(t, 3)
	cases := map[string]func(st *chainState){
		"record modified": func(st *chainState) { st.Records[1].Actor = "mallory" },
		"record deleted":  func(st *chainState) { st.Records = append(st.Records[:1], st.Records[2]) },
		"head rewound":    func(st *chainState) { st.Head = chainRef{Seq: 2, Hash: st.Records[1].Hash} },
		"head missing":    func(st *chainState) { st.HeadState = refAbsent },
		"head malformed":  func(st *chainState) { st.HeadState = refMalformed },
		"allocator lowered": func(st *chainState) {
			st.Allocator = 1
		},
		"anchor malformed": func(st *chainState) { st.AnchorState = refMalformed },
	}
	for name, mutate := range cases {
		st := chainState{
			Records:     append([]AuditEvent(nil), base.Records...),
			Head:        base.Head,
			HeadState:   refValid,
			AnchorState: refAbsent,
			Allocator:   base.Allocator,
		}
		mutate(&st)
		if report := verifyChainState(st); report.Verified {
			t.Errorf("%s: chain verified but must not", name)
		}
		if _, err := planAuditAppend(st, chainEvent("contract.more", chainCoreNow), 1000, 365*24*time.Hour, chainCoreNow); err == nil {
			t.Errorf("%s: append accepted on a broken chain", name)
		}
	}
}

func TestChainCorePruningSetsAnchorAndStaysVerifiable(t *testing.T) {
	st := chainState{}
	// maxAuditItems = 3: the 4th and 5th appends prune the prefix.
	for i := 0; i < 5; i++ {
		plan, err := planAuditAppend(st, chainEvent("contract.append", chainCoreNow.Add(time.Duration(i)*time.Second)), 3, 365*24*time.Hour, chainCoreNow)
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		st = applyPlan(st, plan)
	}
	if len(st.Records) != 3 || st.AnchorState != refValid || st.Anchor.Seq != 2 {
		t.Fatalf("records=%d anchor=%+v, want 3 records anchored at seq 2", len(st.Records), st.Anchor)
	}
	if st.Records[0].Seq != 3 || st.Records[0].PrevHash != st.Anchor.Hash {
		t.Fatalf("first survivor must link to the anchor: %+v vs anchor %+v", st.Records[0], st.Anchor)
	}
	if report := verifyChainState(st); !report.Verified {
		t.Fatalf("pruned chain must verify: %+v", report)
	}
}

func TestChainCoreAppendValidationMatchesBBoltGate(t *testing.T) {
	st := chainState{}
	if _, err := planAuditAppend(st, AuditEvent{Action: "x", Outcome: "success", Status: 200}, 1000, time.Hour, chainCoreNow); err == nil {
		t.Fatal("timestamp-less BUT actor-less event must fail (actor is required)")
	}
	if _, err := planAuditAppend(st, chainEvent("ok", time.Time{}), 1000, time.Hour, chainCoreNow); err != nil {
		t.Fatalf("zero timestamp is stamped, not rejected: %v", err)
	}
	if _, err := planAuditAppend(st, AuditEvent{Timestamp: chainCoreNow, Actor: "a", Action: "x", Outcome: "ok", Status: 700}, 1000, time.Hour, chainCoreNow); err == nil {
		t.Fatal("status > 599 must fail")
	}
	// Sanitization: control characters stripped like the bbolt path.
	plan, err := planAuditAppend(st, AuditEvent{Timestamp: chainCoreNow, Actor: "al\x01ice", Action: "auth.login", Outcome: "success", Status: 200}, 1000, time.Hour, chainCoreNow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.Event.Actor, "\x01") {
		t.Errorf("actor was not sanitized: %q", plan.Event.Actor)
	}
}
