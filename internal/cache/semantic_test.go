package cache

import (
	"testing"
	"time"
)

func semEntry(scope string, vec []float32, body string) SemanticEntry {
	return SemanticEntry{
		Body: []byte(body), ContentType: "application/json", Status: 200,
		CreatedAt: time.Now(), Vector: vec, Scope: scope,
		Deployment: "p/m", TrainsOnData: "no", Embedding: "e/m",
	}
}

func TestCosineSimilarityBasics(t *testing.T) {
	if got := CosineSimilarity([]float32{1, 0}, []float32{1, 0}); got != 1 {
		t.Fatalf("identical: %v", got)
	}
	if got := CosineSimilarity([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Fatalf("orthogonal: %v", got)
	}
	if got := CosineSimilarity([]float32{1, 0}, []float32{1}); got != 0 {
		t.Fatalf("dim mismatch must be 0: %v", got)
	}
}

func TestSemanticLookupScopeThresholdAndProvenance(t *testing.T) {
	c := NewSemantic(time.Minute, 0.9, 8, 1<<20)
	c.Store(semEntry("scope-a", []float32{1, 0.1}, "resp-a"))
	other := semEntry("scope-b", []float32{1, 0.1}, "resp-b")
	c.Store(other)

	// Same scope, near-identical vector -> hit.
	e, score, ok := c.Lookup("scope-a", []float32{1, 0.12}, false)
	if !ok || string(e.Body) != "resp-a" || score < 0.9 {
		t.Fatalf("hit expected, ok=%v score=%v body=%s", ok, score, e.Body)
	}
	// Different scope -> miss even with an identical vector.
	if _, _, ok := c.Lookup("scope-c", []float32{1, 0.1}, false); ok {
		t.Fatal("cross-scope hit")
	}
	// Below threshold -> miss.
	if _, _, ok := c.Lookup("scope-a", []float32{0, 1}, false); ok {
		t.Fatal("below-threshold hit")
	}
	// Provenance gate: entry stored WITHOUT no_training must not
	// serve a no_training request even in-scope and identical.
	plain := semEntry("scope-a", []float32{0.5, 0.5}, "resp-plain")
	plain.NoTraining = false
	plain.TrainsOnData = "no"
	c.Store(plain)
	if _, _, ok := c.Lookup("scope-a", []float32{0.5, 0.5}, true); ok {
		t.Fatal("no_training request served by a non-no_training entry")
	}
	// A proper no_training entry with a "no" origin serves.
	priv := semEntry("scope-a", []float32{0.5, 0.5}, "resp-priv")
	priv.NoTraining = true
	priv.TrainsOnData = "no"
	c.Store(priv)
	e, _, ok = c.Lookup("scope-a", []float32{0.5, 0.5}, true)
	if !ok || string(e.Body) != "resp-priv" {
		t.Fatalf("no_training hit expected, ok=%v body=%s", ok, e.Body)
	}
	// ...but an entry whose origin trains on data never serves it.
	bad := semEntry("scope-a", []float32{0.2, 0.9}, "resp-bad")
	bad.NoTraining = true
	bad.TrainsOnData = "yes"
	c.Store(bad)
	if _, _, ok := c.Lookup("scope-a", []float32{0.2, 0.9}, true); ok {
		t.Fatal("trains_on_data=yes origin served a no_training request")
	}
}

func TestSemanticBoundsAndTTL(t *testing.T) {
	c := NewSemantic(50*time.Millisecond, 0.5, 2, 1<<20)
	c.Store(semEntry("s", []float32{1, 0}, "one"))
	c.Store(semEntry("s", []float32{0.9, 0.1}, "two"))
	c.Store(semEntry("s", []float32{0.8, 0.2}, "three"))
	if got := c.Stats().Entries; got != 2 {
		t.Fatalf("maxEntries bound violated: %d", got)
	}
	time.Sleep(70 * time.Millisecond)
	if _, _, ok := c.Lookup("s", []float32{1, 0}, false); ok {
		t.Fatal("expired entry served")
	}
	c.Store(semEntry("s", []float32{1, 0}, "x"))
	c.Invalidate()
	if got := c.Stats().Entries; got != 0 {
		t.Fatalf("invalidate left %d entries", got)
	}
}
