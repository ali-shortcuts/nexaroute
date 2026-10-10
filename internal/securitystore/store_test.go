package securitystore

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testHash(fill byte) string {
	return hex.EncodeToString(bytesOf(fill, 32))
}

func bytesOf(value byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = value
	}
	return out
}

func TestSessionAndAuditLifecycleIsTransactional(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "security.db")
	store, err := Open(path, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions = %o, want 600", info.Mode().Perm())
	}

	ctx := context.Background()
	now := time.Now().UTC()
	hash := testHash(0x11)
	event := AuditEvent{Timestamp: now, Actor: "subject-1", Action: "auth.login.succeeded", Outcome: "success", Status: 303, Authn: "oidc"}
	session := Session{Subject: "subject-1", Issuer: "https://issuer.example", Roles: []string{"admin"}, PolicyHash: "policy-v1", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}
	if err := store.CreateSession(ctx, hash, session, event); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.Session(ctx, hash)
	if err != nil || !found {
		t.Fatalf("Session() found=%v err=%v", found, err)
	}
	if got.Subject != session.Subject || got.PolicyHash != session.PolicyHash || len(got.Roles) != 1 || got.Roles[0] != "admin" {
		t.Fatalf("session round trip = %#v", got)
	}

	touchedAt := now.Add(5 * time.Minute)
	got, found, err = store.TouchSession(ctx, hash, touchedAt, 10*time.Minute)
	if err != nil || !found || !got.LastSeen.Equal(touchedAt) {
		t.Fatalf("TouchSession() session=%#v found=%v err=%v", got, found, err)
	}

	logout := AuditEvent{Timestamp: touchedAt, Actor: "subject-1", Action: "auth.logout", Outcome: "success", Status: 200, Authn: "oidc-session"}
	if err := store.RevokeSession(ctx, hash, logout); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Session(ctx, hash); err != nil || found {
		t.Fatalf("revoked session found=%v err=%v", found, err)
	}
	events, err := store.AuditRecords(ctx, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("audit records=%d err=%v, want 2", len(events), err)
	}
	if events[0].Action != "auth.logout" || events[1].Action != "auth.login.succeeded" {
		t.Fatalf("audit order = %#v", events)
	}
}

func TestFailedLoginAuditRollsBackSessionCreation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "private", "security.db"), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hash := testHash(0x22)
	session := Session{Subject: "subject-2", Issuer: "https://issuer.example", Roles: []string{"viewer"}, PolicyHash: "policy-v1", ExpiresAt: time.Now().Add(time.Hour)}
	err = store.CreateSession(context.Background(), hash, session, AuditEvent{Action: "auth.login.succeeded", Outcome: "success", Status: 303})
	if !errors.Is(err, ErrInvalidAudit) {
		t.Fatalf("CreateSession error = %v, want ErrInvalidAudit", err)
	}
	if _, found, err := store.Session(context.Background(), hash); err != nil || found {
		t.Fatalf("session survived failed login transaction: found=%v err=%v", found, err)
	}
	events, err := store.AuditRecords(context.Background(), 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("audit records after rollback=%d err=%v", len(events), err)
	}
}

func TestStorePersistsSessionAndAuditAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "security.db")
	hash := testHash(0x44)
	now := time.Now().UTC()
	event := AuditEvent{Timestamp: now, Actor: "subject-restart", Action: "auth.login.succeeded", Outcome: "success", Status: 303}
	session := Session{Subject: "subject-restart", Issuer: "https://issuer.example", Roles: []string{"operator"}, PolicyHash: "policy-v1", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}
	store, err := Open(path, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(context.Background(), hash, session, event); err != nil {
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
	if err != nil || !found || got.Subject != session.Subject || got.Roles[0] != "operator" {
		t.Fatalf("session did not survive reopen: session=%#v found=%v err=%v", got, found, err)
	}
	events, err := store.AuditRecords(context.Background(), 10)
	if err != nil || len(events) != 1 || events[0].Action != event.Action {
		t.Fatalf("audit event did not survive reopen: events=%#v err=%v", events, err)
	}
}

func TestOpenRejectsNonPrivateStoreDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(directory, "security.db"), 100, 30); err == nil {
		t.Fatal("security store opened in a group/world-accessible directory")
	}
}

func TestExpiredSessionCannotBeTouchedBackToLife(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "private", "security.db"), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	hash := testHash(0x33)
	session := Session{Subject: "subject-3", Issuer: "https://issuer.example", Roles: []string{"operator"}, PolicyHash: "policy-v1", CreatedAt: now.Add(-2 * time.Hour), LastSeen: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	if err := store.CreateSession(context.Background(), hash, session, AuditEvent{Actor: "subject-3", Action: "auth.login.succeeded", Outcome: "success", Status: 303}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.TouchSession(context.Background(), hash, now, time.Hour); err != nil || found {
		t.Fatalf("expired session touched: found=%v err=%v", found, err)
	}
}

func TestAuditSanitizesControlCharacters(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "private", "security.db"), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	event := AuditEvent{Actor: "alice\nforged", Action: "admin.write\r", Target: "/settings\u2028", Outcome: "success", Status: 200}
	if err := store.AppendAudit(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	got, err := store.AuditRecords(context.Background(), 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("AuditRecords()=%v err=%v", got, err)
	}
	if got[0].Actor != "aliceforged" || got[0].Action != "admin.write" || got[0].Target != "/settings" {
		t.Fatalf("audit fields were not sanitized: %#v", got[0])
	}
}
