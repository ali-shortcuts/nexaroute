package controlplane

import (
	"context"
	"errors"
	"time"
)

var ErrLeaseLost = errors.New("redis lease is no longer owned")

type RedisClient interface {
	Get(ctx context.Context, key string) ([]byte, error)
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Delete(ctx context.Context, key string) error
	Eval(ctx context.Context, script string, keys []string, args ...string) (any, error)
}

type Lease struct {
	client RedisClient
	key    string
	token  []byte
}

func AcquireLease(ctx context.Context, client RedisClient, key string, token []byte, ttl time.Duration) (*Lease, bool, error) {
	if client == nil || key == "" || len(token) == 0 || ttl <= 0 {
		return nil, false, ErrUnavailable
	}
	ok, err := client.SetNX(ctx, key, append([]byte(nil), token...), ttl)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return &Lease{client: client, key: key, token: append([]byte(nil), token...)}, true, nil
}

// ReleaseLease uses a compare-and-delete script so one owner cannot delete a
// lease acquired by a newer owner after its own TTL expired.
func (l *Lease) Release(ctx context.Context) error {
	if l == nil || l.client == nil {
		return ErrLeaseLost
	}
	_, err := l.client.Eval(ctx, compareDeleteScript, []string{l.key}, string(l.token))
	return err
}

const compareDeleteScript = `local current = redis.call('GET', KEYS[1]); if current == ARGV[1] then return redis.call('DEL', KEYS[1]) else return 0 end`

// TokenBucketScript is intentionally exposed as a stable contract. The Redis
// adapter can execute it atomically; callers must treat a non-positive result
// as a rejected reservation and never retry unboundedly.
const TokenBucketScript = `local now=tonumber(ARGV[1]); local cost=tonumber(ARGV[2]); local capacity=tonumber(ARGV[3]); local refill=tonumber(ARGV[4]); local state=redis.call('HMGET',KEYS[1],'tokens','ts'); local tokens=tonumber(state[1]) or capacity; local ts=tonumber(state[2]) or now; tokens=math.min(capacity,tokens+math.max(0,now-ts)*refill); if tokens<cost then redis.call('HSET',KEYS[1],'tokens',tokens,'ts',now); return 0 end; tokens=tokens-cost; redis.call('HSET',KEYS[1],'tokens',tokens,'ts',now); return 1`
