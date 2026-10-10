package securitystore

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestCoverageSecurityStoreOpenValidationAndPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		max, days  int
	}{
		{"empty path", " ", 100, 1}, {"low max", filepath.Join(t.TempDir(), "db"), 99, 1},
		{"high max", filepath.Join(t.TempDir(), "db"), 1_000_001, 1},
		{"low retention", filepath.Join(t.TempDir(), "db"), 100, 0},
		{"high retention", filepath.Join(t.TempDir(), "db"), 100, 3651},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Open(tc.path, tc.max, tc.days); err == nil {
				t.Fatal("invalid store options accepted")
			}
		})
	}
	root := t.TempDir()
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(private, "readable.db")
	if err := os.WriteFile(path, []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, 100, 1); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("permissive file error = %v", err)
	}
	_ = os.Remove(path)
	if err := os.Symlink(filepath.Join(private, "missing"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, 100, 1); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink file error = %v", err)
	}
	_ = os.Remove(path)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, 100, 1); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory database path error = %v", err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(link, "security.db"), 100, 1); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink parent error = %v", err)
	}
}

func TestCoverageSecurityStoreSessionValidationAndCanceledContexts(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "private", "store.db"), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Now().UTC()
	session := Session{Subject: "subject", Issuer: "issuer", ExpiresAt: now.Add(time.Hour)}
	event := AuditEvent{Actor: "subject", Action: "login", Outcome: "success", Status: 200}
	if err := store.CreateSession(ctx, testHash(1), session, event); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled create error=%v", err)
	}
	if _, _, err := store.Session(ctx, testHash(1)); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled read error=%v", err)
	}
	if _, _, err := store.TouchSession(ctx, testHash(1), now, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled touch error=%v", err)
	}
	if err := store.RevokeSession(ctx, testHash(1), event); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled revoke error=%v", err)
	}
	if err := store.AppendAudit(ctx, event); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled audit error=%v", err)
	}
	if _, err := store.AuditRecords(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled audit read error=%v", err)
	}

	if _, _, err := store.Session(context.Background(), "abc"); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("invalid session hash error=%v", err)
	}
	if _, _, err := store.TouchSession(context.Background(), testHash(1), now, 0); err == nil {
		t.Error("non-positive idle timeout accepted")
	}
	if err := store.CreateSession(context.Background(), "not-a-hash", session, event); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("invalid new hash error=%v", err)
	}
	if err := store.RotateSession(context.Background(), "bad-old-hash", testHash(1), session, event); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("invalid previous hash error=%v", err)
	}
	for _, invalid := range []Session{{Issuer: "issuer", ExpiresAt: now.Add(time.Hour)}, {Subject: "subject", ExpiresAt: now.Add(time.Hour)}, {Subject: "subject", Issuer: "issuer"}} {
		if err := store.CreateSession(context.Background(), testHash(1), invalid, event); err == nil {
			t.Errorf("invalid session accepted: %#v", invalid)
		}
	}
}

func TestCoverageSecurityStoreRotationCollisionRollsBackOldSession(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "private", "store.db"), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	oldHash, takenHash := testHash(0x10), testHash(0x20)
	session := Session{Subject: "initial", Issuer: "issuer", Roles: []string{"admin"}, CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}
	event := AuditEvent{Actor: "initial", Action: "login", Outcome: "success", Status: 200}
	if err := store.CreateSession(ctx, oldHash, session, event); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, takenHash, session, event); err != nil {
		t.Fatal(err)
	}
	updated := session
	updated.Subject = "replacement"
	if err := store.RotateSession(ctx, oldHash, takenHash, updated, event); err == nil {
		t.Fatal("session hash collision accepted")
	}
	got, found, err := store.Session(ctx, oldHash)
	if err != nil || !found || got.Subject != "initial" {
		t.Fatalf("old session lost on failed rotation: %#v, found=%v err=%v", got, found, err)
	}
	got, found, err = store.Session(ctx, takenHash)
	if err != nil || !found || got.Subject != "initial" {
		t.Fatalf("colliding session changed: %#v, found=%v err=%v", got, found, err)
	}
	events, err := store.AuditRecords(ctx, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("failed rotation audit changed: %d events, err=%v", len(events), err)
	}
}

func TestCoverageSecurityStoreFailedLogoutRollsBackRevocation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "private", "store.db"), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	hash := testHash(0x29)
	session := Session{Subject: "logout-rollback", Issuer: "issuer", ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.CreateSession(ctx, hash, session, AuditEvent{Actor: "logout-rollback", Action: "login", Outcome: "success", Status: 200}); err != nil {
		t.Fatal(err)
	}
	err = store.RevokeSession(ctx, hash, AuditEvent{Actor: "logout-rollback", Outcome: "success", Status: 200})
	if !errors.Is(err, ErrInvalidAudit) {
		t.Fatalf("invalid logout audit error=%v, want ErrInvalidAudit", err)
	}
	if got, found, err := store.Session(ctx, hash); err != nil || !found || got.Subject != session.Subject {
		t.Fatalf("failed logout removed session: %#v found=%v err=%v", got, found, err)
	}
	if events, err := store.AuditRecords(ctx, 10); err != nil || len(events) != 1 || events[0].Action != "login" {
		t.Fatalf("failed logout left partial audit: %#v err=%v", events, err)
	}
}

func TestCoverageSecurityStoreTouchExpiresIdleAndClonesRoles(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "private", "store.db"), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	hash := testHash(0x31)
	session := Session{Subject: "touch", Issuer: "issuer", Roles: []string{"admin"}, CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}
	if err := store.CreateSession(ctx, hash, session, AuditEvent{Actor: "touch", Action: "login", Outcome: "success", Status: 200}); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.TouchSession(ctx, hash, time.Time{}, time.Hour)
	if err != nil || !found || got.LastSeen.IsZero() {
		t.Fatalf("zero-time touch result %#v found=%v err=%v", got, found, err)
	}
	got.Roles[0] = "mutated-copy"
	stored, found, err := store.Session(ctx, hash)
	if err != nil || !found || stored.Roles[0] != "admin" {
		t.Fatalf("returned role slice aliases stored session: %#v err=%v", stored, err)
	}
	if _, found, err := store.TouchSession(ctx, hash, now.Add(2*time.Hour), time.Hour); err != nil || found {
		t.Fatalf("absolute-expired touch found=%v err=%v", found, err)
	}
	if _, found, err := store.Session(ctx, hash); err != nil || found {
		t.Fatalf("absolutely expired session remained: found=%v err=%v", found, err)
	}

	idleHash := testHash(0x32)
	session.LastSeen = now.Add(-time.Hour)
	session.ExpiresAt = now.Add(time.Hour)
	if err := store.CreateSession(ctx, idleHash, session, AuditEvent{Actor: "touch", Action: "login", Outcome: "success", Status: 200}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.TouchSession(ctx, idleHash, now, time.Hour); err != nil || found {
		t.Fatalf("idle-expired touch found=%v err=%v", found, err)
	}
}

func TestCoverageSecurityStoreAuditValidationRetentionAndCaps(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "private", "store.db"), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	invalid := []AuditEvent{{Action: "only action", Outcome: "denied", Status: 401}, {Actor: "actor", Outcome: "denied", Status: 401}, {Actor: "actor", Action: "action", Status: 600}, {Actor: "actor", Action: "action", Outcome: "denied", Status: -1}}
	for _, event := range invalid {
		if err := store.AppendAudit(ctx, event); !errors.Is(err, ErrInvalidAudit) {
			t.Errorf("invalid audit event error=%v: %#v", err, event)
		}
	}
	if events, err := store.AuditRecords(ctx, 10); err != nil || len(events) != 0 {
		t.Fatalf("invalid audits partially persisted: %d err=%v", len(events), err)
	}

	store.retention = time.Hour
	old := AuditEvent{Timestamp: time.Now().Add(-2 * time.Hour), Actor: "old", Action: "old", Outcome: "success", Status: 200}
	if err := store.AppendAudit(ctx, old); err != nil {
		t.Fatal(err)
	}
	fresh := AuditEvent{Timestamp: time.Now().UTC(), Actor: "fresh", Action: "fresh", Outcome: "success", Status: 200}
	if err := store.AppendAudit(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if events, err := store.AuditRecords(ctx, 0); err != nil || len(events) != 1 || events[0].Actor != "fresh" {
		t.Fatalf("retention or default limit: %#v err=%v", events, err)
	}

	store.maxAuditItems = 2
	for _, actor := range []string{"a", "b", "c"} {
		if err := store.AppendAudit(ctx, AuditEvent{Timestamp: time.Now().UTC(), Actor: actor, Action: "capped", Outcome: "success", Status: 200}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if events, err := store.AuditRecords(ctx, 100); err != nil || len(events) != 2 || events[0].Actor != "c" || events[1].Actor != "b" {
		t.Fatalf("audit cap/order = %#v err=%v", events, err)
	}
	if events, err := store.AuditRecords(ctx, 500); err != nil || len(events) != 2 {
		t.Fatalf("requested limit not capped to configured max: %d err=%v", len(events), err)
	}
}

func TestCoverageSecurityStoreSanitizesAndBoundsValues(t *testing.T) {
	if got := Sanitize("  abc\x00def\u2028ghi  ", 6); got != "abcdef" {
		t.Fatalf("bounded sanitized text=%q", got)
	}
	if got := Sanitize("text", 0); got != "" {
		t.Fatalf("non-positive bound=%q", got)
	}
	values := cleanValues([]string{"", " role\nadmin ", "roleadmin", "two", "three", "four"}, 2, 32)
	if len(values) != 2 || values[0] != "roleadmin" || values[1] != "two" {
		t.Fatalf("cleanValues result=%#v", values)
	}

	store, err := Open(filepath.Join(t.TempDir(), "private", "store.db"), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	session := Session{Subject: "  alice\nforged ", Issuer: " issuer\x00url ", Roles: []string{"viewer", "viewer", "", "operator"}, ExpiresAt: now.Add(time.Hour)}
	if err := store.CreateSession(context.Background(), testHash(0x41), session, AuditEvent{Actor: " alice\nforged ", Action: " auth\nlogin ", Outcome: " success ", Status: 200}); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.Session(context.Background(), testHash(0x41))
	if err != nil || !found || got.Subject != "aliceforged" || got.Issuer != "issuerurl" || len(got.Roles) != 2 || got.Roles[0] != "viewer" || got.Roles[1] != "operator" {
		t.Fatalf("session sanitization/dedup: %#v found=%v err=%v", got, found, err)
	}
	if events, err := store.AuditRecords(context.Background(), -1); err != nil || len(events) != 1 || events[0].Actor != "aliceforged" || events[0].Action != "authlogin" {
		t.Fatalf("audit sanitization/default: %#v err=%v", events, err)
	}
}

func TestCoverageSecurityStoreMalformedRecordsAndInvalidAuditKeys(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "private", "store.db"), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketSessions).Put(bytesOf(0x51, 32), []byte("{")) }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Session(ctx, hex.EncodeToString(bytesOf(0x51, 32))); err == nil || !strings.Contains(err.Error(), "malformed session") {
		t.Fatalf("malformed session error=%v", err)
	}
	if _, _, err := store.TouchSession(ctx, hex.EncodeToString(bytesOf(0x51, 32)), time.Now(), time.Minute); err == nil || !strings.Contains(err.Error(), "malformed session") {
		t.Fatalf("malformed touched session error=%v", err)
	}
	if err := store.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketAudit).Put([]byte{0}, []byte("not-json")) }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuditRecords(ctx, 10); err == nil || !strings.Contains(err.Error(), "malformed audit") {
		t.Fatalf("malformed audit error=%v", err)
	}
	if err := store.AppendAudit(ctx, AuditEvent{Actor: "a", Action: "x", Outcome: "success", Status: 200}); err == nil || !strings.Contains(err.Error(), "invalid audit key") {
		t.Fatalf("bad audit key error=%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCoverageSecurityStoreNilCloseAndReopenState(t *testing.T) {
	var nilStore *Store
	if err := nilStore.Close(); err != nil {
		t.Fatalf("nil close: %v", err)
	}
	store := &Store{}
	if err := store.Close(); err != nil {
		t.Fatalf("nil db close: %v", err)
	}
	path := filepath.Join(t.TempDir(), "private", "reopen.db")
	hash := testHash(0x61)
	store, err := Open(path, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(context.Background(), hash, Session{Subject: "reopened", Issuer: "issuer", ExpiresAt: time.Now().Add(time.Hour)}, AuditEvent{Actor: "reopened", Action: "created", Outcome: "success", Status: 200}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, found, err := store.Session(context.Background(), hash)
	if err != nil || !found || got.Subject != "reopened" {
		t.Fatalf("reopened session=%#v found=%v err=%v", got, found, err)
	}
}
