package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"
)

type fakeRedis struct {
	key, value string
	evalScript string
	evalArgs   []string
}

func (f *fakeRedis) Get(context.Context, string) ([]byte, error) { return []byte(f.value), nil }
func (f *fakeRedis) SetNX(_ context.Context, key string, value []byte, _ time.Duration) (bool, error) {
	if f.value != "" {
		return false, nil
	}
	f.key, f.value = key, string(value)
	return true, nil
}
func (f *fakeRedis) Delete(context.Context, string) error { f.value = ""; return nil }
func (f *fakeRedis) Eval(_ context.Context, script string, _ []string, args ...string) (any, error) {
	f.evalScript, f.evalArgs = script, args
	return int64(1), nil
}

func TestAcquireLeaseUsesBoundedCompareDelete(t *testing.T) {
	f := &fakeRedis{}
	lease, ok, err := AcquireLease(context.Background(), f, "leader", []byte("owner-a"), time.Second)
	if err != nil || !ok || lease == nil {
		t.Fatalf("lease=%v ok=%v err=%v", lease, ok, err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.evalScript != compareDeleteScript || len(f.evalArgs) != 1 || f.evalArgs[0] != "owner-a" {
		t.Fatalf("unsafe release script=%q args=%v", f.evalScript, f.evalArgs)
	}
	if !strings.Contains(TokenBucketScript, "HMGET") || !strings.Contains(TokenBucketScript, "HSET") {
		t.Fatal("token bucket contract is incomplete")
	}
}
