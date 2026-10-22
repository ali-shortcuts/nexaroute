package cache

// Wave C-2 (#403) supervisor-takeover fuzz targets for the response cache.
//
// FuzzCacheOps: randomized store/lookup/invalidate/reset sequences on the
// exact-match cache — the entry and byte bounds must never be exceeded,
// expired entries must never be served, and a hit must return exactly the
// body of the most recent store for that key.
//
// FuzzSemanticProvenance: randomized stores/lookups on the semantic cache —
// a hit must be in the lookup scope (isolation), at or above the
// similarity threshold, not expired, and when requireNoTraining is set the
// provenance gate (NoTraining && TrainsOnData=="no") must hold.
//
// FuzzCacheKey: key derivation is deterministic and well-formed.

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

const (
	fuzzCacheTTL     = time.Hour
	fuzzCacheMaxEnt  = 8
	fuzzCacheMaxByte = 1024
	fuzzCacheOpLen   = 8
)

// op layout: [op:1][key:1][bodyLen:1][ageSec:2][pad:3]
func FuzzCacheOps(f *testing.F) {
	f.Add([]byte{
		0, 1, 8, 0, 0, 0, 0, 0, // store k1
		1, 1, 0, 0, 0, 0, 0, 0, // lookup k1 -> hit
		0, 2, 4, 0x10, 0x0e, 0, 0, 0, // store k2 expired (age 3600+16)
		1, 2, 0, 0, 0, 0, 0, 0, // lookup k2 -> miss (expired)
	})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 32<<10 {
			t.Skip("input too large for a bounded fuzz iteration")
		}
		c := New(fuzzCacheTTL, fuzzCacheMaxEnt, fuzzCacheMaxByte)
		type stored struct {
			body []byte
			at   time.Time
		}
		shadow := map[string]stored{}
		now := time.Now()
		keyOf := func(b byte) string { return string([]byte{'k', '0' + b%12}) }
		for off := 0; off+fuzzCacheOpLen <= len(ops); off += fuzzCacheOpLen {
			o := ops[off : off+fuzzCacheOpLen]
			op, kb, bodyLen := o[0]%4, keyOf(o[1]), int(o[2])
			age := time.Duration(binary.BigEndian.Uint16(o[3:5])) * time.Second
			key := kb
			switch op {
			case 0: // Store
				if bodyLen == 0 {
					bodyLen = 1
				}
				body := bytes.Repeat([]byte{o[0], o[1], byte(off)}, bodyLen)
				at := now.Add(-age)
				c.Store(key, Entry{Body: body, ContentType: "application/json", Status: 200, CreatedAt: at})
				shadow[key] = stored{body: body, at: at}
			case 1: // Lookup
				got, ok := c.Lookup(key)
				s, wasStored := shadow[key]
				if !wasStored {
					if ok {
						t.Fatalf("phantom hit for never-stored key %q", key)
					}
					continue
				}
				expired := now.Sub(s.at) > fuzzCacheTTL+10*time.Second
				live := now.Sub(s.at) <= fuzzCacheTTL
				switch {
				case expired && ok:
					t.Fatalf("expired entry served for %q (age %v)", key, now.Sub(s.at))
				case live && ok:
					if !bytes.Equal(got.Body, s.body) {
						t.Fatalf("hit body mismatch for %q", key)
					}
					if time.Since(got.CreatedAt) > fuzzCacheTTL+10*time.Second {
						t.Fatalf("served entry older than TTL for %q", key)
					}
				}
				// !live && !expired is the TTL boundary band: either
				// outcome is acceptable (clock granularity).
			case 2: // Invalidate
				c.Invalidate()
				shadow = map[string]stored{}
			default: // Reset
				c.Reset()
				shadow = map[string]stored{}
			}
			st := c.Stats()
			if st.Entries > fuzzCacheMaxEnt {
				t.Fatalf("entry bound exceeded: %d > %d", st.Entries, fuzzCacheMaxEnt)
			}
			if st.BytesStored > fuzzCacheMaxByte {
				t.Fatalf("byte bound exceeded: %d > %d", st.BytesStored, fuzzCacheMaxByte)
			}
			if st.Entries < 0 || st.BytesStored < 0 {
				t.Fatalf("negative occupancy: %+v", st)
			}
		}
	})
}

// semOp layout: [op:1][scope:1][vec:4][flags:1][ageSec:2] (10 bytes)
func FuzzSemanticProvenance(f *testing.F) {
	f.Add([]byte{
		0, 0, 10, 20, 30, 40, 0x02, 0, 0, 0, // store s0 no_training, trains=no
		1, 0, 10, 20, 30, 40, 0x02, 0, 0, 0, // lookup s0 requireNoTraining -> hit
		1, 1, 10, 20, 30, 40, 0x02, 0, 0, 0, // lookup s1 -> miss (scope isolation)
	})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 32<<10 {
			t.Skip("input too large for a bounded fuzz iteration")
		}
		c := NewSemantic(fuzzCacheTTL, 0.9, 16, 8192)
		now := time.Now()
		scopeOf := func(b byte) string { return string([]byte{'s', '0' + b%3}) }
		vecOf := func(b []byte) []float32 {
			return []float32{float32(b[0]) / 255, float32(b[1]) / 255, float32(b[2]) / 255, float32(b[3]) / 255}
		}
		trainsOf := func(flags byte) string {
			return []string{"yes", "no", "unknown"}[(flags>>2)%3]
		}
		for off := 0; off+10 <= len(ops); off += 10 {
			o := ops[off : off+10]
			op := o[0] % 2
			scope := scopeOf(o[1])
			vec := vecOf(o[2:6])
			flags := o[6]
			age := time.Duration(binary.BigEndian.Uint16(o[7:9])) * time.Second
			if op == 0 {
				body := []byte{scope[0], scope[1], o[2], o[3]}
				c.Store(SemanticEntry{
					Body:         body,
					ContentType:  "application/json",
					Status:       200,
					CreatedAt:    now.Add(-age),
					Vector:       vec,
					Scope:        scope,
					NoTraining:   flags&1 != 0,
					TrainsOnData: trainsOf(flags),
					Deployment:   "d1",
				})
				continue
			}
			requireNoTraining := flags&2 != 0
			e, score, ok := c.Lookup(scope, vec, requireNoTraining)
			if !ok {
				continue
			}
			if e.Scope != scope {
				t.Fatalf("cross-scope hit: stored scope %q served for %q", e.Scope, scope)
			}
			if score < 0.9-1e-9 {
				t.Fatalf("below-threshold hit: score=%v", score)
			}
			if time.Since(e.CreatedAt) > fuzzCacheTTL+10*time.Second {
				t.Fatalf("expired semantic entry served (scope %q)", scope)
			}
			if requireNoTraining && (!e.NoTraining || e.TrainsOnData != "no") {
				t.Fatalf("provenance gate bypass: NoTraining=%v TrainsOnData=%q served with requireNoTraining",
					e.NoTraining, e.TrainsOnData)
			}
			st := c.Stats()
			if st.Entries > 16 || st.BytesStored > 8192 {
				t.Fatalf("semantic bound exceeded: %+v", st)
			}
		}
	})
}

func FuzzCacheKey(f *testing.F) {
	f.Add("/v1/chat/completions", []byte(`{"model":"m"}`), []byte("dep-a"))
	f.Add("/v1/responses", []byte(`{"model":"m"}`), []byte{})
	f.Fuzz(func(t *testing.T, path string, body []byte, ctx []byte) {
		if len(path) > 4096 || len(body) > 1<<20 || len(ctx) > 1<<20 {
			t.Skip("input too large")
		}
		k1 := Key(path, body)
		if k1 != Key(path, body) {
			t.Fatal("Key is nondeterministic")
		}
		if len(k1) != 64 {
			t.Fatalf("Key length = %d, want 64 hex chars", len(k1))
		}
		for _, c := range k1 {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Fatalf("Key is not lowercase hex: %q", k1)
			}
		}
		if KeyWithContext(path, body, nil) != k1 {
			t.Fatal("KeyWithContext with nil ctx must equal Key")
		}
		kc := KeyWithContext(path, body, ctx)
		if kc != KeyWithContext(path, body, ctx) || len(kc) != 64 {
			t.Fatal("KeyWithContext is nondeterministic or malformed")
		}
		if len(ctx) > 0 && kc == k1 {
			t.Fatal("context is not isolated in the derived key")
		}
	})
}
