package nexaflow

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFileStoreLegacyEmptyObject(t *testing.T) {
	for _, content := range []string{"{}", "{  }"} {
		path := filepath.Join(t.TempDir(), "store.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		s, err := OpenFileStore(path)
		if err != nil {
			t.Fatalf("legacy empty document %q: %v", content, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFileStoreRejectsMissingOrNullSections(t *testing.T) {
	for _, section := range []string{"drafts", "revisions", "runs", "run_by_idem", "nodes", "approvals"} {
		for _, mode := range []string{"missing", "null"} {
			t.Run(section+"/"+mode, func(t *testing.T) {
				sections := map[string]any{"drafts": map[string]any{}, "revisions": map[string]any{}, "runs": map[string]any{}, "run_by_idem": map[string]any{}, "nodes": map[string]any{}, "approvals": map[string]any{}}
				if mode == "missing" {
					delete(sections, section)
				} else {
					sections[section] = nil
				}
				raw, err := json.Marshal(sections)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "store.json")
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				store, err := OpenFileStore(path)
				if err == nil {
					_ = store.Close()
					t.Fatal("incomplete document accepted")
				}
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, raw) {
					t.Fatalf("document changed: %q %v", got, err)
				}
			})
		}
	}
}
