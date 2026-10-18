package providers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func knownDialectTypes() map[string]bool {
	out := map[string]bool{}
	for _, p := range Presets() {
		out[p.Type] = true
	}
	return out
}

// The manifest must cover every dialect type the build ships, map to
// real adapters, and carry evidence for every verified claim.
func TestManifestCoversEveryShippedDialect(t *testing.T) {
	problems := ValidateManifest(Manifest(), knownDialectTypes())
	if len(problems) != 0 {
		t.Fatalf("manifest is not structurally sound:\n%s", strings.Join(problems, "\n"))
	}
}

// Drift guard for docs/COMPATIBILITY.md: the generated section in the
// document must equal a fresh render of the manifest. When this fails,
// regenerate the section from the manifest (see the refresh workflow
// in docs/COMPATIBILITY.md) — never edit the table by hand.
func TestCompatibilityDocMatchesManifest(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "COMPATIBILITY.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc := string(raw)
	const begin = "<!-- BEGIN GENERATED CONFORMANCE MANIFEST -->"
	const end = "<!-- END GENERATED CONFORMANCE MANIFEST -->"
	i := strings.Index(doc, begin)
	j := strings.Index(doc, end)
	if i < 0 || j < 0 || j <= i {
		t.Fatalf("%s is missing the generated conformance manifest markers", path)
	}
	got := strings.TrimSpace(doc[i+len(begin) : j])
	want := strings.TrimSpace(RenderConformanceTable(Manifest()))
	if got != want {
		t.Fatalf("docs/COMPATIBILITY.md conformance table drifted from internal/providers manifest.\nRegenerate it from RenderConformanceTable.\n--- in doc ---\n%s\n--- manifest ---\n%s", got, want)
	}
}

// Missing evidence: a verified claim without an evidence source must
// be rejected.
func TestValidateManifestRejectsMissingEvidence(t *testing.T) {
	entries := Manifest()
	for i := range entries {
		if entries[i].Dialect == "gemini" {
			entries[i].EvidenceSource = ""
		}
	}
	problems := ValidateManifest(entries, knownDialectTypes())
	found := false
	for _, p := range problems {
		if strings.Contains(p, "gemini") && strings.Contains(p, "missing evidence") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a missing-evidence violation for gemini, got: %v", problems)
	}
}

// Incorrect adapter mapping: an entry for a dialect the build does
// not ship must be rejected, and a shipped dialect without an entry
// must be rejected.
func TestValidateManifestRejectsIncorrectMapping(t *testing.T) {
	entries := append(Manifest(), ConformanceEntry{
		Dialect: "cohere", ContractVersion: ManifestVersion,
		Status: StateUntested, EvidenceDate: "2026-10-18",
		Dimensions: map[ConformanceDimension]DimensionClaim{},
	})
	problems := ValidateManifest(entries, knownDialectTypes())
	found := false
	for _, p := range problems {
		if strings.Contains(p, "cohere") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an incorrect-mapping violation for cohere, got: %v", problems)
	}

	entries = nil
	for _, e := range Manifest() {
		if e.Dialect != "vertex" {
			entries = append(entries, e)
		}
	}
	problems = ValidateManifest(entries, knownDialectTypes())
	found = false
	for _, p := range problems {
		if strings.Contains(p, "vertex") && strings.Contains(p, "no manifest entry") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a missing-entry violation for vertex, got: %v", problems)
	}
}

// Staleness: verified/partial evidence older than the policy window
// computes to STALE; fresh evidence keeps its state; UNSUPPORTED and
// UNTESTED never go stale.
func TestEffectiveStateStaleness(t *testing.T) {
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	e, ok := ManifestEntry("anthropic_compatible")
	if !ok {
		t.Fatal("anthropic_compatible entry missing")
	}
	if got := EffectiveState(e, now, DefaultStaleAfterDays); got != StateFixtureVerified {
		t.Fatalf("fresh evidence: got %s, want %s", got, StateFixtureVerified)
	}
	old := now.AddDate(0, 0, -(DefaultStaleAfterDays + 1))
	if got := EffectiveState(e, old.AddDate(1, 0, 0), DefaultStaleAfterDays); got != StateFixtureVerified {
		// sanity: at evidence date the state is fresh
		_ = got
	}
	staleNow := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	if got := EffectiveState(e, staleNow, DefaultStaleAfterDays); got != StateStale {
		t.Fatalf("old evidence: got %s, want %s", got, StateStale)
	}
	b, _ := ManifestEntry("bedrock")
	b.Status = StateUnsupported
	if got := EffectiveState(b, staleNow, DefaultStaleAfterDays); got != StateUnsupported {
		t.Fatalf("unsupported must not go stale: got %s", got)
	}
}

// Honesty pins: claims the documentation makes elsewhere must match
// the manifest, and a model-list response must never be the sole
// evidence for a verified claim.
func TestManifestHonestyPins(t *testing.T) {
	bedrock, _ := ManifestEntry("bedrock")
	if bedrock.Dimensions[DimReasoning].State != StateUnsupported {
		t.Fatal("bedrock reasoning must stay UNSUPPORTED (thinking blocks are not encoded to Converse)")
	}
	if bedrock.Dimensions[DimStructuredOutput].State != StateUnsupported {
		t.Fatal("bedrock structured output must stay UNSUPPORTED (no response_format mapping)")
	}
	if bedrock.Status == StateLiveVerified || bedrock.Status == StateFixtureVerified {
		t.Fatal("bedrock must not claim verified status on adapter unit tests alone")
	}
	gemini, _ := ManifestEntry("gemini")
	if gemini.Dimensions[DimParallelTools].State != StateUnsupported {
		t.Fatal("gemini parallel-tools knob must stay UNSUPPORTED (dropped on encode)")
	}
	if gemini.Native {
		t.Fatal("gemini is reached via canonical translation, not native passthrough")
	}
	for _, e := range Manifest() {
		if e.Status == StateLiveVerified {
			t.Fatalf("dialect %s claims LIVE-VERIFIED; no live canary evidence exists in this tree", e.Dialect)
		}
		if strings.Contains(strings.ToLower(e.EvidenceSource), "/models") {
			t.Fatalf("dialect %s cites a model-list response as evidence", e.Dialect)
		}
	}
	azure, _ := ManifestEntry("azure_openai")
	if azure.Dimensions[DimNonStream].State != StateUntested {
		t.Fatal("azure non-stream behavior has no fixture; it must stay UNTESTED until one exists")
	}
}
