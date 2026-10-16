package securitystore

// Wave C-1 PostgreSQL adapter suite over the in-test fake: the shared
// Backend contract, cross-backend hash parity with bbolt, two writers
// on one shared fake database, prune-to-empty, and the fail-closed
// error matrix (injected transport failures, malformed rows, canceled
// contexts). The gated integration suite repeats the contract against
// a real server.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPostgresBackendContractAgainstFake(t *testing.T) {
	runBackendContract(t, func(t *testing.T) Backend {
		t.Helper()
		return openFakePostgres(t, 1000, 365*24*time.Hour)
	})
}

func TestFakePostgresHashParityWithBbolt(t *testing.T) {
	ctx := context.Background()
	pg := openFakePostgres(t, 1000, 365*24*time.Hour)
	bb, err := Open(contractStorePath(t, "parity.db"), 1000, 365)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bb.Close() }()
	base := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	hash := strings.Repeat("aa", 32)
	sess := Session{Subject: "alice", Issuer: "local", Roles: []string{"admin"}, CreatedAt: base, LastSeen: base, ExpiresAt: base.Add(time.Hour)}
	login := AuditEvent{Timestamp: base, Actor: "alice", Action: "auth.login", Outcome: "success", Status: 200}
	for i, b := range []Backend{pg, bb} {
		if err := b.CreateSession(ctx, hash, sess, login); err != nil {
			t.Fatalf("backend %d CreateSession: %v", i, err)
		}
		for j, action := range []string{"contract.one", "contract.two", "contract.three"} {
			ev := AuditEvent{Timestamp: base.Add(time.Duration(j+1) * time.Second), Actor: "contract", Action: action, Outcome: "success", Status: 200}
			if err := b.AppendAudit(ctx, ev); err != nil {
				t.Fatalf("backend %d AppendAudit: %v", i, err)
			}
		}
		if err := b.RevokeSession(ctx, hash, AuditEvent{Timestamp: base.Add(4 * time.Second), Actor: "alice", Action: "auth.logout", Outcome: "success", Status: 200}); err != nil {
			t.Fatalf("backend %d RevokeSession: %v", i, err)
		}
	}
	pgRecords, err := pg.AuditRecords(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	bbRecords, err := bb.AuditRecords(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pgRecords) != len(bbRecords) || len(pgRecords) != 5 {
		t.Fatalf("record counts: pg=%d bbolt=%d, want 5/5", len(pgRecords), len(bbRecords))
	}
	for i := range pgRecords {
		if pgRecords[i] != bbRecords[i] {
			t.Fatalf("record %d differs across backends:\npg:    %+v\nbbolt: %+v", i, pgRecords[i], bbRecords[i])
		}
	}
}

func TestFakePostgresTwoWritersOneChain(t *testing.T) {
	shared := newFakePGDB()
	a := openFakePostgresOn(t, shared, 1000, 365*24*time.Hour)
	b := openFakePostgresOn(t, shared, 1000, 365*24*time.Hour)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for _, store := range []*PostgresStore{a, b} {
		wg.Add(1)
		go func(store *PostgresStore) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if err := store.AppendAudit(ctx, AuditEvent{Actor: "writer", Action: "contract.concurrent", Outcome: "success", Status: 200}); err != nil {
					errs <- err
				}
			}
		}(store)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}
	report, err := a.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || report.ChainedRecords != 50 {
		t.Fatalf("chain after concurrent appends: %+v", report)
	}
	records, err := a.AuditRecords(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uint64]bool{}
	for _, r := range records {
		if seen[r.Seq] {
			t.Fatalf("duplicate sequence %d", r.Seq)
		}
		seen[r.Seq] = true
	}
}

func TestFakePostgresPruneToEmptyKeepsHeadAtAnchor(t *testing.T) {
	ctx := context.Background()
	// Negative retention prunes every record including the one just
	// appended: the log empties and head/anchor coincide.
	db := newFakePGDB()
	s := openFakePostgresOn(t, db, 1000, -time.Second)
	if err := s.AppendAudit(ctx, AuditEvent{Actor: "a", Action: "one", Outcome: "success", Status: 200}); err != nil {
		t.Fatal(err)
	}
	report, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || report.TotalRecords != 0 || !report.HasAnchor {
		t.Fatalf("fully pruned state must verify with an anchor: %+v", report)
	}
	if err := s.AppendAudit(ctx, AuditEvent{Actor: "a", Action: "two", Outcome: "success", Status: 200}); err != nil {
		t.Fatalf("append to emptied log: %v", err)
	}
	report, err = s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified {
		t.Fatalf("chain after re-append must verify: %+v", report)
	}
}

func TestFakePostgresFailedMutationRollsBack(t *testing.T) {
	ctx := context.Background()
	db := newFakePGDB()
	s := openFakePostgresOn(t, db, 1000, 365*24*time.Hour)
	hash := strings.Repeat("ab", 32)
	sess := Session{Subject: "s", Issuer: "i", ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.CreateSession(ctx, hash, sess, AuditEvent{Actor: "s", Action: "login", Outcome: "success", Status: 200}); err != nil {
		t.Fatal(err)
	}
	// A revocation whose audit event is invalid must leave the
	// session in place and add no audit record (transactional
	// rollback, as the bbolt backend guarantees).
	if err := s.RevokeSession(ctx, hash, AuditEvent{Actor: "s", Outcome: "success", Status: 200}); err == nil {
		t.Fatal("revocation with an invalid audit event must fail")
	}
	if _, found, err := s.Session(ctx, hash); err != nil || !found {
		t.Fatalf("failed revocation removed the session: found=%v err=%v", found, err)
	}
	records, err := s.AuditRecords(ctx, 10)
	if err != nil || len(records) != 1 {
		t.Fatalf("failed revocation left partial audit: %d records err=%v", len(records), err)
	}
	// A rotation onto a taken hash must likewise keep the old session.
	other := strings.Repeat("cd", 32)
	if err := s.CreateSession(ctx, other, sess, AuditEvent{Actor: "s", Action: "login", Outcome: "success", Status: 200}); err != nil {
		t.Fatal(err)
	}
	if err := s.RotateSession(ctx, hash, other, sess, AuditEvent{Actor: "s", Action: "login", Outcome: "success", Status: 200}); err == nil {
		t.Fatal("rotation onto a taken hash must fail")
	}
	if got, found, err := s.Session(ctx, hash); err != nil || !found || got.Subject != "s" {
		t.Fatalf("failed rotation lost the old session: %#v found=%v err=%v", got, found, err)
	}
}

func TestFakePostgresFailClosedMatrix(t *testing.T) {
	ctx := context.Background()
	newStore := func(t *testing.T) (*PostgresStore, *fakePGDB) {
		t.Helper()
		db := newFakePGDB()
		return openFakePostgresOn(t, db, 1000, 365*24*time.Hour), db
	}
	validEvent := AuditEvent{Actor: "a", Action: "x", Outcome: "success", Status: 200}
	validSession := func() Session {
		return Session{Subject: "s", Issuer: "i", ExpiresAt: time.Now().Add(time.Hour)}
	}

	t.Run("open validation", func(t *testing.T) {
		if _, err := OpenPostgres(ctx, "postgres://x", 99, 365); err == nil {
			t.Error("low maxAuditItems accepted")
		}
		if _, err := OpenPostgres(ctx, "postgres://x", 100, 0); err == nil {
			t.Error("low retentionDays accepted")
		}
		if _, err := OpenPostgres(ctx, "://bad-dsn", 100, 365); err == nil {
			t.Error("unparseable DSN accepted")
		}
		if _, err := OpenPostgres(ctx, "postgres://127.0.0.1:1/nexaroute?connect_timeout=1", 100, 365); err == nil {
			t.Error("unreachable postgres accepted")
		}
	})

	t.Run("migrate failures fail closed", func(t *testing.T) {
		mk := func() (*PostgresStore, *fakePGDB) {
			db := newFakePGDB()
			return &PostgresStore{db: mustOpenFakeSQL(t, db), maxAuditItems: 1000, retention: time.Hour}, db
		}
		s, db := mk()
		db.failBegin = true
		if err := s.migrate(ctx); err == nil {
			t.Error("migrate with failing begin must fail")
		}
		for _, q := range []string{"pg_advisory_xact_lock", "CREATE TABLE", "DO NOTHING"} {
			s, db := mk()
			db.failExec = q
			if err := s.migrate(ctx); err == nil {
				t.Errorf("migrate with failing %q must fail", q)
			}
		}
		s, db = mk()
		db.failCommit = true
		if err := s.migrate(ctx); err == nil {
			t.Error("migrate with failing commit must fail")
		}
	})

	t.Run("writer tx failures", func(t *testing.T) {
		s, db := newStore(t)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := s.AppendAudit(canceled, validEvent); !isContextErr(err) {
			t.Errorf("canceled append err=%v", err)
		}
		// Missing allocator row: the writer lock read fails closed.
		db.mu.Lock()
		delete(db.meta, metaKeyAllocator)
		db.mu.Unlock()
		if err := s.AppendAudit(ctx, validEvent); err == nil {
			t.Error("append without an allocator row must fail")
		}
		db.mu.Lock()
		db.meta[metaKeyAllocator] = []byte("0")
		db.mu.Unlock()
		// Commit failure surfaces.
		db.failCommit = true
		if err := s.AppendAudit(ctx, validEvent); err == nil {
			t.Error("append with failing commit must fail")
		}
		db.failCommit = false
		// Begin failure surfaces.
		db.failBegin = true
		if err := s.AppendAudit(ctx, validEvent); err == nil {
			t.Error("append with failing begin must fail")
		}
		db.failBegin = false
	})

	t.Run("load chain corruption", func(t *testing.T) {
		s, db := newStore(t)
		if err := s.AppendAudit(ctx, validEvent); err != nil {
			t.Fatal(err)
		}
		// Malformed audit JSON fails verification with an error.
		db.mu.Lock()
		db.audit[1] = []byte("{not json")
		db.mu.Unlock()
		if _, err := s.VerifyAuditChain(ctx); err == nil {
			t.Error("malformed audit row must fail verification")
		}
		if err := s.AppendAudit(ctx, validEvent); err == nil {
			t.Error("append onto a malformed chain must fail")
		}
		db.mu.Lock()
		db.audit[1] = mustMarshal(t, AuditEvent{Timestamp: time.Now().UTC(), Actor: "a", Action: "x", Outcome: "success", Status: 200, Seq: 1, PrevHash: "", Hash: "bogus"})
		db.mu.Unlock()
		// Sequence-vs-row mismatch fails closed.
		fakePGCorruptAudit(t, db, 1, func(e *AuditEvent) { e.Seq = 99 })
		if _, err := s.VerifyAuditChain(ctx); err == nil {
			t.Error("seq-mismatched row must fail verification")
		}
		// Malformed allocator metadata fails closed.
		db.mu.Lock()
		db.meta[metaKeyAllocator] = []byte("{bad")
		db.mu.Unlock()
		if _, err := s.VerifyAuditChain(ctx); err == nil {
			t.Error("malformed allocator must fail verification")
		}
		// Injected load-query failure fails closed.
		db.failQuery = securityAuditTable
		if _, err := s.VerifyAuditChain(ctx); err == nil {
			t.Error("failing audit load must fail verification")
		}
		db.failQuery = ""
		db.failQuery = securityMetaTable
		if _, err := s.VerifyAuditChain(ctx); err == nil {
			t.Error("failing meta load must fail verification")
		}
		db.failQuery = ""
	})

	t.Run("session edges", func(t *testing.T) {
		s, db := newStore(t)
		if _, _, err := s.Session(ctx, "not-a-hash"); err == nil {
			t.Error("malformed session hash accepted")
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := s.Session(canceled, strings.Repeat("aa", 32)); !isContextErr(err) {
			t.Errorf("canceled session read err=%v", err)
		}
		if _, _, err := s.TouchSession(ctx, strings.Repeat("aa", 32), time.Now(), 0); err == nil {
			t.Error("non-positive idle timeout accepted")
		}
		if _, _, err := s.TouchSession(canceled, strings.Repeat("aa", 32), time.Now(), time.Minute); !isContextErr(err) {
			t.Errorf("canceled touch err=%v", err)
		}
		if err := s.RotateSession(ctx, "", "bad", validSession(), validEvent); err == nil {
			t.Error("malformed new hash accepted")
		}
		if err := s.RotateSession(ctx, "bad", strings.Repeat("aa", 32), validSession(), validEvent); err == nil {
			t.Error("malformed old hash accepted")
		}
		if err := s.CreateSession(ctx, strings.Repeat("aa", 32), Session{Issuer: "i", ExpiresAt: time.Now().Add(time.Hour)}, validEvent); err == nil {
			t.Error("subject-less session accepted")
		}
		if err := s.RevokeSession(ctx, "bad", validEvent); err == nil {
			t.Error("revoke with malformed hash accepted")
		}
		if err := s.RevokeSession(canceled, strings.Repeat("aa", 32), validEvent); !isContextErr(err) {
			t.Errorf("canceled revoke err=%v", err)
		}
		// Malformed stored session fails closed on read and touch.
		h := strings.Repeat("ef", 32)
		if err := s.CreateSession(ctx, h, validSession(), validEvent); err != nil {
			t.Fatal(err)
		}
		db.mu.Lock()
		db.sessions[h] = []byte("{bad")
		db.mu.Unlock()
		if _, _, err := s.Session(ctx, h); err == nil {
			t.Error("malformed stored session must fail on read")
		}
		if _, _, err := s.TouchSession(ctx, h, time.Now(), time.Hour); err == nil {
			t.Error("malformed stored session must fail on touch")
		}
		// Expired sessions are reaped by touch, idle ones too.
		h2 := strings.Repeat("01", 32)
		now := time.Now().UTC()
		expired := validSession()
		expired.CreatedAt, expired.LastSeen, expired.ExpiresAt = now.Add(-2*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour)
		if err := s.CreateSession(ctx, h2, expired, validEvent); err != nil {
			t.Fatal(err)
		}
		if _, found, err := s.TouchSession(ctx, h2, now, time.Hour); err != nil || found {
			t.Errorf("expired touch found=%v err=%v", found, err)
		}
		h3 := strings.Repeat("02", 32)
		idle := validSession()
		idle.CreatedAt, idle.LastSeen = now.Add(-2*time.Hour), now.Add(-2*time.Hour)
		idle.ExpiresAt = now.Add(time.Hour)
		if err := s.CreateSession(ctx, h3, idle, validEvent); err != nil {
			t.Fatal(err)
		}
		if _, found, err := s.TouchSession(ctx, h3, now, time.Hour); err != nil || found {
			t.Errorf("idle touch found=%v err=%v", found, err)
		}
		// Touch of a missing session is not-found, not an error.
		if _, found, err := s.TouchSession(ctx, strings.Repeat("03", 32), now, time.Hour); err != nil || found {
			t.Errorf("missing touch found=%v err=%v", found, err)
		}
		// Session load transport failure fails closed.
		db.failQuery = securitySessionsTable
		if _, _, err := s.Session(ctx, h); err == nil {
			t.Error("failing session load must fail")
		}
		db.failQuery = ""
	})

	t.Run("audit read edges", func(t *testing.T) {
		s, db := newStore(t)
		if err := s.AppendAudit(ctx, validEvent); err != nil {
			t.Fatal(err)
		}
		if recs, err := s.AuditRecords(ctx, 0); err != nil || len(recs) != 1 {
			t.Errorf("default limit: %d records err=%v", len(recs), err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.AuditRecords(canceled, 10); !isContextErr(err) {
			t.Errorf("canceled list err=%v", err)
		}
		if _, err := s.VerifyAuditChain(canceled); !isContextErr(err) {
			t.Errorf("canceled verify err=%v", err)
		}
		db.mu.Lock()
		db.audit[1] = []byte("{bad")
		db.mu.Unlock()
		if _, err := s.AuditRecords(ctx, 10); err == nil {
			t.Error("malformed audit row must fail the list")
		}
		db.failQuery = securityAuditTable
		if _, err := s.AuditRecords(ctx, 10); err == nil {
			t.Error("failing audit list must fail")
		}
		db.failQuery = ""
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func isContextErr(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "context canceled") || strings.Contains(err.Error(), "context deadline"))
}

func mustOpenFakeSQL(t *testing.T, db *fakePGDB) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("wavec-fake-pg-adhoc-%d", fakePGDriverSeq.Add(1))
	sql.Register(name, &fakePGDriver{db: db})
	raw, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	enc, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}
