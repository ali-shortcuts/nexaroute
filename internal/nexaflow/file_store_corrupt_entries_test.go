package nexaflow

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Valid JSON can still contain missing records, and must not be interpreted as
// an empty/partial store or cause a panic on the next mutation.
func TestFileStoreRejectsNullRecordsWithoutModifyingFile(t *testing.T) {
	for _, tc := range []struct{ name, document string }{
		{"run", `{"runs":{"r":null}}`},
		{"node", `{"nodes":{"r":[null]}}`},
		{"approval", `{"approvals":{"r\u0000n":null}}`},
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
