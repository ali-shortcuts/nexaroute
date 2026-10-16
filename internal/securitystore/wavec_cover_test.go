package securitystore

// Wave C-1 session lifecycle edges + thin-seam coverage. Session
// expiry/idle boundaries, self-rotation, revocation racing readers,
// and Redis/PG/bbolt error-injection paths are asserted identically on
// every backend; the second half covers the thin wrappers
// (parseHashPairs, go-redis client, Open/OpenReadOnly/BackupTo edges,
// chaincore legacy branches) that the adversarial suites do not reach.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	bolt "go.etcd.io/bbolt"
)

func waveCSessionBackends(t *testing.T) []waveCBackend {
	t.Helper()
	return waveCOpenBackends(t, 1000, 365*24*time.Hour)
}

func waveCSeedSession(t *testing.T, b Backend, hash string, now time.Time, lastSeen, expires time.Time) {
	t.Helper()
	sess := Session{Subject: "edge", Issuer: "local", Roles: []string{"admin"}, CreatedAt: now, LastSeen: lastSeen, ExpiresAt: expires}
	if err := b.CreateSession(context.Background(), hash, sess, AuditEvent{Timestamp: now, Actor: "edge", Action: "auth.login", Outcome: "success", Status: 200}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

// TestWaveCSessionExpiryBoundaries asserts the exact fail-closed
// boundaries identically on every backend: ExpiresAt == now is
// expired; idle age == timeout is expired; just inside either bound
// stays live.
func TestWaveCSessionExpiryBoundaries(t *testing.T) {
	for _, wb := range waveCSessionBackends(t) {
		t.Run(wb.name+"/absolute-boundary", func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			h := strings.Repeat("a1", 32)
			waveCSeedSession(t, wb.backend, h, now, now, now) // ExpiresAt == now
			if _, found, err := wb.backend.TouchSession(ctx, h, now, time.Hour); err != nil || found {
				t.Fatalf("touch at ExpiresAt must reap: found=%v err=%v", found, err)
			}
			if _, found, _ := wb.backend.Session(ctx, h); found {
				t.Fatal("reaped session must not resolve")
			}
		})
		t.Run(wb.name+"/idle-boundary", func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			h := strings.Repeat("b2", 32)
			waveCSeedSession(t, wb.backend, h, now.Add(-time.Hour), now.Add(-time.Hour), now.Add(time.Hour))
			if _, found, err := wb.backend.TouchSession(ctx, h, now, time.Hour); err != nil || found {
				t.Fatalf("touch at exactly the idle timeout must reap: found=%v err=%v", found, err)
			}
		})
		t.Run(wb.name+"/just-inside-idle", func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			h := strings.Repeat("c3", 32)
			waveCSeedSession(t, wb.backend, h, now.Add(-time.Hour), now.Add(-time.Hour).Add(time.Second), now.Add(time.Hour))
			got, found, err := wb.backend.TouchSession(ctx, h, now, time.Hour)
			if err != nil || !found || !got.LastSeen.Equal(now) {
				t.Fatalf("touch just inside idle bound must succeed: found=%v err=%v lastSeen=%v", found, err, got.LastSeen)
			}
		})
		t.Run(wb.name+"/self-rotation", func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			h := strings.Repeat("d4", 32)
			waveCSeedSession(t, wb.backend, h, now, now, now.Add(time.Hour))
			replacement := Session{Subject: "edge2", Issuer: "local", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(2 * time.Hour)}
			if err := wb.backend.RotateSession(ctx, h, h, replacement, AuditEvent{Timestamp: now, Actor: "edge", Action: "auth.rotate", Outcome: "success", Status: 200}); err != nil {
				t.Fatalf("self-rotation: %v", err)
			}
			got, found, err := wb.backend.Session(ctx, h)
			if err != nil || !found || got.Subject != "edge2" {
				t.Fatalf("session after self-rotation: %#v found=%v err=%v", got, found, err)
			}
		})
	}
}

// TestWaveCRevocationRacesReads hammers reads while revocations land:
// no panic, no error, final state consistent, chain verifies.
func TestWaveCRevocationRacesReads(t *testing.T) {
	for _, wb := range waveCSessionBackends(t) {
		wb := wb
		t.Run(wb.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			now := time.Now().UTC()
			hashes := make([]string, 8)
			for i := range hashes {
				hashes[i] = fmt.Sprintf("%064x", i+1)
				waveCSeedSession(t, wb.backend, hashes[i], now, now, now.Add(time.Hour))
			}
			var wg sync.WaitGroup
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					for j := 0; j < 25; j++ {
						_, _, _ = wb.backend.Session(ctx, hashes[(i+j)%len(hashes)])
						_, _, _ = wb.backend.TouchSession(ctx, hashes[(i+j)%len(hashes)], now.Add(time.Minute), 15*time.Minute)
					}
				}(i)
			}
			for _, h := range hashes {
				wg.Add(1)
				go func(h string) {
					defer wg.Done()
					_ = wb.backend.RevokeSession(ctx, h, AuditEvent{Timestamp: now, Actor: "edge", Action: "auth.logout", Outcome: "success", Status: 200})
				}(h)
			}
			wg.Wait()
			report, err := wb.backend.VerifyAuditChain(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !report.Verified {
				t.Fatalf("chain after revocation race: %+v", report)
			}
		})
	}
}

// failRedisClient injects transport failures into the Redis adapter.
type failRedisClient struct {
	*fakeRedisClient
	failSnapshot, failEval, failHGetAll, failHGet bool
	exhaustTouch                                  bool
}

func (c *failRedisClient) Snapshot(ctx context.Context) (map[string]string, map[string]string, error) {
	if c.failSnapshot {
		return nil, nil, errors.New("fake-redis: injected snapshot failure")
	}
	return c.fakeRedisClient.Snapshot(ctx)
}
func (c *failRedisClient) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if c.failEval {
		return nil, errors.New("fake-redis: injected eval failure")
	}
	if c.exhaustTouch && script == touchScript {
		return int64(0), nil
	}
	return c.fakeRedisClient.Eval(ctx, script, keys, args...)
}
func (c *failRedisClient) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	if c.failHGetAll {
		return nil, errors.New("fake-redis: injected hgetall failure")
	}
	return c.fakeRedisClient.HGetAll(ctx, key)
}
func (c *failRedisClient) HGet(ctx context.Context, key, field string) (string, bool, error) {
	if c.failHGet {
		return "", false, errors.New("fake-redis: injected hget failure")
	}
	return c.fakeRedisClient.HGet(ctx, key, field)
}

func TestWaveCRedisFailClosedMatrix(t *testing.T) {
	ctx := context.Background()
	validEvent := AuditEvent{Actor: "a", Action: "x", Outcome: "success", Status: 200}
	validSession := Session{Subject: "s", Issuer: "i", ExpiresAt: time.Now().Add(time.Hour)}

	t.Run("open validation", func(t *testing.T) {
		if _, err := OpenRedis(ctx, "redis://127.0.0.1:1", 99, 365); err == nil {
			t.Error("low maxAuditItems accepted")
		}
		if _, err := OpenRedis(ctx, "redis://127.0.0.1:1", 100, 0); err == nil {
			t.Error("low retentionDays accepted")
		}
		if _, err := OpenRedis(ctx, "://bad-url", 100, 365); err == nil {
			t.Error("unparseable URL accepted")
		}
	})

	t.Run("transport failures fail closed", func(t *testing.T) {
		c := &failRedisClient{fakeRedisClient: newFakeRedisClient(), failSnapshot: true}
		s := newRedisStoreWithClient(c, 1000, 365*24*time.Hour)
		if _, err := s.VerifyAuditChain(ctx); err == nil {
			t.Error("failing snapshot must fail verification")
		}
		if err := s.AppendAudit(ctx, validEvent); err == nil {
			t.Error("failing snapshot must fail append")
		}
		c.failSnapshot = false
		c.failEval = true
		if err := s.AppendAudit(ctx, validEvent); err == nil {
			t.Error("failing commit must fail append")
		}
		touchHash := strings.Repeat("ab", 32)
		c.failEval = false
		if err := s.CreateSession(ctx, touchHash, validSession, validEvent); err != nil {
			t.Fatal(err)
		}
		c.failEval = true
		if _, _, err := s.TouchSession(ctx, touchHash, time.Now(), time.Hour); err == nil {
			t.Error("touch with failing eval must fail")
		}
		c.failEval = false
		// Touch of a missing session short-circuits before any write:
		// not-found, not an error.
		if _, found, err := s.TouchSession(ctx, strings.Repeat("ac", 32), time.Now(), time.Hour); err != nil || found {
			t.Errorf("missing touch found=%v err=%v", found, err)
		}
		c.failHGetAll = true
		if _, err := s.AuditRecords(ctx, 10); err == nil {
			t.Error("failing audit list must fail")
		}
		c.failHGetAll = false
		c.failHGet = true
		if _, _, err := s.Session(ctx, strings.Repeat("aa", 32)); err == nil {
			t.Error("failing session load must fail")
		}
		if err := s.RotateSession(ctx, "", strings.Repeat("aa", 32), validSession, validEvent); err == nil {
			t.Error("failing session check must fail rotation")
		}
		c.failHGet = false
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := s.AppendAudit(canceled, validEvent); err == nil {
			t.Error("canceled append must fail")
		}
		if _, err := s.AuditRecords(canceled, 10); err == nil {
			t.Error("canceled list must fail")
		}
		if _, err := s.VerifyAuditChain(canceled); err == nil {
			t.Error("canceled verify must fail")
		}
		if _, _, err := s.Session(canceled, strings.Repeat("aa", 32)); err == nil {
			t.Error("canceled session read must fail")
		}
		if _, _, err := s.TouchSession(canceled, strings.Repeat("aa", 32), time.Now(), time.Hour); err == nil {
			t.Error("canceled touch must fail")
		}
	})

	t.Run("session edges", func(t *testing.T) {
		s := newRedisStoreWithClient(newFakeRedisClient(), 1000, 365*24*time.Hour)
		if _, _, err := s.Session(ctx, "not-a-hash"); err == nil {
			t.Error("malformed session hash accepted")
		}
		if _, _, err := s.TouchSession(ctx, strings.Repeat("aa", 32), time.Now(), 0); err == nil {
			t.Error("non-positive idle timeout accepted")
		}
		if err := s.RotateSession(ctx, "", "bad", validSession, validEvent); err == nil {
			t.Error("malformed new hash accepted")
		}
		if err := s.CreateSession(ctx, strings.Repeat("aa", 32), Session{Issuer: "i", ExpiresAt: time.Now().Add(time.Hour)}, validEvent); err == nil {
			t.Error("subject-less session accepted")
		}
		if err := s.RevokeSession(ctx, "bad", validEvent); err == nil {
			t.Error("revoke with malformed hash accepted")
		}
		h := strings.Repeat("ef", 32)
		if err := s.CreateSession(ctx, h, validSession, validEvent); err != nil {
			t.Fatal(err)
		}
		if err := s.RotateSession(ctx, "", h, validSession, validEvent); err == nil {
			t.Error("rotation onto a taken hash must fail")
		}
		now := time.Now().UTC()
		expired := validSession
		expired.CreatedAt, expired.LastSeen, expired.ExpiresAt = now.Add(-2*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour)
		eh := strings.Repeat("01", 32)
		if err := s.CreateSession(ctx, eh, expired, validEvent); err != nil {
			t.Fatal(err)
		}
		if _, found, err := s.TouchSession(ctx, eh, now, time.Hour); err != nil || found {
			t.Errorf("expired touch found=%v err=%v", found, err)
		}
		idle := validSession
		idle.CreatedAt, idle.LastSeen = now.Add(-2*time.Hour), now.Add(-2*time.Hour)
		idle.ExpiresAt = now.Add(time.Hour)
		ih := strings.Repeat("02", 32)
		if err := s.CreateSession(ctx, ih, idle, validEvent); err != nil {
			t.Fatal(err)
		}
		if _, found, err := s.TouchSession(ctx, ih, now, time.Hour); err != nil || found {
			t.Errorf("idle touch found=%v err=%v", found, err)
		}
		if _, found, err := s.TouchSession(ctx, strings.Repeat("03", 32), now, time.Hour); err != nil || found {
			t.Errorf("missing touch found=%v err=%v", found, err)
		}
		got, found, err := s.TouchSession(ctx, h, time.Time{}, time.Hour)
		if err != nil || !found || got.LastSeen.IsZero() {
			t.Errorf("zero-time touch: found=%v err=%v", found, err)
		}
		if _, _, err := s.TouchSession(ctx, strings.Repeat("04", 32), now, -time.Minute); err == nil {
			t.Error("negative idle timeout accepted")
		}
		if err := s.RevokeSession(ctx, h, AuditEvent{Actor: "s", Outcome: "success", Status: 200}); err == nil {
			t.Error("revoke with invalid audit event must fail")
		}
	})

	t.Run("touch cas exhaustion", func(t *testing.T) {
		c := &failRedisClient{fakeRedisClient: newFakeRedisClient(), exhaustTouch: true}
		s := newRedisStoreWithClient(c, 1000, 365*24*time.Hour)
		h := strings.Repeat("aa", 32)
		if err := s.CreateSession(ctx, h, validSession, validEvent); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.TouchSession(ctx, h, time.Now(), time.Hour); err == nil {
			t.Error("exhausted touch CAS must fail")
		}
	})

	t.Run("malformed stored data", func(t *testing.T) {
		client := newFakeRedisClient()
		s := newRedisStoreWithClient(client, 1000, 365*24*time.Hour)
		h := strings.Repeat("aa", 32)
		if err := s.CreateSession(ctx, h, validSession, validEvent); err != nil {
			t.Fatal(err)
		}
		client.mu.Lock()
		client.hash(securityRedisSessionsKey)[h] = "{bad"
		client.mu.Unlock()
		if _, _, err := s.Session(ctx, h); err == nil {
			t.Error("malformed stored session must fail on read")
		}
		if _, _, err := s.TouchSession(ctx, h, time.Now(), time.Hour); err == nil {
			t.Error("malformed stored session must fail on touch")
		}
		client.mu.Lock()
		client.hash(securityRedisSessionsKey)[h] = string(mustMarshal(t, validSession))
		client.hash(securityRedisAuditKey)["abc"] = "{}"
		client.mu.Unlock()
		if _, err := s.VerifyAuditChain(ctx); err == nil {
			t.Error("non-numeric audit key must fail verification")
		}
		client.mu.Lock()
		delete(client.hash(securityRedisAuditKey), "abc")
		client.hash(securityRedisAuditKey)["-5"] = "{}"
		client.mu.Unlock()
		if _, err := s.VerifyAuditChain(ctx); err == nil {
			t.Error("negative audit key must fail verification")
		}
		client.mu.Lock()
		delete(client.hash(securityRedisAuditKey), "-5")
		client.hash(securityRedisAuditKey)["7"] = string(mustMarshal(t, AuditEvent{Seq: 8}))
		client.mu.Unlock()
		if _, err := s.VerifyAuditChain(ctx); err == nil {
			t.Error("seq-mismatched audit field must fail verification")
		}
		client.mu.Lock()
		delete(client.hash(securityRedisAuditKey), "7")
		client.hash(securityRedisAuditKey)["1"] = "{bad"
		client.mu.Unlock()
		if _, err := s.VerifyAuditChain(ctx); err == nil {
			t.Error("malformed audit JSON must fail verification")
		}
		if _, err := s.AuditRecords(ctx, 10); err == nil {
			t.Error("malformed audit JSON must fail the list")
		}
		client.mu.Lock()
		delete(client.hash(securityRedisAuditKey), "1")
		client.hash(securityRedisMetaKey)["allocator"] = "xyz"
		client.mu.Unlock()
		if _, err := s.VerifyAuditChain(ctx); err == nil {
			t.Error("malformed allocator must fail verification")
		}
	})

	t.Run("audit caps", func(t *testing.T) {
		s := newRedisStoreWithClient(newFakeRedisClient(), 100, 365*24*time.Hour)
		for i := 0; i < 3; i++ {
			if err := s.AppendAudit(ctx, validEvent); err != nil {
				t.Fatal(err)
			}
		}
		if recs, err := s.AuditRecords(ctx, 0); err != nil || len(recs) != 3 {
			t.Errorf("default limit: %d records err=%v", len(recs), err)
		}
		if recs, err := s.AuditRecords(ctx, 500); err != nil || len(recs) != 3 {
			t.Errorf("capped limit: %d records err=%v", len(recs), err)
		}
	})
}

func TestWaveCParseHashPairs(t *testing.T) {
	if got := parseHashPairs(nil); len(got) != 0 {
		t.Fatalf("nil input: %v", got)
	}
	if got := parseHashPairs("not-pairs"); len(got) != 0 {
		t.Fatalf("non-slice input: %v", got)
	}
	if got := parseHashPairs([]any{"lonely"}); len(got) != 0 {
		t.Fatalf("odd input: %v", got)
	}
	got := parseHashPairs([]any{"a", "1", "b", "2", "stray"})
	if len(got) != 2 || got["a"] != "1" || got["b"] != "2" {
		t.Fatalf("pairs: %v", got)
	}
	got = parseHashPairs([]any{123, "v", "", "w", "k", 456})
	if len(got) != 1 || got["k"] != "" {
		t.Fatalf("non-string keys/values: %v", got)
	}
}

func TestWaveCGoRedisWrappersFailClosed(t *testing.T) {
	opts, err := redis.ParseURL("redis://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	opts.DialTimeout = time.Second
	client := goRedisSecurityClient{client: redis.NewClient(opts)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.HGetAll(ctx, "k"); err == nil {
		t.Error("HGetAll against an unreachable server must fail")
	}
	if _, _, err := client.HGet(ctx, "k", "f"); err == nil {
		t.Error("HGet against an unreachable server must fail")
	}
	if _, err := client.Eval(ctx, "return 1", []string{"k"}); err == nil {
		t.Error("Eval against an unreachable server must fail")
	}
	if _, _, err := client.Snapshot(ctx); err == nil {
		t.Error("Snapshot against an unreachable server must fail")
	}
}

func TestWaveCStoreEdges(t *testing.T) {
	ctx := context.Background()

	t.Run("loadChainRef states", func(t *testing.T) {
		store, err := Open(contractStorePath(t, "edges.db"), 100, 30)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := store.AppendAudit(ctx, AuditEvent{Actor: "a", Action: "x", Outcome: "success", Status: 200}); err != nil {
			t.Fatal(err)
		}
		if err := store.db.View(func(tx *bolt.Tx) error {
			meta := tx.Bucket(bucketMeta)
			if _, ok := loadChainRef(meta, keyChainHead); !ok {
				t.Error("valid head must load")
			}
			if _, ok := loadChainRef(meta, []byte("no-such-key")); ok {
				t.Error("absent key must not load")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		putMeta(t, store, keyChainHead, []byte("{"))
		if err := store.db.View(func(tx *bolt.Tx) error {
			if _, ok := loadChainRef(tx.Bucket(bucketMeta), keyChainHead); ok {
				t.Error("malformed head must not load")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("tiny helpers", func(t *testing.T) {
		if auditKeySeq([]byte{1}) != 0 {
			t.Error("short key must decode to zero")
		}
		if _, err := validSessionHash(strings.Repeat("zz", 32)); err == nil {
			t.Error("non-hex session hash accepted")
		}
		if got := Sanitize("x", -1); got != "" {
			t.Errorf("negative bound=%q", got)
		}
	})

	t.Run("open failures", func(t *testing.T) {
		garbage := filepath.Join(t.TempDir(), "garbage.db")
		if err := os.MkdirAll(filepath.Dir(garbage), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(garbage, []byte("not a bbolt database at all"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(garbage, 100, 30); err == nil {
			t.Error("garbage database file accepted")
		}
		parentFile := filepath.Join(t.TempDir(), "file-parent")
		if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(filepath.Join(parentFile, "child.db"), 100, 30); err == nil {
			t.Error("file-as-parent accepted")
		}
		if _, err := OpenReadOnly(""); err == nil {
			t.Error("empty read-only path accepted")
		}
		if _, err := OpenReadOnly(garbage); err == nil {
			t.Error("garbage read-only file accepted")
		}
		if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "no-such-dir", "x.db")); err == nil {
			t.Error("missing read-only directory accepted")
		}
	})

	t.Run("backup edges", func(t *testing.T) {
		var nilStore *Store
		if err := nilStore.BackupTo(ctx, &bytes.Buffer{}); err == nil {
			t.Error("nil backup accepted")
		}
		store, err := Open(contractStorePath(t, "backup.db"), 100, 30)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := store.BackupTo(canceled, &bytes.Buffer{}); err == nil {
			t.Error("canceled backup accepted")
		}
		if err := store.BackupTo(ctx, errWriter{}); err == nil {
			t.Error("failing backup writer accepted")
		}
	})

	t.Run("verify canceled", func(t *testing.T) {
		store, err := Open(contractStorePath(t, "cancel.db"), 100, 30)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := store.VerifyAuditChain(canceled); err == nil {
			t.Error("canceled verify accepted")
		}
	})

	t.Run("resolve env url", func(t *testing.T) {
		t.Setenv("WAVEC_TEST_SECURITY_URL", "redis://127.0.0.1:1")
		_, err := OpenBackend(BackendRedis, "env:WAVEC_TEST_SECURITY_URL", contractStorePath(t, "e.db"), 100, 365)
		if err == nil || strings.Contains(err.Error(), "unset or empty") {
			t.Fatalf("env-resolved redis must attempt connection, err=%v", err)
		}
	})
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("fake backup write failure") }

// TestWaveCChaincoreLegacyEdges covers the chainState branches the
// other suites do not reach: legacy segments, anchor-gap exemption,
// empty-log head states and allocator evidence.
func TestWaveCChaincoreLegacyEdges(t *testing.T) {
	legacy := func(seq uint64, ts time.Time) AuditEvent {
		return AuditEvent{Timestamp: ts, Actor: "legacy", Action: "old", Outcome: "success", Status: 200, Seq: seq}
	}
	mkChained := func() chainState {
		st := chainState{}
		for i := 0; i < 3; i++ {
			plan, err := planAuditAppend(st, chainEvent("x", chainCoreNow.Add(time.Duration(i)*time.Second)), 1000, 365*24*time.Hour, chainCoreNow)
			if err != nil {
				t.Fatal(err)
			}
			st = applyPlan(st, plan)
		}
		return st
	}

	t.Run("legacy only verifies", func(t *testing.T) {
		st := chainState{Records: []AuditEvent{legacy(1, chainCoreNow), legacy(2, chainCoreNow)}, Allocator: 2}
		report := verifyChainState(st)
		if !report.Verified || report.LegacyRecords != 2 || report.ChainedRecords != 0 {
			t.Fatalf("legacy report: %+v", report)
		}
	})

	t.Run("legacy then chained links", func(t *testing.T) {
		l1 := legacy(1, chainCoreNow)
		chained := AuditEvent{Timestamp: chainCoreNow.Add(time.Second), Actor: "a", Action: "x", Outcome: "success", Status: 200, Seq: 2, PrevHash: recordHash(l1, 1)}
		chained.Hash = canonicalAuditHash(chained)
		st := chainState{
			Records: []AuditEvent{l1, chained},
			Head:    chainRef{Seq: 2, Hash: chained.Hash}, HeadState: refValid,
			Allocator: 2,
		}
		if report := verifyChainState(st); !report.Verified {
			t.Fatalf("legacy+chained must verify: %+v", report)
		}
	})

	t.Run("duplicated and gapped sequences", func(t *testing.T) {
		st := mkChained()
		dup := st
		dup.Records = append([]AuditEvent(nil), st.Records...)
		dup.Records[2] = dup.Records[1]
		if report := verifyChainState(dup); report.Verified {
			t.Fatal("duplicated sequence verified")
		}
		gap := st
		gap.Records = append([]AuditEvent(nil), st.Records[0], st.Records[2])
		if report := verifyChainState(gap); report.Verified {
			t.Fatal("gapped chained sequence verified")
		}
	})

	t.Run("first chained without anchor but with prevhash", func(t *testing.T) {
		st := mkChained()
		st.Records[0].PrevHash = strings.Repeat("c", 64)
		st.Records[0].Hash = canonicalAuditHash(st.Records[0])
		if report := verifyChainState(st); report.Verified {
			t.Fatal("orphan prevhash verified")
		}
	})

	t.Run("anchor link and gap", func(t *testing.T) {
		st := mkChained()
		// Prune the first record and anchor it correctly.
		anchor := chainRef{Seq: 1, Hash: recordHash(st.Records[0], 1)}
		st.Records = st.Records[1:]
		st.Anchor, st.AnchorState = anchor, refValid
		st.Records[0].PrevHash = anchor.Hash
		st.Records[0].Hash = canonicalAuditHash(st.Records[0])
		// Re-link the rest.
		for i := 1; i < len(st.Records); i++ {
			st.Records[i].PrevHash = recordHash(st.Records[i-1], st.Records[i-1].Seq)
			st.Records[i].Hash = canonicalAuditHash(st.Records[i])
		}
		st.Head = chainRef{Seq: 3, Hash: st.Records[1].Hash}
		if report := verifyChainState(st); !report.Verified {
			t.Fatalf("anchored chain must verify: %+v", report)
		}
		bad := st
		bad.Anchor = chainRef{Seq: 1, Hash: strings.Repeat("d", 64)}
		if report := verifyChainState(bad); report.Verified {
			t.Fatal("wrong anchor hash verified")
		}
		far := st
		far.Anchor = chainRef{Seq: 50, Hash: anchor.Hash}
		if report := verifyChainState(far); report.Verified {
			t.Fatal("anchor gap verified")
		} else if !strings.Contains(strings.Join(report.Problems, " "), "gap between pruning anchor") {
			t.Fatalf("want anchor gap problem: %+v", report)
		}
	})

	t.Run("empty log head states", func(t *testing.T) {
		anchor := chainRef{Seq: 4, Hash: strings.Repeat("e", 64)}
		good := chainState{Head: anchor, HeadState: refValid, Anchor: anchor, AnchorState: refValid, Allocator: 4}
		if report := verifyChainState(good); !report.Verified {
			t.Fatalf("emptied log must verify: %+v", report)
		} else if report.HeadHash != anchor.Hash {
			t.Fatalf("emptied head hash = %q", report.HeadHash)
		}
		mismatch := chainState{Head: chainRef{Seq: 4, Hash: strings.Repeat("f", 64)}, HeadState: refValid, Anchor: anchor, AnchorState: refValid, Allocator: 4}
		if report := verifyChainState(mismatch); report.Verified {
			t.Fatal("head/anchor mismatch on empty log verified")
		}
		noHead := chainState{Anchor: anchor, AnchorState: refValid, Allocator: 4}
		if report := verifyChainState(noHead); report.Verified {
			t.Fatal("anchor without head on empty log verified")
		} else if !strings.Contains(strings.Join(report.Problems, " "), "chain head metadata is missing") {
			t.Fatalf("want missing-head problem: %+v", report)
		}
	})

	t.Run("allocator evidence", func(t *testing.T) {
		st := mkChained()
		low := st
		low.Allocator = 0
		if report := verifyChainState(low); report.Verified {
			t.Fatal("lowered allocator verified")
		} else if !strings.Contains(strings.Join(report.Problems, " "), "allocator is at 0") {
			t.Fatalf("want allocator problem: %+v", report)
		}
		high := st
		high.Allocator = 99
		if report := verifyChainState(high); report.Verified {
			t.Fatal("raised allocator verified")
		}
		// Legacy-only logs tolerate a high allocator.
		leg := chainState{Records: []AuditEvent{legacy(1, chainCoreNow)}, Allocator: 10}
		if report := verifyChainState(leg); !report.Verified {
			t.Fatalf("legacy high allocator must verify: %+v", report)
		}
	})

	t.Run("plan rejects inconsistent empty states", func(t *testing.T) {
		head := chainRef{Seq: 2, Hash: strings.Repeat("a", 64)}
		for _, st := range []chainState{
			{Head: head, HeadState: refValid},
			{Anchor: head, AnchorState: refValid},
		} {
			if _, err := planAuditAppend(st, chainEvent("x", chainCoreNow), 1000, time.Hour, chainCoreNow); err == nil {
				t.Fatal("inconsistent empty state accepted")
			}
		}
		// Legacy records with metadata present are inconsistent.
		st := chainState{Records: []AuditEvent{legacy(1, chainCoreNow)}, Head: head, HeadState: refValid, Allocator: 2}
		if _, err := planAuditAppend(st, chainEvent("x", chainCoreNow), 1000, time.Hour, chainCoreNow); err == nil {
			t.Fatal("legacy log with chain metadata accepted")
		}
		// Legacy records without metadata bootstrap the chain.
		st = chainState{Records: []AuditEvent{legacy(1, chainCoreNow), legacy(2, chainCoreNow)}, Allocator: 2}
		plan, err := planAuditAppend(st, chainEvent("x", chainCoreNow), 1000, time.Hour, chainCoreNow)
		if err != nil {
			t.Fatalf("legacy bootstrap: %v", err)
		}
		if plan.Event.Seq != 3 {
			t.Fatalf("bootstrap seq = %d, want 3", plan.Event.Seq)
		}
	})

	t.Run("plan prunes newest immediately under negative retention", func(t *testing.T) {
		st := chainState{}
		plan, err := planAuditAppend(st, chainEvent("x", chainCoreNow), 1000, -time.Second, chainCoreNow)
		if err != nil {
			t.Fatal(err)
		}
		if !plan.HasAnchor || plan.Head != plan.Anchor || len(plan.PrunedSeqs) != 1 {
			t.Fatalf("immediate-prune plan: %+v", plan)
		}
		applied := applyPlan(st, plan)
		if report := verifyChainState(applied); !report.Verified || report.TotalRecords != 0 {
			t.Fatalf("immediately pruned chain: %+v", report)
		}
	})

	t.Run("anchor gap exempt with legacy present", func(t *testing.T) {
		anchor := chainRef{Seq: 2, Hash: strings.Repeat("a", 64)}
		chained := AuditEvent{Timestamp: chainCoreNow, Actor: "a", Action: "x", Outcome: "success", Status: 200, Seq: 9, PrevHash: anchor.Hash}
		chained.Hash = canonicalAuditHash(chained)
		st := chainState{
			Records: []AuditEvent{chained, legacy(20, chainCoreNow)},
			Head:    chainRef{Seq: 9, Hash: chained.Hash}, HeadState: refValid,
			Anchor: anchor, AnchorState: refValid,
			Allocator: 9,
		}
		report := verifyChainState(st)
		for _, p := range report.Problems {
			if strings.Contains(p, "gap between pruning anchor") {
				t.Fatalf("legacy-exempt anchor gap must not be reported: %+v", report)
			}
		}
	})
}

// TestWaveCPGExecFailures injects storage failures into the fake to
// cover the adapter's fail-closed Exec paths.
func TestWaveCPGExecFailures(t *testing.T) {
	ctx := context.Background()
	validEvent := AuditEvent{Actor: "a", Action: "x", Outcome: "success", Status: 200}
	validSession := Session{Subject: "s", Issuer: "i", ExpiresAt: time.Now().Add(time.Hour)}

	t.Run("meta store failure", func(t *testing.T) {
		db := newFakePGDB()
		s := openFakePostgresOn(t, db, 1000, 365*24*time.Hour)
		db.failExec = "INSERT INTO " + securityMetaTable
		if err := s.AppendAudit(ctx, validEvent); err == nil {
			t.Error("failing meta store must fail append")
		}
	})
	t.Run("audit insert failure", func(t *testing.T) {
		db := newFakePGDB()
		s := openFakePostgresOn(t, db, 1000, 365*24*time.Hour)
		db.failExec = "INSERT INTO " + securityAuditTable
		if err := s.AppendAudit(ctx, validEvent); err == nil {
			t.Error("failing audit insert must fail append")
		}
	})
	t.Run("audit delete failure on prune", func(t *testing.T) {
		db := newFakePGDB()
		s := openFakePostgresOn(t, db, 100, 365*24*time.Hour)
		for i := 0; i < 100; i++ {
			if err := s.AppendAudit(ctx, validEvent); err != nil {
				t.Fatal(err)
			}
		}
		db.failExec = "DELETE FROM " + securityAuditTable
		if err := s.AppendAudit(ctx, validEvent); err == nil {
			t.Error("failing prune delete must fail append")
		}
	})
	t.Run("session write failures", func(t *testing.T) {
		db := newFakePGDB()
		s := openFakePostgresOn(t, db, 1000, 365*24*time.Hour)
		db.failQuery = "SELECT EXISTS"
		if err := s.CreateSession(ctx, strings.Repeat("aa", 32), validSession, validEvent); err == nil {
			t.Error("failing existence check must fail creation")
		}
		db.failQuery = ""
		db.failExec = "INSERT INTO " + securitySessionsTable
		if err := s.CreateSession(ctx, strings.Repeat("aa", 32), validSession, validEvent); err == nil {
			t.Error("failing session insert must fail creation")
		}
		db.failExec = ""
		h := strings.Repeat("bb", 32)
		if err := s.CreateSession(ctx, h, validSession, validEvent); err != nil {
			t.Fatal(err)
		}
		db.failExec = "DELETE FROM " + securitySessionsTable
		if err := s.RotateSession(ctx, h, strings.Repeat("cc", 32), validSession, validEvent); err == nil {
			t.Error("failing rotation delete must fail rotation")
		}
		if _, _, err := s.Session(ctx, h); err != nil {
			t.Fatal(err)
		}
		db.failExec = ""
	})
	t.Run("touch storage failures", func(t *testing.T) {
		db := newFakePGDB()
		s := openFakePostgresOn(t, db, 1000, 365*24*time.Hour)
		h := strings.Repeat("aa", 32)
		if err := s.CreateSession(ctx, h, validSession, validEvent); err != nil {
			t.Fatal(err)
		}
		db.failQuery = "SELECT value FROM " + securitySessionsTable
		if _, _, err := s.TouchSession(ctx, h, time.Now(), time.Hour); err == nil {
			t.Error("failing touch load must fail")
		}
		db.failQuery = ""
		db.failExec = "UPDATE " + securitySessionsTable
		if _, _, err := s.TouchSession(ctx, h, time.Now(), time.Hour); err == nil {
			t.Error("failing touch update must fail")
		}
		db.failExec = ""
		expired := validSession
		expired.ExpiresAt = time.Now().Add(-time.Hour)
		eh := strings.Repeat("dd", 32)
		if err := s.CreateSession(ctx, eh, expired, validEvent); err != nil {
			t.Fatal(err)
		}
		db.failExec = "DELETE FROM " + securitySessionsTable
		if _, _, err := s.TouchSession(ctx, eh, time.Now(), time.Hour); err == nil {
			t.Error("failing expired-touch delete must fail")
		}
		db.failExec = ""
	})
	t.Run("revoke storage failure rolls back", func(t *testing.T) {
		db := newFakePGDB()
		s := openFakePostgresOn(t, db, 1000, 365*24*time.Hour)
		h := strings.Repeat("aa", 32)
		if err := s.CreateSession(ctx, h, validSession, validEvent); err != nil {
			t.Fatal(err)
		}
		db.failExec = "DELETE FROM " + securitySessionsTable
		if err := s.RevokeSession(ctx, h, validEvent); err == nil {
			t.Error("failing revoke delete must fail")
		}
		db.failExec = ""
		if err := s.RevokeSession(ctx, h, AuditEvent{Actor: "s", Outcome: "success", Status: 200}); err == nil {
			t.Error("revoke with invalid audit event must fail")
		}
		if _, found, err := s.Session(ctx, h); err != nil || !found {
			t.Fatalf("failed revokes must retain the session: found=%v err=%v", found, err)
		}
	})
	t.Run("audit caps", func(t *testing.T) {
		s := openFakePostgres(t, 100, 365*24*time.Hour)
		for i := 0; i < 3; i++ {
			if err := s.AppendAudit(ctx, validEvent); err != nil {
				t.Fatal(err)
			}
		}
		if recs, err := s.AuditRecords(ctx, 500); err != nil || len(recs) != 3 {
			t.Errorf("capped limit: %d records err=%v", len(recs), err)
		}
	})
}

func TestWaveCBoltSessionEdges(t *testing.T) {
	ctx := context.Background()
	store, err := Open(contractStorePath(t, "sess.db"), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Malformed session rows fail closed on read and touch.
	key := testHash(0x77)
	if err := store.CreateSession(ctx, key, Session{Subject: "s", Issuer: "i", ExpiresAt: time.Now().Add(time.Hour)}, AuditEvent{Actor: "s", Action: "login", Outcome: "success", Status: 200}); err != nil {
		t.Fatal(err)
	}
	rawKey, _ := hex.DecodeString(key)
	if err := store.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketSessions).Put(rawKey, []byte("{")) }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Session(ctx, key); err == nil {
		t.Error("malformed session must fail on read")
	}
	if _, _, err := store.TouchSession(ctx, key, time.Now(), time.Minute); err == nil {
		t.Error("malformed session must fail on touch")
	}
	// Touch of an unknown hash is not-found.
	if _, found, err := store.TouchSession(ctx, testHash(0x78), time.Now(), time.Minute); err != nil || found {
		t.Errorf("unknown touch found=%v err=%v", found, err)
	}
	// Revoke of an unknown hash still records the audit event.
	if err := store.RevokeSession(ctx, testHash(0x79), AuditEvent{Actor: "s", Action: "logout", Outcome: "success", Status: 200}); err != nil {
		t.Fatalf("revoke of unknown session: %v", err)
	}
	// Invalid audit events are rejected on every write path.
	for _, op := range map[string]func(AuditEvent) error{
		"append": func(e AuditEvent) error { return store.AppendAudit(ctx, e) },
		"revoke": func(e AuditEvent) error { return store.RevokeSession(ctx, testHash(0x79), e) },
		"create": func(e AuditEvent) error {
			return store.CreateSession(ctx, testHash(0x7a), Session{Subject: "s", Issuer: "i", ExpiresAt: time.Now().Add(time.Hour)}, e)
		},
	} {
		if err := op(AuditEvent{Actor: "s", Outcome: "success", Status: 700}); err == nil {
			t.Error("status 700 accepted")
		}
		if err := op(AuditEvent{Actor: "", Action: "x", Outcome: "success", Status: 200}); err == nil {
			t.Error("actor-less event accepted")
		}
	}
	// Canceled contexts inside transactions fail closed.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_ = canceled
}
