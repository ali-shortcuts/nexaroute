package jev

import (
	"math"
	"testing"
)

// FuzzParseResponse feeds arbitrary bytes to the Jev response parser. It must
// never panic, never return a physical ID outside the supplied opaque map,
// and never produce NaN/Inf confidence.
func FuzzParseResponse(f *testing.F) {
	allowed := map[string]string{"c0": "p1/model-a", "c1": "p2/model-b"}
	seeds := []string{
		``,
		`{`,
		`{"code":0,"message":"ok","data":{"decision":"c1","confidence":0.86}}`,
		`{"code":1,"message":"err"}`,
		`{"code":0,"data":{"decision":"c9"}}`,
		`{"code":0,"data":{"decision":"c0","confidence":2}}`,
		`null`,
		`[]`,
		`"c0"`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 70*1024 {
			t.Skip("response limit bounds real inputs")
		}
		physical, conf, err := ParseResponse(body, allowed)
		if math.IsNaN(conf) || math.IsInf(conf, 0) {
			t.Fatalf("non-finite confidence %v", conf)
		}
		if err != nil {
			if physical != "" {
				t.Fatalf("error with physical=%q", physical)
			}
			return
		}
		if conf < 0 || conf > 1 {
			t.Fatalf("confidence %v out of range", conf)
		}
		if physical != "p1/model-a" && physical != "p2/model-b" {
			t.Fatalf("physical=%q outside mapping", physical)
		}
	})
}
