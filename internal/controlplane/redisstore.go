package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// RedisHashClient is the minimal Redis surface RedisStore needs. The
// gateway binary adapts a concrete Redis client to it; this package
// stays client-agnostic, like SQLStore stays driver-agnostic.
type RedisHashClient interface {
	HGetAll(ctx context.Context, key string) (map[string]string, error)
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)
}

// redisSnapshotPrefix namespaces every control-plane snapshot key.
const redisSnapshotPrefix = "nexaroute:control:snapshot:"

// RedisStore implements Store on Redis. Each namespace is one Redis hash
// (revision, schema, payload, updated_at). Writes and deletes are
// compare-and-set Lua scripts, which Redis executes atomically, so
// revision semantics match the SQL and file stores exactly: a writer
// with a stale expected revision gets ErrConflict, never a silent
// overwrite.
//
// Durability note: Redis is only as durable as its own configuration.
// Operators relying on this backend for durable control-plane state must
// run Redis with persistence (AOF and/or RDB) and appropriate replica /
// failover management; the store reports Redis errors as failures and
// never converts them into false success.
type RedisStore struct {
	client RedisHashClient
}

func NewRedisStore(client RedisHashClient) (*RedisStore, error) {
	if client == nil {
		return nil, errors.New("nil redis client")
	}
	return &RedisStore{client: client}, nil
}

func (s *RedisStore) key(namespace string) string { return redisSnapshotPrefix + namespace }

func (s *RedisStore) Get(ctx context.Context, namespace string) (Snapshot, error) {
	fields, err := s.client.HGetAll(ctx, s.key(namespace))
	if err != nil {
		return Snapshot{}, fmt.Errorf("get control snapshot: %w", err)
	}
	if len(fields) == 0 {
		return Snapshot{}, ErrNotFound
	}
	revision, err := strconv.ParseUint(fields["revision"], 10, 64)
	if err != nil || revision == 0 {
		return Snapshot{}, fmt.Errorf("get control snapshot: corrupt record for namespace %q (revision)", namespace)
	}
	schema, err := strconv.Atoi(fields["schema"])
	if err != nil || schema <= 0 {
		return Snapshot{}, fmt.Errorf("get control snapshot: corrupt record for namespace %q (schema)", namespace)
	}
	payload, ok := fields["payload"]
	if !ok {
		return Snapshot{}, fmt.Errorf("get control snapshot: corrupt record for namespace %q (payload)", namespace)
	}
	updated, err := time.Parse(time.RFC3339Nano, fields["updated_at"])
	if err != nil {
		return Snapshot{}, fmt.Errorf("get control snapshot: corrupt record for namespace %q (updated_at)", namespace)
	}
	return Snapshot{Revision: revision, Schema: schema, Payload: []byte(payload), UpdatedAt: updated}, nil
}

// redisPutScript performs the revision compare-and-set atomically and
// returns {1, schema} on success or {0, ""} on conflict. An existing
// record's schema is preserved; a new record takes the caller's schema.
const redisPutScript = `
local cur = redis.call('HGET', KEYS[1], 'revision')
local curRev = tonumber(cur) or 0
if curRev ~= tonumber(ARGV[1]) then return {0, ''} end
local schema = redis.call('HGET', KEYS[1], 'schema') or ARGV[3]
redis.call('HSET', KEYS[1], 'revision', ARGV[2], 'schema', schema, 'payload', ARGV[4], 'updated_at', ARGV[5])
return {1, schema}`

func (s *RedisStore) Put(ctx context.Context, namespace string, expectedRevision uint64, payload []byte) (Snapshot, error) {
	if namespace == "" || len(payload) == 0 {
		return Snapshot{}, ErrNotFound
	}
	next := expectedRevision + 1
	now := time.Now().UTC()
	res, err := s.client.Eval(ctx, redisPutScript, []string{s.key(namespace)},
		strconv.FormatUint(expectedRevision, 10),
		strconv.FormatUint(next, 10),
		"1",
		string(payload),
		now.Format(time.RFC3339Nano))
	if err != nil {
		return Snapshot{}, fmt.Errorf("write control snapshot: %w", err)
	}
	parts, ok := res.([]any)
	if !ok || len(parts) != 2 {
		return Snapshot{}, fmt.Errorf("write control snapshot: unexpected script result %T", res)
	}
	if success, _ := parts[0].(int64); success != 1 {
		return Snapshot{}, ErrConflict
	}
	schema, err := strconv.Atoi(fmt.Sprint(parts[1]))
	if err != nil || schema <= 0 {
		return Snapshot{}, fmt.Errorf("write control snapshot: unexpected schema in script result")
	}
	return Snapshot{Revision: next, Schema: schema, Payload: append([]byte(nil), payload...), UpdatedAt: now}, nil
}

// redisDeleteScript deletes only when the stored revision matches.
const redisDeleteScript = `
local cur = redis.call('HGET', KEYS[1], 'revision')
if tonumber(cur) ~= tonumber(ARGV[1]) then return 0 end
return redis.call('DEL', KEYS[1])`

func (s *RedisStore) Delete(ctx context.Context, namespace string, expectedRevision uint64) error {
	res, err := s.client.Eval(ctx, redisDeleteScript, []string{s.key(namespace)}, strconv.FormatUint(expectedRevision, 10))
	if err != nil {
		return fmt.Errorf("delete control snapshot: %w", err)
	}
	if deleted, _ := res.(int64); deleted != 1 {
		return ErrConflict
	}
	return nil
}
