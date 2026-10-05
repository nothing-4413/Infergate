package cache

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/infergate/infergate/internal/embed"
)

// MemoryStore keeps entries in the gateway process. It is the default backend
// because it needs no dependency and because a single-replica gateway gets the
// whole benefit from it; Redis is what makes the cache survive a restart and be
// shared by replicas.
//
// Eviction is true LRU (a doubly linked list), not the "delete an arbitrary
// key" shortcut: an Agent's conversation history is exactly the access pattern
// where recency matters, and evicting the entry that is about to be asked again
// turns a hit rate into a coin flip.
type MemoryStore struct {
	mu     sync.Mutex
	scopes map[string]*memScope

	maxPerScope int
	now         func() time.Time
	stats       StoreStats

	// closeCh stops the janitor.
	closeCh chan struct{}
	once    sync.Once
}

type memScope struct {
	entries map[string]*list.Element // key -> element holding Entry
	lru     *list.List               // front = most recently used
}

// MemoryOptions configures a MemoryStore.
type MemoryOptions struct {
	// MaxEntriesPerScope is the LRU bound. Zero means DefaultMaxEntriesPerScope.
	MaxEntriesPerScope int
	// Now overrides the clock, for tests.
	Now func() time.Time
	// SweepInterval controls the janitor that drops expired entries. Zero means
	// one minute; a negative value disables the janitor (tests that want to
	// observe lazy expiry).
	SweepInterval time.Duration
}

// NewMemoryStore builds an in-process store and starts its janitor.
func NewMemoryStore(opts MemoryOptions) *MemoryStore {
	s := &MemoryStore{
		scopes:      make(map[string]*memScope),
		maxPerScope: opts.MaxEntriesPerScope,
		now:         opts.Now,
		closeCh:     make(chan struct{}),
	}
	if s.maxPerScope <= 0 {
		s.maxPerScope = DefaultMaxEntriesPerScope
	}
	if s.now == nil {
		s.now = time.Now
	}
	interval := opts.SweepInterval
	if interval == 0 {
		interval = time.Minute
	}
	if interval > 0 {
		go s.janitor(interval)
	}
	return s
}

// janitor deletes entries whose TTL passed. Expiry is also checked lazily on
// every read, so the janitor affects memory footprint, never correctness - but
// without it a gateway that stored a thousand one-off prompts would hold them
// all until they were touched again, which is never.
func (s *MemoryStore) janitor(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.closeCh:
			return
		case <-t.C:
			s.sweep()
		}
	}
}

func (s *MemoryStore) sweep() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, sc := range s.scopes {
		for key, el := range sc.entries {
			e := el.Value.(*Entry)
			if e.Expired(now) {
				sc.lru.Remove(el)
				delete(sc.entries, key)
				s.stats.Expired++
			}
		}
		if len(sc.entries) == 0 {
			delete(s.scopes, name)
		}
	}
}

func (s *MemoryStore) scope(name string) *memScope {
	sc, ok := s.scopes[name]
	if !ok {
		sc = &memScope{entries: make(map[string]*list.Element), lru: list.New()}
		s.scopes[name] = sc
	}
	return sc
}

// Get returns an unexpired entry and marks it as most recently used.
func (s *MemoryStore) Get(_ context.Context, scope, key string) (Entry, bool, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Gets++
	sc, ok := s.scopes[scope]
	if !ok {
		return Entry{}, false, nil
	}
	el, ok := sc.entries[key]
	if !ok {
		return Entry{}, false, nil
	}
	e := el.Value.(*Entry)
	if e.Expired(now) {
		sc.lru.Remove(el)
		delete(sc.entries, key)
		s.stats.Expired++
		return Entry{}, false, nil
	}
	sc.lru.MoveToFront(el)
	// Return a copy: the caller may keep or mutate the entry (the hit counter
	// is bumped by re-storing it) and must not reach into the LRU's storage.
	return cloneEntry(e), true, nil
}

// Put inserts or replaces an entry and trims the scope to its LRU bound.
func (s *MemoryStore) Put(_ context.Context, e Entry, ttl time.Duration) error {
	now := s.now()
	if e.ExpiresAt.IsZero() {
		e.ExpiresAt = now.Add(ttl)
	}
	stored := cloneEntry(&e)
	stored.Vector = append([]float32(nil), e.Vector...)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Puts++
	sc := s.scope(e.Scope)
	if el, ok := sc.entries[e.Key]; ok {
		el.Value = &stored
		sc.lru.MoveToFront(el)
		return nil
	}
	el := sc.lru.PushFront(&stored)
	sc.entries[e.Key] = el
	for len(sc.entries) > s.maxPerScope {
		back := sc.lru.Back()
		if back == nil {
			break
		}
		victim := back.Value.(*Entry)
		sc.lru.Remove(back)
		delete(sc.entries, victim.Key)
		s.stats.Evicted++
	}
	return nil
}

// Search scans one scope and returns the closest entries. A scope of hundreds of
// 512-dimensional vectors is a few tens of microseconds of cosine, so this is
// deliberately a linear scan (see the Store doc comment).
func (s *MemoryStore) Search(_ context.Context, scope string, vec []float32, threshold float64, limit int) ([]Match, error) {
	if embed.IsZero(vec) {
		// A caller that could not compute a vector must miss, not match
		// everything: see embed.IsZero.
		return nil, nil
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Searches++
	sc, ok := s.scopes[scope]
	if !ok {
		return nil, nil
	}
	out := make([]Match, 0, len(sc.entries))
	for _, el := range sc.entries {
		e := el.Value.(*Entry)
		s.stats.Scanned++
		if e.Expired(now) || len(e.Vector) == 0 {
			// An entry without a vector cannot be found semantically; skipping
			// it here is what makes "exact-match only" entries behave as such.
			continue
		}
		sim := embed.Cosine(vec, e.Vector)
		if sim < threshold {
			continue
		}
		out = append(out, Match{Entry: cloneEntry(e), Similarity: sim})
	}
	Rank(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Delete removes one entry.
func (s *MemoryStore) Delete(_ context.Context, scope, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Deletes++
	sc, ok := s.scopes[scope]
	if !ok {
		return false, nil
	}
	el, ok := sc.entries[key]
	if !ok {
		return false, nil
	}
	sc.lru.Remove(el)
	delete(sc.entries, key)
	if len(sc.entries) == 0 {
		delete(s.scopes, scope)
	}
	return true, nil
}

// Flush removes every entry in a scope, or all of them when scope is empty.
func (s *MemoryStore) Flush(_ context.Context, scope string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Flushes++
	if scope == "" {
		n := 0
		for _, sc := range s.scopes {
			n += len(sc.entries)
		}
		s.scopes = make(map[string]*memScope)
		return n, nil
	}
	sc, ok := s.scopes[scope]
	if !ok {
		return 0, nil
	}
	n := len(sc.entries)
	delete(s.scopes, scope)
	return n, nil
}

// Len counts entries. An empty scope means every scope.
func (s *MemoryStore) Len(_ context.Context, scope string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if scope != "" {
		if sc, ok := s.scopes[scope]; ok {
			return len(sc.entries), nil
		}
		return 0, nil
	}
	n := 0
	for _, sc := range s.scopes {
		n += len(sc.entries)
	}
	return n, nil
}

// Scopes lists the scope names with their entry counts, for /admin/cache.
//
// The signature matches RedisStore.Scopes deliberately: /admin/cache discovers
// the capability by type assertion, so a memory deployment that could not list
// its scopes would report an empty cache exactly when it is working.
func (s *MemoryStore) Scopes(ctx context.Context) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.scopes))
	for name, sc := range s.scopes {
		out[name] = len(sc.entries)
	}
	return out, nil
}

// Stats returns the counters.
func (s *MemoryStore) Stats() StoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Name identifies the backend.
func (s *MemoryStore) Name() string { return "memory" }

// Close stops the janitor. It is idempotent.
func (s *MemoryStore) Close() error {
	s.once.Do(func() { close(s.closeCh) })
	return nil
}

func cloneEntry(e *Entry) Entry {
	out := *e
	if e.Headers != nil {
		out.Headers = make(map[string]string, len(e.Headers))
		for k, v := range e.Headers {
			out.Headers[k] = v
		}
	}
	if e.Body != nil {
		out.Body = append([]byte(nil), e.Body...)
	}
	if e.Vector != nil {
		out.Vector = append([]float32(nil), e.Vector...)
	}
	return out
}
