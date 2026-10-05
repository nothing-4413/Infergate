package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/embed"
	"github.com/infergate/infergate/internal/redis"
)

// RedisStore keeps entries in Redis so a cache survives a gateway restart and
// is shared by every replica. It is what makes the cache a system property
// rather than a per-process accident: with two replicas and a memory store, a
// prompt repeated round-robin misses half the time.
//
// Key layout (one scope = one tenant+model; see ScopeFor):
//
//	ig:cache:<scope>:meta  HASH  field=<key> value=<Entry as JSON, vector omitted>
//	ig:cache:<scope>:vec   HASH  field=<key> value=<base64 float32 vector>
//	ig:cache:<scope>:idx   ZSET  member=<key> score=<createdAt unix ms>
//	ig:cache:<scope>:exp   ZSET  member=<key> score=<expiresAt unix ms>
//	ig:cache:scopes        ZSET  member=<scope> score=<last write unix ms>
//
// Three deliberate choices:
//
//   - The vector lives beside the entry rather than inside it, because a
//     lookup only needs the vectors: Search reads the `vec` hash (compact,
//     base64) and fetches the JSON of the winner alone. Putting the vector in
//     the JSON would make every lookup deserialise every entry in the scope.
//   - Expiry is per entry in the `exp` ZSET, not a single TTL on the hash. A
//     key-level EXPIRE would make every entry in a scope die together, so one
//     busy prompt would extend the life of an unrelated stale answer. The
//     key-level EXPIRE is still set, but only as a safety net (two TTLs) so a
//     scope that nobody ever reads again does not accumulate.
//   - Eviction is FIFO by creation time (ZREMRANGEBYRANK), not LRU: Redis does
//     not expose the read order, and a cache that is wrong about recency is
//     worse than one that is honest about age. The memory store has true LRU.
type RedisStore struct {
	cli    *redis.Client
	prefix string
	opts   RedisOptions
	now    func() time.Time
	stats  StoreStats
}

// RedisOptions configures a RedisStore.
type RedisOptions struct {
	// Addr is host:port. Empty means the store is not usable and NewRedisStore
	// returns an error, because a cache that silently does nothing is worse
	// than one that fails at startup.
	Addr     string
	Password string
	DB       int
	// Prefix overrides the "ig:cache" key prefix, so several gateways can share
	// one Redis database without colliding.
	Prefix string
	// MaxEntriesPerScope bounds a scope; it is enforced with FIFO trimming.
	MaxEntriesPerScope int
	// MaxScan caps how many vectors one Search will read. A scope that grows
	// past it keeps working (the scan is unbounded by default) but the number
	// is recorded in the store stats so an operator can see the cost.
	MaxScan int
	// Now overrides the clock, for tests.
	Now func() time.Time
	// DialTimeout/ReadTimeout/WriteTimeout/PoolSize pass through to the client.
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolSize     int
}

// NewRedisStore builds a Redis-backed store. It pings once so a misconfigured
// address fails at startup with a clear message instead of turning every
// request into a silent miss.
func NewRedisStore(opts RedisOptions) (*RedisStore, error) {
	if strings.TrimSpace(opts.Addr) == "" {
		return nil, fmt.Errorf("cache: redis addr is empty")
	}
	if opts.Prefix == "" {
		opts.Prefix = "ig:cache"
	}
	if opts.MaxEntriesPerScope <= 0 {
		opts.MaxEntriesPerScope = DefaultMaxEntriesPerScope
	}
	if opts.MaxScan <= 0 {
		opts.MaxScan = 4096
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	cli := redis.NewClient(redis.Options{
		Addr:         opts.Addr,
		Password:     opts.Password,
		DB:           opts.DB,
		DialTimeout:  opts.DialTimeout,
		ReadTimeout:  opts.ReadTimeout,
		WriteTimeout: opts.WriteTimeout,
		PoolSize:     opts.PoolSize,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := cli.Ping(ctx); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("cache: cannot reach redis at %s: %w", opts.Addr, err)
	}
	return &RedisStore{cli: cli, prefix: opts.Prefix, opts: opts, now: now}, nil
}

func (s *RedisStore) metaKey(scope string) string { return s.prefix + ":" + scope + ":meta" }
func (s *RedisStore) vecKey(scope string) string  { return s.prefix + ":" + scope + ":vec" }
func (s *RedisStore) idxKey(scope string) string  { return s.prefix + ":" + scope + ":idx" }
func (s *RedisStore) expKey(scope string) string  { return s.prefix + ":" + scope + ":exp" }
func (s *RedisStore) scopesKey() string           { return s.prefix + ":scopes" }

// Name identifies the backend.
func (s *RedisStore) Name() string { return "redis" }

// Client exposes the underlying client so /admin/cache can report its pool and
// error counters.
func (s *RedisStore) Client() *redis.Client { return s.cli }

// Stats returns the counters. The Redis client's own transport failures are
// folded in as Errors, because from an operator's point of view "the cache is
// not answering" is one number.
func (s *RedisStore) Stats() StoreStats {
	out := s.stats
	out.Errors += s.cli.Stats().Errors
	return out
}

// Close releases the pool. It is idempotent.
func (s *RedisStore) Close() error { return s.cli.Close() }

// Get reads one entry and drops it if its TTL passed (so the store is
// self-healing even when nobody runs a sweep).
func (s *RedisStore) Get(ctx context.Context, scope, key string) (Entry, bool, error) {
	s.stats.Gets++
	reply, err := s.cli.Do(ctx, "HGET", s.metaKey(scope), key)
	if err != nil {
		s.stats.Errors++
		return Entry{}, false, err
	}
	if reply.Null {
		return Entry{}, false, nil
	}
	e, err := decodeEntry(reply.Str)
	if err != nil {
		s.stats.Errors++
		// A corrupt value is treated as a miss, but it is also deleted: leaving
		// it would make every future lookup for this prompt pay the parse.
		_, _ = s.Delete(ctx, scope, key)
		return Entry{}, false, err
	}
	if e.Expired(s.now()) {
		_, _ = s.Delete(ctx, scope, key)
		s.stats.Expired++
		return Entry{}, false, nil
	}
	// The vector is a separate field; without it the entry can only ever be an
	// exact hit, which is legitimate (an entry stored while the embedder was
	// down), so a failure to read it is not an error.
	if vreply, verr := s.cli.Do(ctx, "HGET", s.vecKey(scope), key); verr == nil && !vreply.Null {
		if vec, derr := embed.DecodeVector(vreply.Str); derr == nil {
			e.Vector = vec
		}
	}
	return e, true, nil
}

// Put writes an entry with the four-key layout, then trims the scope.
func (s *RedisStore) Put(ctx context.Context, e Entry, ttl time.Duration) error {
	now := s.now()
	if e.ExpiresAt.IsZero() {
		e.ExpiresAt = now.Add(ttl)
	}
	vec := e.Vector
	// The vector is stored in its own hash, so the JSON copy would be wasted
	// bytes and would make the meta hash ~2.7 KiB per entry instead of ~1 KiB.
	stored := cloneEntry(&e)
	stored.Vector = nil
	blob, err := json.Marshal(stored)
	if err != nil {
		s.stats.Errors++
		return err
	}

	scope := e.Scope
	cmds := [][]string{
		{"HSET", s.metaKey(scope), e.Key, string(blob)},
		{"ZADD", s.idxKey(scope), strconv.FormatInt(e.CreatedAt.UnixMilli(), 10), e.Key},
		{"ZADD", s.expKey(scope), strconv.FormatInt(e.ExpiresAt.UnixMilli(), 10), e.Key},
		{"ZADD", s.scopesKey(), strconv.FormatInt(now.UnixMilli(), 10), scope},
	}
	if len(vec) > 0 {
		cmds = append(cmds, []string{"HSET", s.vecKey(scope), e.Key, embed.EncodeVector(vec)})
	} else {
		cmds = append(cmds, []string{"HDEL", s.vecKey(scope), e.Key})
	}
	// Safety net only: two TTLs. The per-entry exp ZSET is authoritative.
	seconds := int64((2 * ttl).Seconds())
	if seconds < 60 {
		seconds = 60
	}
	sec := strconv.FormatInt(seconds, 10)
	for _, k := range []string{s.metaKey(scope), s.vecKey(scope), s.idxKey(scope), s.expKey(scope)} {
		cmds = append(cmds, []string{"EXPIRE", k, sec})
	}

	if _, err := s.cli.Pipeline(ctx, cmds); err != nil {
		s.stats.Errors++
		return err
	}
	s.stats.Puts++
	return s.trim(ctx, scope)
}

// trim enforces the per-scope bound by removing the OLDEST entries (FIFO).
func (s *RedisStore) trim(ctx context.Context, scope string) error {
	reply, err := s.cli.Do(ctx, "ZCARD", s.idxKey(scope))
	if err != nil {
		s.stats.Errors++
		return err
	}
	over := int(reply.Int) - s.opts.MaxEntriesPerScope
	if over <= 0 {
		return nil
	}
	// Ask which members fall outside the bound before removing them: the
	// meta/vec fields have to go too, and a ZSET trim alone would leave the
	// payload behind forever (a leak that would only show up as Redis memory).
	victims, err := s.cli.Do(ctx, "ZRANGE", s.idxKey(scope), "0", strconv.Itoa(over-1))
	if err != nil {
		s.stats.Errors++
		return err
	}
	members, err := victims.Strings()
	if err != nil {
		return err
	}
	cmds := make([][]string, 0, len(members)*4+2)
	for _, m := range members {
		cmds = append(cmds,
			[]string{"HDEL", s.metaKey(scope), m},
			[]string{"HDEL", s.vecKey(scope), m},
			[]string{"ZREM", s.expKey(scope), m},
			[]string{"ZREM", s.idxKey(scope), m},
		)
	}
	if len(cmds) > 0 {
		if _, err := s.cli.Pipeline(ctx, cmds); err != nil {
			s.stats.Errors++
			return err
		}
		s.stats.Evicted += int64(len(members))
	}
	return nil
}

// Search reads the scope's vectors, ranks them locally and returns the closest
// entries. See the Store doc comment for why this is a scan.
func (s *RedisStore) Search(ctx context.Context, scope string, vec []float32, threshold float64, limit int) ([]Match, error) {
	if embed.IsZero(vec) {
		// See embed.IsZero: "no direction" must be a miss, not a match on
		// everything whose similarity happens to equal the threshold.
		return nil, nil
	}
	s.stats.Searches++
	if err := s.pruneExpired(ctx, scope); err != nil {
		return nil, err
	}
	reply, err := s.cli.Do(ctx, "HGETALL", s.vecKey(scope))
	if err != nil {
		s.stats.Errors++
		return nil, err
	}
	flat, err := reply.Strings()
	if err != nil {
		s.stats.Errors++
		return nil, err
	}
	if len(flat)/2 > s.opts.MaxScan {
		// Not fatal: the scan still runs, but the number is recorded so the
		// cost is visible rather than surprising.
		s.stats.Errors++
	}
	type cand struct {
		key string
		sim float64
	}
	cands := make([]cand, 0, len(flat)/2)
	for i := 0; i+1 < len(flat); i += 2 {
		key, blob := flat[i], flat[i+1]
		s.stats.Scanned++
		v, derr := embed.DecodeVector(blob)
		if derr != nil || len(v) == 0 {
			continue
		}
		sim := embed.Cosine(vec, v)
		if sim < threshold {
			continue
		}
		cands = append(cands, cand{key: key, sim: sim})
	}
	if len(cands) == 0 {
		return nil, nil
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].sim > cands[j].sim })
	if limit > 0 && len(cands) > limit {
		cands = cands[:limit]
	}
	// Only the winners' JSON is fetched: this is why the vector is not in it.
	out := make([]Match, 0, len(cands))
	for _, c := range cands {
		e, ok, gerr := s.Get(ctx, scope, c.key)
		if gerr != nil || !ok {
			continue
		}
		out = append(out, Match{Entry: e, Similarity: c.sim})
	}
	Rank(out)
	return out, nil
}

// pruneExpired removes entries whose TTL passed, using the exp ZSET so the work
// is proportional to what expired rather than to the size of the scope.
func (s *RedisStore) pruneExpired(ctx context.Context, scope string) error {
	now := strconv.FormatInt(s.now().UnixMilli(), 10)
	reply, err := s.cli.Do(ctx, "ZRANGEBYSCORE", s.expKey(scope), "-inf", now)
	if err != nil {
		s.stats.Errors++
		return err
	}
	members, err := reply.Strings()
	if err != nil || len(members) == 0 {
		return err
	}
	cmds := make([][]string, 0, len(members)*4)
	for _, m := range members {
		cmds = append(cmds,
			[]string{"HDEL", s.metaKey(scope), m},
			[]string{"HDEL", s.vecKey(scope), m},
			[]string{"ZREM", s.idxKey(scope), m},
			[]string{"ZREM", s.expKey(scope), m},
		)
	}
	if _, err := s.cli.Pipeline(ctx, cmds); err != nil {
		s.stats.Errors++
		return err
	}
	s.stats.Expired += int64(len(members))
	return nil
}

// Delete removes one entry from all four keys.
func (s *RedisStore) Delete(ctx context.Context, scope, key string) (bool, error) {
	s.stats.Deletes++
	replies, err := s.cli.Pipeline(ctx, [][]string{
		{"HDEL", s.metaKey(scope), key},
		{"HDEL", s.vecKey(scope), key},
		{"ZREM", s.idxKey(scope), key},
		{"ZREM", s.expKey(scope), key},
	})
	if err != nil {
		s.stats.Errors++
		return false, err
	}
	if len(replies) == 0 {
		return false, nil
	}
	return replies[0].Int > 0, nil
}

// Flush removes one scope, or every scope when scope is empty. Flush-all walks
// the scope registry, which is why the registry exists: there is no cheap way
// to enumerate "ig:cache:*:meta" without KEYS, and KEYS blocks Redis.
func (s *RedisStore) Flush(ctx context.Context, scope string) (int, error) {
	s.stats.Flushes++
	if scope != "" {
		return s.flushScope(ctx, scope)
	}
	reply, err := s.cli.Do(ctx, "ZRANGE", s.scopesKey(), "0", "-1")
	if err != nil {
		s.stats.Errors++
		return 0, err
	}
	scopes, err := reply.Strings()
	if err != nil {
		s.stats.Errors++
		return 0, err
	}
	total := 0
	for _, sc := range scopes {
		n, err := s.flushScope(ctx, sc)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (s *RedisStore) flushScope(ctx context.Context, scope string) (int, error) {
	card, err := s.cli.Do(ctx, "ZCARD", s.idxKey(scope))
	if err != nil {
		s.stats.Errors++
		return 0, err
	}
	if _, err := s.cli.Pipeline(ctx, [][]string{
		{"DEL", s.metaKey(scope)},
		{"DEL", s.vecKey(scope)},
		{"DEL", s.idxKey(scope)},
		{"DEL", s.expKey(scope)},
		{"ZREM", s.scopesKey(), scope},
	}); err != nil {
		s.stats.Errors++
		return 0, err
	}
	return int(card.Int), nil
}

// Len counts entries in a scope, or in every scope when scope is empty. It
// prunes expired entries first so the number matches what a lookup would see.
func (s *RedisStore) Len(ctx context.Context, scope string) (int, error) {
	if scope != "" {
		if err := s.pruneExpired(ctx, scope); err != nil {
			return 0, err
		}
		reply, err := s.cli.Do(ctx, "ZCARD", s.idxKey(scope))
		if err != nil {
			s.stats.Errors++
			return 0, err
		}
		return int(reply.Int), nil
	}
	reply, err := s.cli.Do(ctx, "ZRANGE", s.scopesKey(), "0", "-1")
	if err != nil {
		s.stats.Errors++
		return 0, err
	}
	scopes, err := reply.Strings()
	if err != nil {
		return 0, err
	}
	total := 0
	for _, sc := range scopes {
		n, err := s.Len(ctx, sc)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// Scopes lists the known scopes with their entry counts, for /admin/cache.
func (s *RedisStore) Scopes(ctx context.Context) (map[string]int, error) {
	reply, err := s.cli.Do(ctx, "ZRANGE", s.scopesKey(), "0", "-1")
	if err != nil {
		s.stats.Errors++
		return nil, err
	}
	scopes, err := reply.Strings()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(scopes))
	for _, sc := range scopes {
		n, err := s.Len(ctx, sc)
		if err != nil {
			return out, err
		}
		out[sc] = n
	}
	return out, nil
}

func decodeEntry(blob string) (Entry, error) {
	var e Entry
	if err := json.Unmarshal([]byte(blob), &e); err != nil {
		return Entry{}, fmt.Errorf("cache: corrupt entry: %w", err)
	}
	return e, nil
}
