package jev

import (
	"encoding/json"
	"math"
	"math/rand"
	"testing"
	"time"
)

func TestJevResponseParser_Property_NeverPanic(t *testing.T) {
	// Property: parser never panics, never returns NaN/Inf, never outside opaque map
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	allowed := map[string]struct{}{"c0": {}, "c1": {}, "c2": {}}

	// Generate random payloads
	for i := 0; i < 1000; i++ {
		var payload []byte
		switch rng.Intn(6) {
		case 0:
			// Random garbage
			payload = []byte(randomString(rng, rng.Intn(200)))
		case 1:
			// Random JSON but not envelope
			m := map[string]interface{}{
				"random": randomString(rng, rng.Intn(100)),
				"num":    rng.Float64(),
			}
			b, _ := json.Marshal(m)
			payload = b
		case 2:
			// Envelope with random decision
			decision := randomString(rng, rng.Intn(20))
			conf := rng.Float64()*2 - 0.5
			envData := JevModelRouteData{Decision: decision, Confidence: &conf}
			env := JevEnvelope{
				Code:    rng.Intn(5),
				Message: randomString(rng, 20),
			}
			b, _ := json.Marshal(envData)
			env.Data = b
			bb, _ := json.Marshal(env)
			payload = bb
		case 3:
			// Valid structure but invalid confidence
			payload = []byte(`{"code":0,"message":"ok","data":{"decision":"c0","confidence":NaN}}`)
			if rng.Intn(2) == 0 {
				payload = []byte(`{"code":0,"message":"ok","data":{"decision":"c0","confidence":Infinity}}`)
			}
		case 4:
			// Valid with unknown candidate
			conf := 0.5
			envData := JevModelRouteData{Decision: "c999", Confidence: &conf}
			b, _ := json.Marshal(envData)
			env := JevEnvelope{Code: 0, Message: "ok", Data: b}
			bb, _ := json.Marshal(env)
			payload = bb
		case 5:
			// Valid with c0/c1
			choice := []string{"c0", "c1", "c2"}[rng.Intn(3)]
			conf2 := rng.Float64()
			envData2 := JevModelRouteData{Decision: choice, Confidence: &conf2}
			b2, _ := json.Marshal(envData2)
			env2 := JevEnvelope{Code: 0, Message: "ok", Data: b2}
			bb2, _ := json.Marshal(env2)
			payload = bb2
		}

		// Should never panic
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("parser panicked on payload %s: %v", string(payload), r)
				}
			}()
			result, err := ParseAndValidate(payload, allowed)
			if err == nil {
				// Check confidence not NaN/Inf
				if math.IsNaN(result.Confidence) || math.IsInf(result.Confidence, 0) {
					t.Fatalf("parser returned NaN/Inf confidence: %v", result.Confidence)
				}
				// Check selected ID in allowed
				if _, ok := allowed[result.SelectedID]; !ok {
					t.Fatalf("parser returned ID outside allowed map: %s", result.SelectedID)
				}
				// Confidence in [0,1]
				if result.Confidence < 0 || result.Confidence > 1 {
					t.Fatalf("confidence outside [0,1]: %f", result.Confidence)
				}
			}
		}()
	}
}

func TestJevResponseParser_Fuzz(t *testing.T) {
	// Fuzz-like: random bytes
	rng := rand.New(rand.NewSource(42))
	allowed := map[string]struct{}{"c0": {}, "c1": {}}
	for i := 0; i < 500; i++ {
		size := rng.Intn(500)
		b := make([]byte, size)
		rng.Read(b)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on random bytes: %v", r)
				}
			}()
			_, _ = ParseAndValidate(b, allowed)
		}()
	}
}

func TestJevResponseParser_NeverOutsideOpaqueMap(t *testing.T) {
	allowed := map[string]struct{}{"c0": {}, "c1": {}}
	tests := []string{
		`{"code":0,"message":"ok","data":{"decision":"c2"}}`,    // outside
		`{"code":0,"message":"ok","data":{"decision":"p1/m1"}}`, // physical ID
		`{"code":0,"message":"ok","data":{"decision":""}}`,
		`{"code":0,"message":"ok","data":{"decision":"c0","confidence":0.5}}`, // valid
	}
	for _, tc := range tests {
		result, err := ParseAndValidate([]byte(tc), allowed)
		if err == nil {
			if _, ok := allowed[result.SelectedID]; !ok {
				t.Fatalf("returned ID %s not in allowed", result.SelectedID)
			}
		}
	}
}

func TestJevResponseParser_NoNaNInf(t *testing.T) {
	allowed := map[string]struct{}{"c0": {}}
	// These should be rejected, not return NaN
	invalid := []string{
		`{"code":0,"message":"ok","data":{"decision":"c0","confidence":1.5}}`,
		`{"code":0,"message":"ok","data":{"decision":"c0","confidence":-0.1}}`,
	}
	for _, s := range invalid {
		_, err := ParseAndValidate([]byte(s), allowed)
		if err == nil {
			t.Fatalf("expected error for %s", s)
		}
	}
	// Valid 0 confidence
	valid := `{"code":0,"message":"ok","data":{"decision":"c0"}}`
	res, err := ParseAndValidate([]byte(valid), allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Confidence != 0 {
		t.Fatalf("expected 0 confidence for missing, got %f", res.Confidence)
	}
}

func randomString(rng *rand.Rand, n int) string {
	letters := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_{}:[]\"',. ")
	b := make([]rune, n)
	for i := range b {
		b[i] = letters[rng.Intn(len(letters))]
	}
	return string(b)
}
