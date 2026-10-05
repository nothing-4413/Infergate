package quota

import (
	"context"
	"testing"
	"time"
)

// The Store interface promises that a ttl applies when a counter is created
// and never extends the life of an existing one: a sliding expiry would reset a
// day bucket at whatever hour the tenant last made a request, which is exactly
// when the window needs to close.
func TestMemoryStoreTTL(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)

	t.Run("a key expires with its ttl", func(t *testing.T) {
		clock := newTestClock(base)
		s := NewMemoryStoreWithClock(clock.Now)
		t.Cleanup(func() { _ = s.Close() })

		if got, err := s.Add(ctx, "k", 10, time.Minute); err != nil || got != 10 {
			t.Fatalf("Add = %d, %v; want 10, nil", got, err)
		}
		if got, err := s.Get(ctx, "k"); err != nil || got != 10 {
			t.Fatalf("Get = %d, %v; want 10, nil", got, err)
		}
		clock.Set(base.Add(59 * time.Second))
		if got, _ := s.Get(ctx, "k"); got != 10 {
			t.Fatalf("the key expired one second early: Get = %d, want 10", got)
		}
		clock.Set(base.Add(61 * time.Second))
		if got, _ := s.Get(ctx, "k"); got != 0 {
			t.Fatalf("Get after the ttl = %d, want 0", got)
		}
		if n := s.Len(); n != 0 {
			t.Fatalf("Len = %d after the only key expired, want 0", n)
		}
		// An expired key is a fresh window, not an accumulator.
		if got, _ := s.Add(ctx, "k", 3, time.Minute); got != 3 {
			t.Fatalf("Add over an expired key = %d, want 3: the old value leaked into the new window", got)
		}
	})

	t.Run("a second Add does not extend the ttl", func(t *testing.T) {
		clock := newTestClock(base)
		s := NewMemoryStoreWithClock(clock.Now)
		t.Cleanup(func() { _ = s.Close() })

		if got, _ := s.Add(ctx, "k", 1, time.Minute); got != 1 {
			t.Fatalf("Add = %d, want 1", got)
		}
		clock.Set(base.Add(40 * time.Second))
		// A far longer ttl on the second write must be ignored: the window ends
		// when the window ends, whoever writes to it.
		if got, _ := s.Add(ctx, "k", 1, time.Hour); got != 2 {
			t.Fatalf("Add = %d, want 2", got)
		}
		clock.Set(base.Add(61 * time.Second))
		if got, _ := s.Get(ctx, "k"); got != 0 {
			t.Fatalf("Get = %d, want 0: the second Add extended the ttl", got)
		}
		if n := s.Len(); n != 0 {
			t.Fatalf("Len = %d, want 0", n)
		}
	})

	t.Run("a value that lands at or below zero removes the key", func(t *testing.T) {
		s := NewMemoryStoreWithClock(func() time.Time { return base })
		t.Cleanup(func() { _ = s.Close() })

		if _, err := s.Add(ctx, "k", 5, time.Hour); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if n := s.Len(); n != 1 {
			t.Fatalf("Len = %d, want 1", n)
		}
		got, err := s.Add(ctx, "k", -5, time.Hour)
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if got != 0 {
			t.Fatalf("Add = %d, want 0 when the counter lands on the floor", got)
		}
		if n := s.Len(); n != 0 {
			t.Fatalf("Len = %d after the counter hit zero, want 0", n)
		}
		if got, _ := s.Get(ctx, "k"); got != 0 {
			t.Fatalf("Get = %d, want 0", got)
		}

		// A negative delta against a missing key must not create one either.
		if got, _ := s.Add(ctx, "ghost", -3, time.Hour); got != 0 {
			t.Fatalf("Add(-3) on a missing key = %d, want 0", got)
		}
		if n := s.Len(); n != 0 {
			t.Fatalf("Len = %d, want 0: a negative delta created a key", n)
		}
	})

	t.Run("a zero ttl never expires", func(t *testing.T) {
		clock := newTestClock(base)
		s := NewMemoryStoreWithClock(clock.Now)
		t.Cleanup(func() { _ = s.Close() })

		if _, err := s.Add(ctx, "k", 1, 0); err != nil {
			t.Fatalf("Add: %v", err)
		}
		clock.Advance(1000 * time.Hour)
		if got, _ := s.Get(ctx, "k"); got != 1 {
			t.Fatalf("Get after 1000 hours with ttl 0 = %d, want 1", got)
		}
		if n := s.Len(); n != 1 {
			t.Fatalf("Len = %d, want 1", n)
		}
	})

	t.Run("Close is a no-op", func(t *testing.T) {
		s := NewMemoryStore()
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}
