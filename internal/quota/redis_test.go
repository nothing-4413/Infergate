package quota

import (
	"context"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/mockredis"
)

// newTestRedisStore starts an in-process RESP2 server and points a RedisStore
// at it. The listener is on 127.0.0.1:0 rather than a fixed port so two runs of
// the suite cannot collide; srv.Addr() reports the port the OS chose.
func newTestRedisStore(t *testing.T) *RedisStore {
	t.Helper()
	srv := mockredis.New(mockredis.Options{Addr: "127.0.0.1:0"})
	if err := srv.Start(); err != nil {
		t.Fatalf("mockredis.Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	s, err := NewRedisStore(RedisOptions{
		Addr:         srv.Addr(),
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRedisStore(%s): %v", srv.Addr(), err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ttlSeconds reads the key's ttl straight off the wire. TTL is -1 for a key
// with no expiry and -2 for a missing one.
func ttlSeconds(t *testing.T, s *RedisStore, key string) int64 {
	t.Helper()
	rep, err := s.cli.Do(context.Background(), "TTL", key)
	if err != nil {
		t.Fatalf("TTL %s: %v", key, err)
	}
	if rep.Kind != ':' {
		t.Fatalf("TTL %s replied with %q, want an integer", key, string(rep.Kind))
	}
	return rep.Int
}

func TestRedisStore(t *testing.T) {
	ctx := context.Background()

	t.Run("add and get round trip", func(t *testing.T) {
		s := newTestRedisStore(t)
		if got, err := s.Get(ctx, "missing"); err != nil || got != 0 {
			t.Fatalf("Get on a missing key = %d, %v; want 0, nil", got, err)
		}
		if got, err := s.Add(ctx, "k", 40, time.Hour); err != nil || got != 40 {
			t.Fatalf("Add = %d, %v; want 40, nil", got, err)
		}
		if got, err := s.Get(ctx, "k"); err != nil || got != 40 {
			t.Fatalf("Get = %d, %v; want 40, nil", got, err)
		}
		if s.Addr() == "" {
			t.Fatal("Addr() is empty")
		}
		// The counter is written as a decimal string, so the second increment
		// has to parse what the first one wrote.
		if _, err := s.Add(ctx, "k", 2, time.Hour); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if got, _ := s.Get(ctx, "k"); got != 42 {
			t.Fatalf("Get = %d, want 42", got)
		}
	})

	t.Run("the ttl is set once and not extended", func(t *testing.T) {
		s := newTestRedisStore(t)
		if got, err := s.Add(ctx, "k", 40, time.Hour); err != nil || got != 40 {
			t.Fatalf("Add = %d, %v; want 40, nil", got, err)
		}
		first := ttlSeconds(t, s, "k")
		if first <= 0 {
			t.Fatalf("TTL = %d after the first positive Add, want > 0", first)
		}
		if first > 3600 {
			t.Fatalf("TTL = %d after adding with a one hour ttl, want <= 3600", first)
		}

		if got, err := s.Add(ctx, "k", 10, time.Minute); err != nil || got != 50 {
			t.Fatalf("second Add = %d, %v; want 50, nil", got, err)
		}
		second := ttlSeconds(t, s, "k")
		if second <= 0 {
			t.Fatal("the ttl disappeared on the second Add")
		}
		if second > first {
			t.Fatalf("the second Add extended the ttl from %ds to %ds", first, second)
		}

		// A negative first write is not new traffic, so it must not arm a ttl.
		if _, err := s.Add(ctx, "other", -1, time.Hour); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if got := ttlSeconds(t, s, "other"); got != -1 {
			t.Fatalf("TTL of a counter created by a negative delta = %d, want -1 (no expiry)", got)
		}
		if got, err := s.Get(ctx, "other"); err != nil || got != -1 {
			// Redis keeps the negative counter; MemoryStore deletes anything
			// that lands at or below zero. Both are unreachable from the
			// manager, whose deltas never take a counter below zero.
			t.Fatalf("Get = %d, %v; want -1, nil", got, err)
		}
	})

	t.Run("a negative delta decrements", func(t *testing.T) {
		s := newTestRedisStore(t)
		if got, _ := s.Add(ctx, "k", 40, time.Hour); got != 40 {
			t.Fatalf("Add = %d, want 40", got)
		}
		if got, err := s.Add(ctx, "k", -25, time.Hour); err != nil || got != 15 {
			t.Fatalf("Add(-25) = %d, %v; want 15, nil", got, err)
		}
		if got, _ := s.Get(ctx, "k"); got != 15 {
			t.Fatalf("Get = %d, want 15", got)
		}
		if got, _ := s.Add(ctx, "k", -15, time.Hour); got != 0 {
			t.Fatalf("Add(-15) = %d, want 0", got)
		}
		// MemoryStore drops the key here; Redis keeps the stored 0. Both read
		// back as 0, so the difference is not observable through Get.
		if got, err := s.Get(ctx, "k"); err != nil || got != 0 {
			t.Fatalf("Get after the counter reached zero = %d, %v; want 0, nil", got, err)
		}
	})

	t.Run("close refuses new work", func(t *testing.T) {
		s := newTestRedisStore(t)
		if got, err := s.Add(ctx, "k", 1, time.Hour); err != nil || got != 1 {
			t.Fatalf("Add = %d, %v; want 1, nil", got, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		if _, err := s.Add(ctx, "k", 1, time.Hour); err == nil {
			t.Fatal("Add after Close must fail: the client refuses new work")
		}
		if _, err := s.Get(ctx, "k"); err == nil {
			t.Fatal("Get after Close must fail")
		}
	})
}

func TestNewRedisStoreRejectsABadAddress(t *testing.T) {
	if _, err := NewRedisStore(RedisOptions{}); err == nil {
		t.Fatal("an empty address must fail at construction, not silently disable the quota")
	}
	if _, err := NewRedisStore(RedisOptions{Addr: "   "}); err == nil {
		t.Fatal("a whitespace address must fail at construction")
	}
	if _, err := NewRedisStore(RedisOptions{
		Addr:         "127.0.0.1:1",
		DialTimeout:  300 * time.Millisecond,
		ReadTimeout:  300 * time.Millisecond,
		WriteTimeout: 300 * time.Millisecond,
	}); err == nil {
		t.Fatal("an unreachable address must fail at construction with a clear error")
	}
}
