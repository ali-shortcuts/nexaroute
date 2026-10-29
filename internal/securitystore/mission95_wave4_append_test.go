package securitystore

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// A legacy (pre-chain) record plus valid chain-head metadata is refused:
// chain metadata must never describe a log with no chained records, and
// the refusal must not append anything.
func TestWave4AppendLegacyWithMetadataRefused(t *testing.T) {
	store, _ := chainTestStore(t, 100)
	ts := time.Unix(1700000000, 0).UTC()
	legacy := AuditEvent{Timestamp: ts, Actor: "alice", Action: "legacy-action", Outcome: "success", Status: 200}
	err := store.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		audit := tx.Bucket(bucketAudit)
		key := make([]byte, 16)
		binary.BigEndian.PutUint64(key[:8], uint64(ts.UnixNano()))
		binary.BigEndian.PutUint64(key[8:], 1)
		raw, err := json.Marshal(legacy)
		if err != nil {
			return err
		}
		if err := audit.Put(key, raw); err != nil {
			return err
		}
		if err := storeChainRef(meta, keyChainHead, chainRef{Seq: 1, Hash: recordHash(legacy, 1)}); err != nil {
			return err
		}
		return meta.SetSequence(1)
	})
	if err != nil {
		t.Fatal(err)
	}
	appendErr := store.AppendAudit(context.Background(), AuditEvent{Actor: "alice", Action: "after-legacy", Outcome: "success", Status: 200})
	if appendErr == nil || !strings.Contains(appendErr.Error(), "chain metadata but no chained audit records") {
		t.Fatalf("err = %v, want the chain-metadata-without-chained-records refusal", appendErr)
	}
	records, err := store.AuditRecords(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Action != "legacy-action" {
		t.Fatalf("records = %v, want only the original legacy record (refusal must not append)", records)
	}
}

// A storage write failure inside the append transaction surfaces as an
// error instead of a silent success: driving appendAuditTx with a
// read-only transaction makes the sequence allocation fail, and the
// caller must see that failure.
func TestWave4AppendStorageWriteFailureSurfaces(t *testing.T) {
	store, _ := chainTestStore(t, 100)
	if err := store.AppendAudit(context.Background(), AuditEvent{Actor: "alice", Action: "first", Outcome: "success", Status: 200}); err != nil {
		t.Fatal(err)
	}
	viewErr := store.db.View(func(tx *bolt.Tx) error {
		_, err := appendAuditTx(tx, store, AuditEvent{Actor: "alice", Action: "second", Outcome: "success", Status: 200})
		return err
	})
	if viewErr == nil || !strings.Contains(viewErr.Error(), "writable") {
		t.Fatalf("err = %v, want a not-writable storage error", viewErr)
	}
	records, err := store.AuditRecords(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1: the failed append must not have persisted", len(records))
	}
}
