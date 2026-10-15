package securitystore

import "testing"

// Regression (audit 2026-10-15): on the Postgres/Redis backends, a
// stored chain ref that merely parsed as JSON was classified
// refValid — including null, {} and {"seq":0,"hash":""}. The bbolt
// backend classifies those as malformed. With zeroed head/anchor
// metadata and zero records, chain verification reported VERIFIED
// for an "emptied" log and the next append silently forked the
// chain from seq 1 with an empty PrevHash: a tamper-evidence
// bypass reachable by anyone (or any bug) that can write the
// metadata rows. No legitimate write path ever stores a zero ref.
func TestParseChainRefMetaRejectsZeroRefs(t *testing.T) {
	for _, raw := range []string{"null", "{}", `{"seq":0,"hash":""}`, `{"seq":0,"hash":"abc"}`, `{"seq":3,"hash":""}`} {
		if _, state := parseChainRefMeta([]byte(raw)); state != refMalformed {
			t.Errorf("parseChainRefMeta(%s) state = %v, want refMalformed", raw, state)
		}
	}
	if _, state := parseChainRefMeta(nil); state != refAbsent {
		t.Errorf("absent ref state = %v, want refAbsent", state)
	}
	ref, state := parseChainRefMeta([]byte(`{"seq":7,"hash":"deadbeef"}`))
	if state != refValid || ref.Seq != 7 || ref.Hash != "deadbeef" {
		t.Errorf("valid ref = (%+v, %v), want ({7 deadbeef}, refValid)", ref, state)
	}
	if _, state := parseChainRefMeta([]byte(`{not json`)); state != refMalformed {
		t.Errorf("unparseable ref state = %v, want refMalformed", state)
	}
}
