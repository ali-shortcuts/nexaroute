package core

import "testing"

func FuzzParseAnthContent(f *testing.F) {
	f.Add([]byte(`"hello"`))
	f.Add([]byte(`[{"type":"text","text":"hi"}]`))
	f.Add([]byte(`[{"type":"tool_result","tool_use_id":"x","content":"ok"}]`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = ParseAnthContent(raw)
	})
}
