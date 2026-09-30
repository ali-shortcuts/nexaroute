// Package doccheck guards repository documentation contracts with
// deterministic regression tests. See issue #178 (audit finding F15).
package doccheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from this test file until go.mod is found.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test working directory")
		}
		dir = parent
	}
}

// TestKnownGapsRecordsTop15AuditBoundaries is the deterministic regression
// test for issue #178 (audit finding F15): docs/KNOWN_GAPS.md must keep the
// top-15 audit record with status labels, mitigations, and owner references.
// No secrets, credentials, or internal URLs may appear in the record.
func TestKnownGapsRecordsTop15AuditBoundaries(t *testing.T) {
	path := filepath.Join(repoRoot(t), "docs", "KNOWN_GAPS.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read KNOWN_GAPS.md: %v", err)
	}
	doc := string(raw)

	// Required audit-record anchors: one per mandated area.
	anchors := []string{
		"Audit follow-up record",
		"http.ErrAbortHandler",
		"internal_panic",
		"ValidateProviderConfig",
		"NEXAROUTE_STRESS",
		"NEXAROUTE_SOAK",
		"scripts/stress.sh",
		"cliSnippet",
		"internal/compat",
		"internal/core",
		"cmd/gateway",
		"intentional boundary",
	}
	for _, a := range anchors {
		if !strings.Contains(doc, a) {
			t.Errorf("docs/KNOWN_GAPS.md missing required audit-record anchor %q", a)
		}
	}

	// Every mandated status label must be used at least once.
	for _, label := range []string{"`open`", "`intentional boundary`"} {
		if !strings.Contains(doc, label) {
			t.Errorf("docs/KNOWN_GAPS.md missing status label %s", label)
		}
	}

	// Owner references: parent audit, this issue, the credential/SSRF owner,
	// and each of the nine audit follow-ups.
	refs := []string{"#57", "#178", "#125", "F3", "F4", "F11", "F1", "F2", "F7"}
	for _, r := range refs {
		if !strings.Contains(doc, r) {
			t.Errorf("docs/KNOWN_GAPS.md missing owner reference %q", r)
		}
	}
	for _, n := range []string{"#169", "#170", "#171", "#172", "#173", "#174", "#175", "#176", "#177"} {
		if !strings.Contains(doc, n) {
			t.Errorf("docs/KNOWN_GAPS.md missing audit follow-up reference %q", n)
		}
	}
}
