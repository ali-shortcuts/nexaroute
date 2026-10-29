package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wave3ManifestTar builds a bundle tar from an explicit manifest and
// payload entries, so malformed-manifest cases can be crafted.
func wave3ManifestTar(t *testing.T, manifest backupManifest, payloads map[string][]byte) string {
	t.Helper()
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	entries := []struct {
		name string
		size int64
		body []byte
	}{{backupEntryManifest, int64(len(manifestBytes)), manifestBytes}}
	for name, body := range payloads {
		entries = append(entries, struct {
			name string
			size int64
			body []byte
		}{name, int64(len(body)), body})
	}
	return wave3CraftTar(t, entries)
}

func TestWave3ReadAndVerifyBundleManifestChecks(t *testing.T) {
	cfgBytes := []byte(`{"version":1}`)
	secBytes := []byte("security-bytes")
	goodManifest := func() backupManifest {
		return backupManifest{Format: backupFormat, Files: []backupManifestFile{
			{Name: backupEntryConfig, SHA256: sha256Hex(cfgBytes), Size: int64(len(cfgBytes))},
		}}
	}
	cases := []struct {
		name     string
		manifest backupManifest
		payloads map[string][]byte
		want     string
	}{
		{"no manifest", backupManifest{}, map[string][]byte{backupEntryConfig: cfgBytes}, "has no manifest"},
		{"manifest does not parse", backupManifest{}, map[string][]byte{backupEntryConfig: cfgBytes}, "does not parse"},
		{"unsupported format", backupManifest{Format: "bogus/1"}, map[string][]byte{backupEntryConfig: cfgBytes}, "unsupported backup format"},
		{"listed file missing", backupManifest{Format: backupFormat, Files: []backupManifestFile{
			{Name: backupEntryConfig, SHA256: sha256Hex(cfgBytes), Size: int64(len(cfgBytes))},
			{Name: backupEntrySecurity, SHA256: sha256Hex(secBytes), Size: int64(len(secBytes))},
		}}, map[string][]byte{backupEntryConfig: cfgBytes}, "missing the listed file"},
		{"hash mismatch", backupManifest{Format: backupFormat, Files: []backupManifestFile{
			{Name: backupEntryConfig, SHA256: strings.Repeat("0", 64), Size: int64(len(cfgBytes))},
		}}, map[string][]byte{backupEntryConfig: cfgBytes}, "fails its manifest hash/size check"},
		{"unlisted file", goodManifest(), map[string][]byte{backupEntryConfig: cfgBytes, backupEntrySecurity: secBytes}, "contains an unlisted file"},
		{"no config payload", backupManifest{Format: backupFormat, Files: []backupManifestFile{
			{Name: backupEntrySecurity, SHA256: sha256Hex(secBytes), Size: int64(len(secBytes))},
		}}, map[string][]byte{backupEntrySecurity: secBytes}, "has no config payload"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var bundle string
			if tc.name == "manifest does not parse" {
				bundle = wave3CraftTar(t, []struct {
					name string
					size int64
					body []byte
				}{
					{backupEntryManifest, 8, []byte("not json")},
					{backupEntryConfig, int64(len(cfgBytes)), cfgBytes},
				})
			} else if tc.name == "no manifest" {
				bundle = wave3CraftTar(t, []struct {
					name string
					size int64
					body []byte
				}{{backupEntryConfig, int64(len(cfgBytes)), cfgBytes}})
			} else {
				bundle = wave3ManifestTar(t, tc.manifest, tc.payloads)
			}
			_, _, _, err := readAndVerifyBundle(bundle)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestWave3AcceptanceEvidenceClass(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"https://example.com/v1", "external"},
		{"http://127.0.0.1:9/v1", "loopback"},
		{"http://localhost:8080/v1", "loopback"},
		{"://bad", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := acceptanceEvidenceClass(tc.raw); got != tc.want {
			t.Errorf("acceptanceEvidenceClass(%q)=%q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestWave3SanitizeAcceptanceURL(t *testing.T) {
	if got := sanitizeAcceptanceURL("https://example.com/v1/path"); got != "https://example.com/v1/path" {
		t.Fatalf("got=%q, want the URL unchanged", got)
	}
	for _, raw := range []string{"example.com", "://bad", ""} {
		if got := sanitizeAcceptanceURL(raw); got != "" {
			t.Fatalf("sanitizeAcceptanceURL(%q)=%q, want empty", raw, got)
		}
	}
}

func TestWave3CanonicalPathDotSegments(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, ".", "config.json"), filepath.Join(dir, "sub", "..", "config.json")} {
		got, err := canonicalPath(p)
		if err != nil {
			t.Fatal(err)
		}
		if got != target {
			t.Fatalf("canonicalPath(%q)=%q, want %q", p, got, target)
		}
	}
}

func TestWave3RunRestoreReportRequiresVerify(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runRestore([]string{"--report", "report.json"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--report requires --verify") {
		t.Fatalf("exit=%d, want 2 '--report requires --verify' (stderr=%s)", code, stderr.String())
	}
}

func TestWave3ScanRestoreJournalsCompletedExcluded(t *testing.T) {
	dir := filepath.Join(os.TempDir(), "nexaroute-restore-journal-wave3completed")
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJournalManifest(dir, &restoreJournalManifest{
		Format: restoreJournalFormat, ID: "abcdef0123456789", State: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	interrupted, _ := scanRestoreJournals()
	for _, d := range interrupted {
		if d == dir {
			t.Fatalf("completed journal %q must not be reported as interrupted", dir)
		}
	}
}

func TestWave3SHA256FileMissing(t *testing.T) {
	if _, _, err := sha256File(filepath.Join(t.TempDir(), "missing.bin")); err == nil {
		t.Fatal("sha256 of a missing file must fail")
	}
}
