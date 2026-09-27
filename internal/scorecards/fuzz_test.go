package scorecards

import (
	"encoding/json"
	"testing"
	"time"
)

// FuzzImportJSON asserts the import path is safe under adversarial artifacts: no
// panics, and every accepted scorecard has provenance-complete values.
func FuzzImportJSON(f *testing.F) {
	f.Add(`{"scorecards":[{"deployment_id":"p/m","version":1,"generated_at":"2026-09-26T12:00:00Z","values":{"coding":{"score":0.5,"provenance":"imported","sample_count":3,"source":"s"}}}]}`)
	f.Add(`{"scorecards":[]}`)
	f.Add(`{"scorecards":[{"deployment_id":"p/m","version":1,"values":{}}]}`)
	f.Add(`not json`)
	f.Fuzz(func(t *testing.T, doc string) {
		if len(doc) > 1<<20 {
			t.Skip("bounded by the importer's own size limit")
		}
		list, err := ImportJSON([]byte(doc), "fuzz.json", 8)
		if err != nil {
			return
		}
		if len(list) == 0 || len(list) > 8 {
			t.Fatalf("imported %d scorecards, want 1..8", len(list))
		}
		for _, sc := range list {
			if err := sc.Validate(); err != nil {
				t.Fatalf("import accepted an invalid scorecard: %v", err)
			}
			if len(sc.Values) == 0 {
				t.Fatalf("import accepted a scorecard without values")
			}
			for d, v := range sc.Values {
				if !v.Provenance.Valid() {
					t.Fatalf("dimension %s has no provenance", d)
				}
				if v.Provenance.Measured() && v.SampleCount < 1 {
					t.Fatalf("dimension %s claims measured provenance without samples", d)
				}
			}
		}
	})
}

// FuzzValueValidation asserts the validator never panics and never accepts a
// fabricated (provenance-less) value, whatever the numbers are.
func FuzzValueValidation(f *testing.F) {
	f.Add(0.5, 4, "evaluation", "1")
	f.Add(1.5, 0, "", "")
	f.Add(0.0, -3, "vibes", "")
	f.Fuzz(func(t *testing.T, score float64, samples int, provenance, suiteVersion string) {
		v := Value{
			Score:        score,
			Provenance:   Provenance(provenance),
			SampleCount:  samples,
			Confidence:   0.5,
			EvaluatedAt:  time.Unix(0, 0).UTC(),
			SuiteVersion: suiteVersion,
			Source:       "fuzz",
			Unit:         DimCoding.Unit(),
		}
		err := ValidateValue(DimCoding, v)
		if err == nil {
			if !v.Provenance.Valid() {
				t.Fatalf("accepted a value with provenance %q", provenance)
			}
			if v.Score < 0 || v.Score > 1 {
				t.Fatalf("accepted a score outside [0,1]: %v", v.Score)
			}
			if v.Provenance.Measured() && v.SampleCount < 1 {
				t.Fatalf("accepted measured value without samples: %+v", v)
			}
		}
		// A value must always survive JSON round-tripping (used by state files).
		if _, err := json.Marshal(v); err != nil {
			t.Fatalf("value not serializable: %v", err)
		}
	})
}
