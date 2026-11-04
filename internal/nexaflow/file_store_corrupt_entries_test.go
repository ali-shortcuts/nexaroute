package nexaflow

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Valid JSON can still contain missing records, and must not be interpreted as
// an empty/partial store or cause a panic on the next mutation.
func TestFileStoreRejectsNullRecordsWithoutModifyingFile(t *testing.T) {
	for _, tc := range []struct{ name, document string }{
		{"run", `{"drafts":{},"revisions":{},"runs":{"r":null},"run_by_idem":{},"nodes":{},"approvals":{}}`},
		{"node", `{"drafts":{},"revisions":{},"runs":{},"run_by_idem":{},"nodes":{"r":[null]},"approvals":{}}`},
		{"approval", `{"drafts":{},"revisions":{},"runs":{},"run_by_idem":{},"nodes":{},"approvals":{"r\u0000n":null}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.json")
			original := []byte(tc.document)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := OpenFileStore(path)
			if err == nil {
				s.Close()
				t.Fatal("corrupt record accepted")
			}
			if !strings.Contains(err.Error(), "null "+tc.name) {
				t.Fatalf("wrong corruption error: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("file modified: %q, %v", got, err)
			}
			// A failed open must release its advisory lock.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			s, err = OpenFileStore(path)
			if err != nil {
				t.Fatalf("lock retained after failure: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
