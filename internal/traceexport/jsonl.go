// Package traceexport ships finished traces off the request path: to a JSONL
// file that an operator or a log shipper can tail, and to an OTLP/HTTP
// collector in the standard JSON encoding.
//
// Both exporters share one design rule: Export NEVER blocks on I/O. A trace is
// a debugging artifact, and a debugging artifact is never worth a millisecond
// of request latency, let alone a stalled request. Each exporter therefore owns
// exactly one worker goroutine and one bounded queue; Export copies a handle
// into the queue and returns. When the queue is full the trace is dropped and
// counted, because losing a trace must not slow a request -- the counters
// (Written/Dropped/Err, Stats) are how an operator learns that it happened.
//
// Neither exporter is a metrics hook and neither takes a lock on the request
// path: the only mutex-protected state is the small counter/error block that a
// reader polls.
package traceexport

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	"github.com/infergate/infergate/internal/tracing"
)

const (
	// defaultBuffer is the queue depth used when an Options Buffer is <= 0. It
	// is a trace COUNT, not bytes: ~256 finished traces is a few hundred
	// kilobytes of pointer-sized handles, and it rides out a multi-second disk
	// or network stall without dropping anything at a normal request rate.
	defaultBuffer = 256

	// jsonlFileMode is the permission a NEW file is created with. 0640 because
	// traces carry request ids, routes and model names: readable by the
	// gateway's group, not by the world.
	jsonlFileMode = 0o640

	// jsonlWriterBuf is the bufio.Writer size. 64 KiB is large enough that a
	// burst of traces costs one write syscall and small enough that an idle
	// exporter's resident buffer is irrelevant.
	jsonlWriterBuf = 64 << 10
)

// JSONLOptions configures NewJSONL.
type JSONLOptions struct {
	// Path is the file to append finished traces to. Required.
	//
	// The file is opened O_CREATE|O_WRONLY|O_APPEND: a restarted gateway
	// continues the previous run's file instead of destroying it, which is the
	// whole point of a durable trace log. O_APPEND also makes every write an
	// atomic append to the end, so a second process sharing the path cannot
	// overwrite the first one's lines.
	Path string

	// Buffer is the number of traces that may be queued for the writer before
	// Export starts dropping. <= 0 means 256.
	Buffer int

	// Logger receives drop diagnostics. Optional; nil discards. The worker does
	// not log each drop -- a full queue drops in bursts and a log line per drop
	// would be its own flood -- it is read from Dropped()/Err() instead.
	Logger *slog.Logger
}

// JSONL is a non-blocking JSONL file exporter. It is safe for concurrent use.
//
// The file itself is touched by exactly one goroutine, created in NewJSONL and
// owned by the worker loop; no other goroutine ever holds the *os.File or the
// bufio.Writer.
type JSONL struct {
	// store renders each trace. WriteJSONL is a method on tracing.Store but
	// touches nothing in it, so one one-trace store is enough for the lifetime
	// of the exporter and costs no ring buffer.
	store *tracing.Store

	path string
	ch   chan *tracing.Trace

	closed    atomic.Bool
	done      chan struct{} // closed by the worker when it has exited
	closeOnce sync.Once
	closeErr  error // written by the worker before close(done); read after <-done

	written atomic.Int64
	dropped atomic.Int64

	mu  sync.Mutex
	err error // last write/flush error; nil until something fails

	log *slog.Logger
}

// NewJSONL opens (or creates) the file at opts.Path and starts the single
// writer goroutine that owns it.
//
// The file is opened HERE, synchronously, rather than lazily by the worker: a
// misconfigured path (a directory, a read-only mount, a typo'd parent) must
// fail at construction, where a caller can still decide not to enable the
// feature, instead of surfacing later as a silent stream of dropped traces.
func NewJSONL(opts JSONLOptions) (*JSONL, error) {
	if opts.Path == "" {
		return nil, errors.New("traceexport: JSONL Path is required")
	}
	f, err := os.OpenFile(opts.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, jsonlFileMode)
	if err != nil {
		return nil, fmt.Errorf("traceexport: open %s: %w", opts.Path, err)
	}
	buf := opts.Buffer
	if buf <= 0 {
		buf = defaultBuffer
	}
	j := &JSONL{
		store: tracing.NewStore(1),
		path:  opts.Path,
		ch:    make(chan *tracing.Trace, buf),
		done:  make(chan struct{}),
		log:   opts.Logger,
	}
	// The file is handed to the worker and never used from here again; the
	// worker closes it on the way out.
	go j.worker(f)
	return j, nil
}

// Export queues t for writing. It never blocks on file I/O.
//
// If the queue is full the trace is dropped, Dropped is incremented and Export
// returns immediately. If the exporter was closed, the trace is dropped the
// same way: the counter always accounts for what did not get written.
//
// t is stored by reference, not cloned. That is safe because the contract of
// tracing.Trace is that a "finished" trace is immutable and handed off -- the
// gateway's tracing.Store already clones at that boundary -- and cloning again
// here would put a deep copy of every span on the request path, which is
// exactly the cost this type exists to avoid. A nil trace is ignored.
func (j *JSONL) Export(t *tracing.Trace) {
	if j == nil || t == nil {
		return
	}
	if j.closed.Load() {
		j.dropped.Add(1)
		return
	}
	select {
	case j.ch <- t:
	default:
		j.dropped.Add(1)
	}
}

// Close drains the queue, flushes the file and closes it. It is idempotent:
// the second and later calls wait for the same drain and report the same error.
//
// Close does NOT impose a timeout. The queue is bounded and, once the channel
// is closed, nothing more can be added, so the drain terminates as long as the
// filesystem does; a write stuck in a hung filesystem is a condition no
// deadline here can fix, and truncating the drain would silently discard the
// traces most worth having -- the last second before shutdown.
//
// The returned error is the first write/flush error (the same value Err
// reports), then the file-close error, or nil when everything landed.
func (j *JSONL) Close() error {
	if j == nil {
		return nil
	}
	j.closeOnce.Do(func() {
		j.closed.Store(true)
		close(j.ch)
	})
	<-j.done
	return j.closeErr
}

// Written returns the number of traces successfully written to the file. It
// counts COMPLETED writes, so it reaches its final value only after Close (the
// worker increments it as it drains, not when Export is called).
func (j *JSONL) Written() int64 {
	if j == nil {
		return 0
	}
	return j.written.Load()
}

// Dropped returns the number of traces Export refused, because the queue was
// full or because the exporter was already closed.
func (j *JSONL) Dropped() int64 {
	if j == nil {
		return 0
	}
	return j.dropped.Load()
}

// Err returns the last write or flush error, or nil if every write so far
// succeeded.
//
// One failure does not stop the worker: a momentary ENOSPC that frees up, or a
// log rotation that briefly replaces the path, should not end tracing for the
// life of the process, so the worker keeps draining and keeps the most recent
// error for the operator. The consequence -- bytes already lost inside
// bufio.Writer are not retried -- is accepted; retrying a failing disk from the
// tracing path is how a debugging aid becomes an outage.
func (j *JSONL) Err() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

// setErr records the most recent failure.
func (j *JSONL) setErr(err error) {
	j.mu.Lock()
	j.err = err
	j.mu.Unlock()
}

// worker is the only goroutine that touches f. It drains ch until Close closes
// it, buffering writes and flushing when the queue runs dry.
//
// A hand-rolled select loop rather than `for t := range ch`: batching wants to
// know whether another trace is ALREADY waiting, and only a non-blocking
// receive answers that without stalling the drain. The rule is "flush when the
// queue is momentarily empty", so a burst of traces becomes one syscall and a
// trace enqueued just before Close still reaches the disk via the final flush.
func (j *JSONL) worker(f *os.File) {
	var closeErr error
	fileClosed := false
	defer func() {
		if !fileClosed {
			closeErr = f.Close()
		}
		j.closeErr = errors.Join(j.Err(), closeErr)
		close(j.done)
	}()

	bw := bufio.NewWriterSize(f, jsonlWriterBuf)
	j.run(bw)
	// The final flush is where a trace enqueued immediately before Close
	// reaches the disk. run flushes at the end of every batch, so this second
	// call is almost always a no-op; it is kept so the "Close flushes" promise
	// does not depend on run's internal control flow.
	if err := bw.Flush(); err != nil {
		j.setErr(fmt.Errorf("traceexport: flush %s: %w", j.path, err))
	}
}

// run is the worker's drain loop. It is separated from worker so the flush-on-
// empty-queue rule is testable in isolation and so worker reads as nothing but
// resource ownership.
//
// This is a hand-rolled select loop rather than `for t := range j.ch`: batching
// wants to know whether another trace is ALREADY waiting, and only a
// non-blocking receive answers that without stalling the drain. The rule is
// "flush when the queue is momentarily empty", so a burst of traces becomes one
// write syscall and a trace enqueued just before Close still reaches the disk
// before the loop ends.
func (j *JSONL) run(bw *bufio.Writer) {
	// flush pushes the buffer down to the OS. A failure is latched into Err;
	// the bytes lost inside bufio on error are not retried, because retrying a
	// failing disk from the tracing path is how a debugging aid becomes an
	// outage.
	flush := func() {
		if err := bw.Flush(); err != nil {
			j.setErr(fmt.Errorf("traceexport: flush %s: %w", j.path, err))
		}
	}

	for t := range j.ch {
		j.writeOne(bw, t)
		// A burst that overflows our buffer must go down to the OS promptly:
		// bytes above jsonlWriterBuf would otherwise sit in the buffer (and be
		// exposed to a later flush failure) for the length of the burst.
		if bw.Buffered() >= jsonlWriterBuf {
			flush()
			continue
		}
	batch:
		// Drain everything already queued without flushing, then flush once,
		// so a burst costs one syscall instead of one per trace. The default
		// arm means the queue ran dry: the end of the burst.
		for {
			select {
			case next, ok := <-j.ch:
				if !ok {
					break batch
				}
				j.writeOne(bw, next)
			default:
				break batch
			}
		}
		flush()
	}
}

// writeOne renders one trace into the buffer and accounts for it.
//
// A nil trace cannot arrive (Export rejects it) but is skipped rather than
// written, because WriteJSONL treats nil as an error and a blank line in a
// JSONL stream is worse than a missing one: it parses as nothing and therefore
// reports no corruption.
func (j *JSONL) writeOne(bw *bufio.Writer, t *tracing.Trace) {
	if t == nil {
		return
	}
	if err := j.store.WriteJSONL(bw, t); err != nil {
		j.setErr(fmt.Errorf("traceexport: encode %s: %w", j.path, err))
		return
	}
	j.written.Add(1)
}
