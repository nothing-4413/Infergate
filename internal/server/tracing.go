package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/gateway"
	"github.com/infergate/infergate/internal/tracing"
)

// defaultTraceCapacity mirrors config.Defaults().
//
// Duplicated rather than imported from the config package because a caller that
// builds a Config in Go (every test in this package) never runs
// config.Validate, so a zero capacity can reach this file — and a store of
// capacity 0 would silently drop every trace while the config says tracing is
// on, which is the one failure mode that looks exactly like success.
const defaultTraceCapacity = 1024

// maxTraceLimit caps one /admin/traces page.
//
// The ring is at most a few thousand entries, but each trace is a nested JSON
// document with spans, events and attributes; a client asking for everything is
// usually a script that meant limit=1 and would otherwise pull megabytes.
const maxTraceLimit = 500

// tracePlane owns the trace store, the gateway's Tracer and the exporters that
// carry finished traces out of the process.
//
// It exists for the same reason the cache and quota planes do: built once in
// NewServer, held on the Server even when disabled so /admin/traces can report
// the configuration, and closed from main after the listener stops. The store is
// deliberately NOT allocated when tracing is off: an empty ring is cheap, but it
// would still cost a capacity-slot allocation and, worse, would make a
// misconfigured gateway indistinguishable from a recording one.
type tracePlane struct {
	cfg    config.TracingConfig
	store  *tracing.Store
	tracer *gateway.Tracer

	exporters []gateway.TraceExporter
	closers   []namedCloser
	report    []traceReporter
}

// namedCloser keeps the exporter's name next to its closer so a failure to
// flush says which sink failed.
type namedCloser struct {
	name  string
	close func() error
}

// traceReporter is the read-only half of an exporter: its name and a snapshot
// of its counters for /admin/tracing. Kept separate from the gateway's
// TraceExporter interface on purpose — the request path only ever calls
// Export(), and adding reporting methods to that interface would force every
// exporter (including test doubles) to implement counters nobody reads.
type traceReporter struct {
	name  string
	stats func() map[string]any
}

// exportStats renders each exporter's counters as {name: {...}}.
func (t *tracePlane) exportStats() map[string]any {
	out := make(map[string]any, len(t.report))
	for _, r := range t.report {
		if r.stats == nil {
			continue
		}
		out[r.name] = r.stats()
	}
	return out
}

// buildTracing wires the configured trace store and exporters.
//
// A failure to build an exporter is fatal to startup, for the same reason an
// unreachable redis cache is: an operator who configured a trace sink must learn
// at boot that it is unusable, not by finding an empty dashboard hours later.
func buildTracing(cfg *config.Config, logger logAdapter) (*tracePlane, error) {
	tc := cfg.Tracing
	tp := &tracePlane{cfg: tc}
	if !tc.Enabled {
		return tp, nil
	}

	capacity := tc.Capacity
	if capacity <= 0 {
		capacity = defaultTraceCapacity
	}
	tp.store = tracing.NewStore(capacity)

	ratio := tc.SampleRatio
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}

	exporters, closers, report, err := buildTraceExporters(tc, logger)
	if err != nil {
		return nil, err
	}
	tp.exporters = exporters
	tp.closers = closers
	tp.report = report
	tp.tracer = gateway.NewTracer(tp.store, ratio, exporters...)

	if logger != nil {
		logger.Info("tracing enabled",
			"capacity", capacity, "sample_ratio", ratio,
			"jsonl_path", tc.JSONLPath, "otlp_endpoint", tc.OTLP.Endpoint,
			"exporters", len(exporters))
	}
	return tp, nil
}

// Close drains and closes every exporter. The store needs no cleanup.
func (t *tracePlane) Close() error {
	if t == nil {
		return nil
	}
	var firstErr error
	for _, c := range t.closers {
		if c.close == nil {
			continue
		}
		if err := c.close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", c.name, err)
		}
	}
	return firstErr
}

// handleTraces lists recent traces, newest first.
//
// Two renderings, and the second is why this endpoint matters during a load
// test: ?format=jsonl streams each trace as one flushed line so the output can
// be piped into jq or a file without buffering the whole ring, while the default
// JSON body is what a browser and the acceptance script read.
func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	if s.trace == nil || s.trace.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":  false,
			"capacity": s.traceCapacity(),
			"count":    0,
			"dropped":  0,
			"traces":   []any{},
		})
		return
	}

	limit, err := traceLimit(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "infergate_bad_request", err.Error())
		return
	}
	store := s.trace.store
	summaries := store.List(limit)

	if strings.EqualFold(r.URL.Query().Get("format"), "jsonl") {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		out := newFlusher(w)
		for _, sum := range summaries {
			tr, ok := store.Get(sum.TraceID)
			if !ok {
				continue // evicted between List and Get; drop it silently
			}
			if err := store.WriteJSONL(out, tr); err != nil {
				return // the client went away mid-stream
			}
			out.Flush()
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":  true,
		"capacity": s.traceCapacity(),
		"stored":   store.Len(),
		"dropped":  store.Dropped(),
		"count":    len(summaries),
		"traces":   summaries,
	})
}

// handleTraceByID returns one whole trace, looked up by trace id OR by request
// id. Matching the request id too is the point: a caller holding only the
// X-InferGate-Request-Id response header must still be able to find the trace it
// belongs to, and requiring the 32-hex trace id first would make this surface
// useless during an incident.
func (s *Server) handleTraceByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "infergate_bad_request", "trace id is required")
		return
	}
	if !validTraceID(id) {
		writeError(w, http.StatusBadRequest, "infergate_bad_request",
			"trace id must be 32 hex characters (a trace id) or at most 64 characters from [A-Za-z0-9._:-] (a request id)")
		return
	}
	if s.trace == nil || s.trace.store == nil {
		writeError(w, http.StatusNotFound, "infergate_not_found", "tracing is disabled")
		return
	}

	tr, ok := s.trace.store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "infergate_not_found", "trace not found: "+id)
		return
	}
	if strings.EqualFold(r.URL.Query().Get("format"), "jsonl") {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = s.trace.store.WriteJSONL(w, tr)
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

// handleTracingConfig reports the tracing configuration and the exporter
// counters.
//
// A separate endpoint from /admin/traces because the two questions are asked at
// different times: "is tracing on, and where is it going" while setting up, and
// "what did it capture" while investigating.
func (s *Server) handleTracingConfig(w http.ResponseWriter, r *http.Request) {
	var tc config.TracingConfig
	if s.trace != nil {
		tc = s.trace.cfg
	}

	body := map[string]any{
		"enabled":      tc.Enabled,
		"capacity":     s.traceCapacity(),
		"sample_ratio": tc.SampleRatio,
		"jsonl_path":   tc.JSONLPath,
		"otlp": map[string]any{
			"endpoint":     tc.OTLP.Endpoint,
			"timeout":      tc.OTLP.Timeout.String(),
			"service_name": tc.OTLP.ServiceName,
			"headers":      len(tc.OTLP.Headers),
		},
		"stored":       0,
		"dropped":      0,
		"exporters":    []string{},
		"export_stats": map[string]any{},
	}
	if s.trace != nil {
		names := make([]string, 0, len(s.trace.exporters))
		for _, r := range s.trace.report {
			names = append(names, r.name)
		}
		body["exporters"] = names
		body["export_stats"] = s.trace.exportStats()
		if s.trace.store != nil {
			body["stored"] = s.trace.store.Len()
			body["dropped"] = s.trace.store.Dropped()
		}
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) traceCapacity() int {
	if s.trace == nil {
		return defaultTraceCapacity
	}
	if c := s.trace.cfg.Capacity; c > 0 {
		return c
	}
	return defaultTraceCapacity
}

// CloseTracing drains and closes the trace exporters.
//
// Called from main after the listener has stopped, next to CloseCache: the
// request path is the only producer of traces, so by then nothing new can
// arrive and a bounded drain is enough.
func (s *Server) CloseTracing() error {
	if s.trace == nil {
		return nil
	}
	return s.trace.Close()
}

func traceLimit(r *http.Request) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return 50, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("limit must be an integer, got %q", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("limit must be positive, got %d", n)
	}
	if n > maxTraceLimit {
		n = maxTraceLimit
	}
	return n, nil
}

// validTraceID accepts a 32-hex trace id or a looser request id, and rejects
// anything that could be used to probe the store with junk. The charset is
// deliberately narrow: request ids are generated by this gateway or by a
// provider, so letters, digits, dot, dash, colon and underscore cover every
// shape observed in this repo's logs.
func validTraceID(id string) bool {
	if len(id) == 32 {
		for i := 0; i < len(id); i++ {
			c := id[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
		return true
	}
	if len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

// flusherWriter is the io.Writer the JSONL rendering writes through, so each
// trace reaches the client as soon as it is encoded rather than when the handler
// returns. A trace list is exactly the kind of response someone tails.
type flusherWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newFlusher(w http.ResponseWriter) *flusherWriter {
	f, _ := w.(http.Flusher)
	return &flusherWriter{w: w, f: f}
}

func (f *flusherWriter) Write(p []byte) (int, error) { return f.w.Write(p) }

func (f *flusherWriter) Flush() {
	if f.f != nil {
		f.f.Flush()
	}
}

// writeError emits the same error envelope the rest of /admin uses.
func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": message, "type": kind},
	})
}
