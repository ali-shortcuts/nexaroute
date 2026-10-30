package securitystore

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Continue95Store creates a fresh disposable store for one test.
func Continue95Store(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "security.db")
	store, err := Open(path, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func Continue95Session(now time.Time) Session {
	return Session{Subject: "subject-1", Issuer: "https://issuer.example", Roles: []string{"admin"}, PolicyHash: "policy-v1", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}
}

func Continue95Event(now time.Time) AuditEvent {
	return AuditEvent{Timestamp: now, Actor: "subject-1", Action: "auth.login.succeeded", Outcome: "success", Status: 303, Authn: "oidc"}
}

func TestContinue95OpenAndReadOnlyErrorPaths(t *testing.T) {
	// A file that exists with correct permissions but is not a bbolt
	// database fails at bolt.Open, not at the permission checks.
	root := t.TempDir()
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(private, "security.db")
	if err := os.WriteFile(garbage, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(garbage, 100, 1); err == nil || !strings.Contains(err.Error(), "open security store") {
		t.Fatalf("garbage file Open error = %v", err)
	}

	// OpenReadOnly permission/symlink gates on a real store.
	store, path := Continue95Store(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(path); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("permissive file OpenReadOnly error = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	linkFile := filepath.Join(dir, "linked.db")
	if err := os.Symlink(path, linkFile); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(linkFile); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink file OpenReadOnly error = %v", err)
	}
	_ = os.Remove(linkFile)
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	// The store file must live under the symlinked directory for the
	// directory check to fire, so move the real file there first.
	moved := filepath.Join(outside, "security.db")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "linked")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(filepath.Join(linkDir, "security.db")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink dir OpenReadOnly error = %v", err)
	}
}

func TestContinue95InvalidSessionHashes(t *testing.T) {
	store, _ := Continue95Store(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, _, err := store.TouchSession(ctx, "not-a-hash", now, time.Hour); err == nil {
		t.Fatal("TouchSession accepted an invalid hash")
	}
	if err := store.RevokeSession(ctx, "zz", Continue95Event(now)); err == nil {
		t.Fatal("RevokeSession accepted an invalid hash")
	}
}

// Continue95BlockedWriter runs fn while another write transaction holds
// the bbolt write lock, cancelling ctx mid-wait, and reports fn's error.
// The method's closure must then observe the cancelled context.
func Continue95BlockedWriter(t *testing.T, store *Store, ctx context.Context, cancel context.CancelFunc, fn func() error) error {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = store.db.Update(func(tx *bolt.Tx) error {
			close(started)
			<-release
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("writer transaction did not start")
	}
	errCh := make(chan error, 1)
	go func() { errCh <- fn() }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	close(release)
	select {
	case err := <-errCh:
		<-done
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("method did not return after writer release")
		return nil
	}
}

func TestContinue95WriteInnerContextCancellation(t *testing.T) {
	store, _ := Continue95Store(t)
	now := time.Now().UTC()
	hash := testHash(0x11)
	// RotateSession (CreateSession with a previous hash) inner ctx.
	ctx, cancel := context.WithCancel(context.Background())
	err := Continue95BlockedWriter(t, store, ctx, cancel, func() error {
		return store.CreateSession(ctx, hash, Continue95Session(now), Continue95Event(now))
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateSession inner ctx error = %v", err)
	}
	// TouchSession inner ctx (checked before any bucket read).
	ctx2, cancel2 := context.WithCancel(context.Background())
	err = Continue95BlockedWriter(t, store, ctx2, cancel2, func() error {
		_, _, err := store.TouchSession(ctx2, hash, now, time.Hour)
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("TouchSession inner ctx error = %v", err)
	}
	// RevokeSession inner ctx.
	ctx3, cancel3 := context.WithCancel(context.Background())
	err = Continue95BlockedWriter(t, store, ctx3, cancel3, func() error {
		return store.RevokeSession(ctx3, hash, Continue95Event(now))
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RevokeSession inner ctx error = %v", err)
	}
	// AppendAudit inner ctx.
	ctx4, cancel4 := context.WithCancel(context.Background())
	err = Continue95BlockedWriter(t, store, ctx4, cancel4, func() error {
		return store.AppendAudit(ctx4, Continue95Event(now))
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AppendAudit inner ctx error = %v", err)
	}
}

func TestContinue95MalformedAuditRecordFailsVerification(t *testing.T) {
	store, path := Continue95Store(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 16)
	binary.BigEndian.PutUint64(key[:8], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint64(key[8:], 1)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketAudit).Put(key, []byte("not-json"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	report, err := reopened.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified {
		t.Fatal("chain with a malformed record verified")
	}
	found := false
	for _, p := range report.Problems {
		if strings.Contains(p, "malformed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("problems = %v, want a malformed-record problem", report.Problems)
	}
}

func TestContinue95ChainCoreLegacyMetadataRefusal(t *testing.T) {
	legacy := AuditEvent{Seq: 1, Timestamp: chainCoreNow, Actor: "legacy", Action: "legacy.login", Outcome: "success", Status: 200}
	st := chainState{
		Records:     []AuditEvent{legacy},
		Head:        chainRef{Seq: 1, Hash: recordHash(legacy, 1)},
		HeadState:   refValid,
		AnchorState: refAbsent,
		Allocator:   1,
	}
	if report := verifyChainState(st); !report.Verified {
		t.Fatalf("legacy+head state must verify, report = %+v", report)
	}
	if _, err := planAuditAppend(st, chainEvent("contract.more", chainCoreNow), 1000, 365*24*time.Hour, chainCoreNow); err == nil || !strings.Contains(err.Error(), "no chained audit records") {
		t.Fatalf("legacy+metadata append error = %v", err)
	}
}

func TestContinue95ChainCoreAnchorCarryover(t *testing.T) {
	// Build a pruned chain (anchor at seq 2, records 3..5), then append
	// with a generous bound so nothing is pruned: the plan must carry
	// the existing anchor instead of clearing it.
	st := chainState{}
	for i := 0; i < 5; i++ {
		plan, err := planAuditAppend(st, chainEvent("contract.append", chainCoreNow.Add(time.Duration(i)*time.Second)), 3, 365*24*time.Hour, chainCoreNow)
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		st = applyPlan(st, plan)
	}
	if st.AnchorState != refValid || st.Anchor.Seq != 2 {
		t.Fatalf("pruned state anchor = %+v", st.Anchor)
	}
	plan, err := planAuditAppend(st, chainEvent("contract.keep", chainCoreNow.Add(6*time.Second)), 1000, 365*24*time.Hour, chainCoreNow)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.HasAnchor || plan.Anchor != st.Anchor {
		t.Fatalf("anchor carryover plan = %+v, want %+v", plan.Anchor, st.Anchor)
	}
	if len(plan.PrunedSeqs) != 0 {
		t.Fatalf("no-prune plan pruned %v", plan.PrunedSeqs)
	}
	// The carried-over plan must still verify after application.
	next := applyPlan(st, plan)
	if report := verifyChainState(next); !report.Verified {
		t.Fatalf("post-carryover chain report = %+v", report)
	}
	// And the carried anchor JSON round-trips through the store codec.
	encoded, err := json.Marshal(chainRef{Seq: 2, Hash: "anchor-hash"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded chainRef
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Seq != 2 {
		t.Fatalf("chainRef round trip = %+v err=%v", decoded, err)
	}
}
