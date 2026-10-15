package securitystore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis security store (M4-2 part B): the Backend contract over three
// hashes — sessions, audit records and chain metadata — with the
// optimistic-CAS discipline of the control-plane Redis store. Every
// audit-carrying mutation reads the chain state, plans in Go with the
// shared chaincore logic, and commits through a Lua script that
// applies only when the metadata version is unchanged; contention
// retries the whole plan against fresh state (bounded). Session
// touches, which carry no audit event, use a field-level
// compare-and-set script. Semantics match the bbolt and PostgreSQL
// stores (shared contract suite + cross-backend parity tests).

const (
	securityRedisMetaKey     = "nexaroute:security:meta"
	securityRedisAuditKey    = "nexaroute:security:audit"
	securityRedisSessionsKey = "nexaroute:security:sessions"

	redisCASMaxAttempts = 16
)

// RedisSecurityClient is the minimal Redis surface the adapter needs.
type RedisSecurityClient interface {
	HGetAll(ctx context.Context, key string) (map[string]string, error)
	HGet(ctx context.Context, key, field string) (string, bool, error)
	// Snapshot returns the meta and audit hashes from ONE Lua call:
	// reading them with separate commands can tear (a concurrent
	// commit lands between the reads) and the chain gate would reject
	// a healthy chain, so the pair is always read atomically.
	Snapshot(ctx context.Context) (meta, audit map[string]string, err error)
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)
	Ping(ctx context.Context) error
	Close() error
}

// snapshotScript reads the meta and audit hashes in one atomic step.
const snapshotScript = `
local meta = redis.call('HGETALL', KEYS[1])
local audit = redis.call('HGETALL', KEYS[2])
return {meta, audit}`

func parseHashPairs(v any) map[string]string {
	out := map[string]string{}
	pairs, ok := v.([]any)
	if !ok {
		return out
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		k, _ := pairs[i].(string)
		val, _ := pairs[i+1].(string)
		if k != "" {
			out[k] = val
		}
	}
	return out
}

type goRedisSecurityClient struct{ client *redis.Client }

func (a goRedisSecurityClient) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return a.client.HGetAll(ctx, key).Result()
}

func (a goRedisSecurityClient) HGet(ctx context.Context, key, field string) (string, bool, error) {
	v, err := a.client.HGet(ctx, key, field).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (a goRedisSecurityClient) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return a.client.Eval(ctx, script, keys, args...).Result()
}

func (a goRedisSecurityClient) Snapshot(ctx context.Context) (map[string]string, map[string]string, error) {
	res, err := a.client.Eval(ctx, snapshotScript, []string{securityRedisMetaKey, securityRedisAuditKey}).Result()
	if err != nil {
		return nil, nil, err
	}
	parts, ok := res.([]any)
	if !ok || len(parts) != 2 {
		return nil, nil, errors.New("securitystore: unexpected snapshot response")
	}
	return parseHashPairs(parts[0]), parseHashPairs(parts[1]), nil
}

func (a goRedisSecurityClient) Ping(ctx context.Context) error {
	return a.client.Ping(ctx).Err()
}

func (a goRedisSecurityClient) Close() error { return a.client.Close() }

// commitScript applies a planned mutation atomically iff the meta
// version still equals the expected one. ARGV[1] = expected version,
// ARGV[2] = changes JSON (see redisChanges).
const commitScript = `
local v = redis.call('HGET', KEYS[1], 'version')
if not v then v = '0' end
if v ~= ARGV[1] then return 0 end
local ch = cjson.decode(ARGV[2])
if ch.audit_del then for _, f in ipairs(ch.audit_del) do redis.call('HDEL', KEYS[2], f) end end
if ch.audit_put then redis.call('HSET', KEYS[2], ch.audit_put.seq, ch.audit_put.json) end
if ch.session_del then for _, f in ipairs(ch.session_del) do redis.call('HDEL', KEYS[3], f) end end
if ch.session_put then for _, p in ipairs(ch.session_put) do redis.call('HSET', KEYS[3], p.hash, p.json) end end
redis.call('HSET', KEYS[1], 'head', ch.head, 'allocator', ch.allocator, 'version', tostring(tonumber(v) + 1))
if ch.anchor then redis.call('HSET', KEYS[1], 'anchor', ch.anchor) end
return 1`

// touchScript compare-and-sets one session field: ARGV[1] = field,
// ARGV[2] = expected current value, ARGV[3] = new value ("" deletes).
const touchScript = `
local cur = redis.call('HGET', KEYS[1], ARGV[1])
if not cur then cur = '' end
if cur ~= ARGV[2] then return 0 end
if ARGV[3] == '' then redis.call('HDEL', KEYS[1], ARGV[1]) else redis.call('HSET', KEYS[1], ARGV[1], ARGV[3]) end
return 1`

// redisChanges is the JSON payload of commitScript.
type redisChanges struct {
	AuditDel []string `json:"audit_del,omitempty"`
	AuditPut *struct {
		Seq  string `json:"seq"`
		JSON string `json:"json"`
	} `json:"audit_put,omitempty"`
	SessionDel []string `json:"session_del,omitempty"`
	SessionPut []struct {
		Hash string `json:"hash"`
		JSON string `json:"json"`
	} `json:"session_put,omitempty"`
	Head      string  `json:"head"`
	Anchor    *string `json:"anchor,omitempty"`
	Allocator string  `json:"allocator"`
}

// RedisStore is a Backend over Redis.
type RedisStore struct {
	client        RedisSecurityClient
	maxAuditItems int
	retention     time.Duration
}

var _ Backend = (*RedisStore)(nil)

// OpenRedis connects with the bounded client posture of the
// control-plane Redis backend (small pool, bounded timeouts/retries)
// and pings before returning. URL parse errors are reported by class
// only; the URL is never logged or embedded in errors.
func OpenRedis(ctx context.Context, rawURL string, maxAuditItems, retentionDays int) (*RedisStore, error) {
	if maxAuditItems < 100 || maxAuditItems > 1_000_000 {
		return nil, errors.New("security audit max_events must be between 100 and 1000000")
	}
	if retentionDays < 1 || retentionDays > 3650 {
		return nil, errors.New("security audit retention_days must be between 1 and 3650")
	}
	opts, err := redis.ParseURL(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, errors.New("securitystore: invalid redis URL (expected redis:// or rediss://)")
	}
	opts.PoolSize = 10
	opts.DialTimeout = 5 * time.Second
	opts.ReadTimeout = 5 * time.Second
	opts.WriteTimeout = 5 * time.Second
	opts.MaxRetries = 2
	store := &RedisStore{
		client:        goRedisSecurityClient{client: redis.NewClient(opts)},
		maxAuditItems: maxAuditItems,
		retention:     time.Duration(retentionDays) * 24 * time.Hour,
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := store.client.Ping(pingCtx); err != nil {
		_ = store.client.Close()
		return nil, errors.New("securitystore: redis is unreachable")
	}
	return store, nil
}

// newRedisStoreWithClient is the test seam for fake clients.
func newRedisStoreWithClient(client RedisSecurityClient, maxAuditItems int, retention time.Duration) *RedisStore {
	return &RedisStore{client: client, maxAuditItems: maxAuditItems, retention: retention}
}

// loadChain reads the full chain state plus the meta version used for
// the CAS commit, as one atomic snapshot.
func (s *RedisStore) loadChain(ctx context.Context) (chainState, string, error) {
	st := chainState{}
	meta, audit, err := s.client.Snapshot(ctx)
	if err != nil {
		return st, "", fmt.Errorf("securitystore: load chain snapshot: %w", err)
	}
	seqs := make([]int, 0, len(audit))
	bySeq := map[int]string{}
	for field, raw := range audit {
		n, err := strconv.Atoi(field)
		if err != nil || n < 0 {
			return st, "", errors.New("security store contains a malformed audit record key")
		}
		seqs = append(seqs, n)
		bySeq[n] = raw
	}
	sort.Ints(seqs)
	for _, n := range seqs {
		var event AuditEvent
		if err := json.Unmarshal([]byte(bySeq[n]), &event); err != nil {
			return st, "", errors.New("security store contains a malformed audit record")
		}
		if event.Seq != uint64(n) {
			return st, "", errors.New("security store contains an audit record whose sequence does not match its key")
		}
		st.Records = append(st.Records, event)
	}
	if raw, ok := meta["head"]; ok {
		st.Head, st.HeadState = parseChainRefMeta([]byte(raw))
	}
	if raw, ok := meta["anchor"]; ok {
		st.Anchor, st.AnchorState = parseChainRefMeta([]byte(raw))
	}
	if raw, ok := meta["allocator"]; ok {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return st, "", errors.New("security store chain allocator metadata is malformed")
		}
		st.Allocator = n
	}
	version := meta["version"]
	if version == "" {
		version = "0"
	}
	return st, version, nil
}

// mutate runs one audit-carrying mutation through the CAS loop:
// plan against fresh state (sessionPlan inspects the same snapshot
// and contributes session changes), commit iff the version held.
func (s *RedisStore) mutate(ctx context.Context, event AuditEvent, sessionPlan func(ctx context.Context) (del []string, put map[string]string, err error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for attempt := 0; attempt < redisCASMaxAttempts; attempt++ {
		st, version, err := s.loadChain(ctx)
		if err != nil {
			return err
		}
		plan, err := planAuditAppend(st, event, s.maxAuditItems, s.retention, time.Now())
		if err != nil {
			return err
		}
		changes := redisChanges{Allocator: strconv.FormatUint(plan.Allocator, 10)}
		headJSON, err := json.Marshal(plan.Head)
		if err != nil {
			return err
		}
		changes.Head = string(headJSON)
		if plan.HasAnchor {
			anchorJSON, err := json.Marshal(plan.Anchor)
			if err != nil {
				return err
			}
			anchor := string(anchorJSON)
			changes.Anchor = &anchor
		}
		for _, seq := range plan.PrunedSeqs {
			changes.AuditDel = append(changes.AuditDel, strconv.FormatUint(seq, 10))
		}
		insert := true
		for _, seq := range plan.PrunedSeqs {
			if seq == plan.Event.Seq {
				insert = false
			}
		}
		if insert {
			encoded, err := json.Marshal(plan.Event)
			if err != nil {
				return err
			}
			put := struct {
				Seq  string `json:"seq"`
				JSON string `json:"json"`
			}{Seq: strconv.FormatUint(plan.Event.Seq, 10), JSON: string(encoded)}
			changes.AuditPut = &put
		}
		if sessionPlan != nil {
			del, puts, err := sessionPlan(ctx)
			if err != nil {
				return err
			}
			changes.SessionDel = del
			for hash, raw := range puts {
				entry := struct {
					Hash string `json:"hash"`
					JSON string `json:"json"`
				}{Hash: hash, JSON: raw}
				changes.SessionPut = append(changes.SessionPut, entry)
			}
		}
		payload, err := json.Marshal(changes)
		if err != nil {
			return err
		}
		res, err := s.client.Eval(ctx, commitScript,
			[]string{securityRedisMetaKey, securityRedisAuditKey, securityRedisSessionsKey},
			version, string(payload))
		if err != nil {
			return fmt.Errorf("securitystore: commit mutation: %w", err)
		}
		if n, ok := res.(int64); ok && n == 1 {
			return nil
		}
		// Lost the race: replan against fresh state.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 2 * time.Millisecond):
		}
	}
	return errors.New("securitystore: redis concurrent modification; retry budget exhausted")
}

func (s *RedisStore) AppendAudit(ctx context.Context, event AuditEvent) error {
	return s.mutate(ctx, event, nil)
}

func (s *RedisStore) AuditRecords(ctx context.Context, limit int) ([]AuditEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = 100
	}
	if limit > s.maxAuditItems {
		limit = s.maxAuditItems
	}
	audit, err := s.client.HGetAll(ctx, securityRedisAuditKey)
	if err != nil {
		return nil, fmt.Errorf("securitystore: list audit records: %w", err)
	}
	records := make([]AuditEvent, 0, len(audit))
	for _, raw := range audit {
		var event AuditEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return nil, errors.New("security store contains a malformed audit record")
		}
		records = append(records, event)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Seq > records[j].Seq })
	if len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

func (s *RedisStore) VerifyAuditChain(ctx context.Context) (ChainReport, error) {
	if err := ctx.Err(); err != nil {
		return ChainReport{}, err
	}
	st, _, err := s.loadChain(ctx)
	if err != nil {
		return ChainReport{}, err
	}
	return verifyChainState(st), nil
}

func (s *RedisStore) CreateSession(ctx context.Context, hash string, value Session, event AuditEvent) error {
	return s.RotateSession(ctx, "", hash, value, event)
}

func (s *RedisStore) RotateSession(ctx context.Context, oldHash, hash string, value Session, event AuditEvent) error {
	if _, err := validSessionHash(hash); err != nil {
		return err
	}
	if oldHash != "" {
		if _, err := validSessionHash(oldHash); err != nil {
			return err
		}
	}
	encoded, err := prepareSession(value)
	if err != nil {
		return err
	}
	return s.mutate(ctx, event, func(ctx context.Context) ([]string, map[string]string, error) {
		// The existence check is sound inside the CAS loop: every
		// session creator carries an audit event and therefore passes
		// through the same version gate; a lost race replans and
		// re-checks.
		if _, exists, err := s.client.HGet(ctx, securityRedisSessionsKey, hash); err != nil {
			return nil, nil, fmt.Errorf("securitystore: check session: %w", err)
		} else if exists {
			return nil, nil, errors.New("session identifier collision")
		}
		var del []string
		if oldHash != "" {
			del = []string{oldHash}
		}
		return del, map[string]string{hash: string(encoded)}, nil
	})
}

func (s *RedisStore) Session(ctx context.Context, hash string) (Session, bool, error) {
	if _, err := validSessionHash(hash); err != nil {
		return Session{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return Session{}, false, err
	}
	raw, exists, err := s.client.HGet(ctx, securityRedisSessionsKey, hash)
	if err != nil {
		return Session{}, false, fmt.Errorf("securitystore: load session: %w", err)
	}
	if !exists {
		return Session{}, false, nil
	}
	var value Session
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return Session{}, false, errors.New("security store contains a malformed session record")
	}
	value.Roles = append([]string(nil), value.Roles...)
	return value, true, nil
}

func (s *RedisStore) TouchSession(ctx context.Context, hash string, now time.Time, idleTimeout time.Duration) (Session, bool, error) {
	if _, err := validSessionHash(hash); err != nil {
		return Session{}, false, err
	}
	if idleTimeout <= 0 {
		return Session{}, false, errors.New("session idle timeout must be positive")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := ctx.Err(); err != nil {
		return Session{}, false, err
	}
	for attempt := 0; attempt < redisCASMaxAttempts; attempt++ {
		raw, exists, err := s.client.HGet(ctx, securityRedisSessionsKey, hash)
		if err != nil {
			return Session{}, false, fmt.Errorf("securitystore: load session: %w", err)
		}
		if !exists {
			return Session{}, false, nil
		}
		var value Session
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return Session{}, false, errors.New("security store contains a malformed session record")
		}
		replacement := ""
		if !now.Before(value.ExpiresAt) || now.Sub(value.LastSeen) >= idleTimeout {
			replacement = "" // delete via the CAS script
		} else {
			value.LastSeen = now.UTC()
			encoded, err := json.Marshal(value)
			if err != nil {
				return Session{}, false, err
			}
			replacement = string(encoded)
		}
		res, err := s.client.Eval(ctx, touchScript, []string{securityRedisSessionsKey}, hash, raw, replacement)
		if err != nil {
			return Session{}, false, fmt.Errorf("securitystore: touch session: %w", err)
		}
		if n, ok := res.(int64); ok && n == 1 {
			if replacement == "" {
				return Session{}, false, nil
			}
			value.Roles = append([]string(nil), value.Roles...)
			return value, true, nil
		}
		select {
		case <-ctx.Done():
			return Session{}, false, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 2 * time.Millisecond):
		}
	}
	return Session{}, false, errors.New("securitystore: redis concurrent modification; retry budget exhausted")
}

func (s *RedisStore) RevokeSession(ctx context.Context, hash string, event AuditEvent) error {
	if _, err := validSessionHash(hash); err != nil {
		return err
	}
	return s.mutate(ctx, event, func(ctx context.Context) ([]string, map[string]string, error) {
		return []string{hash}, nil, nil
	})
}

func (s *RedisStore) Close() error {
	return s.client.Close()
}
