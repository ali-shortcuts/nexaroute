package httpapi

import (
	"encoding/json"
	"testing"
)

func FuzzPatchJSONModel(f *testing.F) {
	f.Add([]byte(`{"model":"old","messages":[]}`), "new")
	f.Add([]byte(`{}`), "m")
	f.Fuzz(func(t *testing.T, raw []byte, model string) {
		out, err := patchJSONModel(raw, model)
		if err != nil {
			return
		}
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("patch returned invalid json: %v", err)
		}
		if got, _ := obj["model"].(string); got != model {
			t.Fatalf("model=%q want %q", got, model)
		}
	})
}
