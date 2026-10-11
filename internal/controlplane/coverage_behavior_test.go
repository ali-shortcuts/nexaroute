package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"
)

type coverageRedis struct {
	setOK   bool
	setErr  error
	evalErr error
}

func (c *coverageRedis) Get(context.Context, string) ([]byte, error) { return nil, nil }
func (c *coverageRedis) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return c.setOK, c.setErr
}
func (c *coverageRedis) Delete(context.Context, string) error { return nil }
func (c *coverageRedis) Eval(context.Context, string, []string, ...string) (any, error) {
	return nil, c.evalErr
}

func TestManagerConstructorAndLoadErrorPolicy(t *testing.T) {
	if _, err := NewManager(nil, FailOpen); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil store error=%v", err)
	}
	if _, err := NewManager(NewMemoryStore(), FailureMode("bad")); !errors.Is(err, ErrInvalidRevision) {
		t.Fatalf("invalid mode error=%v", err)
	}
	store := NewMemoryStore()
	manager, err := NewManager(store, LastKnownGood)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Load(ctx, "config"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled load error=%v", err)
	}
	store.SetUnavailable(true)
	if _, err := manager.Load(context.Background(), "missing"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("uncached unavailable load error=%v", err)
	}
}

func TestAcquireLeaseValidationContentionAndClientErrors(t *testing.T) {
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"nil client": func() error { _, _, err := AcquireLease(ctx, nil, "key", []byte("token"), time.Second); return err },
		"empty key": func() error {
			_, _, err := AcquireLease(ctx, &coverageRedis{}, "", []byte("token"), time.Second)
			return err
		},
		"empty token": func() error { _, _, err := AcquireLease(ctx, &coverageRedis{}, "key", nil, time.Second); return err },
		"bad ttl":     func() error { _, _, err := AcquireLease(ctx, &coverageRedis{}, "key", []byte("token"), 0); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	if _, _, err := AcquireLease(ctx, &coverageRedis{setErr: errors.New("redis down")}, "key", []byte("token"), time.Second); err == nil {
		t.Fatal("SetNX failure was swallowed")
	}
	if lease, ok, err := AcquireLease(ctx, &coverageRedis{setOK: false}, "key", []byte("token"), time.Second); err != nil || ok || lease != nil {
		t.Fatalf("contended lease=%v ok=%v err=%v", lease, ok, err)
	}
	lease, ok, err := AcquireLease(ctx, &coverageRedis{setOK: true, evalErr: errors.New("eval failed")}, "key", []byte("token"), time.Second)
	if err != nil || !ok || lease == nil {
		t.Fatalf("acquire lease=%v ok=%v err=%v", lease, ok, err)
	}
	if err := lease.Release(ctx); err == nil {
		t.Fatal("Eval failure was swallowed")
	}
	var nilLease *Lease
	if err := nilLease.Release(ctx); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("nil lease error=%v", err)
	}
}
