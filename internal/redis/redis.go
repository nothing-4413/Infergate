// Package redis is a small RESP2 client, written by hand for the same reason
// internal/miniyaml was: the gateway builds from a bare Go toolchain with no
// module downloads, so a Redis dependency would be a build requirement for a
// milestone whose actual subject is caching.
//
// The surface is deliberately narrow — the cache store's needs, nothing more:
//
//	Do(ctx, args...)      one command, one reply
//	Pipeline(ctx, cmds)   N commands, N replies, one round trip, no MULTI
//
// RESP2 is implemented in full for the types the store uses (simple string,
// error, integer, bulk string, nil bulk, array) including replies nested inside
// arrays. `EVAL` exists in the real server but is not used here: a Lua script
// would hide the store's semantics from anyone reading the Go code, which is the
// opposite of this project's goal.
//
// What is NOT here: RESP3 (hello/attributes/push), pub/sub, MULTI/EXEC,
// cluster redirects, and TLS. Each is a real Redis feature and none of them is
// needed to share a cache between replicas.
package redis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxReplyDepth bounds recursion while decoding. A malicious or confused server
// could otherwise nest arrays until the stack overflows — a client crash caused
// by a reply, which is the worst kind of failure to debug.
const maxReplyDepth = 32

// maxReplySize bounds a single bulk string. Vectors are stored base64-encoded
// (a few KiB), so anything in the megabytes means either a wrong key or a
// hostile peer.
const maxReplySize = 64 << 20

// Error is a reply of type "-". Redis uses errors for both real failures
// ("WRONGTYPE") and expected outcomes ("NOSCRIPT"), so it is a value, not a
// transport failure.
type Error struct {
	Message string
}

func (e *Error) Error() string { return "redis: " + e.Message }

// IsNotFound reports whether err is Redis reporting a missing key, which the
// store treats as a cache miss rather than as an error.
func IsNotFound(err error) bool {
	var re *Error
	if errors.As(err, &re) {
		return strings.HasPrefix(re.Message, "ERR no such key")
	}
	return false
}

// Reply is one decoded RESP2 value.
type Reply struct {
	// Kind is the RESP type byte: '+', '-', ':', '$' or '*'.
	Kind byte

	// Str holds the payload of '+' and '$'.
	Str string

	// Int holds the payload of ':'.
	Int int64

	// Array holds the elements of '*'.
	Array []Reply

	// Null is true for a RESP2 nil bulk string ("$-1") or nil array ("*-1").
	Null bool
}

// Text returns the payload of a bulk or simple string.
func (r Reply) Text() (string, error) {
	switch r.Kind {
	case '+', '$':
		if r.Null {
			return "", errors.New("redis: reply is a null string")
		}
		return r.Str, nil
	default:
		return "", fmt.Errorf("redis: reply of type %q is not a string", string(r.Kind))
	}
}

// Strings returns the elements of an array as strings.
//
// It is the shape most store operations want (HGETALL, ZRANGE), and doing it
// here keeps the type switches out of the cache code. A nil element becomes an
// empty string rather than an error: Redis returns nils inside HGETALL replies
// for fields that vanished between the two internal steps.
func (r Reply) Strings() ([]string, error) {
	if r.Kind != '*' {
		return nil, fmt.Errorf("redis: reply of type %q is not an array", string(r.Kind))
	}
	if r.Null {
		return nil, nil
	}
	out := make([]string, 0, len(r.Array))
	for _, el := range r.Array {
		switch el.Kind {
		case '+', '$':
			out = append(out, el.Str)
		case ':':
			out = append(out, strconv.FormatInt(el.Int, 10))
		default:
			return nil, fmt.Errorf("redis: array element of type %q is not a string", string(el.Kind))
		}
	}
	return out, nil
}

// Ints returns the elements of an array as integers.
func (r Reply) Ints() ([]int64, error) {
	if r.Kind != '*' {
		return nil, fmt.Errorf("redis: reply of type %q is not an array", string(r.Kind))
	}
	out := make([]int64, 0, len(r.Array))
	for _, el := range r.Array {
		switch el.Kind {
		case ':':
			out = append(out, el.Int)
		case '$', '+':
			n, err := strconv.ParseInt(strings.TrimSpace(el.Str), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("redis: array element %q is not an integer", el.Str)
			}
			out = append(out, n)
		default:
			return nil, fmt.Errorf("redis: array element of type %q is not an integer", string(el.Kind))
		}
	}
	return out, nil
}

// AsError converts an error reply into a Go error and leaves other replies
// alone. Callers that only care about success can ignore it; callers that need
// to distinguish WRONGTYPE from a timeout must not.
func (r Reply) AsError() error {
	if r.Kind != '-' {
		return nil
	}
	return &Error{Message: r.Str}
}

// ---------------------------------------------------------------------------
// Options and client
// ---------------------------------------------------------------------------

// Options configures a Client.
type Options struct {
	// Addr is "host:port".
	Addr string

	// Password, when set, is sent as AUTH on every new connection. AUTH on
	// connect rather than on demand: a pooled connection has no request context
	// to carry the credentials.
	Password string

	// DB selects a logical database (SELECT). Zero is the default and the value
	// a single-tenant deployment should use; a non-zero DB is how two
	// environments share one Redis without sharing keys.
	DB int

	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// PoolSize caps concurrent connections. A cache lookup must never queue
	// behind a pool that is smaller than the request concurrency, so this
	// defaults to 16 and the client counts how often it waited (Stats.Waits).
	PoolSize int

	// IdleTimeout closes a pooled connection that has been idle. Redis closes
	// idle connections itself (timeout 0 by default, i.e. never), but a NAT or a
	// load balancer in between usually does, and a reused dead socket turns into
	// a spurious cache miss.
	IdleTimeout time.Duration
}

func (o *Options) applyDefaults() {
	if o.Addr == "" {
		o.Addr = "127.0.0.1:6379"
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 2 * time.Second
	}
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = 2 * time.Second
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 2 * time.Second
	}
	if o.PoolSize <= 0 {
		o.PoolSize = 16
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 5 * time.Minute
	}
}

// Stats reports client-level counters. They exist so a "the cache stopped
// hitting" investigation can tell a dead Redis from a changed threshold.
type Stats struct {
	Dials    int64 `json:"dials"`
	Reused   int64 `json:"reused"`
	Timeouts int64 `json:"timeouts"`
	Errors   int64 `json:"errors"`
	Waits    int64 `json:"pool_waits"`
	Open     int64 `json:"open_conns"`
}

// Client is a pooled RESP2 client. It is safe for concurrent use.
type Client struct {
	opts Options

	mu     sync.Mutex
	idle   []*conn
	open   int
	closed bool

	// sem bounds concurrency to PoolSize. A channel is used instead of a
	// WaitGroup because acquisition needs a timeout and a cancellation.
	sem chan struct{}

	statMu sync.Mutex
	stats  Stats
}

type conn struct {
	nc  net.Conn
	buf *bufio.Reader
	// lastUsed drives idle eviction.
	lastUsed time.Time
}

// NewClient returns a client. It does not connect: a cache that cannot reach
// Redis must start anyway and degrade to misses, so the first failure has to
// happen on a request, not during startup.
func NewClient(opts Options) *Client {
	opts.applyDefaults()
	return &Client{opts: opts, sem: make(chan struct{}, opts.PoolSize)}
}

// Addr returns the configured address.
func (c *Client) Addr() string { return c.opts.Addr }

func (c *Client) addStat(f func(*Stats)) {
	c.statMu.Lock()
	f(&c.stats)
	c.statMu.Unlock()
}

// Stats returns a snapshot of the client counters.
func (c *Client) Stats() Stats {
	c.statMu.Lock()
	defer c.statMu.Unlock()
	s := c.stats
	c.mu.Lock()
	s.Open = int64(c.open)
	c.mu.Unlock()
	return s
}

func (c *Client) acquire(ctx context.Context) (*conn, error) {
	select {
	case c.sem <- struct{}{}:
	default:
		c.addStat(func(s *Stats) { s.Waits++ })
		select {
		case c.sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	c.mu.Lock()
	for len(c.idle) > 0 {
		cn := c.idle[len(c.idle)-1]
		c.idle = c.idle[:len(c.idle)-1]
		if time.Since(cn.lastUsed) > c.opts.IdleTimeout {
			c.open--
			c.mu.Unlock()
			_ = cn.nc.Close()
			c.mu.Lock()
			continue
		}
		c.mu.Unlock()
		c.addStat(func(s *Stats) { s.Reused++ })
		return cn, nil
	}
	if c.closed {
		c.mu.Unlock()
		<-c.sem
		return nil, errors.New("redis: client is closed")
	}
	c.open++
	c.mu.Unlock()

	c.addStat(func(s *Stats) { s.Dials++ })
	cn, err := c.dial(ctx)
	if err != nil {
		c.mu.Lock()
		c.open--
		c.mu.Unlock()
		<-c.sem
		c.addStat(func(s *Stats) { s.Errors++ })
		return nil, err
	}
	return cn, nil
}

func (c *Client) release(cn *conn, healthy bool) {
	if cn == nil {
		return
	}
	if !healthy {
		// A connection with a half-read reply or a dead socket must be closed,
		// never returned: the next borrower would read the previous command's
		// reply and the cache would store it under the wrong key.
		c.mu.Lock()
		c.open--
		c.mu.Unlock()
		_ = cn.nc.Close()
		<-c.sem
		return
	}
	cn.lastUsed = time.Now()
	c.mu.Lock()
	if c.closed {
		c.open--
		c.mu.Unlock()
		_ = cn.nc.Close()
		<-c.sem
		return
	}
	c.idle = append(c.idle, cn)
	c.mu.Unlock()
	<-c.sem
}

func (c *Client) dial(ctx context.Context) (*conn, error) {
	d := net.Dialer{Timeout: c.opts.DialTimeout}
	nc, err := d.DialContext(ctx, "tcp", c.opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("redis: dial %s: %w", c.opts.Addr, err)
	}
	cn := &conn{nc: nc, buf: bufio.NewReaderSize(nc, 32<<10), lastUsed: time.Now()}

	// Handshake on the connection, not per command: AUTH and SELECT are
	// connection state in Redis, so doing them here means the store never has to
	// think about them.
	if c.opts.Password != "" {
		reply, err := c.roundTrip(cn, []string{"AUTH", c.opts.Password})
		if err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("redis: AUTH: %w", err)
		}
		if err := reply.AsError(); err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("redis: AUTH rejected: %w", err)
		}
	}
	if c.opts.DB != 0 {
		reply, err := c.roundTrip(cn, []string{"SELECT", strconv.Itoa(c.opts.DB)})
		if err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("redis: SELECT %d: %w", c.opts.DB, err)
		}
		if err := reply.AsError(); err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("redis: SELECT rejected: %w", err)
		}
	}
	return cn, nil
}

// Close closes idle connections and refuses new work.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	idle := c.idle
	c.idle = nil
	c.open -= len(idle)
	c.mu.Unlock()
	var firstErr error
	for _, cn := range idle {
		if err := cn.nc.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Do sends one command and returns its reply.
func (c *Client) Do(ctx context.Context, args ...string) (Reply, error) {
	if len(args) == 0 {
		return Reply{}, errors.New("redis: Do needs at least one argument")
	}
	replies, err := c.Pipeline(ctx, [][]string{args})
	if err != nil {
		return Reply{}, err
	}
	return replies[0], nil
}

// Pipeline sends every command and reads every reply on one connection.
//
// No MULTI/EXEC: the store's writes are individually coherent (each HSET
// replaces a whole entry), and wrapping them in a transaction would make a cache
// write able to block a Redis instance that other replicas depend on.
func (c *Client) Pipeline(ctx context.Context, cmds [][]string) ([]Reply, error) {
	if len(cmds) == 0 {
		return nil, nil
	}
	cn, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	healthy := false
	defer func() { c.release(cn, healthy) }()

	deadline := time.Now().Add(c.opts.WriteTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := cn.nc.SetWriteDeadline(deadline); err != nil {
		return nil, err
	}

	var buf []byte
	for _, args := range cmds {
		buf = appendCommand(buf, args)
	}
	if _, err := cn.nc.Write(buf); err != nil {
		c.addStat(func(s *Stats) { s.Errors++ })
		return nil, fmt.Errorf("redis: write: %w", err)
	}

	readDeadline := time.Now().Add(c.opts.ReadTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(readDeadline) {
		readDeadline = d
	}
	if err := cn.nc.SetReadDeadline(readDeadline); err != nil {
		return nil, err
	}

	out := make([]Reply, 0, len(cmds))
	for range cmds {
		reply, err := readReply(cn.buf, 0)
		if err != nil {
			c.addStat(func(s *Stats) { s.Errors++ })
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				c.addStat(func(s *Stats) { s.Timeouts++ })
			}
			return nil, err
		}
		out = append(out, reply)
	}
	healthy = true
	return out, nil
}

// roundTrip is used only during the connection handshake, where the reply must
// be read before the connection joins the pool.
func (c *Client) roundTrip(cn *conn, args []string) (Reply, error) {
	if err := cn.nc.SetWriteDeadline(time.Now().Add(c.opts.WriteTimeout)); err != nil {
		return Reply{}, err
	}
	if _, err := cn.nc.Write(appendCommand(nil, args)); err != nil {
		return Reply{}, err
	}
	if err := cn.nc.SetReadDeadline(time.Now().Add(c.opts.ReadTimeout)); err != nil {
		return Reply{}, err
	}
	return readReply(cn.buf, 0)
}

// Ping checks connectivity, which is what /healthz reports.
func (c *Client) Ping(ctx context.Context) error {
	reply, err := c.Do(ctx, "PING")
	if err != nil {
		return err
	}
	if err := reply.AsError(); err != nil {
		return err
	}
	if s, err := reply.Text(); err != nil || s != "PONG" {
		return fmt.Errorf("redis: PING returned %q", s)
	}
	return nil
}

// ---------------------------------------------------------------------------
// RESP2 wire format
// ---------------------------------------------------------------------------

// appendCommand appends one command as a RESP2 array of bulk strings.
//
// Everything is a bulk string, including numbers: Redis parses them, and the
// inline-command form is a trap for values containing spaces.
func appendCommand(buf []byte, args []string) []byte {
	buf = append(buf, '*')
	buf = strconv.AppendInt(buf, int64(len(args)), 10)
	buf = append(buf, '\r', '\n')
	for _, a := range args {
		buf = append(buf, '$')
		buf = strconv.AppendInt(buf, int64(len(a)), 10)
		buf = append(buf, '\r', '\n')
		buf = append(buf, a...)
		buf = append(buf, '\r', '\n')
	}
	return buf
}

func readReply(r *bufio.Reader, depth int) (Reply, error) {
	if depth > maxReplyDepth {
		return Reply{}, fmt.Errorf("redis: reply nesting exceeds %d levels", maxReplyDepth)
	}
	prefix, err := r.ReadByte()
	if err != nil {
		return Reply{}, fmt.Errorf("redis: read reply: %w", err)
	}
	switch prefix {
	case '+':
		line, err := readLine(r)
		if err != nil {
			return Reply{}, err
		}
		return Reply{Kind: '+', Str: line}, nil
	case '-':
		line, err := readLine(r)
		if err != nil {
			return Reply{}, err
		}
		return Reply{Kind: '-', Str: line}, nil
	case ':':
		line, err := readLine(r)
		if err != nil {
			return Reply{}, err
		}
		n, perr := strconv.ParseInt(line, 10, 64)
		if perr != nil {
			return Reply{}, fmt.Errorf("redis: invalid integer reply %q", line)
		}
		return Reply{Kind: ':', Int: n}, nil
	case '$':
		line, err := readLine(r)
		if err != nil {
			return Reply{}, err
		}
		n, perr := strconv.ParseInt(line, 10, 64)
		if perr != nil {
			return Reply{}, fmt.Errorf("redis: invalid bulk length %q", line)
		}
		if n < 0 {
			// "$-1" is RESP2's null, which Redis uses for a missing key.
			return Reply{Kind: '$', Null: true}, nil
		}
		if n > maxReplySize {
			return Reply{}, fmt.Errorf("redis: bulk reply of %d bytes exceeds the %d byte cap", n, maxReplySize)
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return Reply{}, fmt.Errorf("redis: read bulk reply: %w", err)
		}
		return Reply{Kind: '$', Str: string(buf[:n])}, nil
	case '*':
		line, err := readLine(r)
		if err != nil {
			return Reply{}, err
		}
		n, perr := strconv.ParseInt(line, 10, 64)
		if perr != nil {
			return Reply{}, fmt.Errorf("redis: invalid array length %q", line)
		}
		if n < 0 {
			return Reply{Kind: '*', Null: true}, nil
		}
		if n > maxReplySize {
			return Reply{}, fmt.Errorf("redis: array of %d elements exceeds the cap", n)
		}
		arr := make([]Reply, 0, n)
		for i := int64(0); i < n; i++ {
			el, err := readReply(r, depth+1)
			if err != nil {
				return Reply{}, err
			}
			arr = append(arr, el)
		}
		return Reply{Kind: '*', Array: arr}, nil
	default:
		return Reply{}, fmt.Errorf("redis: unknown reply type %q", string(prefix))
	}
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("redis: read line: %w", err)
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line, nil
}
