package securitystore

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// M3-6 audit hash-chain tests. Tamper cases rewrite the bbolt
// records directly, simulating an attacker (or corruption) with
// write access to the database file but no ability to recompute a
// consistent chain.

func chainTestStore(t *testing.T, maxItems int) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "store.db")
	store, err := Open(path, maxItems, 3650)
	if err != nil {
		t.Fatal(err)
	}
	return store, path
}

func appendChainEvents(t *testing.T, store *Store, n int, base time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		err := store.AppendAudit(context.Background(), AuditEvent{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Actor:     "alice", Action: fmt.Sprintf("action-%d", i), Outcome: "success", Status: 200,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func verifyOK(t *testing.T, store *Store) ChainReport {
	t.Helper()
	report, err := store.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified {
		t.Fatalf("chain should verify, problems: %v", report.Problems)
	}
	return report
}

func rewriteRecord(t *testing.T, store *Store, pick int, mutate func(*AuditEvent)) {
	t.Helper()
	err := store.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketAudit)
		c := bucket.Cursor()
		idx := 0
		for k, raw := c.First(); k != nil; k, raw = c.Next() {
			if idx == pick {
				var event AuditEvent
				if err := json.Unmarshal(raw, &event); err != nil {
					return err
				}
				mutate(&event)
				encoded, err := json.Marshal(event)
				if err != nil {
					return err
				}
				keyCopy := make([]byte, len(k))
				copy(keyCopy, k)
				return bucket.Put(keyCopy, encoded)
			}
			idx++
		}
		return fmt.Errorf("record %d not found", pick)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func deleteRecord(t *testing.T, store *Store, pick int) {
	t.Helper()
	err := store.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketAudit)
		c := bucket.Cursor()
		idx := 0
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			if idx == pick {
				return c.Delete()
			}
			idx++
		}
		return fmt.Errorf("record %d not found", pick)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuditChainBuildsAndVerifiesAcrossRestart(t *testing.T) {
	store, path := chainTestStore(t, 100)
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	appendChainEvents(t, store, 5, base)
	report := verifyOK(t, store)
	if report.TotalRecords != 5 || report.ChainedRecords != 5 || report.LegacyRecords != 0 {
		t.Fatalf("report: %+v", report)
	}
	if report.HeadHash == "" || report.LastSeq != 5 {
		t.Fatalf("head: %+v", report)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, 100, 3650)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	verifyOK(t, reopened)
	appendChainEvents(t, reopened, 1, base.Add(10*time.Second))
	report = verifyOK(t, reopened)
	if report.ChainedRecords != 6 || report.LastSeq != 6 {
		t.Fatalf("after restart append: %+v", report)
	}
}

func TestAuditChainDetectsModifiedRecord(t *testing.T) {
	store, _ := chainTestStore(t, 100)
	defer store.Close()
	appendChainEvents(t, store, 3, time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	rewriteRecord(t, store, 1, func(e *AuditEvent) { e.Actor = "mallory" })
	report, err := store.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified || !strings.Contains(strings.Join(report.Problems, " "), "modified") {
		t.Fatalf("modification not detected: %+v", report)
	}
}

func TestAuditChainDetectsDeletedMiddleRecord(t *testing.T) {
	store, _ := chainTestStore(t, 100)
	defer store.Close()
	appendChainEvents(t, store, 4, time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	deleteRecord(t, store, 1)
	report, err := store.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified || !strings.Contains(strings.Join(report.Problems, " "), "does not link") {
		t.Fatalf("deletion not detected: %+v", report)
	}
}

func TestAuditChainDetectsTailTruncation(t *testing.T) {
	store, _ := chainTestStore(t, 100)
	defer store.Close()
	appendChainEvents(t, store, 3, time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	deleteRecord(t, store, 2)
	report, err := store.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified || !strings.Contains(strings.Join(report.Problems, " "), "head metadata") {
		t.Fatalf("tail truncation not detected: %+v", report)
	}
}

func TestAuditChainDetectsSequenceTampering(t *testing.T) {
	store, _ := chainTestStore(t, 100)
	defer store.Close()
	appendChainEvents(t, store, 3, time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	rewriteRecord(t, store, 2, func(e *AuditEvent) { e.Seq = 2 }) // duplicate of record 2's seq
	report, err := store.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified {
		t.Fatalf("sequence tampering not detected: %+v", report)
	}
}

func TestAuditChainPruningByAgeKeepsVerifiableAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "store.db")
	store, err := Open(path, 100, 1) // 1-day retention
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	old := time.Now().UTC().Add(-72 * time.Hour)
	appendChainEvents(t, store, 3, old)
	// A fresh append prunes the three aged records in its transaction.
	appendChainEvents(t, store, 1, time.Now().UTC())
	report := verifyOK(t, store)
	if report.TotalRecords != 1 || !report.HasAnchor || report.AnchorSeq != 3 {
		t.Fatalf("anchor report: %+v", report)
	}
	// Deleting the sole survivor (head truncation past the anchor)
	// breaks the head/anchor invariant and must be detected.
	deleteRecord(t, store, 0)
	report, err = store.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified {
		t.Fatalf("post-anchor truncation not detected: %+v", report)
	}
}

func TestAuditChainPruningByCountKeepsVerifiableAnchor(t *testing.T) {
	store, _ := chainTestStore(t, 100)
	defer store.Close()
	base := time.Now().UTC().Add(-time.Hour)
	appendChainEvents(t, store, 130, base)
	report := verifyOK(t, store)
	if report.TotalRecords != 100 || !report.HasAnchor {
		t.Fatalf("count pruning report: %+v", report)
	}
	if report.FirstSeq != report.AnchorSeq+1 {
		t.Fatalf("first surviving seq %d should follow anchor seq %d", report.FirstSeq, report.AnchorSeq)
	}
}

func TestAuditChainLegacySegmentIsReportedNotVerified(t *testing.T) {
	store, path := chainTestStore(t, 100)
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	// Plant pre-chain records exactly as the old format stored them:
	// no seq/prev_hash/hash fields (omitted via omitempty zero values).
	err := store.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		audit := tx.Bucket(bucketAudit)
		for i := 0; i < 3; i++ {
			seq, err := meta.NextSequence()
			if err != nil {
				return err
			}
			event := AuditEvent{Timestamp: base.Add(time.Duration(i) * time.Second), Actor: "legacy", Action: "old", Outcome: "success", Status: 200}
			encoded, err := json.Marshal(event)
			if err != nil {
				return err
			}
			k := make([]byte, 16)
			binary.BigEndian.PutUint64(k[:8], uint64(event.Timestamp.UnixNano()))
			binary.BigEndian.PutUint64(k[8:], seq)
			if err := audit.Put(k, encoded); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, 100, 3650)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	appendChainEvents(t, reopened, 2, base.Add(10*time.Second))
	report := verifyOK(t, reopened)
	if report.LegacyRecords != 3 || report.ChainedRecords != 2 || report.TotalRecords != 5 {
		t.Fatalf("legacy report: %+v", report)
	}
}

func TestAuditChainEmptyStoreAndReadOnlyVerify(t *testing.T) {
	store, path := chainTestStore(t, 100)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ro.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || report.TotalRecords != 0 {
		t.Fatalf("empty store report: %+v", report)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("OpenReadOnly on a missing file should fail")
	}
}

func TestAuditChainConcurrentAppendsStayLinear(t *testing.T) {
	store, _ := chainTestStore(t, 1000)
	defer store.Close()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				err := store.AppendAudit(context.Background(), AuditEvent{
					Timestamp: time.Now().UTC(), Actor: fmt.Sprintf("worker-%d", g),
					Action: "work", Outcome: "success", Status: 200,
				})
				if err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	report := verifyOK(t, store)
	if report.ChainedRecords != 80 {
		t.Fatalf("concurrent chain: %+v", report)
	}
}

// --- PR #301 review fixes ---

func TestAuditChainVerifyFailsClosedOnMissingBuckets(t *testing.T) {
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)

	t.Run("audit bucket deleted", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		appendChainEvents(t, store, 3, base)
		if err := store.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket(bucketAudit) }); err != nil {
			t.Fatal(err)
		}
		report, err := store.VerifyAuditChain(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if report.Verified || !strings.Contains(strings.Join(report.Problems, " "), "audit bucket") {
			t.Fatalf("missing audit bucket must fail closed: %+v", report)
		}
	})

	t.Run("meta bucket deleted", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		appendChainEvents(t, store, 3, base)
		if err := store.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket(bucketMeta) }); err != nil {
			t.Fatal(err)
		}
		report, err := store.VerifyAuditChain(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if report.Verified || !strings.Contains(strings.Join(report.Problems, " "), "meta bucket") {
			t.Fatalf("missing meta bucket must fail closed: %+v", report)
		}
	})

	t.Run("tail truncation with meta bucket deleted", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		appendChainEvents(t, store, 3, base)
		deleteRecord(t, store, 2) // tail truncation
		if err := store.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket(bucketMeta) }); err != nil {
			t.Fatal(err)
		}
		report, err := store.VerifyAuditChain(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if report.Verified {
			t.Fatalf("truncated chain with missing meta must never verify: %+v", report)
		}
	})
}

func TestAuditChainBootstrapUsesHighestSequenceNotKeyOrder(t *testing.T) {
	store, path := chainTestStore(t, 100)
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	// Legacy records whose timestamps run OPPOSITE to their
	// sequences: key order (timestamp-major) is seq 3, 2, 1, so a
	// key-order Last() would wrongly pick seq 1 as the predecessor.
	err := store.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		audit := tx.Bucket(bucketAudit)
		for i := 0; i < 3; i++ {
			seq, err := meta.NextSequence()
			if err != nil {
				return err
			}
			event := AuditEvent{
				Timestamp: base.Add(time.Duration(30-10*i) * time.Second), // seq1:+30s seq2:+20s seq3:+10s
				Actor:     "legacy", Action: "old", Outcome: "success", Status: 200,
			}
			encoded, err := json.Marshal(event)
			if err != nil {
				return err
			}
			k := make([]byte, 16)
			binary.BigEndian.PutUint64(k[:8], uint64(event.Timestamp.UnixNano()))
			binary.BigEndian.PutUint64(k[8:], seq)
			if err := audit.Put(k, encoded); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, 100, 3650)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	appendChainEvents(t, reopened, 1, base.Add(40*time.Second))
	report := verifyOK(t, reopened)
	if report.LegacyRecords != 3 || report.ChainedRecords != 1 {
		t.Fatalf("bootstrap report: %+v", report)
	}
}

func moveRecordKey(t *testing.T, store *Store, pick int, newKey func(old []byte) []byte) {
	t.Helper()
	err := store.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketAudit)
		c := bucket.Cursor()
		idx := 0
		for k, raw := c.First(); k != nil; k, raw = c.Next() {
			if idx == pick {
				oldCopy := make([]byte, len(k))
				copy(oldCopy, k)
				rawCopy := make([]byte, len(raw))
				copy(rawCopy, raw)
				if err := bucket.Put(newKey(oldCopy), rawCopy); err != nil {
					return err
				}
				return bucket.Delete(oldCopy)
			}
			idx++
		}
		return fmt.Errorf("record %d not found", pick)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuditChainDetectsKeyTampering(t *testing.T) {
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)

	t.Run("key sequence modified", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		appendChainEvents(t, store, 3, base)
		moveRecordKey(t, store, 1, func(old []byte) []byte {
			k := make([]byte, 16)
			copy(k, old)
			binary.BigEndian.PutUint64(k[8:], binary.BigEndian.Uint64(old[8:])+100)
			return k
		})
		report, err := store.VerifyAuditChain(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if report.Verified || !strings.Contains(strings.Join(report.Problems, " "), "storage key sequence") {
			t.Fatalf("key sequence tampering not detected: %+v", report)
		}
	})

	t.Run("key timestamp modified", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		appendChainEvents(t, store, 3, base)
		moveRecordKey(t, store, 1, func(old []byte) []byte {
			k := make([]byte, 16)
			copy(k, old)
			binary.BigEndian.PutUint64(k[:8], binary.BigEndian.Uint64(old[:8])+uint64(time.Hour.Nanoseconds()))
			return k
		})
		report, err := store.VerifyAuditChain(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if report.Verified || !strings.Contains(strings.Join(report.Problems, " "), "storage key timestamp") {
			t.Fatalf("key timestamp tampering not detected: %+v", report)
		}
	})
}

// --- PR #301 review round 3: fail-closed chain-head recovery ---

func snapshotStore(t *testing.T, store *Store) map[string][]byte {
	t.Helper()
	snap := map[string][]byte{}
	err := store.db.View(func(tx *bolt.Tx) error {
		for _, bucketName := range [][]byte{bucketAudit, bucketMeta} {
			b := tx.Bucket(bucketName)
			if b == nil {
				snap[string(bucketName)+"::missing"] = nil
				continue
			}
			if err := b.ForEach(func(k, v []byte) error {
				snap[string(bucketName)+"/"+string(k)] = append([]byte(nil), v...)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func assertAppendFailsClosedAndStoreUnchanged(t *testing.T, store *Store) {
	t.Helper()
	before := snapshotStore(t, store)
	err := store.AppendAudit(context.Background(), AuditEvent{
		Timestamp: time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC),
		Actor:     "alice", Action: "after-corruption", Outcome: "success", Status: 200,
	})
	if err == nil {
		t.Fatal("append must fail closed on corrupt chain state")
	}
	report, verr := store.VerifyAuditChain(context.Background())
	if verr != nil {
		t.Fatal(verr)
	}
	if report.Verified {
		t.Fatalf("corrupt chain state must not verify: %+v", report)
	}
	after := snapshotStore(t, store)
	if len(before) != len(after) {
		t.Fatalf("failed append mutated the store: before %d entries, after %d", len(before), len(after))
	}
	for k, v := range before {
		if !bytes.Equal(v, after[k]) {
			t.Fatalf("failed append mutated store entry %q", k)
		}
	}
}

func putMeta(t *testing.T, store *Store, key, value []byte) {
	t.Helper()
	if err := store.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketMeta).Put(key, value) }); err != nil {
		t.Fatal(err)
	}
}

func deleteMeta(t *testing.T, store *Store, key []byte) {
	t.Helper()
	if err := store.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketMeta).Delete(key) }); err != nil {
		t.Fatal(err)
	}
}

func TestAuditChainAppendFailsClosedOnCorruptHeadStates(t *testing.T) {
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)

	t.Run("tail deleted, head intact", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		appendChainEvents(t, store, 3, base)
		deleteRecord(t, store, 2)
		assertAppendFailsClosedAndStoreUnchanged(t, store)
	})

	t.Run("tail deleted and head removed (false-verify scenario)", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		appendChainEvents(t, store, 3, base)
		deleteRecord(t, store, 2)
		deleteMeta(t, store, keyChainHead)
		// On the pre-fix code this append silently bootstrapped a
		// replacement head from the surviving records and the
		// shortened chain then reported VERIFIED.
		assertAppendFailsClosedAndStoreUnchanged(t, store)
	})

	t.Run("head JSON corrupted", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		appendChainEvents(t, store, 3, base)
		putMeta(t, store, keyChainHead, []byte("{"))
		assertAppendFailsClosedAndStoreUnchanged(t, store)
	})

	t.Run("head with wrong sequence", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		appendChainEvents(t, store, 3, base)
		report := verifyOK(t, store)
		putMeta(t, store, keyChainHead, []byte(fmt.Sprintf(`{"seq":99,"hash":%q}`, report.HeadHash)))
		assertAppendFailsClosedAndStoreUnchanged(t, store)
	})

	t.Run("head with wrong hash", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		appendChainEvents(t, store, 3, base)
		putMeta(t, store, keyChainHead, []byte(`{"seq":3,"hash":"0000000000000000000000000000000000000000000000000000000000000000"}`))
		assertAppendFailsClosedAndStoreUnchanged(t, store)
	})

	t.Run("head removed while pruning anchor exists", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "private", "store.db")
		store, err := Open(path, 100, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		old := time.Now().UTC().Add(-72 * time.Hour)
		appendChainEvents(t, store, 3, old)
		appendChainEvents(t, store, 1, time.Now().UTC()) // prunes 1-3, anchor=3, head=4
		if report := verifyOK(t, store); !report.HasAnchor {
			t.Fatalf("expected a pruning anchor: %+v", report)
		}
		deleteMeta(t, store, keyChainHead)
		assertAppendFailsClosedAndStoreUnchanged(t, store)
	})
}

func TestAuditChainEmptyStoreAcceptsFirstEvent(t *testing.T) {
	store, _ := chainTestStore(t, 100)
	defer store.Close()
	appendChainEvents(t, store, 1, time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	report := verifyOK(t, store)
	if report.ChainedRecords != 1 || report.FirstSeq != 1 {
		t.Fatalf("genesis report: %+v", report)
	}
}

func TestAuditChainSequenceGapInChainedSegmentDetected(t *testing.T) {
	store, _ := chainTestStore(t, 100)
	defer store.Close()
	appendChainEvents(t, store, 4, time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	deleteRecord(t, store, 2) // removes seq 3, leaving 1, 2, 4
	report, err := store.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified || !strings.Contains(strings.Join(report.Problems, " "), "sequence gap") {
		t.Fatalf("chained sequence gap not detected: %+v", report)
	}
}

// --- PR #301 review round 4: full-chain validation before append ---
// A tail-only check cannot prove the chain intact: every case below
// keeps the stored tail and/or head metadata looking consistent
// while the chain itself is broken. Verification must fail, the
// append must fail, and the store must stay byte-identical.

func TestAuditChainAppendRejectsCorruptionBeyondTheTail(t *testing.T) {
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		corrupt func(t *testing.T, store *Store)
	}{
		{"tail payload modified, stored hash and head unchanged", func(t *testing.T, store *Store) {
			rewriteRecord(t, store, 2, func(e *AuditEvent) { e.Actor = "mallory" })
		}},
		{"middle payload modified", func(t *testing.T, store *Store) {
			rewriteRecord(t, store, 1, func(e *AuditEvent) { e.Actor = "mallory" })
		}},
		{"prev hash modified", func(t *testing.T, store *Store) {
			rewriteRecord(t, store, 1, func(e *AuditEvent) { e.PrevHash = strings.Repeat("f", 64) })
		}},
		{"middle record removed, tail and head intact", func(t *testing.T, store *Store) {
			deleteRecord(t, store, 1)
		}},
		{"tail hash replaced and head forged to match", func(t *testing.T, store *Store) {
			fake := strings.Repeat("a", 64)
			rewriteRecord(t, store, 2, func(e *AuditEvent) { e.Hash = fake })
			putMeta(t, store, keyChainHead, []byte(fmt.Sprintf(`{"seq":3,"hash":%q}`, fake)))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := chainTestStore(t, 100)
			defer store.Close()
			appendChainEvents(t, store, 3, base)
			tc.corrupt(t, store)
			assertAppendFailsClosedAndStoreUnchanged(t, store)
		})
	}

	t.Run("pruning anchor hash corrupted", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "private", "store.db")
		store, err := Open(path, 100, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		appendChainEvents(t, store, 3, time.Now().UTC().Add(-72*time.Hour))
		appendChainEvents(t, store, 1, time.Now().UTC())
		putMeta(t, store, keyChainAnchor, []byte(`{"seq":3,"hash":"0000000000000000000000000000000000000000000000000000000000000000"}`))
		assertAppendFailsClosedAndStoreUnchanged(t, store)
	})

	t.Run("first retained record link to anchor corrupted", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "private", "store.db")
		store, err := Open(path, 100, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		appendChainEvents(t, store, 3, time.Now().UTC().Add(-72*time.Hour))
		appendChainEvents(t, store, 1, time.Now().UTC())
		rewriteRecord(t, store, 0, func(e *AuditEvent) { e.PrevHash = strings.Repeat("b", 64) })
		assertAppendFailsClosedAndStoreUnchanged(t, store)
	})
}

// --- PR #301 review round 5: sequence allocator validation ---
// bbolt persists the meta bucket's sequence counter in the bucket
// header and SetSequence accepts any value, so a lowered allocator
// is durable corruption: the next append would reuse a sequence.
// Verification must catch it and appends must refuse.

func setAllocator(t *testing.T, store *Store, v uint64) {
	t.Helper()
	if err := store.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketMeta).SetSequence(v) }); err != nil {
		t.Fatal(err)
	}
}

func readAllocator(t *testing.T, store *Store) uint64 {
	t.Helper()
	var v uint64
	if err := store.db.View(func(tx *bolt.Tx) error { v = tx.Bucket(bucketMeta).Sequence(); return nil }); err != nil {
		t.Fatal(err)
	}
	return v
}

func assertAllocatorRejected(t *testing.T, store *Store) {
	t.Helper()
	allocBefore := readAllocator(t, store)
	before := snapshotStore(t, store)
	report, err := store.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified || !strings.Contains(strings.Join(report.Problems, " "), "allocator") {
		t.Fatalf("allocator inconsistency not reported: %+v", report)
	}
	err = store.AppendAudit(context.Background(), AuditEvent{
		Timestamp: time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC),
		Actor:     "alice", Action: "after-allocator-tamper", Outcome: "success", Status: 200,
	})
	if err == nil {
		t.Fatal("append must fail closed on an inconsistent sequence allocator")
	}
	if got := readAllocator(t, store); got != allocBefore {
		t.Fatalf("failed append changed the allocator: %d -> %d", allocBefore, got)
	}
	after := snapshotStore(t, store)
	if len(before) != len(after) {
		t.Fatalf("failed append mutated the store: before %d entries, after %d", len(before), len(after))
	}
	for k, v := range before {
		if !bytes.Equal(v, after[k]) {
			t.Fatalf("failed append mutated store entry %q", k)
		}
	}
}

func TestAuditChainAllocatorValidation(t *testing.T) {
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)

	t.Run("allocator below tail sequence", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		appendChainEvents(t, store, 3, base)
		setAllocator(t, store, 1)
		assertAllocatorRejected(t, store)
	})

	t.Run("allocator reset to zero", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		appendChainEvents(t, store, 3, base)
		setAllocator(t, store, 0)
		assertAllocatorRejected(t, store)
	})

	t.Run("allocator above head violates the allocation contract", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		appendChainEvents(t, store, 3, base)
		setAllocator(t, store, 99)
		assertAllocatorRejected(t, store)
	})

	t.Run("allocator inconsistent with pruning anchor and head", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "private", "store.db")
		store, err := Open(path, 100, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		appendChainEvents(t, store, 3, time.Now().UTC().Add(-72*time.Hour))
		appendChainEvents(t, store, 1, time.Now().UTC()) // anchor 3, head 4, allocator 4
		setAllocator(t, store, 2)
		assertAllocatorRejected(t, store)
	})

	t.Run("legacy allocator above surviving records is legitimate", func(t *testing.T) {
		store, path := chainTestStore(t, 100)
		defer store.Close()
		plantLegacyRecords(t, store, 3, base)
		// Pre-chain timestamp pruning may have retired the highest
		// sequences: allocator 10 with survivors at 1..3 is valid.
		setAllocator(t, store, 10)
		report := verifyOK(t, store)
		if report.LegacyRecords != 3 {
			t.Fatalf("legacy report: %+v", report)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(path, 100, 365)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		appendChainEvents(t, reopened, 1, base.Add(4*time.Minute))
		got := verifyOK(t, reopened)
		if got.ChainedRecords != 1 || got.LastSeq != 11 {
			t.Fatalf("bootstrap after high legacy allocator: %+v", got)
		}
	})

	t.Run("legacy history fully pruned before chain metadata existed", func(t *testing.T) {
		store, _ := chainTestStore(t, 100)
		defer store.Close()
		setAllocator(t, store, 7) // all legacy records pruned; counter remains
		verifyOK(t, store)
		appendChainEvents(t, store, 1, base)
		report := verifyOK(t, store)
		if report.ChainedRecords != 1 || report.FirstSeq != 8 {
			t.Fatalf("append after fully pruned legacy history: %+v", report)
		}
	})
}

func plantLegacyRecords(t *testing.T, store *Store, n int, base time.Time) {
	t.Helper()
	err := store.db.Update(func(tx *bolt.Tx) error {
		audit := tx.Bucket(bucketAudit)
		for i := 0; i < n; i++ {
			ts := base.Add(time.Duration(i) * time.Minute)
			ev := AuditEvent{
				Timestamp: ts, Actor: "legacy", Action: fmt.Sprintf("legacy-%d", i),
				Outcome: "success", Status: 200,
			}
			encoded, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			key := make([]byte, 16)
			binary.BigEndian.PutUint64(key[:8], uint64(ts.UnixNano()))
			binary.BigEndian.PutUint64(key[8:], uint64(i+1))
			if err := audit.Put(key, encoded); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- PR #301 review round 6: fully pruned chained log requires its head ---

func TestAuditChainFullyPrunedLogRequiresHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "store.db")
	store, err := Open(path, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	past := time.Now().UTC().Add(-72 * time.Hour) // beyond the 1-day retention
	appendChainEvents(t, store, 1, past)
	appendChainEvents(t, store, 1, past.Add(time.Minute)) // prunes both; anchor/head at seq 2

	report := verifyOK(t, store)
	if report.TotalRecords != 0 || !report.HasAnchor || report.AnchorSeq != 2 {
		t.Fatalf("intact fully pruned state must verify: %+v", report)
	}

	deleteMeta(t, store, keyChainHead) // anchor and allocator (== 2) untouched

	report, err = store.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified || !strings.Contains(strings.Join(report.Problems, " "), "chain head metadata is missing") {
		t.Fatalf("fully pruned log without its head must not verify: %+v", report)
	}
	assertAppendFailsClosedAndStoreUnchanged(t, store)
}
