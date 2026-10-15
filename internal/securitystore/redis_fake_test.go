package securitystore

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeRedisClient emulates the two Lua scripts' semantics in memory
// so the adapter's CAS logic is exercised without a server. The real
// scripts run against a real Redis in the gated integration suite.
type fakeRedisClient struct {
	mu     sync.Mutex
	hashes map[string]map[string]string
}

func newFakeRedisClient() *fakeRedisClient {
	return &fakeRedisClient{hashes: map[string]map[string]string{}}
}

func (f *fakeRedisClient) hash(key string) map[string]string {
	h, ok := f.hashes[key]
	if !ok {
		h = map[string]string{}
		f.hashes[key] = h
	}
	return h
}

func (f *fakeRedisClient) HGetAll(_ context.Context, key string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.hashes[key] {
		out[k] = v
	}
	return out, nil
}

func (f *fakeRedisClient) HGet(_ context.Context, key, field string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.hashes[key][field]
	return v, ok, nil
}

func (f *fakeRedisClient) Eval(_ context.Context, script string, keys []string, args ...any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch script {
	case commitScript:
		meta := f.hash(keys[0])
		version := meta["version"]
		if version == "" {
			version = "0"
		}
		if version != args[0].(string) {
			return int64(0), nil
		}
		var ch redisChanges
		if err := json.Unmarshal([]byte(args[1].(string)), &ch); err != nil {
			return nil, err
		}
		audit := f.hash(keys[1])
		for _, field := range ch.AuditDel {
			delete(audit, field)
		}
		if ch.AuditPut != nil {
			audit[ch.AuditPut.Seq] = ch.AuditPut.JSON
		}
		sessions := f.hash(keys[2])
		for _, field := range ch.SessionDel {
			delete(sessions, field)
		}
		for _, p := range ch.SessionPut {
			sessions[p.Hash] = p.JSON
		}
		n, _ := strconv.Atoi(version)
		meta["head"] = ch.Head
		meta["allocator"] = ch.Allocator
		meta["version"] = strconv.Itoa(n + 1)
		if ch.Anchor != nil {
			meta["anchor"] = *ch.Anchor
		}
		return int64(1), nil
	case touchScript:
		sessions := f.hash(keys[0])
		cur := sessions[args[0].(string)]
		if cur != args[1].(string) {
			return int64(0), nil
		}
		if args[2].(string) == "" {
			delete(sessions, args[0].(string))
		} else {
			sessions[args[0].(string)] = args[2].(string)
		}
		return int64(1), nil
	}
	return nil, errFakeUnknownScript
}

var errFakeUnknownScript = &fakeError{"unknown script"}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

func (f *fakeRedisClient) Snapshot(_ context.Context) (map[string]string, map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := func(key string) map[string]string {
		out := map[string]string{}
		for k, v := range f.hashes[key] {
			out[k] = v
		}
		return out
	}
	return cp(securityRedisMetaKey), cp(securityRedisAuditKey), nil
}

func (f *fakeRedisClient) Ping(context.Context) error { return nil }
func (f *fakeRedisClient) Close() error               { return nil }

// conflictClient simulates lost CAS races: the first `conflicts`
// commit attempts report a version mismatch without applying.
type conflictClient struct {
	*fakeRedisClient
	mu        sync.Mutex
	conflicts int
}

func (c *conflictClient) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if script == commitScript {
		c.mu.Lock()
		if c.conflicts > 0 {
			c.conflicts--
			c.mu.Unlock()
			return int64(0), nil
		}
		c.mu.Unlock()
	}
	return c.fakeRedisClient.Eval(ctx, script, keys, args...)
}

func TestRedisBackendContractAgainstFake(t *testing.T) {
	runBackendContract(t, func(t *testing.T) Backend {
		t.Helper()
		return newRedisStoreWithClient(newFakeRedisClient(), 1000, 365*24*time.Hour)
	})
}

func TestRedisCASRetriesThroughConflicts(t *testing.T) {
	client := &conflictClient{fakeRedisClient: newFakeRedisClient(), conflicts: 5}
	store := newRedisStoreWithClient(client, 1000, 365*24*time.Hour)
	ctx := context.Background()
	if err := store.AppendAudit(ctx, AuditEvent{Actor: "alice", Action: "auth.login", Outcome: "success", Status: 200}); err != nil {
		t.Fatalf("append through conflicts: %v", err)
	}
	report, err := store.VerifyAuditChain(ctx)
	if err != nil || !report.Verified || report.ChainedRecords != 1 {
		t.Fatalf("report: err=%v %+v", err, report)
	}
}

// Regression for the torn-snapshot failure the real-Redis CI suite
// caught: meta and audit read in separate round trips can straddle a
// concurrent commit (head seq N with records through N+1), and the
// chain gate then refuses a healthy chain. The fake's per-call
// locking allows exactly that tear, so this test fails against
// non-atomic snapshot reads and passes with the Lua snapshot.
func TestRedisTwoStoresConcurrentAgainstFake(t *testing.T) {
	shared := newFakeRedisClient()
	a := newRedisStoreWithClient(shared, 1000, 365*24*time.Hour)
	b := newRedisStoreWithClient(shared, 1000, 365*24*time.Hour)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for _, store := range []*RedisStore{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if err := store.AppendAudit(ctx, AuditEvent{Actor: "writer", Action: "contract.concurrent", Outcome: "success", Status: 200}); err != nil {
					errs <- err
				}
			}
		}()
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
}

func TestRedisCASExhaustionFailsClosed(t *testing.T) {
	client := &conflictClient{fakeRedisClient: newFakeRedisClient(), conflicts: 1000}
	store := newRedisStoreWithClient(client, 1000, 365*24*time.Hour)
	if err := store.AppendAudit(context.Background(), AuditEvent{Actor: "alice", Action: "auth.login", Outcome: "success", Status: 200}); err == nil {
		t.Fatal("append must fail after the CAS retry budget is exhausted")
	}
}
