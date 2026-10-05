package quota

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/redis"
)

// RedisStore keeps the counters in Redis so that every replica enforces one
// shared budget.
//
// Key layout:
//
//	ig:quota:<tenant>:day:<YYYYMMDD>:tokens        INCRBY counter, TTL to day end
//	ig:quota:<tenant>:day:<YYYYMMDD>:cost_micros   INCRBY counter, micro-USD
//	ig:quota:<tenant>:minute:<YYYYMMDDHHmm>:requests
//	ig:quota:<tenant>:session:<id>:tokens
//
// Two properties of this layout are load-bearing:
//
//   - Each counter is a single STRING mutated by INCRBY, never a hash field or
//     a list element, because INCRBY on a string is atomic and is the entire
//     concurrency story (see the package doc's "Why INCRBY and not Lua").
//   - The window is part of the KEY, not a TTL on a fixed key. Two windows
//     therefore never share a counter, so a request at 23:59:59 and one at
//     00:00:01 cannot race each other's rollover, and reading "today" is a
//     plain GET with no arithmetic. The TTL is only garbage collection.
type RedisStore struct {
	cli *redis.Client
}

// RedisOptions configures a RedisStore.
type RedisOptions struct {
	// Addr is host:port. Empty is an error from NewRedisStore: a quota store
	// that silently does nothing would enforce no budget at all while looking
	// configured.
	Addr     string
	Password string
	DB       int

	// DialTimeout/ReadTimeout/WriteTimeout bound one command. They are short by
	// default: a budget check is on the request path, and waiting seconds for a
	// counter read is worse than the fail-open/fail-closed decision the config
	// already made.
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolSize     int
}

// NewRedisStore builds a Redis-backed store and pings once, so a wrong address
// fails at startup with a clear message rather than turning into a per-request
// store error - which, with the default fail-closed policy, would refuse all
// traffic.
func NewRedisStore(opts RedisOptions) (*RedisStore, error) {
	if strings.TrimSpace(opts.Addr) == "" {
		return nil, fmt.Errorf("quota: redis addr is empty")
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
		return nil, fmt.Errorf("quota: cannot reach redis at %s: %w", opts.Addr, err)
	}
	return &RedisStore{cli: cli}, nil
}

// Add implements Store with INCRBY, plus an EXPIRE only when the counter was
// just created.
//
// The EXPIRE is conditional on `total == delta`, the signature of the first
// increment. Redis keeps an existing TTL on INCRBY, so the unconditional
// version would be a no-op on every later call anyway - except in the one case
// that matters, a counter that settled back to zero, where it would extend the
// window.
func (s *RedisStore) Add(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	reply, err := s.cli.Do(ctx, "INCRBY", key, strconv.FormatInt(delta, 10))
	if err != nil {
		return 0, err
	}
	if e := reply.AsError(); e != nil {
		return 0, fmt.Errorf("quota: INCRBY: %w", e)
	}
	total := reply.Int
	if delta > 0 && total == delta && ttl > 0 {
		secs := int64(ttl / time.Second)
		if secs < 1 {
			secs = 1
		}
		if _, err := s.cli.Do(ctx, "EXPIRE", key, strconv.FormatInt(secs, 10)); err != nil {
			// The counter is correct; only its garbage collection is missing.
			// Failing the request over it would trade a real budget check for a
			// memory leak, so the error is returned but the caller decides.
			return total, fmt.Errorf("quota: EXPIRE: %w", err)
		}
	}
	return total, nil
}

// Get implements Store.
func (s *RedisStore) Get(ctx context.Context, key string) (int64, error) {
	reply, err := s.cli.Do(ctx, "GET", key)
	if err != nil {
		return 0, err
	}
	if e := reply.AsError(); e != nil {
		return 0, fmt.Errorf("quota: GET: %w", e)
	}
	if reply.Null || reply.Str == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(strings.TrimSpace(reply.Str), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("quota: counter %s is not an integer: %w", key, err)
	}
	return v, nil
}

// Close implements Store.
func (s *RedisStore) Close() error { return s.cli.Close() }

// Addr reports the configured address, for the startup log.
func (s *RedisStore) Addr() string { return s.cli.Addr() }

// Stats reports the client's connection pool counters.
func (s *RedisStore) Stats() redis.Stats { return s.cli.Stats() }
