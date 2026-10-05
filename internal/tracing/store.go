package tracing

import (
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// DefaultCapacity is the trace-store capacity used when NewStore is given a
// non-positive capacity.
//
// 1024 traces of typical size (a handful of spans, a few hundred bytes each)
// is on the order of a megabyte -- small enough to leave resident forever on a
// gateway host, large enough to cover the "what happened to that request five
// minutes ago" question on a busy instance. It is a memory ceiling, not a
// target: the store holds traces so they can be inspected while the interesting
// window is still open, and an operator who needs history should export, which
// is what WriteJSONL is for.
const DefaultCapacity = 1024

// ErrNilTrace is returned by WriteJSONL when asked to render no trace.
var ErrNilTrace = errors.New("tracing: nil trace")

// entry is one stored trace. The clone is made on the way in (see Add) and on
// the way out (see Get), so nothing outside the store ever holds a reference to
// this pointer.
type entry struct {
	trace   *Trace
	summary Summary
}

// Store is a bounded, insertion-ordered, concurrency-safe set of recent traces
// indexed by both trace id and request id.
//
// # Design
//
//   - Eviction is FIFO by insertion, not LRU. A stored trace is read at most a
//     handful of times -- an operator replaying a request -- so recency of
//     ACCESS carries almost no information, while the bookkeeping an LRU needs
//     is another thing to get wrong under a mutex. Oldest-first drops the trace
//     least likely to still be interesting.
//
//   - Entries live in a RING BUFFER, not a slice. This is the structural
//     decision that makes the index cheap: with a slice, evicting entries[0]
//     shifts every stored position and every id in the index becomes wrong,
//     which is the worst failure this package can have (an operator debugging an
//     incident reads a reply belonging to someone else's request). With a ring,
//     an entry's slot never changes, so the index is patched in O(1) per Add and
//     eviction is two map deletes.
//
//   - The store does NOT hold its lock across I/O. WriteJSONL takes an explicit
//     writer and trace and is not a method on Store for exactly this reason:
//     rendering always happens outside the mutex, so a blocked writer (an
//     operator's curl, a log file on a full disk) cannot stall request-path
//     Adds.
//
//   - Add copies in and Get copies out. Neither direction of aliasing is
//     allowed to survive, which is why the lock is held while cloning rather
//     than around the whole call.
//
// A Store must be built with NewStore; the zero value has no capacity and the
// method set assumes the mutex is usable, not that the fields are initialized.
type Store struct {
	mu       sync.RWMutex
	capacity int
	entries  []entry        // ring buffer, len == capacity
	head     int            // index of the oldest entry
	count    int            // number of live entries (head .. head+count-1 mod cap)
	index    map[string]int // trace id or request id -> fixed ring slot
	dropped  int64
}

// NewStore returns a store holding at most capacity traces. A capacity <= 0
// means DefaultCapacity; the store never has capacity 0, because a store that
// silently holds nothing looks identical to broken instrumentation.
func NewStore(capacity int) *Store {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Store{
		capacity: capacity,
		entries:  make([]entry, capacity),
		index:    make(map[string]int, capacity*2),
	}
}

// Add stores a COPY of t, evicting the oldest trace when the store is at
// capacity. Dropped counts evictions for the lifetime of the store (Reset does
// not clear it: "how many traces did I lose" is a counter, not a gauge).
//
// The copy is the point of the method. After Add returns, the caller may append
// spans and write into t's attribute maps as much as it likes; nothing it does
// is visible through Get, and -- more importantly -- nothing it does races with
// a concurrent Get. Both directions are covered by tests.
//
// A trace with an EMPTY TraceID is stored and counted by Len (WriteJSONL exists
// to dump it), but is deliberately NOT indexed: an empty key would collide
// across every such trace and make Get("") return an arbitrary one.
// Instrumentation that forgets to set a trace id therefore gets a trace that
// can be listed and counted but never fetched -- a loud, cheap failure mode. Do
// not "fix" this by indexing the empty string. Request ids are indexed
// independently, so such a trace is still reachable by request id.
//
// Re-adding a trace id that is already present REPLACES the payload IN PLACE:
// the slot does not move, so List order is stable and Get immediately returns
// the newer version. Replace rather than append because a trace id arriving
// twice means a request was re-finalized, and two entries for one id would make
// Get return whichever the caller meant least. The old request id keeps
// resolving to the slot (it now yields the newer payload) unless the new
// payload carries a different request id, which is also indexed.
//
// Add is safe for concurrent use, and never does I/O under the lock.
func (s *Store) Add(t *Trace) {
	if t == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addLocked(t)
}

// addLocked inserts a clone of t. The caller holds the write lock.
func (s *Store) addLocked(t *Trace) {
	cp := t.Clone()

	// Replace-in-place path: the trace id is already indexed.
	if cp.TraceID != "" {
		if slot, ok := s.index[cp.TraceID]; ok {
			s.entries[slot] = entry{trace: cp, summary: cp.Summary()}
			if cp.RequestID != "" {
				s.index[cp.RequestID] = slot
			}
			return
		}
	}

	// Evict the oldest entry, releasing its index keys so they cannot resolve
	// to a slot that now holds a different trace.
	if s.count == s.capacity {
		old := &s.entries[s.head]
		if old.trace != nil {
			delete(s.index, old.trace.TraceID)
			delete(s.index, old.trace.RequestID)
		}
		old.trace, old.summary = nil, Summary{}
		s.head = (s.head + 1) % s.capacity
		s.count--
		s.dropped++
	}

	slot := (s.head + s.count) % s.capacity
	s.entries[slot] = entry{trace: cp, summary: cp.Summary()}
	s.count++
	if cp.TraceID != "" {
		s.index[cp.TraceID] = slot
	}
	if cp.RequestID != "" {
		s.index[cp.RequestID] = slot
	}
}

// entryAt returns the live entry in the given slot.
func (s *Store) entryAt(slot int) (entry, bool) {
	if slot < 0 || slot >= s.capacity {
		return entry{}, false
	}
	e := s.entries[slot]
	if e.trace == nil {
		return entry{}, false
	}
	return e, true
}

// Get returns a CLONE of the trace matching id, which may be either a trace id
// (32 hex chars) or a request id. Lookup is exact and case-sensitive: the store
// holds exactly the strings it was given, and a case-insensitive match would
// let two distinct ids resolve to one trace.
//
// It returns a clone for the same reason Add makes one: handing out the stored
// pointer would let a reader mutate a trace that concurrent readers are
// summarizing, and would make the store's contents depend on who read them.
//
// The boolean is worth checking even when the trace "must" exist: for an id
// that is absent, this method cannot distinguish "never recorded", "evicted"
// and "malformed id". Only the caller can, and only the caller knows which
// explanation to put in front of an operator. Dropped() is the number that
// separates the first two.
func (s *Store) Get(id string) (*Trace, bool) {
	if id == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	slot, ok := s.index[id]
	if !ok {
		return nil, false
	}
	e, ok := s.entryAt(slot)
	if !ok {
		return nil, false
	}
	return e.trace.Clone(), true
}

// List returns up to limit summaries, NEWEST FIRST (reverse insertion order).
//
// It returns the summaries computed once at Add time and never rebuilds them or
// touches the spans: a list endpoint must be cheap and allocation-light, and
// cloning whole traces to render a table is how an observability feature turns
// into a latency problem on the request path it is supposed to measure.
//
// limit <= 0 means "all"; a limit larger than the store means the same. The
// returned slice is always freshly allocated, so the caller may sort or
// truncate it without corrupting the store.
//
// The order is INSERTION order, which is deterministic but not necessarily
// start-time order: a long request that started first but finished (and was
// added) last appears newest. Sorting by StartedAt would match a UI's intuition
// and would make every List O(n log n); the caller that wants time order can
// sort this slice, because these are values, not shared state.
func (s *Store) List(limit int) []Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > s.count {
		limit = s.count
	}
	out := make([]Summary, 0, limit)
	for i := 0; i < limit; i++ {
		slot := (s.head + s.count - 1 - i + s.capacity) % s.capacity
		out = append(out, s.entries[slot].summary)
	}
	return out
}

// Len returns the number of traces currently held. It is a live gauge: Reset
// takes it back to 0, while Dropped keeps counting.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.count
}

// Dropped returns the number of traces evicted because the store was full.
//
// Dropped is a LIFETIME counter and Reset does not clear it. The question it
// answers is "was the trace I am looking for never recorded, or pushed out?",
// and a counter that reset would erase exactly the evidence an operator needs
// after a config reload or a manual Reset. If Dropped is climbing steadily, the
// answer is "raise the capacity or export", not "the instrumentation is broken".
func (s *Store) Dropped() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dropped
}

// Reset empties the store, keeping the configured capacity and the Dropped
// counter.
//
// It clears the whole ring and the whole index rather than walking entries: a
// half-cleared structure is the one state in which a stale index entry can
// outlive the trace it names, and an empty ring is trivially consistent. After
// a Reset, re-adding a previously stored trace id is an ordinary insert.
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		s.entries[i] = entry{}
	}
	s.index = make(map[string]int, s.capacity*2)
	s.head, s.count = 0, 0
}

// WriteJSONL writes t as one compact JSON object followed by '\n'.
//
// It is deliberately a FUNCTION taking a writer and a trace, not a method that
// renders the store: rendering is the only operation here that can block on
// something outside the process (a pipe, a slow console, a full disk), and a
// store-rendering method would invite a caller to hold the store's mutex across
// that write and stall every Add on the request path. The intended usage is
// Get (which returns a clone) followed by WriteJSONL with no lock held.
//
// One object per line and no indentation: JSONL is a stream format for `jq` and
// for log shippers, both of which care about lines far more than about bytes.
//
// A nil trace is an error, not an empty line: writing a blank line where a
// trace was expected produces a stream that parses as nothing and therefore
// reports no corruption.
func (s *Store) WriteJSONL(w io.Writer, t *Trace) error {
	if t == nil {
		return ErrNilTrace
	}
	enc := json.NewEncoder(w)
	return enc.Encode(t)
}
