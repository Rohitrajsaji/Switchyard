// Package cache stores disposable configuration snapshots, never user decisions.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"switchyard/pkg/snapshot"
)

var ErrMiss = errors.New("snapshot cache miss")
var ErrConflict = errors.New("snapshot cache revision conflict")

type Redis struct {
	client *redis.Client
	prefix string
	now    func() time.Time
}

// NewRedis creates a bounded lazy connection pool. Redis failure never prevents
// process startup; the coordinator will use PostgreSQL instead. No automatic
// client caching or pipelines are enabled: freshness is our explicit contract.
func NewRedis(url, prefix string, now func() time.Time) (*Redis, error) {
	options, err := redis.ParseURL(url)
	if err != nil || now == nil || len(prefix) == 0 || len(prefix) > 128 || strings.ContainsAny(prefix, " \r\n\x00") {
		return nil, errors.New("invalid Redis snapshot configuration")
	}
	options.Protocol = 2
	options.PoolSize, options.MaxActiveConns = 4, 4
	options.MinIdleConns = 0
	options.DialTimeout = 300 * time.Millisecond
	options.ReadTimeout, options.WriteTimeout, options.PoolTimeout = 200*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond
	options.MaxRetries = -1
	options.ContextTimeoutEnabled = true
	options.DisableIdentity = true
	return &Redis{client: redis.NewClient(options), prefix: prefix, now: now}, nil
}

func (r *Redis) Close() error { return r.client.Close() }

func (r *Redis) key(key snapshot.Key) string {
	// Fixed struct order plus JSON encoding makes the scope unambiguous. Hashing
	// bounds key length without including user IDs or arbitrary attributes.
	body, _ := json.Marshal(key)
	hash := sha256.Sum256(body)
	return r.prefix + hex.EncodeToString(hash[:])
}

func (r *Redis) Get(ctx context.Context, key snapshot.Key) (*snapshot.Snapshot, error) {
	body, err := r.client.HGet(ctx, r.key(key), "payload").Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrMiss
	}
	if err != nil {
		return nil, err
	}
	s, err := snapshot.Decode(body, key)
	if err != nil {
		return nil, err
	}
	if !s.Usable(r.now()) {
		return nil, snapshot.ErrExpired
	}
	return s, nil
}

// Canonical decimal strings avoid Lua's floating-point loss for Go int64
// revisions above 2^53. The revision comparison and all hash fields/TTL change
// in one Redis script, so a delayed old writer cannot restore an enabled flag.
const putScript = `
local function valid(s)
  return s and string.match(s, '^[1-9][0-9]*$') and string.len(s) <= 19
end
local function cmp(a,b)
  if string.len(a) ~= string.len(b) then
    if string.len(a) > string.len(b) then return 1 else return -1 end
  end
  if a == b then return 0 end
  if a > b then return 1 else return -1 end
end
local old = redis.call('HMGET', KEYS[1], 'revision', 'verified_ms', 'definition', 'flag_id')
if old[1] then
  if not valid(old[1]) or not old[2] or not string.match(old[2], '^%d+$') then return -2 end
  if old[4] ~= ARGV[6] then return -1 end
  local order = cmp(ARGV[1], old[1])
  if order < 0 then return 0 end
  if order == 0 then
    if old[3] ~= ARGV[3] then return -1 end
    if cmp(ARGV[2],old[2]) <= 0 then return 0 end
  end
end
redis.call('HSET', KEYS[1], 'revision', ARGV[1], 'verified_ms', ARGV[2], 'definition', ARGV[3], 'payload', ARGV[4], 'flag_id', ARGV[6])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return 1
`

// Put preserves the remaining authoritative age. A Redis read/copy/restart does
// not renew it. False means an equal/newer value already won the race.
func (r *Redis) Put(ctx context.Context, s *snapshot.Snapshot) (bool, error) {
	if s == nil {
		return false, snapshot.ErrInvalid
	}
	ttl := s.Remaining(r.now()).Milliseconds()
	if ttl <= 0 {
		return false, snapshot.ErrExpired
	}
	body, err := s.MarshalJSON()
	if err != nil {
		return false, err
	}
	result, err := r.client.Eval(ctx, putScript, []string{r.key(s.Key())},
		strconv.FormatInt(s.Revision(), 10), strconv.FormatInt(s.VerifiedAt().UnixMilli(), 10),
		s.DefinitionJSON(), body, ttl, s.FlagID()).Int()
	if err != nil {
		return false, err
	}
	switch result {
	case 1:
		return true, nil
	case 0:
		return false, nil
	case -1:
		return false, ErrConflict
	default:
		return false, snapshot.ErrInvalid
	}
}
