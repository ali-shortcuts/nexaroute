package securitystore

// Wave C-1 tamper-evidence property suite. For a populated chain, every
// corruption below must fail verification AND fail the next append
// closed, on all three backends (bbolt real; PG/Redis via the fakes):
// flipping any byte in any record (exhaustive over positions for a
// small chain, sampled for a large one), swapping two records,
// deleting a middle record, truncating the tail, partial snapshot
// replay (records without meta and vice versa), corrupting the anchor
// or head, lowering the allocator, and the zero-value/wrong-type
// metadata matrix (#362 contracts).

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// waveCTamperBackend exposes one Backend plus backend-specific
// corruption hooks. Corruption simulates an attacker (or decay) with
// write access below the adapter.
type waveCTamperBackend struct {
	name         string
	backend      Backend
	corrupt      func(t *testing.T, seq uint64, mutate func(*AuditEvent))
	deleteSeq    func(t *testing.T, seq uint64)
	swapSeqs     func(t *testing.T, a, b uint64)
	putHead      func(t *testing.T, raw string) // "" deletes the key
	putAnchor    func(t *testing.T, raw string)
	setAllocator func(t *testing.T, v uint64)
	snapshot     func(t *testing.T) any
	restoreRecs  func(t *testing.T, snap any)
	restoreMeta  func(t *testing.T, snap any)
}

var waveCBase = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func waveCPopulate(t *testing.T, b Backend, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if err := b.AppendAudit(ctx, AuditEvent{
			Timestamp: waveCBase.Add(time.Duration(i) * time.Second),
			Actor:     "alice", Action: fmt.Sprintf("op-%d", i),
			Outcome: "success", Status: 200,
		}); err != nil {
			t.Fatalf("populate %d: %v", i, err)
		}
	}
	report, err := b.VerifyAuditChain(ctx)
	if err != nil || !report.Verified {
		t.Fatalf("populated chain must verify: err=%v report=%+v", err, report)
	}
}

// assertChainBroken requires fail-closed behavior after corruption:
// verification must not pass (an error counts as fail-closed) and the
// next append must be refused.
func assertChainBroken(t *testing.T, b Backend, what string) {
	t.Helper()
	ctx := context.Background()
	report, err := b.VerifyAuditChain(ctx)
	if err == nil && report.Verified {
		t.Fatalf("%s: corrupted chain verified: %+v", what, report)
	}
	if err := b.AppendAudit(ctx, AuditEvent{Actor: "alice", Action: "after-corruption", Outcome: "success", Status: 200}); err == nil {
		t.Fatalf("%s: append to a corrupted chain must fail", what)
	}
}

func waveCBoltTamper(t *testing.T, maxItems int) waveCTamperBackend {
	t.Helper()
	store, err := Open(contractStorePath(t, "tamper.db"), maxItems, 365)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bySeq := func(t *testing.T, seq uint64) (key, raw []byte) {
		t.Helper()
		var k, v []byte
		if err := store.db.View(func(tx *bolt.Tx) error {
			c := tx.Bucket(bucketAudit).Cursor()
			for kk, vv := c.First(); kk != nil; kk, vv = c.Next() {
				var e AuditEvent
				if jerr := json.Unmarshal(vv, &e); jerr != nil {
					continue
				}
				s := e.Seq
				if e.Hash == "" {
					s = auditKeySeq(kk)
				}
				if s == seq {
					k = append([]byte(nil), kk...)
					v = append([]byte(nil), vv...)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if k == nil {
			t.Fatalf("bbolt record seq %d not found", seq)
		}
		return k, v
	}
	return waveCTamperBackend{
		name:    "bbolt",
		backend: store,
		corrupt: func(t *testing.T, seq uint64, mutate func(*AuditEvent)) {
			t.Helper()
			k, raw := bySeq(t, seq)
			var e AuditEvent
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatal(err)
			}
			mutate(&e)
			enc, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketAudit).Put(k, enc) }); err != nil {
				t.Fatal(err)
			}
		},
		deleteSeq: func(t *testing.T, seq uint64) {
			t.Helper()
			k, _ := bySeq(t, seq)
			if err := store.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketAudit).Delete(k) }); err != nil {
				t.Fatal(err)
			}
		},
		swapSeqs: func(t *testing.T, a, b uint64) {
			t.Helper()
			ka, ra := bySeq(t, a)
			kb, rb := bySeq(t, b)
			if err := store.db.Update(func(tx *bolt.Tx) error {
				bkt := tx.Bucket(bucketAudit)
				if err := bkt.Put(ka, rb); err != nil {
					return err
				}
				return bkt.Put(kb, ra)
			}); err != nil {
				t.Fatal(err)
			}
		},
		putHead: func(t *testing.T, raw string) {
			t.Helper()
			if err := store.db.Update(func(tx *bolt.Tx) error {
				if raw == "" {
					return tx.Bucket(bucketMeta).Delete(keyChainHead)
				}
				return tx.Bucket(bucketMeta).Put(keyChainHead, []byte(raw))
			}); err != nil {
				t.Fatal(err)
			}
		},
		putAnchor: func(t *testing.T, raw string) {
			t.Helper()
			if err := store.db.Update(func(tx *bolt.Tx) error {
				if raw == "" {
					return tx.Bucket(bucketMeta).Delete(keyChainAnchor)
				}
				return tx.Bucket(bucketMeta).Put(keyChainAnchor, []byte(raw))
			}); err != nil {
				t.Fatal(err)
			}
		},
		setAllocator: func(t *testing.T, v uint64) {
			t.Helper()
			if err := store.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketMeta).SetSequence(v) }); err != nil {
				t.Fatal(err)
			}
		},
		snapshot: func(t *testing.T) any { t.Helper(); return snapshotStore(t, store) },
		restoreRecs: func(t *testing.T, snap any) {
			t.Helper()
			saved := snap.(map[string][]byte)
			if err := store.db.Update(func(tx *bolt.Tx) error {
				bkt := tx.Bucket(bucketAudit)
				c := bkt.Cursor()
				for k, _ := c.First(); k != nil; k, _ = c.Next() {
					if err := c.Delete(); err != nil {
						return err
					}
				}
				for k, v := range saved {
					if !strings.HasPrefix(k, string(bucketAudit)+"/") {
						continue
					}
					if err := bkt.Put([]byte(k[len(bucketAudit)+1:]), v); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		},
		restoreMeta: func(t *testing.T, snap any) {
			t.Helper()
			saved := snap.(map[string][]byte)
			if err := store.db.Update(func(tx *bolt.Tx) error {
				bkt := tx.Bucket(bucketMeta)
				if err := bkt.Delete(keyChainHead); err != nil {
					return err
				}
				if err := bkt.Delete(keyChainAnchor); err != nil {
					return err
				}
				for k, v := range saved {
					if !strings.HasPrefix(k, string(bucketMeta)+"/") {
						continue
					}
					if err := bkt.Put([]byte(k[len(bucketMeta)+1:]), v); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		},
	}
}

func waveCRedisTamper(t *testing.T, maxItems int) waveCTamperBackend {
	t.Helper()
	client := newFakeRedisClient()
	store := newRedisStoreWithClient(client, maxItems, 365*24*time.Hour)
	lock := func() func() { client.mu.Lock(); return client.mu.Unlock }
	return waveCTamperBackend{
		name:    "redis-fake",
		backend: store,
		corrupt: func(t *testing.T, seq uint64, mutate func(*AuditEvent)) {
			t.Helper()
			defer lock()()
			field := strconv.FormatUint(seq, 10)
			raw, ok := client.hashes[securityRedisAuditKey][field]
			if !ok {
				t.Fatalf("redis record %d missing", seq)
			}
			var e AuditEvent
			if err := json.Unmarshal([]byte(raw), &e); err != nil {
				t.Fatal(err)
			}
			mutate(&e)
			enc, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			client.hash(securityRedisAuditKey)[field] = string(enc)
		},
		deleteSeq: func(t *testing.T, seq uint64) {
			t.Helper()
			defer lock()()
			delete(client.hash(securityRedisAuditKey), strconv.FormatUint(seq, 10))
		},
		swapSeqs: func(t *testing.T, a, b uint64) {
			t.Helper()
			defer lock()()
			h := client.hash(securityRedisAuditKey)
			fa, fb := strconv.FormatUint(a, 10), strconv.FormatUint(b, 10)
			h[fa], h[fb] = h[fb], h[fa]
		},
		putHead: func(t *testing.T, raw string) {
			t.Helper()
			defer lock()()
			if raw == "" {
				delete(client.hash(securityRedisMetaKey), "head")
			} else {
				client.hash(securityRedisMetaKey)["head"] = raw
			}
		},
		putAnchor: func(t *testing.T, raw string) {
			t.Helper()
			defer lock()()
			if raw == "" {
				delete(client.hash(securityRedisMetaKey), "anchor")
			} else {
				client.hash(securityRedisMetaKey)["anchor"] = raw
			}
		},
		setAllocator: func(t *testing.T, v uint64) {
			t.Helper()
			defer lock()()
			client.hash(securityRedisMetaKey)["allocator"] = strconv.FormatUint(v, 10)
		},
		snapshot: func(t *testing.T) any {
			t.Helper()
			defer lock()()
			out := map[string]map[string]string{}
			for hk, h := range client.hashes {
				cp := map[string]string{}
				for k, v := range h {
					cp[k] = v
				}
				out[hk] = cp
			}
			return out
		},
		restoreRecs: func(t *testing.T, snap any) {
			t.Helper()
			defer lock()()
			client.hashes[securityRedisAuditKey] = snap.(map[string]map[string]string)[securityRedisAuditKey]
		},
		restoreMeta: func(t *testing.T, snap any) {
			t.Helper()
			defer lock()()
			client.hashes[securityRedisMetaKey] = snap.(map[string]map[string]string)[securityRedisMetaKey]
		},
	}
}

func waveCPGTamper(t *testing.T, maxItems int) waveCTamperBackend {
	t.Helper()
	db := newFakePGDB()
	store := openFakePostgresOn(t, db, maxItems, 365*24*time.Hour)
	return waveCTamperBackend{
		name:    "postgres-fake",
		backend: store,
		corrupt: func(t *testing.T, seq uint64, mutate func(*AuditEvent)) {
			t.Helper()
			fakePGCorruptAudit(t, db, int64(seq), mutate)
		},
		deleteSeq: func(t *testing.T, seq uint64) {
			t.Helper()
			db.mu.Lock()
			defer db.mu.Unlock()
			delete(db.audit, int64(seq))
		},
		swapSeqs: func(t *testing.T, a, b uint64) {
			t.Helper()
			db.mu.Lock()
			defer db.mu.Unlock()
			db.audit[int64(a)], db.audit[int64(b)] = db.audit[int64(b)], db.audit[int64(a)]
		},
		putHead: func(t *testing.T, raw string) {
			t.Helper()
			db.mu.Lock()
			defer db.mu.Unlock()
			putFakePGMeta(db, metaKeyHead, raw)
		},
		putAnchor: func(t *testing.T, raw string) {
			t.Helper()
			db.mu.Lock()
			defer db.mu.Unlock()
			putFakePGMeta(db, metaKeyAnchor, raw)
		},
		setAllocator: func(t *testing.T, v uint64) {
			t.Helper()
			db.mu.Lock()
			defer db.mu.Unlock()
			db.meta[metaKeyAllocator] = []byte(strconv.FormatUint(v, 10))
		},
		snapshot: func(t *testing.T) any {
			t.Helper()
			db.mu.Lock()
			defer db.mu.Unlock()
			return map[string]any{"audit": cloneAuditMap(db.audit), "meta": cloneBytesMap(db.meta)}
		},
		restoreRecs: func(t *testing.T, snap any) {
			t.Helper()
			db.mu.Lock()
			defer db.mu.Unlock()
			db.audit = cloneAuditMap(snap.(map[string]any)["audit"].(map[int64][]byte))
		},
		restoreMeta: func(t *testing.T, snap any) {
			t.Helper()
			db.mu.Lock()
			defer db.mu.Unlock()
			db.meta = cloneBytesMap(snap.(map[string]any)["meta"].(map[string][]byte))
		},
	}
}

func putFakePGMeta(db *fakePGDB, key, raw string) {
	if raw == "" {
		delete(db.meta, key)
	} else {
		db.meta[key] = []byte(raw)
	}
}

// waveCTamperCtors builds one fresh tamper rig per backend with the
// given count bound.
func waveCTamperCtors(maxItems int) map[string]func(t *testing.T) waveCTamperBackend {
	return map[string]func(t *testing.T) waveCTamperBackend{
		"bbolt":         func(t *testing.T) waveCTamperBackend { return waveCBoltTamper(t, maxItems) },
		"redis-fake":    func(t *testing.T) waveCTamperBackend { return waveCRedisTamper(t, maxItems) },
		"postgres-fake": func(t *testing.T) waveCTamperBackend { return waveCPGTamper(t, maxItems) },
	}
}

// waveCTamperBackends builds one fresh tamper rig per backend.
func waveCTamperBackends(t *testing.T) []waveCTamperBackend {
	t.Helper()
	return []waveCTamperBackend{waveCBoltTamper(t, 1000), waveCRedisTamper(t, 1000), waveCPGTamper(t, 1000)}
}

// TestWaveCTamperRecordFlipExhaustive flips one record's payload at
// every position of a small chain: each must break verification and
// refuse the next append.
func TestWaveCTamperRecordFlipExhaustive(t *testing.T) {
	for _, tb := range waveCTamperBackends(t) {
		tb := tb
		for pos := uint64(1); pos <= 5; pos++ {
			t.Run(fmt.Sprintf("%s/pos-%d", tb.name, pos), func(t *testing.T) {
				fresh := waveCTamperCtors(1000)[tb.name](t)
				waveCPopulate(t, fresh.backend, 5)
				fresh.corrupt(t, pos, func(e *AuditEvent) { e.Actor = "mallory" })
				assertChainBroken(t, fresh.backend, fmt.Sprintf("%s flip record %d", tb.name, pos))
			})
		}
	}
}

// TestWaveCTamperStructural covers swaps, deletions, truncation and
// partial snapshot replays.
func TestWaveCTamperStructural(t *testing.T) {
	mk := waveCTamperCtors(1000)
	cases := []struct {
		name  string
		apply func(t *testing.T, tb waveCTamperBackend)
	}{
		{"swap-adjacent", func(t *testing.T, tb waveCTamperBackend) { tb.swapSeqs(t, 2, 3) }},
		{"swap-ends", func(t *testing.T, tb waveCTamperBackend) { tb.swapSeqs(t, 1, 5) }},
		{"delete-middle", func(t *testing.T, tb waveCTamperBackend) { tb.deleteSeq(t, 3) }},
		{"delete-first", func(t *testing.T, tb waveCTamperBackend) { tb.deleteSeq(t, 1) }},
		{"truncate-tail", func(t *testing.T, tb waveCTamperBackend) { tb.deleteSeq(t, 5) }},
		{"truncate-two", func(t *testing.T, tb waveCTamperBackend) { tb.deleteSeq(t, 5); tb.deleteSeq(t, 4) }},
		{"prevhash-rewritten", func(t *testing.T, tb waveCTamperBackend) {
			tb.corrupt(t, 3, func(e *AuditEvent) { e.PrevHash = strings.Repeat("f", 64) })
		}},
		{"seq-renumbered", func(t *testing.T, tb waveCTamperBackend) {
			tb.corrupt(t, 4, func(e *AuditEvent) { e.Seq = 2 })
		}},
		{"replay-records-without-meta", func(t *testing.T, tb waveCTamperBackend) {
			waveCPopulate(t, tb.backend, 3)
			snap := tb.snapshot(t)
			waveCPopulate(t, tb.backend, 2)
			tb.restoreRecs(t, snap)
		}},
		{"replay-meta-without-records", func(t *testing.T, tb waveCTamperBackend) {
			waveCPopulate(t, tb.backend, 3)
			snap := tb.snapshot(t)
			waveCPopulate(t, tb.backend, 2)
			tb.restoreMeta(t, snap)
		}},
		{"allocator-lowered", func(t *testing.T, tb waveCTamperBackend) { tb.setAllocator(t, 1) }},
		{"allocator-zeroed", func(t *testing.T, tb waveCTamperBackend) { tb.setAllocator(t, 0) }},
		{"head-wrong-seq", func(t *testing.T, tb waveCTamperBackend) {
			report, err := tb.backend.VerifyAuditChain(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			tb.putHead(t, fmt.Sprintf(`{"seq":99,"hash":%q}`, report.HeadHash))
		}},
		{"head-wrong-hash", func(t *testing.T, tb waveCTamperBackend) {
			tb.putHead(t, `{"seq":5,"hash":"0000000000000000000000000000000000000000000000000000000000000000"}`)
		}},
		{"head-missing", func(t *testing.T, tb waveCTamperBackend) { tb.putHead(t, "") }},
	}
	for name, ctor := range mk {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				tb := ctor(t)
				if strings.HasPrefix(tc.name, "replay-") {
					tc.apply(t, tb) // replay cases populate internally
				} else {
					waveCPopulate(t, tb.backend, 5)
					tc.apply(t, tb)
				}
				assertChainBroken(t, tb.backend, name+"/"+tc.name)
			})
		}
	}
}

// TestWaveCTamperAnchor exhausts anchor corruptions on a pruned chain
// (where the anchor is load-bearing, not decorative).
func TestWaveCTamperAnchor(t *testing.T) {
	mk := waveCTamperCtors(100)
	// 130 appends capped at 100 force a pruning anchor on every backend.
	setup := func(t *testing.T, ctor func(t *testing.T) waveCTamperBackend) waveCTamperBackend {
		t.Helper()
		tb := ctor(t)
		waveCPopulate(t, tb.backend, 130)
		report, err := tb.backend.VerifyAuditChain(context.Background())
		if err != nil || !report.Verified || !report.HasAnchor {
			t.Fatalf("pruned setup must verify with anchor: err=%v %+v", err, report)
		}
		return tb
	}
	cases := []struct {
		name  string
		apply func(t *testing.T, tb waveCTamperBackend)
	}{
		{"anchor-wrong-hash", func(t *testing.T, tb waveCTamperBackend) {
			tb.putAnchor(t, `{"seq":30,"hash":"0000000000000000000000000000000000000000000000000000000000000000"}`)
		}},
		{"anchor-seq-mismatch", func(t *testing.T, tb waveCTamperBackend) {
			report, err := tb.backend.VerifyAuditChain(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			records, err := tb.backend.AuditRecords(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			_ = records
			tb.putAnchor(t, fmt.Sprintf(`{"seq":%d,"hash":%q}`, report.AnchorSeq+50, "0000000000000000000000000000000000000000000000000000000000000000"))
		}},
		{"anchor-null", func(t *testing.T, tb waveCTamperBackend) { tb.putAnchor(t, "null") }},
		{"anchor-empty-object", func(t *testing.T, tb waveCTamperBackend) { tb.putAnchor(t, "{}") }},
		{"anchor-malformed", func(t *testing.T, tb waveCTamperBackend) { tb.putAnchor(t, "{") }},
		{"anchor-missing", func(t *testing.T, tb waveCTamperBackend) { tb.putAnchor(t, "") }},
		{"first-survivor-unlinked", func(t *testing.T, tb waveCTamperBackend) {
			report, err := tb.backend.VerifyAuditChain(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			tb.corrupt(t, report.AnchorSeq+1, func(e *AuditEvent) { e.PrevHash = strings.Repeat("b", 64) })
		}},
	}
	for name, ctor := range mk {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				tb := setup(t, ctor)
				tc.apply(t, tb)
				assertChainBroken(t, tb.backend, name+"/"+tc.name)
			})
		}
	}
}

// TestWaveCMalformedRefMatrix mirrors the #362 contracts on every
// backend: zero-value and wrong-type chain refs are malformed — never
// valid, never absent — so verification fails and appends refuse.
func TestWaveCMalformedRefMatrix(t *testing.T) {
	mk := waveCTamperCtors(1000)
	raws := []string{
		"null", "{}", `{"seq":0,"hash":""}`, `{"seq":0,"hash":"abc"}`,
		`{"seq":3,"hash":""}`, "[1,2]", `"str"`, "42", "{",
		`{"seq":"3","hash":"abc"}`,
	}
	for name, ctor := range mk {
		for _, raw := range raws {
			for _, where := range []string{"head", "anchor"} {
				t.Run(fmt.Sprintf("%s/%s/%q", name, where, raw), func(t *testing.T) {
					tb := ctor(t)
					waveCPopulate(t, tb.backend, 3)
					if where == "head" {
						tb.putHead(t, raw)
					} else {
						tb.putAnchor(t, raw)
					}
					assertChainBroken(t, tb.backend, fmt.Sprintf("%s %s=%s", name, where, raw))
				})
			}
		}
	}
}

// TestWaveCTamperLargeChainSampled flips records at sampled positions
// of a 60-record chain on every backend.
func TestWaveCTamperLargeChainSampled(t *testing.T) {
	for _, tb := range waveCTamperBackends(t) {
		for _, pos := range []uint64{1, 17, 30, 59, 60} {
			t.Run(fmt.Sprintf("%s/pos-%d", tb.name, pos), func(t *testing.T) {
				mk := waveCTamperCtors(1000)[tb.name]
				fresh := mk(t)
				waveCPopulate(t, fresh.backend, 60)
				fresh.corrupt(t, pos, func(e *AuditEvent) { e.Action = "forged" })
				assertChainBroken(t, fresh.backend, fmt.Sprintf("%s large flip %d", tb.name, pos))
			})
		}
	}
}
