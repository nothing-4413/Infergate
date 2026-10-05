package quota

import (
	"context"
	"sync"
	"time"
)

// Store is the counter backend. Two implementations exist: an in-process map
// for a single replica, and Redis for a fleet.
//
// The surface is deliberately two methods. Everything the quota needs is
// expressible as "add this delta to this counter" and "read this counter", and
// a wider interface would invite a read-modify-write that is not atomic.
type Store interface {
	// Add adds delta to the counter at key and returns the new total. A missing
	// key starts at zero. ttl is applied when the counter is created and must
	// NOT extend the life of an existing one: a sliding expiry would reset a
	// day bucket at midnight plus however busy the tenant was, which is exactly
	// when the window needs to close.
	Add(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error)

	// Get returns the current value, or 0 when the key is absent or expired.
	Get(ctx context.Context, key string) (int64, error)

	// Close releases any resources.
	Close() error
}

// MemoryStore is an in-process counter store.
//
// It is correct for exactly one gateway process. With two replicas each keeps
// its own half of the budget and the fleet spends double the limit, which is
// why the config requires an address when the store is Redis and the section is
// enabled - the failure mode is invisible from the outside.
type MemoryStore struct {
	mu     sync.Mutex
	now    func() time.Time
	values map[string]memoryValue
}

type memoryValue struct {
	value  int64
	expire time.Time // zero means no expiry
}

// NewMemoryStore builds an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{now: time.Now, values: make(map[string]memoryValue)}
}

// NewMemoryStoreWithClock builds an empty store with an injected clock, so a
// test can watch a day bucket roll over without sleeping until midnight.
func NewMemoryStoreWithClock(now func() time.Time) *MemoryStore {
	return &MemoryStore{now: now, values: make(map[string]memoryValue)}
}

// Add implements Store.
func (s *MemoryStore) Add(_ context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	v, ok := s.values[key]
	if ok && !v.expire.IsZero() && !now.Before(v.expire) {
		// Expired: the next Add starts a fresh window.
		delete(s.values, key)
		ok = false
	}
	if !ok {
		v = memoryValue{}
		if ttl > 0 {
			v.expire = now.Add(ttl)
		}
	}
	v.value += delta
	if v.value <= 0 {
		// Nothing is owed: drop the key rather than keep a zero that would
		// outlive its window. This is the ONE place the two stores differ
		// observably: a Redis INCRBY that lands on zero (or below) keeps the
		// key and its value, whereas this one forgets it. The manager never
		// issues a net-negative delta for a counter that has not been reserved
		// first, so the difference cannot change an admission decision; it can
		// only change what a raw store dump shows.
		delete(s.values, key)
		return 0, nil
	}
	s.values[key] = v
	return v.value, nil
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[key]
	if !ok {
		return 0, nil
	}
	if !v.expire.IsZero() && !s.now().Before(v.expire) {
		delete(s.values, key)
		return 0, nil
	}
	return v.value, nil
}

// Close implements Store.
func (s *MemoryStore) Close() error { return nil }

// Len reports how many counters are live. It exists for tests and for the admin
// endpoint's "is anything being tracked" question, not for the request path.
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for _, v := range s.values {
		if v.expire.IsZero() || now.Before(v.expire) {
			n++
		}
	}
	return n
}
