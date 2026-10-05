// Package idempotency implements the replay store behind the Idempotency-Key
// request header.
//
// The problem it solves is not caching. A cache answers "have I seen this
// question before?" and is allowed to miss; a replay store answers "has this
// exact request already been carried out?" and must be able to say yes, because
// the caller is an agent whose retry after a timeout must not charge a second
// time, must not call the upstream a second time, and must not produce a second
// side effect. The two differ in the details that matter:
//
//   - the key is the caller's, not a hash of the payload, so two requests that
//     differ only in a timestamp still collide on purpose;
//   - a key is claimed BEFORE the work starts, so a concurrent duplicate is a
//     conflict rather than a second upstream call;
//   - a duplicate with a DIFFERENT body under the same key is an error, not a
//     hit: serving it would answer a question the caller did not ask;
//   - entries are scoped by tenant, because two tenants that both pick the key
//     "1" are not talking about the same request.
package idempotency

import (
	"container/list"
	"sort"
	"strings"
	"sync"
	"time"
)

// Decision is what Begin concluded about a key.
type Decision int

const (
	// Proceed means no entry exists. The caller now owns the key and must call
	// Complete when it has a response, or Abort when it never got one.
	Proceed Decision = iota
	// Replay means a completed response is stored under this key and must be
	// served verbatim, without touching the upstream.
	Replay
	// ConflictBody means the key was used before for a different request body.
	ConflictBody
	// ConflictInFlight means another request with this key is being served
	// right now.
	ConflictInFlight
)

// String names the decision for logs and JSON.
func (d Decision) String() string {
	switch d {
	case Proceed:
		return "proceed"
	case Replay:
		return "replay"
	case ConflictBody:
		return "conflict_body"
	case ConflictInFlight:
		return "in_flight"
	}
	return "unknown"
}

// Entry is a completed response plus the facts a replay reports back.
//
// Headers is a full http.Header-shaped map rather than the small subset the
// gateway itself sets, because a replay must reproduce what the caller saw:
// the content type and any upstream headers (a request id, a rate-limit
// remainder) are part of the answer.
type Entry struct {
	Scope       string              `json:"scope,omitempty"`
	Key         string              `json:"key"`
	RequestHash string              `json:"request_hash,omitempty"`
	Status      int                 `json:"status"`
	Headers     map[string][]string `json:"-"`
	Body        []byte              `json:"-"`
	RequestID   string              `json:"request_id,omitempty"`
	Upstream    string              `json:"upstream,omitempty"`
	Model       string              `json:"model,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
	Outcome     string              `json:"outcome,omitempty"`
	CreatedAt   time.Time           `json:"created_at"`
	CompletedAt time.Time           `json:"completed_at"`
	ExpiresAt   time.Time           `json:"expires_at"`
}

// Age reports how long ago the stored response was completed.
func (e Entry) Age(now time.Time) time.Duration {
	if e.CompletedAt.IsZero() {
		return 0
	}
	return now.Sub(e.CompletedAt)
}

// Expired reports whether the entry's TTL has passed.
func (e Entry) Expired(now time.Time) bool {
	return !e.ExpiresAt.IsZero() && !now.Before(e.ExpiresAt)
}

// BodyLen is the stored body size, for /admin output.
func (e Entry) BodyLen() int { return len(e.Body) }

// Stats are the counters /metrics and /admin/idempotency report.
type Stats struct {
	// Lookups counts every Begin. Misses counts the first use of a key, so
	// Stored/Misses is "how many of the requests we were asked about were
	// remembered" and Hits/Lookups is the share answered without an upstream.
	Lookups   uint64 `json:"lookups"`
	Hits      uint64 `json:"hits"`
	Misses    uint64 `json:"misses"`
	Conflicts uint64 `json:"conflicts"`
	// InFlightRejects counts duplicate requests refused because the original
	// was still running. It is a counter, not a gauge: the live count of
	// claimed-but-unfinished keys is Store.InFlight().
	InFlightRejects uint64 `json:"in_flight_rejects"`
	Stored          uint64 `json:"stored"`
	Oversize        uint64 `json:"oversize"`
	Evicted         uint64 `json:"evicted"`
	Expired         uint64 `json:"expired"`
	Aborted         uint64 `json:"aborted"`
}

type item struct {
	entry    Entry
	inflight bool
}

type scope struct {
	keys map[string]*list.Element // key -> element holding *item
	lru  *list.List               // front = most recently used
}

// Store is the in-process replay store. It mirrors internal/cache's memory
// store (scopes, true LRU, a janitor for footprint rather than correctness)
// because the access pattern is the same shape: a hot small working set and a
// long tail of one-off keys.
type Store struct {
	mu     sync.Mutex
	scopes map[string]*scope

	capacity         int
	ttl              time.Duration
	maxResponseBytes int64
	now              func() time.Time
	stats            Stats

	closeCh chan struct{}
	once    sync.Once
}

// Options configures a Store.
type Options struct {
	// Capacity is the LRU bound across all scopes. Zero means
	// DefaultCapacity.
	Capacity int
	// TTL is how long a completed entry stays replayable, measured from the
	// moment it completed. Zero means DefaultTTL.
	TTL time.Duration
	// MaxResponseBytes bounds a single stored response. Zero means
	// DefaultMaxResponseBytes. A response larger than this is served but not
	// remembered, which is reported rather than silently dropped.
	MaxResponseBytes int64
	// Now overrides the clock, for tests.
	Now func() time.Time
	// SweepInterval controls the janitor. Zero means one minute; a negative
	// value disables it (expiry is also checked on every read, so the janitor
	// only affects memory footprint).
	SweepInterval time.Duration
}

// Defaults for Options. They are exported so config validation, the admin
// surface and the docs quote the same numbers.
const (
	DefaultCapacity         = 2048
	DefaultTTL              = 15 * time.Minute
	DefaultMaxResponseBytes = 1 << 20 // 1 MiB
)

// New builds a store and starts its janitor.
func New(opts Options) *Store {
	s := &Store{
		scopes:           make(map[string]*scope),
		capacity:         opts.Capacity,
		ttl:              opts.TTL,
		maxResponseBytes: opts.MaxResponseBytes,
		now:              opts.Now,
		closeCh:          make(chan struct{}),
	}
	if s.capacity <= 0 {
		s.capacity = DefaultCapacity
	}
	if s.ttl <= 0 {
		s.ttl = DefaultTTL
	}
	if s.maxResponseBytes <= 0 {
		s.maxResponseBytes = DefaultMaxResponseBytes
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

func (s *Store) janitor(interval time.Duration) {
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

func (s *Store) sweep() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, sc := range s.scopes {
		for key, el := range sc.keys {
			it := el.Value.(*item)
			// An in-flight claim is never swept: its TTL is a timeout on a
			// request that is still running, and dropping it would let a
			// duplicate start a second upstream call.
			if !it.inflight && it.entry.Expired(now) {
				sc.lru.Remove(el)
				delete(sc.keys, key)
				s.stats.Expired++
			}
		}
		if len(sc.keys) == 0 {
			delete(s.scopes, name)
		}
	}
}

func (s *Store) scope(name string) *scope {
	sc, ok := s.scopes[name]
	if !ok {
		sc = &scope{keys: make(map[string]*list.Element), lru: list.New()}
		s.scopes[name] = sc
	}
	return sc
}

// Begin decides what to do with (scope, key) and, on Proceed, claims the key.
//
// requestHash is the caller's fingerprint of the request body. It is compared
// only against a previously stored entry, never used as the lookup key: the
// lookup key is the caller's own key, which is the whole point of the header.
func (s *Store) Begin(scopeName, key, requestHash string) (Decision, Entry) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Lookups++

	sc, ok := s.scopes[scopeName]
	if !ok {
		s.stats.Misses++
		return Proceed, s.claim(sc, scopeName, key, requestHash, now)
	}
	el, ok := sc.keys[key]
	if !ok {
		s.stats.Misses++
		return Proceed, s.claim(sc, scopeName, key, requestHash, now)
	}
	it := el.Value.(*item)
	if !it.inflight && it.entry.Expired(now) {
		sc.lru.Remove(el)
		delete(sc.keys, key)
		s.stats.Expired++
		s.stats.Misses++
		return Proceed, s.claim(sc, scopeName, key, requestHash, now)
	}
	if it.inflight {
		s.stats.InFlightRejects++
		return ConflictInFlight, Entry{}
	}
	if it.entry.RequestHash != requestHash {
		s.stats.Conflicts++
		return ConflictBody, clone(it.entry)
	}
	sc.lru.MoveToFront(el)
	s.stats.Hits++
	return Replay, clone(it.entry)
}

// claim inserts an in-flight placeholder. The caller must hold s.mu.
func (s *Store) claim(sc *scope, scopeName, key, requestHash string, now time.Time) Entry {
	if sc == nil {
		sc = s.scope(scopeName)
	}
	e := Entry{
		Scope:       scopeName,
		Key:         key,
		RequestHash: requestHash,
		CreatedAt:   now,
		// The TTL of a claim only bounds how long a crashed request can block
		// its own key; Complete replaces it with a completion-relative TTL.
		ExpiresAt: now.Add(s.ttl),
	}
	sc.keys[key] = sc.lru.PushFront(&item{entry: e, inflight: true})
	s.trim(sc)
	return e
}

// Complete stores the finished response under the key claimed by Begin.
//
// It returns false with a reason when the response is not replayable (only
// "oversize" today). A false return still releases the claim: the caller got a
// correct answer, it just cannot be replayed, and leaving the key in flight
// forever would turn "too big to remember" into "permanently broken".
func (s *Store) Complete(scopeName, key string, e Entry) (bool, string) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	sc := s.scope(scopeName)
	el, ok := sc.keys[key]
	if !ok {
		// The claim was evicted or flushed while the request ran. Store the
		// result anyway: the key is in the caller's hands and the answer is
		// worth remembering.
		el = sc.lru.PushFront(&item{})
		sc.keys[key] = el
	}
	// The request hash belongs to the claim, not to the response: Begin
	// compared it and Complete must keep comparing against it, or every replay
	// would look like a different body.
	if prev := el.Value.(*item); prev.entry.RequestHash != "" {
		e.RequestHash = prev.entry.RequestHash
	}
	if int64(len(e.Body)) > s.maxResponseBytes {
		sc.lru.Remove(el)
		delete(sc.keys, key)
		if len(sc.keys) == 0 {
			delete(s.scopes, scopeName)
		}
		s.stats.Oversize++
		return false, "oversize"
	}
	e.Scope = scopeName
	e.Key = key
	e.CompletedAt = now
	e.ExpiresAt = now.Add(s.ttl)
	el.Value = &item{entry: clone(e)}
	sc.lru.MoveToFront(el)
	s.stats.Stored++
	s.trim(sc)
	return true, ""
}

// Abort releases a claim whose request produced no replayable response (a
// transport failure the caller must be free to retry).
func (s *Store) Abort(scopeName, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.scopes[scopeName]
	if !ok {
		return
	}
	el, ok := sc.keys[key]
	if !ok {
		return
	}
	if !el.Value.(*item).inflight {
		return
	}
	sc.lru.Remove(el)
	delete(sc.keys, key)
	if len(sc.keys) == 0 {
		delete(s.scopes, scopeName)
	}
	s.stats.Aborted++
}

// Get returns a completed entry without claiming anything, for /admin lookups.
func (s *Store) Get(scopeName, key string) (Entry, bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.scopes[scopeName]
	if !ok {
		return Entry{}, false
	}
	el, ok := sc.keys[key]
	if !ok {
		return Entry{}, false
	}
	it := el.Value.(*item)
	if it.inflight || it.entry.Expired(now) {
		return Entry{}, false
	}
	return clone(it.entry), true
}

// List returns completed entries, newest first, capped at limit.
func (s *Store) List(limit int) []Entry {
	now := s.now()
	s.mu.Lock()
	out := make([]Entry, 0, 16)
	for _, sc := range s.scopes {
		for _, el := range sc.keys {
			it := el.Value.(*item)
			if it.inflight || it.entry.Expired(now) {
				continue
			}
			out = append(out, clone(it.entry))
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CompletedAt.Equal(out[j].CompletedAt) {
			return out[i].Key < out[j].Key
		}
		return out[i].CompletedAt.After(out[j].CompletedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Flush removes every entry in one scope, or all of them when scope is empty.
// In-flight claims are dropped too: this is an operator action.
func (s *Store) Flush(scopeName string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if scopeName == "" {
		n := 0
		for _, sc := range s.scopes {
			n += len(sc.keys)
		}
		s.scopes = make(map[string]*scope)
		return n
	}
	sc, ok := s.scopes[scopeName]
	if !ok {
		return 0
	}
	n := len(sc.keys)
	delete(s.scopes, scopeName)
	return n
}

// Len counts completed entries.
func (s *Store) Len() int {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, sc := range s.scopes {
		for _, el := range sc.keys {
			it := el.Value.(*item)
			if !it.inflight && !it.entry.Expired(now) {
				n++
			}
		}
	}
	return n
}

// InFlight counts keys claimed but not completed.
func (s *Store) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, sc := range s.scopes {
		for _, el := range sc.keys {
			if el.Value.(*item).inflight {
				n++
			}
		}
	}
	return n
}

// Scopes lists the tenant scopes with their entry counts.
func (s *Store) Scopes() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.scopes))
	for name, sc := range s.scopes {
		out[name] = len(sc.keys)
	}
	return out
}

// trim enforces the LRU bound. The caller must hold s.mu.
func (s *Store) trim(sc *scope) {
	for len(sc.keys) > s.capacity {
		back := sc.lru.Back()
		if back == nil {
			return
		}
		victim := back.Value.(*item)
		sc.lru.Remove(back)
		delete(sc.keys, victim.entry.Key)
		s.stats.Evicted++
	}
}

// Stats returns the counters.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Capacity, TTL and MaxResponseBytes expose the effective bounds so the admin
// surface reports what the store actually enforces rather than what the config
// asked for.
func (s *Store) Capacity() int             { return s.capacity }
func (s *Store) TTL() time.Duration        { return s.ttl }
func (s *Store) MaxResponseBytes() int64   { return s.maxResponseBytes }
func (s *Store) ScopeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.scopes)
}

// Close stops the janitor. It is idempotent.
func (s *Store) Close() error {
	s.once.Do(func() { close(s.closeCh) })
	return nil
}

// KeyOf normalises a caller's key. Keys are case-sensitive (they are opaque
// tokens, and lowercasing them would merge two keys a caller deliberately
// distinguished) but surrounding whitespace is not part of the token.
func KeyOf(raw string) string { return strings.TrimSpace(raw) }

func clone(e Entry) Entry {
	out := e
	if e.Headers != nil {
		out.Headers = make(map[string][]string, len(e.Headers))
		for k, v := range e.Headers {
			out.Headers[k] = append([]string(nil), v...)
		}
	}
	if e.Body != nil {
		out.Body = append([]byte(nil), e.Body...)
	}
	return out
}
