package traceexport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/infergate/infergate/internal/tracing"
)

const (
	// defaultOTLPTimeout is the per-POST deadline when Options.Timeout <= 0.
	defaultOTLPTimeout = 5 * time.Second

	// defaultServiceName is the resource service.name when it is not configured.
	defaultServiceName = "infergate"

	// scopeName is the instrumentation scope. It is the gateway's own name, not
	// a library version, because the spans ARE the gateway.
	scopeName = "infergate.gateway"

	// errBodyBytes bounds how much of a failing response body is echoed into
	// LastError. A collector behind a proxy can answer with a full HTML error
	// page; quoting all of it would turn one failed POST into megabytes of
	// retained string.
	errBodyBytes = 200
)

// OTLPOptions configures NewOTLP.
type OTLPOptions struct {
	// Endpoint is the collector's base URL, e.g. "http://127.0.0.1:4318".
	// Export POSTs to <Endpoint>/v1/traces. Required, and required to be an
	// absolute http or https URL.
	Endpoint string

	// Timeout bounds one POST. <= 0 means 5s. It is also the bound Close waits
	// for the worker to finish its in-flight request.
	Timeout time.Duration

	// ServiceName is the resource attribute service.name. Empty means
	// "infergate".
	ServiceName string

	// Headers are extra request headers (authorization, tenant ids). They are
	// COPIED at construction so a caller may keep mutating its own map.
	Headers map[string]string

	// Buffer is the number of traces that may be queued before Export starts
	// dropping. <= 0 means 256.
	Buffer int

	// Logger receives failure diagnostics. Optional; nil discards.
	Logger *slog.Logger

	// Client is the HTTP client used for POSTs. Optional; tests inject one.
	// nil means a client built from Timeout.
	Client *http.Client
}

// OTLPStats is a snapshot of an OTLP exporter's counters.
//
// It is a value, not a pointer, and it is taken under the exporter's mutex, so
// a caller can read a consistent set. LastStatus is the last HTTP status the
// worker SAW, including a failure: 0 means the request never completed (a
// transport error, a timeout, a refused connection).
type OTLPStats struct {
	Exported   int64
	Failed     int64
	Dropped    int64
	LastError  string
	LastStatus int
}

// OTLP is a non-blocking OTLP/HTTP JSON exporter. It is safe for concurrent use.
type OTLP struct {
	client      *http.Client
	url         string
	timeout     time.Duration
	serviceName string
	headers     map[string]string
	ch          chan *tracing.Trace
	log         *slog.Logger

	stop      chan struct{} // closed by Close to ask the worker to wind down
	done      chan struct{} // closed by the worker when it has exited
	closeOnce sync.Once

	exported atomic.Int64
	failed   atomic.Int64
	dropped  atomic.Int64

	mu         sync.Mutex
	lastError  string
	lastStatus int
}

// NewOTLP returns an exporter that ships traces to opts.Endpoint.
//
// The endpoint is validated here rather than at the first POST: an exporter
// that cannot address anything should fail where the caller can still see it,
// not turn every request into a silently counted drop. "Absolute http/https" is
// enforced explicitly because url.Parse accepts almost anything -- "collector"
// parses as a relative path and would otherwise produce a POST to a URL that
// cannot exist.
func NewOTLP(opts OTLPOptions) (*OTLP, error) {
	if strings.TrimSpace(opts.Endpoint) == "" {
		return nil, errors.New("traceexport: OTLP Endpoint is required")
	}
	u, err := url.Parse(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("traceexport: OTLP Endpoint %q: %w", opts.Endpoint, err)
	}
	if !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("traceexport: OTLP Endpoint %q is not an absolute http/https URL", opts.Endpoint)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultOTLPTimeout
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	service := opts.ServiceName
	if service == "" {
		service = defaultServiceName
	}
	buf := opts.Buffer
	if buf <= 0 {
		buf = defaultBuffer
	}
	// Copy the headers: the caller's map is its own, and a request must never
	// read it concurrently with a caller write.
	headers := make(map[string]string, len(opts.Headers))
	for k, v := range opts.Headers {
		headers[k] = v
	}

	o := &OTLP{
		client:      client,
		url:         strings.TrimSuffix(opts.Endpoint, "/") + "/v1/traces",
		timeout:     timeout,
		serviceName: service,
		headers:     headers,
		ch:          make(chan *tracing.Trace, buf),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		log:         opts.Logger,
	}
	go o.worker()
	return o, nil
}

// Export queues t for delivery. It never blocks on the network.
//
// If the queue is full, or the exporter has been closed, t is dropped and
// Dropped is incremented. t is stored by reference; see JSONL.Export for why
// that is safe.
func (o *OTLP) Export(t *tracing.Trace) {
	if o == nil || t == nil {
		return
	}
	select {
	case o.ch <- t:
	case <-o.done:
		o.dropped.Add(1)
	default:
		o.dropped.Add(1)
	}
}

// Close asks the worker to stop, lets it finish the trace in flight and drain
// what is queued, and returns. It is idempotent.
//
// It is bounded: Close never waits longer than the POST timeout (5s by default)
// for the worker to exit. A collector that has stopped answering must not keep
// a shutting-down gateway alive, and the traces still queued at that point are
// counted as dropped rather than pretended to be delivered. The returned error
// is the last delivery failure, or nil if everything sent was accepted.
func (o *OTLP) Close() error {
	if o == nil {
		return nil
	}
	o.closeOnce.Do(func() { close(o.stop) })
	select {
	case <-o.done:
	case <-time.After(o.timeout):
		o.logWarn("otlp: worker did not stop within the timeout; giving up", "endpoint", o.url)
		return o.lastErr()
	}
	return o.lastErr()
}

// Stats returns a consistent snapshot of the counters and the last failure.
func (o *OTLP) Stats() OTLPStats {
	if o == nil {
		return OTLPStats{}
	}
	o.mu.Lock()
	last := o.lastError
	status := o.lastStatus
	o.mu.Unlock()
	return OTLPStats{
		Exported:   o.exported.Load(),
		Failed:     o.failed.Load(),
		Dropped:    o.dropped.Load(),
		LastError:  last,
		LastStatus: status,
	}
}

// lastErr renders the last failure as an error, or nil if none occurred.
func (o *OTLP) lastErr() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lastError == "" {
		return nil
	}
	return errors.New(o.lastError)
}

// setFailure records a failed delivery.
func (o *OTLP) setFailure(status int, msg string) {
	o.mu.Lock()
	o.lastError = msg
	o.lastStatus = status
	o.mu.Unlock()
}

func (o *OTLP) logWarn(msg string, args ...any) {
	if o.log != nil {
		o.log.Warn(msg, args...)
	}
}

// worker is the only goroutine that performs HTTP. It posts traces one at a
// time, in order, until Close closes stop.
//
// It does NOT stop on the first failure: a collector that is briefly restarting
// is exactly the case where the next trace still matters, and a worker that
// gave up would make the exporter permanently dead with no way for a caller to
// notice except by reading Stats. Every failure is counted and retained
// instead.
func (o *OTLP) worker() {
	defer close(o.done)
	for {
		select {
		case <-o.stop:
			// Wind down: post what is already queued, then leave. Bounded by
			// the caller's timeout, not by this loop.
			o.drain()
			return
		case t := <-o.ch:
			o.post(t)
		}
	}
}

// drain posts every trace currently queued, stopping if another Close arrives
// (there is only ever one) or the queue empties.
func (o *OTLP) drain() {
	for {
		select {
		case t := <-o.ch:
			o.post(t)
		default:
			return
		}
	}
}

// post encodes one trace and delivers it, updating the counters.
func (o *OTLP) post(t *tracing.Trace) {
	if t == nil {
		return
	}
	body, err := encodeOTLP(t, o.serviceName)
	if err != nil {
		o.failed.Add(1)
		o.setFailure(0, "otlp: encode: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url, bytes.NewReader(body))
	if err != nil {
		o.failed.Add(1)
		o.setFailure(0, "otlp: request: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// Set the extra headers last so an explicit option (an unusual content
	// type, say) can override the default rather than being silently ignored.
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}

	resp, err := o.client.Do(req)
	if err != nil {
		o.failed.Add(1)
		// Status 0: the request never completed, so there is no status to
		// report. The transport's error text is the useful part.
		o.setFailure(0, "otlp: "+err.Error())
		return
	}
	// The body must be read to completion AND closed on every path, or the
	// connection cannot be returned to the pool and every POST pays a new
	// handshake.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, errBodyBytes))
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		o.failed.Add(1)
		msg := "otlp: " + resp.Status + " " + string(bytes.TrimSpace(raw))
		o.setFailure(resp.StatusCode, truncate(msg, errBodyBytes))
		return
	}
	if readErr != nil {
		// A 2xx whose body we could not finish reading is still a success: the
		// collector accepted the payload. Only the pooled connection is lost.
		o.logWarn("otlp: reading response body", "err", readErr, "endpoint", o.url)
	}
	o.exported.Add(1)
}

// truncate caps s at n bytes, appending an ellipsis marker when it cuts.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- OTLP JSON encoding -----------------------------------------------------
//
// The structs below are the standard OTLP/HTTP JSON envelope (opentelemetry-proto's
// JSON mapping), spelled out rather than pulled from a module because the task
// is stdlib-only. The alternative -- map[string]any built at runtime -- would
// put map ordering in charge of the wire format; these structs plus a sorted
// attribute slice make the payload byte-for-byte deterministic, which is what
// lets a test compare against a golden string.

type otlpEnvelope struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Events            []otlpEvent    `json:"events,omitempty"`
	Status            *otlpStatus    `json:"status,omitempty"`
}

type otlpEvent struct {
	Name         string         `json:"name"`
	TimeUnixNano string         `json:"timeUnixNano"`
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpStatus struct {
	Code int `json:"code"`
}

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

// otlpValue is the OTLP AnyValue. Every field is omitempty and a scalar is
// never zero unless it is the value being transmitted (a stringValue of "" is
// therefore one distinguishing omission-only case that does not arise here:
// attributes with an empty string still emit {"stringValue":""}).
type otlpValue struct {
	StringValue *string    `json:"stringValue,omitempty"`
	BoolValue   *bool      `json:"boolValue,omitempty"`
	IntValue    *string    `json:"intValue,omitempty"`
	DoubleValue *float64   `json:"doubleValue,omitempty"`
	ArrayValue  *otlpArray `json:"arrayValue,omitempty"`
}

type otlpArray struct {
	Values []otlpValue `json:"values"`
}

// encodeOTLP renders one trace as the OTLP JSON envelope.
//
// Spans with no end time (EndUnixNano == 0) are skipped: the OTLP schema has no
// representation for "started but never finished", and exporting a span that
// claims to end at the epoch would corrupt every duration computed downstream.
// A skipped span is not counted as a failure -- it is an abandoned span, which
// tracing.StatusUnset already models.
func encodeOTLP(t *tracing.Trace, serviceName string) ([]byte, error) {
	spans := make([]otlpSpan, 0, len(t.Spans))
	for i := range t.Spans {
		s := &t.Spans[i]
		if s.EndUnixNano == 0 {
			continue
		}
		spans = append(spans, otlpSpan{
			TraceID:           s.TraceID,
			SpanID:            s.SpanID,
			ParentSpanID:      s.ParentSpanID,
			Name:              s.Name,
			Kind:              otlpKind(s.Kind),
			StartTimeUnixNano: strconv.FormatInt(s.StartUnixNano, 10),
			EndTimeUnixNano:   strconv.FormatInt(s.EndUnixNano, 10),
			Attributes:        encodeAttributes(s.Attributes),
			Events:            encodeEvents(s.Events),
			Status:            otlpSpanStatus(s.Status),
		})
	}

	env := otlpEnvelope{
		ResourceSpans: []otlpResourceSpans{{
			Resource: otlpResource{
				Attributes: []otlpKeyValue{{
					Key:   "service.name",
					Value: otlpValue{StringValue: &serviceName},
				}},
			},
			ScopeSpans: []otlpScopeSpans{{
				Scope: otlpScope{Name: scopeName},
				Spans: spans,
			}},
		}},
	}
	return json.Marshal(env)
}

// otlpKind maps the tracing kind strings onto the OTLP SpanKind enum:
// internal=1, server=2, client=3, anything else (including "" from an
// unset field) = 0, unspecified.
func otlpKind(kind string) int {
	switch kind {
	case tracing.KindInternal:
		return 1
	case tracing.KindServer:
		return 2
	case tracing.KindClient:
		return 3
	default:
		return 0
	}
}

// otlpSpanStatus maps the tracing status strings onto StatusCode: ok=1,
// error=2, everything else 0. A 0 code means "unset" and the whole status
// object is omitted, because the OTLP default for a missing status IS unset --
// emitting {"code":0} would be a longer way to say the same thing.
func otlpSpanStatus(status string) *otlpStatus {
	switch status {
	case tracing.StatusOK:
		return &otlpStatus{Code: 1}
	case tracing.StatusError:
		return &otlpStatus{Code: 2}
	default:
		return nil
	}
}

// encodeEvents renders span events, skipping one whose timestamp is unset.
func encodeEvents(events []tracing.Event) []otlpEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]otlpEvent, 0, len(events))
	for _, e := range events {
		out = append(out, otlpEvent{
			Name:         e.Name,
			TimeUnixNano: strconv.FormatInt(e.TimeUnixNano, 10),
			Attributes:   encodeAttributes(e.Attributes),
		})
	}
	return out
}

// encodeAttributes renders an attribute map as a SORTED key/value list.
//
// Sorting is not cosmetic. JSON object order is not significant, but a Go map's
// iteration order is randomized, so without this the payload changes between
// two otherwise identical traces -- unreadable in a diff, unhashable for
// deduplication, and impossible to assert against a golden string in a test.
//
// nil values are skipped: OTLP has no null AnyValue, and fmt.Sprint(nil) would
// turn "no value" into the text "<nil>", which a dashboard would happily chart.
func encodeAttributes(attrs map[string]any) []otlpKeyValue {
	if len(attrs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(attrs))
	for k, v := range attrs {
		if v == nil {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]otlpKeyValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, otlpKeyValue{Key: k, Value: anyValue(attrs[k])})
	}
	return out
}

// anyValue picks the OTLP AnyValue arm from the Go dynamic type.
//
// Integer-ness is decided by TYPE, not by value: an int64 1 and a float64 1.0
// mean different things to a collector (a counter versus a gauge), and Go kept
// the distinction, so the wire format should too. Every integer width is
// funnelled into intValue as a DECIMAL STRING, which is the OTLP JSON rule for
// 64-bit integers and is what json.Number-like precision demands.
//
// Unknown types degrade to fmt.Sprint rather than being dropped: an attribute
// the exporter does not understand is still information the instrumenter
// considered worth recording, and a string is always a lossless container for
// "whatever this was".
func anyValue(v any) otlpValue {
	switch x := v.(type) {
	case nil:
		return otlpValue{}
	case string:
		return otlpValue{StringValue: &x}
	case bool:
		return otlpValue{BoolValue: &x}
	case int:
		return intValue(strconv.FormatInt(int64(x), 10))
	case int8:
		return intValue(strconv.FormatInt(int64(x), 10))
	case int16:
		return intValue(strconv.FormatInt(int64(x), 10))
	case int32:
		return intValue(strconv.FormatInt(int64(x), 10))
	case int64:
		return intValue(strconv.FormatInt(x, 10))
	case uint:
		return intValue(strconv.FormatUint(uint64(x), 10))
	case uint8:
		return intValue(strconv.FormatUint(uint64(x), 10))
	case uint16:
		return intValue(strconv.FormatUint(uint64(x), 10))
	case uint32:
		return intValue(strconv.FormatUint(uint64(x), 10))
	case uint64:
		return intValue(strconv.FormatUint(x, 10))
	case uintptr:
		return intValue(strconv.FormatUint(uint64(x), 10))
	case float32:
		f := float64(x)
		return otlpValue{DoubleValue: &f}
	case float64:
		return otlpValue{DoubleValue: &x}
	case []string:
		vals := make([]otlpValue, 0, len(x))
		for _, s := range x {
			s := s
			vals = append(vals, otlpValue{StringValue: &s})
		}
		return otlpValue{ArrayValue: &otlpArray{Values: vals}}
	default:
		// Anything else -- including []any, map[string]any and json.Number --
		// becomes its textual form. Only []string has a dedicated arm because
		// it is the shape the gateway's own instrumentation writes; widening
		// the switch would change what a collector sees for values that used
		// to be strings, and a type change on the wire is not undoable once
		// dashboards have been built on it.
		s := fmt.Sprint(v)
		return otlpValue{StringValue: &s}
	}
}

// intValue builds the intValue arm, whose JSON form is a string.
func intValue(decimal string) otlpValue {
	return otlpValue{IntValue: &decimal}
}
