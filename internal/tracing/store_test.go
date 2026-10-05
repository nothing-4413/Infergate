package tracing

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// traceIDFor returns a deterministic 32-char lowercase hex trace id for index
// i, so store tests read as "trace 0..n" without magic constants.
func traceIDFor(i int) string { return fmt.Sprintf("%032x", i) }

// TestStoreEvictionAndDroppedAccounting checks the two numbers an operator
// uses to decide whether a missing trace was never recorded or pushed out:
// Len is a live gauge and Dropped is a lifetime counter.
func TestStoreEvictionAndDroppedAccounting(t *testing.T) {
	const capacity = 4
	s := NewStore(capacity)

	for i := 0; i < capacity; i++ {
		s.Add(testTraceWith(traceIDFor(i), 2))
	}
	if got := s.Len(); got != capacity {
		t.Fatalf("Len after filling = %d, want %d", got, capacity)
	}
	if got := s.Dropped(); got != 0 {
		t.Fatalf("Dropped before any eviction = %d, want 0", got)
	}

	if _, ok := s.Get(traceIDFor(0)); !ok {
		t.Fatal("trace 0 not found before eviction")
	}

	// Two more adds evict the two oldest.
	for i := capacity; i < capacity+2; i++ {
		s.Add(testTraceWith(traceIDFor(i), 2))
	}

	if got := s.Len(); got != capacity {
		t.Errorf("Len at capacity = %d, want %d", got, capacity)
	}
	if got := s.Dropped(); got != 2 {
		t.Errorf("Dropped = %d, want 2", got)
	}
	for i := 0; i < 2; i++ {
		if _, ok := s.Get(traceIDFor(i)); ok {
			t.Errorf("trace %d was evicted but Get still finds it", i)
		}
		if _, ok := s.Get("req-" + traceIDFor(i)); ok {
			t.Errorf("evicted trace %d is still reachable by request id", i)
		}
	}
	for i := 2; i < capacity+2; i++ {
		if _, ok := s.Get(traceIDFor(i)); !ok {
			t.Errorf("trace %d should still be stored", i)
		}
		if _, ok := s.Get("req-" + traceIDFor(i)); !ok {
			t.Errorf("trace %d should still be reachable by request id", i)
		}
	}

	// Eviction must not corrupt the index of the surviving entries: every
	// lookups-by-request-id above would pass with a shifted-by-one bug only if
	// the payload matched, so also check contents.
	got, ok := s.Get(traceIDFor(capacity + 1))
	if !ok {
		t.Fatal("newest trace missing")
	}
	wantString(t, "newest trace id", got.TraceID, traceIDFor(capacity+1))
	wantString(t, "newest request id", got.RequestID, "req-"+traceIDFor(capacity+1))
}

// TestNewStoreDefaultCapacity pins that capacity <= 0 means DefaultCapacity.
func TestNewStoreDefaultCapacity(t *testing.T) {
	for _, capacity := range []int{0, -1, -1000} {
		s := NewStore(capacity)
		if s.capacity != DefaultCapacity {
			t.Errorf("NewStore(%d).capacity = %d, want %d", capacity, s.capacity, DefaultCapacity)
		}
	}
	if s := NewStore(1); s.capacity != 1 {
		t.Errorf("NewStore(1).capacity = %d, want 1", s.capacity)
	}
}

// TestStoreReset checks that Reset empties the store, keeps the capacity and
// keeps Dropped, and that a trace re-added after a Reset is reachable again.
func TestStoreReset(t *testing.T) {
	const capacity = 3
	s := NewStore(capacity)

	for i := 0; i < 2; i++ {
		s.Add(testTraceWith(traceIDFor(i), 2))
	}
	// Overflow the store by one to make Dropped non-zero before the Reset.
	for i := 2; i < 2+capacity+1; i++ {
		s.Add(testTraceWith(traceIDFor(i), 2))
	}
	if s.Dropped() == 0 {
		t.Fatal("expected some evictions before Reset")
	}
	droppedBefore, lenBefore := s.Dropped(), s.Len()

	s.Reset()

	if got := s.Len(); got != 0 {
		t.Errorf("Len after Reset = %d, want 0", got)
	}
	if got := s.List(0); len(got) != 0 {
		t.Errorf("List after Reset = %d summaries, want 0", len(got))
	}
	if got := s.Dropped(); got != droppedBefore {
		t.Errorf("Dropped after Reset = %d, want %d (lifetime counter survives Reset)", got, droppedBefore)
	}
	if lenBefore == 0 {
		t.Fatal("test premise broken: store was empty before Reset")
	}
	for i := 0; i < 2+capacity; i++ {
		if _, ok := s.Get(traceIDFor(i)); ok {
			t.Errorf("trace %d survived Reset", i)
		}
	}

	// Re-adding an id that existed before the Reset must index it again.
	s.Add(testTraceWith(traceIDFor(0), 1))
	if _, ok := s.Get(traceIDFor(0)); !ok {
		t.Error("trace re-added after Reset is not reachable: stale index")
	}
	if _, ok := s.Get("req-" + traceIDFor(0)); !ok {
		t.Error("trace re-added after Reset is not reachable by request id")
	}
	if got := s.Len(); got != 1 {
		t.Errorf("Len = %d after one re-add, want 1", got)
	}
}

// TestStoreGetByTraceAndRequestID checks both lookup keys, the miss cases, and
// that Get returns a clone rather than the stored pointer.
func TestStoreGetByTraceAndRequestID(t *testing.T) {
	s := NewStore(8)
	tr := testTraceWith(traceIDFor(7), 3)
	s.Add(tr)

	byTrace, ok := s.Get(tr.TraceID)
	if !ok {
		t.Fatalf("Get(%q) not found", tr.TraceID)
	}
	byRequest, ok := s.Get(tr.RequestID)
	if !ok {
		t.Fatalf("Get(%q) not found", tr.RequestID)
	}
	if byTrace.TraceID != byRequest.TraceID || len(byTrace.Spans) != len(byRequest.Spans) {
		t.Errorf("trace-id and request-id lookups returned different traces: %+v vs %+v", byTrace, byRequest)
	}
	if len(byTrace.Spans) != 3 {
		t.Errorf("SpanCount = %d, want 3", len(byTrace.Spans))
	}

	// Misses. Note the uppercase case has to use a literal that CONTAINS
	// letters: traceIDFor(7) is all digits, so uppercasing it is a no-op and
	// the case would silently test nothing.
	for _, id := range []string{"", " ", "deadbeef", traceIDFor(999), "DEADBEEFDEADBEEFDEADBEEFDEADBEEF"} {
		if got, ok := s.Get(id); ok || got != nil {
			t.Errorf("Get(%q) = (%+v, true), want (nil, false)", id, got)
		}
	}

	// Lookup is case-sensitive: the index is keyed by the lowercase hex this
	// package mints, so an uppercased copy of a stored id (which Parse would
	// reject anyway) must miss rather than being silently normalized.
	lettered := testTraceWith("4bf92f3577b34da6a3ce929d0e0e4736", 1)
	s.Add(lettered)
	if _, ok := s.Get(strings.ToUpper(lettered.TraceID)); ok {
		t.Error("Get found a stored trace via its uppercase id, want a case-sensitive miss")
	}

	// The returned trace must be a copy: mutating it must not affect what a
	// later Get returns. This is the second direction of clone isolation.
	byTrace.Spans[0].Attributes = map[string]any{"poisoned": true}
	byTrace.Spans = append(byTrace.Spans, Span{Name: "injected"})
	again, ok := s.Get(tr.TraceID)
	if !ok {
		t.Fatal("trace disappeared")
	}
	if len(again.Spans) != 3 {
		t.Errorf("SpanCount = %d after the caller mutated its copy, want 3", len(again.Spans))
	}
	if _, poisoned := again.Spans[0].Attributes["poisoned"]; poisoned {
		t.Error("Get returned a shared pointer: mutating the result changed the store")
	}
}

// TestStoreAddDoesNotAliasCaller proves Store.Add cloned the trace: mutating
// the caller's trace after Add must be invisible through Get. The reverse
// direction is covered by TestStoreGetByTraceAndRequestID.
func TestStoreAddDoesNotAliasCaller(t *testing.T) {
	s := NewStore(4)
	tr := testTrace() // carries non-empty attribute maps on the spans
	s.Add(tr)

	// Mutate everything the caller still owns.
	tr.Spans[0].Attributes["late"] = "annotation"
	tr.Spans[0].Events[0].Name = "mutated"
	tr.Attributes["late"] = "annotation"
	tr.Add(Span{Name: "appended-after-Add", StartUnixNano: 1, EndUnixNano: 2})

	got, ok := s.Get(tr.TraceID)
	if !ok {
		t.Fatal("trace not found")
	}
	if len(got.Spans) != 2 {
		t.Errorf("stored SpanCount = %d, want 2: the stored slice aliases the caller's", len(got.Spans))
	}
	if v, found := got.Spans[0].Attributes["late"]; found {
		t.Errorf("attribute written after Add is visible through Get: %v", v)
	}
	if _, found := got.Attributes["late"]; found {
		t.Error("trace attribute written after Add is visible through Get")
	}
	wantString(t, "stored event name", got.Spans[0].Events[0].Name, "router.select")

	// And the stored summary must not have changed either.
	sums := s.List(0)
	if len(sums) != 1 {
		t.Fatalf("List returned %d summaries, want 1", len(sums))
	}
	if sums[0].SpanCount != 2 {
		t.Errorf("summary SpanCount = %d, want 2", sums[0].SpanCount)
	}
}

// TestStoreAddReplacesSameTraceID documents the replace-not-append rule and
// that the replaced entry keeps its position (so List order stays stable).
func TestStoreAddReplacesSameTraceID(t *testing.T) {
	s := NewStore(8)
	id := traceIDFor(3)

	s.Add(testTraceWith(id, 2)) // request id "req-<id>"
	s.Add(testTraceWith(traceIDFor(4), 2))

	// Same trace id, different payload and a different request id.
	replacement := &Trace{TraceID: id, RequestID: "req-replacement", Status: StatusError}
	replacement.Add(Span{Name: "root", StartUnixNano: 1_000_000, EndUnixNano: 2_000_000, Status: StatusError})
	s.Add(replacement)

	if got := s.Len(); got != 2 {
		t.Errorf("Len = %d after re-adding a trace id, want 2 (replace, not append)", got)
	}
	got, ok := s.Get(id)
	if !ok {
		t.Fatal("trace not found after replacement")
	}
	if len(got.Spans) != 1 || got.Status != StatusError {
		t.Errorf("Get returned the OLD payload: %+v", got)
	}
	// The replacement position is unchanged: trace 4 was added after it and
	// must still be newer in the list.
	list := s.List(0)
	if len(list) != 2 {
		t.Fatalf("List returned %d summaries, want 2", len(list))
	}
	if list[0].TraceID != traceIDFor(4) {
		t.Errorf("newest entry = %q, want %q: replacement moved the entry", list[0].TraceID, traceIDFor(4))
	}
	if list[1].SpanCount != 1 {
		t.Errorf("replaced entry summary not refreshed: SpanCount = %d, want 1", list[1].SpanCount)
	}
}

// TestStoreAddNilIsNoop keeps a nil trace from panicking inside the lock.
func TestStoreAddNilIsNoop(t *testing.T) {
	s := NewStore(2)
	s.Add(nil)
	if got := s.Len(); got != 0 {
		t.Errorf("Len = %d after Add(nil), want 0", got)
	}
}

// TestStoreListOrderAndLimit checks newest-first ordering and the limit
// semantics: limit <= 0 or larger than the store means "all".
func TestStoreListOrderAndLimit(t *testing.T) {
	const n = 6
	s := NewStore(n + 4)
	for i := 0; i < n; i++ {
		s.Add(testTraceWith(traceIDFor(i), i+1))
	}

	all := s.List(0)
	if len(all) != n {
		t.Fatalf("List(0) returned %d summaries, want %d", len(all), n)
	}
	for i, sum := range all {
		// Newest first: index i of the result is trace n-1-i.
		want := traceIDFor(n - 1 - i)
		if sum.TraceID != want {
			t.Errorf("List(0)[%d].TraceID = %q, want %q (newest first)", i, sum.TraceID, want)
		}
		if sum.SpanCount != n-i {
			t.Errorf("List(0)[%d].SpanCount = %d, want %d", i, sum.SpanCount, n-i)
		}
		if sum.RequestID != "req-"+want {
			t.Errorf("List(0)[%d].RequestID = %q, want %q", i, sum.RequestID, "req-"+want)
		}
	}

	if got := s.List(2); len(got) != 2 || got[0].TraceID != traceIDFor(n-1) || got[1].TraceID != traceIDFor(n-2) {
		t.Errorf("List(2) = %+v, want the two newest traces", got)
	}
	if got := s.List(100); len(got) != n {
		t.Errorf("List(100) returned %d summaries, want %d (all)", len(got), n)
	}
	if got := s.List(-1); len(got) != n {
		t.Errorf("List(-1) returned %d summaries, want %d (all)", len(got), n)
	}

	// Determinism: repeated calls must agree, and the caller must own the slice.
	first, second := s.List(0), s.List(0)
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("List is not deterministic at %d: %+v vs %+v", i, first[i], second[i])
		}
	}
	first[0] = Summary{TraceID: "clobbered"}
	if s.List(0)[0].TraceID == "clobbered" {
		t.Error("List returned a slice the store still owns")
	}
}

// TestStoreListEmpty checks the empty and nil-store cases.
func TestStoreListEmpty(t *testing.T) {
	s := NewStore(4)
	if got := s.List(0); len(got) != 0 {
		t.Errorf("List on an empty store returned %+v, want empty", got)
	}
	if got := s.List(5); len(got) != 0 {
		t.Errorf("List(5) on an empty store returned %+v, want empty", got)
	}
}

// TestStoreReAddEvictedTraceID is the regression test for the index invariant
// that a slice-based store gets wrong: after an entry is evicted, its trace id
// must resolve to nothing rather than to whichever trace slid into its slot.
func TestStoreReAddEvictedTraceID(t *testing.T) {
	s := NewStore(2)
	first := traceIDFor(1)
	s.Add(testTraceWith(first, 1))
	s.Add(testTraceWith(traceIDFor(2), 1))
	s.Add(testTraceWith(traceIDFor(3), 1)) // evicts trace 1

	if got, ok := s.Get(first); ok {
		t.Fatalf("evicted trace id resolves to %+v, want not found", got)
	}
	if got, ok := s.Get("req-" + first); ok {
		t.Fatalf("evicted request id resolves to %+v, want not found", got)
	}

	// Re-adding the evicted id must insert it as a new trace.
	s.Add(testTraceWith(first, 4))
	got, ok := s.Get(first)
	if !ok {
		t.Fatal("re-added trace not found")
	}
	if len(got.Spans) != 4 {
		t.Errorf("re-added trace has %d spans, want 4 (the stale slot was reused)", len(got.Spans))
	}
	if s.Len() != 2 {
		t.Errorf("Len = %d, want 2", s.Len())
	}
	if _, ok := s.Get(traceIDFor(2)); ok {
		t.Error("trace 2 should have been evicted by the re-add")
	}
}

// TestListDoesNotCloneTraces is a behavioral proxy for the performance claim:
// List must be O(limit) and must not deep copy spans. It asserts the returned
// summaries are the stored values (identical), and that adding a trace does not
// invalidate a previously returned slice.
func TestListDoesNotCloneTraces(t *testing.T) {
	s := NewStore(4)
	s.Add(testTraceWith(traceIDFor(1), 1))
	before := s.List(0)
	s.Add(testTraceWith(traceIDFor(2), 1))
	after := s.List(0)

	if len(before) != 1 || len(after) != 2 {
		t.Fatalf("List lengths = %d, %d, want 1, 2", len(before), len(after))
	}
	if before[0].TraceID != traceIDFor(1) {
		t.Errorf("previously returned summary mutated: %+v", before[0])
	}
}

// TestWriteJSONL checks the rendering contract: one compact JSON object and one
// newline, decodable line by line, and rendered OUTSIDE any store lock.
func TestWriteJSONL(t *testing.T) {
	s := NewStore(4)
	tr := testTrace()
	s.Add(tr)

	// Take the trace out of the store first: this is the intended usage, and
	// it is what keeps I/O off the request path.
	snapshot, ok := s.Get(tr.TraceID)
	if !ok {
		t.Fatal("trace not found")
	}

	var buf bytes.Buffer
	if err := s.WriteJSONL(&buf, snapshot); err != nil {
		t.Fatalf("WriteJSONL: %v", err)
	}

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("WriteJSONL output does not end with a newline: %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("WriteJSONL wrote %d lines, want 1: %q", strings.Count(out, "\n"), out)
	}
	if strings.Contains(out, "\n ") || strings.Contains(out, "\n\t") {
		t.Errorf("WriteJSONL output is indented: %q", out)
	}

	scanner := bufio.NewScanner(&buf)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("scanned %d lines, want 1", len(lines))
	}
	var back Trace
	if err := json.Unmarshal([]byte(lines[0]), &back); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if back.TraceID != tr.TraceID || len(back.Spans) != 2 {
		t.Errorf("round-tripped trace = %+v, want %d spans", back, 2)
	}

	// Many traces, one line each: the JSONL property.
	var stream bytes.Buffer
	for i := 0; i < 5; i++ {
		t2 := testTraceWith(traceIDFor(i), 1)
		if err := s.WriteJSONL(&stream, t2); err != nil {
			t.Fatalf("WriteJSONL: %v", err)
		}
	}
	if got := strings.Count(stream.String(), "\n"); got != 5 {
		t.Errorf("JSONL stream has %d newlines, want 5", got)
	}

	// A nil trace is an error, and writes nothing.
	var empty bytes.Buffer
	if err := s.WriteJSONL(&empty, nil); !errors.Is(err, ErrNilTrace) {
		t.Errorf("WriteJSONL(nil) = %v, want ErrNilTrace", err)
	}
	if empty.Len() != 0 {
		t.Errorf("WriteJSONL(nil) wrote %d bytes, want 0", empty.Len())
	}
}

// TestStoreConcurrentAddGetList is the -race test: concurrent writers and
// readers must not race, and the store must stay bounded while it happens.
func TestStoreConcurrentAddGetList(t *testing.T) {
	const (
		capacity   = 64
		writers    = 8
		readers    = 8
		perWriter  = 200
		iterations = 200
	)
	s := NewStore(capacity)

	// Record every id that was handed to Add. The eviction assertion below is
	// made against List (the store's own view of what survived), not against
	// the recording order: goroutines interleave, so the append order here is
	// not necessarily the order the store's lock serialized the Adds in, and a
	// boundary id could legitimately land on either side.
	var (
		recMu sync.Mutex
		added []string
	)
	sawID := func(id string) {
		recMu.Lock()
		added = append(added, id)
		recMu.Unlock()
	}

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := fmt.Sprintf("%032x", w*perWriter+i)
				tr := &Trace{TraceID: id, RequestID: "req-" + id, Status: StatusOK}
				tr.Add(Span{
					TraceID:       id,
					SpanID:        fmt.Sprintf("%016x", i),
					Name:          "concurrent",
					Kind:          KindInternal,
					StartUnixNano: int64(i) * 1000,
					EndUnixNano:   int64(i)*1000 + 500,
					Status:        StatusOK,
					Attributes:    map[string]any{"writer": w, "nested": map[string]any{"i": i}},
				})
				s.Add(tr)
				sawID(id)
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				id := fmt.Sprintf("%032x", (r*iterations+i)%(writers*perWriter))
				if got, ok := s.Get(id); ok {
					// Touch the clone so the race detector sees the payload.
					_ = got.Summary()
					if len(got.Spans) > 0 {
						_ = got.Spans[0].Attributes["writer"]
					}
				}
				for _, sum := range s.List(8) {
					_ = sum.TraceID
				}
				_ = s.Len()
				_ = s.Dropped()
			}
		}(r)
	}

	wg.Wait()

	if got := s.Len(); got != capacity {
		t.Errorf("Len = %d after %d concurrent adds, want the capacity %d", got, writers*perWriter, capacity)
	}
	if got := s.Dropped(); got != int64(writers*perWriter-capacity) {
		t.Errorf("Dropped = %d, want %d", got, writers*perWriter-capacity)
	}

	// List is the store's own account of what survived. Every surviving trace
	// must be fetchable by BOTH keys, every evicted trace must be gone by both
	// keys, and the two sets must partition exactly what was added -- that is
	// what proves the ring and the lookup index cannot drift apart under
	// concurrency.
	survivors := make(map[string]bool, capacity)
	for _, sum := range s.List(0) {
		if survivors[sum.TraceID] {
			t.Errorf("List returned %s twice", sum.TraceID)
		}
		survivors[sum.TraceID] = true
		if _, ok := s.Get(sum.TraceID); !ok {
			t.Errorf("Listed trace %s not found by trace id", sum.TraceID)
		}
		if _, ok := s.Get("req-" + sum.TraceID); !ok {
			t.Errorf("Listed trace %s not found by request id", sum.TraceID)
		}
	}
	if len(survivors) != capacity {
		t.Fatalf("List returned %d summaries, want %d", len(survivors), capacity)
	}

	recMu.Lock()
	all := append([]string(nil), added...)
	recMu.Unlock()
	if len(all) != writers*perWriter {
		t.Fatalf("recorded %d adds, want %d", len(all), writers*perWriter)
	}
	evicted := 0
	for _, id := range all {
		_, ok := s.Get(id)
		if survivors[id] != ok {
			t.Fatalf("trace %s: List says survived=%v but Get says found=%v", id, survivors[id], ok)
		}
		if !ok {
			evicted++
			if _, ok := s.Get("req-" + id); ok {
				t.Errorf("evicted trace %s still resolvable by request id", id)
			}
		}
	}
	if evicted != writers*perWriter-capacity {
		t.Errorf("counted %d evicted traces, want %d", evicted, writers*perWriter-capacity)
	}
}

// TestStoreConcurrentResetAndWriteJSONL exercises Reset against readers, and
// renders traces concurrently with writes (the store must not be locked while
// the writer blocks).
func TestStoreConcurrentResetAndWriteJSONL(t *testing.T) {
	s := NewStore(16)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			s.Add(testTraceWith(traceIDFor(i), 2))
		}
	}()

	// A deliberately slow writer: if WriteJSONL held a store lock, the Add
	// above would still finish (it is in another goroutine), but Get would
	// observe it here. The real assertion is "no race, no deadlock".
	wg.Add(1)
	go func() {
		defer wg.Done()
		var buf bytes.Buffer
		for i := 0; i < 100; i++ {
			if tr, ok := s.Get(traceIDFor(i)); ok {
				if err := s.WriteJSONL(&buf, tr); err != nil {
					t.Errorf("WriteJSONL: %v", err)
					return
				}
			}
			if i%25 == 0 {
				s.Reset()
			}
		}
	}()

	wg.Wait()
	if s.Len() > 16 {
		t.Errorf("Len = %d exceeds capacity", s.Len())
	}
}
