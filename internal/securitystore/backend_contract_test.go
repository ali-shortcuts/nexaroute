package securitystore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// contractStorePath returns a store path inside a 0700 directory, as the
// store requires for its parent.
func contractStorePath(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "security")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, name)
}

// runBackendContract exercises the full Backend contract against any
// implementation. M4-2 adapters (Redis/PostgreSQL) must run this same
// suite against the real services (opt-in) before they may be selected;
// here it runs against the default bbolt store. The suite is behavioral:
// session lifecycle, audit append/read ordering, chain verification, and
// fail-closed handling of malformed inputs.
func runBackendContract(t *testing.T, open func(t *testing.T) Backend) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	b := open(t)
	defer func() { _ = b.Close() }()

	hashA := strings.Repeat("aa", 32)
	hashB := strings.Repeat("bb", 32)
	login := AuditEvent{Timestamp: now, Actor: "alice", Action: "auth.login", Outcome: "success", Status: 200}
	sess := Session{Subject: "alice", Issuer: "local", Roles: []string{"admin"}, CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}

	// Session lifecycle: create → read → touch → rotate → revoke.
	if err := b.CreateSession(ctx, hashA, sess, login); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, ok, err := b.Session(ctx, hashA)
	if err != nil || !ok || got.Subject != "alice" {
		t.Fatalf("Session after create: ok=%v err=%v subject=%q", ok, err, got.Subject)
	}
	if _, ok, err := b.Session(ctx, strings.Repeat("cc", 32)); err != nil || ok {
		t.Fatalf("Session unknown hash: ok=%v err=%v, want not-found", ok, err)
	}
	touched, ok, err := b.TouchSession(ctx, hashA, now.Add(time.Minute), 15*time.Minute)
	if err != nil || !ok || !touched.LastSeen.Equal(now.Add(time.Minute)) {
		t.Fatalf("TouchSession: ok=%v err=%v lastSeen=%v", ok, err, touched.LastSeen)
	}
	rotated := sess
	rotated.LastSeen = now.Add(2 * time.Minute)
	if err := b.RotateSession(ctx, hashA, hashB, rotated, login); err != nil {
		t.Fatalf("RotateSession: %v", err)
	}
	if _, ok, _ := b.Session(ctx, hashA); ok {
		t.Fatal("old session hash must not resolve after rotation")
	}
	if _, ok, err := b.Session(ctx, hashB); err != nil || !ok {
		t.Fatalf("Session after rotate: ok=%v err=%v", ok, err)
	}
	if err := b.RevokeSession(ctx, hashB, AuditEvent{Timestamp: now, Actor: "alice", Action: "auth.logout", Outcome: "success", Status: 200}); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, ok, _ := b.Session(ctx, hashB); ok {
		t.Fatal("revoked session must not resolve")
	}

	// Audit: appended events come back in order and the chain verifies.
	for _, action := range []string{"contract.one", "contract.two", "contract.three"} {
		if err := b.AppendAudit(ctx, AuditEvent{Timestamp: now, Actor: "contract", Action: action, Outcome: "success", Status: 200}); err != nil {
			t.Fatalf("AppendAudit %s: %v", action, err)
		}
	}
	records, err := b.AuditRecords(ctx, 100)
	if err != nil {
		t.Fatalf("AuditRecords: %v", err)
	}
	// AuditRecords returns the most recent records, newest first (the
	// admin audit view's contract); adapters must match this ordering.
	var order []string
	for _, rec := range records {
		if strings.HasPrefix(rec.Action, "contract.") {
			order = append(order, rec.Action)
		}
	}
	if len(order) != 3 || order[0] != "contract.three" || order[1] != "contract.two" || order[2] != "contract.one" {
		t.Fatalf("audit order = %v, want the three appended events newest-first", order)
	}
	report, err := b.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
	if !report.Verified || len(report.Problems) != 0 {
		t.Fatalf("chain must verify after contract appends: %+v", report)
	}

	// Fail-closed malformed inputs: a malformed session hash and an
	// invalid session-attached audit event are both rejected. (Bare
	// AppendAudit stamps a zero timestamp itself; session-attached
	// events are validated strictly.)
	if err := b.CreateSession(ctx, "not-a-hash", sess, login); err == nil {
		t.Fatal("CreateSession with a malformed hash must fail")
	}
	if err := b.CreateSession(ctx, strings.Repeat("dd", 32), sess, AuditEvent{Action: "auth.login", Outcome: "success", Status: 200}); err == nil {
		t.Fatal("CreateSession with a timestamp-less audit event must fail")
	}
}

func TestBBoltBackendContract(t *testing.T) {
	runBackendContract(t, func(t *testing.T) Backend {
		t.Helper()
		b, err := OpenBackend("", "", contractStorePath(t, "security.db"), 1000, 365)
		if err != nil {
			t.Fatalf("OpenBackend default: %v", err)
		}
		return b
	})
}

func TestOpenBackendSelectionFailsClosed(t *testing.T) {
	// Explicit bbolt works like the default.
	b, err := OpenBackend(BackendBBolt, "", contractStorePath(t, "a.db"), 1000, 365)
	if err != nil {
		t.Fatalf("explicit bbolt: %v", err)
	}
	_ = b.Close()
	// A URL with bbolt is a configuration error, not silently ignored.
	if _, err := OpenBackend(BackendBBolt, "redis://127.0.0.1:6379", contractStorePath(t, "b.db"), 1000, 365); err == nil {
		t.Fatal("bbolt with a URL must fail")
	}
	// redis/postgres are recognized but unavailable in this build: the
	// factory must refuse, never substitute another store.
	for _, backend := range []string{BackendRedis, BackendPostgres} {
		if _, err := OpenBackend(backend, "db://127.0.0.1:1", contractStorePath(t, "c.db"), 1000, 365); err == nil ||
			!strings.Contains(err.Error(), "not available in this build") {
			t.Fatalf("%s: err=%v, want fail-closed unavailability", backend, err)
		}
		if _, err := OpenBackend(backend, "", contractStorePath(t, "c.db"), 1000, 365); err == nil {
			t.Fatalf("%s without URL must fail", backend)
		}
	}
	// Unknown backends fail closed too.
	if _, err := OpenBackend("sqlite", "", contractStorePath(t, "d.db"), 1000, 365); err == nil {
		t.Fatal("unknown backend must fail")
	}
	if !ValidBackend("") || !ValidBackend("bbolt") || !ValidBackend("redis") || !ValidBackend("postgres") || ValidBackend("sqlite") {
		t.Fatal("ValidBackend mismatch")
	}
}
