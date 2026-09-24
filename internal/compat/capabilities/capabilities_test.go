package capabilities

import "testing"

func TestTriStateDefaultsUnknown(t *testing.T) {
	m := DefaultContract()
	for _, k := range AllKeys() {
		if got := m.Get(k).Value; got != Unknown {
			t.Fatalf("%s = %s, want unknown", k, got)
		}
	}
	if m.Get("nope").Value != Unknown {
		t.Fatalf("unknown key should return Unknown cell")
	}
}

func TestFromStaticBools(t *testing.T) {
	m := FromStaticBools(true, false, true, false)
	if m.Streaming.Value != Supported || m.Vision.Value != Supported {
		t.Fatalf("configured true must seed SUPPORTED: %+v", m)
	}
	// Configured false stays UNKNOWN so probing can verify real behavior.
	if m.Tools.Value != Unknown || m.Reasoning.Value != Unknown {
		t.Fatalf("configured false must seed UNKNOWN: %+v", m)
	}
}

func TestConservativeLearning(t *testing.T) {
	s := NewStore()
	id := "prov/model"
	// Low-confidence UNSUPPORTED must never be learned.
	s.Learn(id, "tools", Unsupported, SourceRuntime, ConfidenceLow, "random 500")
	if got, _ := s.Get(id); got.Get("tools").Value != Unknown {
		t.Fatalf("low-confidence must not teach UNSUPPORTED, got %s", got.Get("tools").Value)
	}
	// High-confidence runtime success updates UNKNOWN.
	s.Learn(id, "tools", Supported, SourceRuntime, ConfidenceHigh, "real tool call ok")
	if got, _ := s.Get(id); got.Get("tools").Value != Supported {
		t.Fatalf("high-confidence SUPPORTED not learned")
	}
	// Medium confidence must not flip an established value.
	s.Learn(id, "tools", Unsupported, SourceRuntime, ConfidenceMedium, "flaky")
	if got, _ := s.Get(id); got.Get("tools").Value != Supported {
		t.Fatalf("medium confidence flipped an established value")
	}
	// High confidence may flip.
	s.Learn(id, "tools", Unsupported, SourceRuntime, ConfidenceHigh, "verified 400")
	if got, _ := s.Get(id); got.Get("tools").Value != Unsupported {
		t.Fatalf("high confidence did not flip")
	}
}

func TestScorecard(t *testing.T) {
	m := DefaultContract()
	m.Set("text", Cell{Value: Supported})
	m.Set("streaming", Cell{Value: Supported})
	m.Set("tools", Cell{Value: Supported})
	sc := m.Score("nvidia/a", "healthy")
	if sc.Status != "CLAUDE_CODE_READY" {
		t.Fatalf("status = %s", sc.Status)
	}
	m.Set("tools", Cell{Value: Unsupported})
	sc = m.Score("nvidia/c", "healthy")
	if sc.Status != "CHAT_READY" || sc.FailureReason == "" {
		t.Fatalf("expected CHAT_READY with reason, got %+v", sc)
	}
}

func TestInvalidation(t *testing.T) {
	s := NewStore()
	s.Set("p/m", FromStaticBools(true, true, true, true))
	if s.InvalidateIfChanged("p/m", "http://a", "openai", "m", "v1") {
		t.Fatalf("first observation must not invalidate")
	}
	if !s.InvalidateIfChanged("p/m", "http://b", "openai", "m", "v1") {
		t.Fatalf("base URL change must invalidate")
	}
	if _, ok := s.Get("p/m"); ok {
		t.Fatalf("contract should be dropped after invalidation")
	}
}

func TestRetain(t *testing.T) {
	s := NewStore()
	s.Set("p/a", FromStaticBools(true, false, false, false))
	s.Set("p/b", FromStaticBools(true, false, false, false))
	s.Retain(map[string]struct{}{"p/a": {}})
	if _, ok := s.Get("p/b"); ok {
		t.Fatalf("stale deployment should be dropped")
	}
	if _, ok := s.Get("p/a"); !ok {
		t.Fatalf("valid deployment should be kept")
	}
}
