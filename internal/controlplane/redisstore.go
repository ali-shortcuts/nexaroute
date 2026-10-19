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

const redisMaxUint64Decimal = "18446744073709551615"
const redisMaxInt64Decimal = "9223372036854775807"

// RedisStore implements Store on Redis. Each namespace is one Redis hash
// (revision, schema, payload, updated_at). Writes and deletes are
// compare-and-set Lua scripts, which Redis executes atomically. Revision
// arithmetic remains decimal-string based in Lua so all uint64 values are
// exact even though Lua numbers are floating point.
//
// Durability note: Redis is only as durable as its own configuration.
// Operators relying on this backend for durable control-plane state must
// run Redis with persistence (AOF and/or RDB) and appropriate replica /
// failover management; this store does not promise cross-replica safety.
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
	if namespace == "" {
		return Snapshot{}, ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("get control snapshot: %w", err)
	}
	fields, err := s.client.HGetAll(ctx, s.key(namespace))
	if err != nil {
		return Snapshot{}, wrapStoreError("get control snapshot", err)
	}
	if len(fields) == 0 {
		return Snapshot{}, ErrNotFound
	}
	return parseRedisSnapshot(fields)
}

func parseRedisSnapshot(fields map[string]string) (Snapshot, error) {
	if len(fields) != 4 {
		return Snapshot{}, ErrMalformedSnapshot
	}
	revisionText, ok := fields["revision"]
	if !ok {
		return Snapshot{}, ErrMalformedSnapshot
	}
	revision, err := strconv.ParseUint(revisionText, 10, 64)
	if err != nil || revision == 0 || strconv.FormatUint(revision, 10) != revisionText {
		return Snapshot{}, ErrMalformedSnapshot
	}
	schemaText, ok := fields["schema"]
	if !ok {
		return Snapshot{}, ErrMalformedSnapshot
	}
	schema, err := strconv.Atoi(schemaText)
	if err != nil || schema <= 0 || strconv.Itoa(schema) != schemaText {
		return Snapshot{}, ErrMalformedSnapshot
	}
	payload, ok := fields["payload"]
	if !ok || len(payload) == 0 {
		return Snapshot{}, ErrMalformedSnapshot
	}
	updatedText, ok := fields["updated_at"]
	if !ok {
		return Snapshot{}, ErrMalformedSnapshot
	}
	updated, err := time.Parse(time.RFC3339Nano, updatedText)
	if err != nil {
		return Snapshot{}, ErrMalformedSnapshot
	}
	return Snapshot{Revision: revision, Schema: schema, Payload: []byte(payload), UpdatedAt: updated}, nil
}

// validateFieldsScript is embedded into both mutation scripts. Redis Lua's
// numbers cannot precisely represent every uint64, so revisions and schema
// values are compared as canonical decimal strings instead.
const redisValidateFieldsLua = `
local MAX_U64 = '18446744073709551615'
local MAX_INT = '9223372036854775807'
local function valid_uint(s, allow_zero, maximum)
  if type(s) ~= 'string' or not string.match(s, '^%d+$') then return false end
  if string.len(s) > 1 and string.sub(s, 1, 1) == '0' then return false end
  if not allow_zero and s == '0' then return false end
  if string.len(s) > string.len(maximum) then return false end
  if string.len(s) == string.len(maximum) and s > maximum then return false end
  return true
end
local function valid_updated_at(s)
  if type(s) ~= 'string' then return false end
  local year, month, day, hour, minute, second = string.match(s, '^(%d%d%d%d)%-(%d%d)%-(%d%d)T(%d%d):(%d%d):(%d%d)Z$')
  local fraction
  if not year then
    year, month, day, hour, minute, second, fraction = string.match(s, '^(%d%d%d%d)%-(%d%d)%-(%d%d)T(%d%d):(%d%d):(%d%d)%.(%d+)Z$')
  end
  if not year or (fraction and string.len(fraction) > 9) then return false end
  year, month, day = tonumber(year), tonumber(month), tonumber(day)
  hour, minute, second = tonumber(hour), tonumber(minute), tonumber(second)
  if month < 1 or month > 12 or hour > 23 or minute > 59 or second > 59 then return false end
  local month_days = {31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
  if month == 2 and (year % 400 == 0 or (year % 4 == 0 and year % 100 ~= 0)) then month_days[2] = 29 end
  return day >= 1 and day <= month_days[month]
end
local function read_record(key)
  if redis.call('EXISTS', key) == 0 then return nil, -1 end
  if redis.call('HLEN', key) ~= 4 then return nil, -2 end
  local values = redis.call('HMGET', key, 'revision', 'schema', 'payload', 'updated_at')
  if type(values[1]) ~= 'string' or not valid_uint(values[1], false, MAX_U64) then return nil, -2 end
  if type(values[2]) ~= 'string' or not valid_uint(values[2], false, MAX_INT) then return nil, -2 end
  if type(values[3]) ~= 'string' or string.len(values[3]) == 0 then return nil, -2 end
  if not valid_updated_at(values[4]) then return nil, -2 end
  return values, 0
end
local function increment_decimal(s)
  local i = string.len(s)
  while i > 0 do
    local d = string.byte(s, i) - 48
    if d < 9 then
      return string.sub(s, 1, i - 1) .. string.char(49 + d) .. string.rep('0', string.len(s) - i)
    end
    i = i - 1
  end
  return nil
end
`

// redisPutScript performs a revision-aware mutation atomically. It returns
// {1, schema, revision} on success, 0 on conflict, -2 for malformed stored
// fields, and -3 when the revision cannot be incremented.
const redisPutScript = redisValidateFieldsLua + `
local fields, status = read_record(KEYS[1])
if status == -2 then return -2 end
local current = '0'
local schema = ARGV[2]
if status == 0 then
  current = fields[1]
  schema = fields[2]
end
if current ~= ARGV[1] then return 0 end
local next = increment_decimal(current)
if not next then return -3 end
redis.call('HSET', KEYS[1], 'revision', next, 'schema', schema, 'payload', ARGV[3], 'updated_at', ARGV[4])
return {1, schema, next}`

func (s *RedisStore) Put(ctx context.Context, namespace string, expectedRevision uint64, payload []byte) (Snapshot, error) {
	if namespace == "" || len(payload) == 0 {
		return Snapshot{}, ErrNotFound
	}
	if expectedRevision == ^uint64(0) {
		return Snapshot{}, ErrInvalidRevision
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("write control snapshot: %w", err)
	}
	next := expectedRevision + 1
	now := time.Now().UTC()
	res, err := s.client.Eval(ctx, redisPutScript, []string{s.key(namespace)},
		strconv.FormatUint(expectedRevision, 10),
		"1",
		string(payload),
		now.Format(time.RFC3339Nano))
	if err != nil {
		return Snapshot{}, wrapStoreError("write control snapshot", err)
	}
	if status, ok := res.(int64); ok {
		switch status {
		case 0:
			return Snapshot{}, ErrConflict
		case -2:
			return Snapshot{}, ErrMalformedSnapshot
		case -3:
			return Snapshot{}, ErrInvalidRevision
		default:
			return Snapshot{}, fmt.Errorf("write control snapshot: unexpected script status %d", status)
		}
	}
	parts, ok := res.([]any)
	if !ok || len(parts) != 3 {
		return Snapshot{}, fmt.Errorf("write control snapshot: unexpected script result %T", res)
	}
	success, ok := parts[0].(int64)
	if !ok || success != 1 {
		return Snapshot{}, fmt.Errorf("write control snapshot: unexpected script result %T", res)
	}
	schemaText, ok := parts[1].(string)
	if !ok {
		if bytes, bytesOK := parts[1].([]byte); bytesOK {
			schemaText, ok = string(bytes), true
		}
	}
	revisionText, revisionOK := parts[2].(string)
	if !revisionOK {
		if bytes, bytesOK := parts[2].([]byte); bytesOK {
			revisionText, revisionOK = string(bytes), true
		}
	}
	if !ok || !revisionOK {
		return Snapshot{}, fmt.Errorf("write control snapshot: unexpected script result fields")
	}
	schema, schemaErr := strconv.Atoi(schemaText)
	revision, revisionErr := strconv.ParseUint(revisionText, 10, 64)
	if schemaErr != nil || schema <= 0 || revisionErr != nil || revision != next {
		return Snapshot{}, fmt.Errorf("write control snapshot: invalid script result fields")
	}
	return Snapshot{Revision: next, Schema: schema, Payload: append([]byte(nil), payload...), UpdatedAt: now}, nil
}

// redisDeleteScript deletes only when the complete stored record is valid
// and its exact decimal revision matches the caller's expected revision.
// A MALFORMED record is the one exception: it is deleted and reported
// with status 2, because otherwise the namespace would be bricked
// forever — Put refuses to overwrite malformed content and Delete
// refused to remove it, leaving manual Redis surgery as the only
// recovery. The caller maps status 2 to ErrNotFound: no valid
// snapshot existed.
const redisDeleteScript = redisValidateFieldsLua + `
local fields, status = read_record(KEYS[1])
if status == -1 then return -1 end
if status == -2 then redis.call('DEL', KEYS[1]) return 2 end
if fields[1] ~= ARGV[1] then return 0 end
return redis.call('DEL', KEYS[1])`

func (s *RedisStore) Delete(ctx context.Context, namespace string, expectedRevision uint64) error {
	if namespace == "" {
		return ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("delete control snapshot: %w", err)
	}
	res, err := s.client.Eval(ctx, redisDeleteScript, []string{s.key(namespace)}, strconv.FormatUint(expectedRevision, 10))
	if err != nil {
		return wrapStoreError("delete control snapshot", err)
	}
	deleted, ok := res.(int64)
	if !ok {
		return fmt.Errorf("delete control snapshot: unexpected script result %T", res)
	}
	switch deleted {
	case 1:
		return nil
	case 2:
		// The stored record was malformed and has been removed:
		// the namespace-repair path. No valid snapshot existed.
		return ErrNotFound
	case 0:
		return ErrConflict
	case -1:
		return ErrNotFound
	case -2:
		return ErrMalformedSnapshot
	default:
		return fmt.Errorf("delete control snapshot: unexpected script status %d", deleted)
	}
}
