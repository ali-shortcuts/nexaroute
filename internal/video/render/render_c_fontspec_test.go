package render

import "testing"

// Agent C coverage: the fontconfig fallback in fontSpec. The system
// font candidates are a package variable so this test can empty them
// without mutating the host font tree.
func TestFontSpecFallsBackToFontconfig(t *testing.T) {
	old := fontCandidates
	fontCandidates = nil
	t.Cleanup(func() { fontCandidates = old })

	r := &Renderer{}
	spec, err := r.fontSpec()
	if err != nil {
		t.Fatalf("fontSpec: %v", err)
	}
	if spec != "font=DejaVu Sans" {
		t.Fatalf("fontSpec = %q, want the fontconfig fallback", spec)
	}
}
