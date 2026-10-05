package redis

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infergate/infergate/internal/mockredis"
)

// startServer runs the in-process RESP2 server on an ephemeral port. The client
// under test is the production client; only the peer is a test double, and that
// peer has its own independent protocol parser (see internal/mockredis).
func startServer(t *testing.T, opts mockredis.Options) *mockredis.Server {
	t.Helper()
	opts.Addr = "127.0.0.1:0"
	srv := mockredis.New(opts)
	if err := srv.Start(); err != nil {
		t.Fatalf("mockredis.Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func newClient(t *testing.T, srv *mockredis.Server, mutate ...func(*Options)) *Client {
	t.Helper()
	opts := Options{Addr: srv.Addr(), DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second}
	for _, m := range mutate {
		m(&opts)
	}
	c := NewClient(opts)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ---------------------------------------------------------------------------
// Wire format
// ---------------------------------------------------------------------------

func TestAppendCommandEncodesRESP2(t *testing.T) {
	got := string(appendCommand(nil, []string{"SET", "key", "a b"}))
	want := "*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$3\r\na b\r\n"
	if got != want {
		t.Fatalf("encoded command:\n%q\nwant\n%q", got, want)
	}
	// Binary-safe: everything is a bulk string, including numbers, so a value
	// with spaces or CRLF cannot be mistaken for protocol framing.
	got = string(appendCommand(nil, []string{"SET", "k", "a\r\nb"}))
	if !strings.Contains(got, "$4\r\na\r\nb\r\n") {
		t.Fatalf("a CRLF inside a value must stay inside its bulk string, got %q", got)
	}
}

func TestReadReplyDecodesEveryRESP2Type(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Reply
	}{
		{"simple", "+OK\r\n", Reply{Kind: '+', Str: "OK"}},
		{"error", "-ERR nope\r\n", Reply{Kind: '-', Str: "ERR nope"}},
		{"integer", ":42\r\n", Reply{Kind: ':', Int: 42}},
		{"negative", ":-7\r\n", Reply{Kind: ':', Int: -7}},
		{"bulk", "$5\r\nhello\r\n", Reply{Kind: '$', Str: "hello"}},
		{"empty bulk", "$0\r\n\r\n", Reply{Kind: '$', Str: ""}},
		{"nil bulk", "$-1\r\n", Reply{Kind: '$', Null: true}},
		{"nil array", "*-1\r\n", Reply{Kind: '*', Null: true}},
		{"empty array", "*0\r\n", Reply{Kind: '*', Array: []Reply{}}},
		{"bulk with CRLF", "$4\r\na\r\nb\r\n", Reply{Kind: '$', Str: "a\r\nb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readReply(bufio.NewReader(strings.NewReader(tc.in)), 0)
			if err != nil {
				t.Fatalf("readReply: %v", err)
			}
			if got.Kind != tc.want.Kind || got.Str != tc.want.Str || got.Int != tc.want.Int || got.Null != tc.want.Null {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if tc.want.Array != nil && len(got.Array) != len(tc.want.Array) {
				t.Fatalf("array length %d, want %d", len(got.Array), len(tc.want.Array))
			}
		})
	}

	nested, err := readReply(bufio.NewReader(strings.NewReader("*2\r\n*2\r\n:1\r\n$3\r\ntwo\r\n+three\r\n")), 0)
	if err != nil {
		t.Fatalf("readReply nested: %v", err)
	}
	if len(nested.Array) != 2 || nested.Array[0].Kind != '*' || len(nested.Array[0].Array) != 2 {
		t.Fatalf("nested array decoded as %+v", nested)
	}
	if s, err := nested.Array[1].Text(); err != nil || s != "three" {
		t.Fatalf("nested element = %q, %v", s, err)
	}
}

func TestReadReplyRejectsMalformedFraming(t *testing.T) {
	cases := map[string]string{
		"unknown type":     "?1\r\n",
		"bad integer":      ":abc\r\n",
		"bad bulk length":  "$x\r\n",
		"short bulk":       "$10\r\nabc\r\n",
		"bad array length": "*x\r\n",
		"truncated array":  "*2\r\n$1\r\na\r\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := readReply(bufio.NewReader(strings.NewReader(in)), 0); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

func TestReadReplyBoundsNesting(t *testing.T) {
	// A server that sends 100 nested arrays must not overflow the stack.
	deep := strings.Repeat("*1\r\n", 100) + "$1\r\na\r\n"
	if _, err := readReply(bufio.NewReader(strings.NewReader(deep)), 0); err == nil {
		t.Fatal("nesting beyond the depth cap must be an error, not a crash")
	}
}

func TestReplyAccessors(t *testing.T) {
	arr := Reply{Kind: '*', Array: []Reply{{Kind: '$', Str: "a"}, {Kind: ':', Int: 3}, {Kind: '$', Null: true}}}
	ss, err := arr.Strings()
	if err != nil {
		t.Fatalf("Strings: %v", err)
	}
	if len(ss) != 3 || ss[0] != "a" || ss[1] != "3" || ss[2] != "" {
		t.Fatalf("Strings = %v", ss)
	}
	if _, err := (Reply{Kind: '$', Str: "x"}).Strings(); err == nil {
		t.Fatal("Strings on a non-array must error")
	}
	ints, err := (Reply{Kind: '*', Array: []Reply{{Kind: ':', Int: 1}, {Kind: '$', Str: "2"}}}).Ints()
	if err != nil || len(ints) != 2 || ints[0] != 1 || ints[1] != 2 {
		t.Fatalf("Ints = %v, %v", ints, err)
	}
	if _, err := (Reply{Kind: '*', Array: []Reply{{Kind: '$', Str: "x"}}}).Ints(); err == nil {
		t.Fatal("Ints on a non-numeric element must error")
	}
	if err := (Reply{Kind: '-', Str: "ERR boom"}).AsError(); err == nil {
		t.Fatal("an error reply must convert to a Go error")
	}
	if err := (Reply{Kind: '+', Str: "OK"}).AsError(); err != nil {
		t.Fatalf("a non-error reply must convert to nil, got %v", err)
	}
	var re *Error
	if err := (Reply{Kind: '-', Str: "ERR boom"}).AsError(); !errors.As(err, &re) || re.Message != "ERR boom" {
		t.Fatalf("error message lost: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Client against the RESP2 server
// ---------------------------------------------------------------------------

func TestClientRoundTripsTheStoreCommands(t *testing.T) {
	srv := startServer(t, mockredis.Options{})
	c := newClient(t, srv)
	ctx := context.Background()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if reply, err := c.Do(ctx, "SET", "k", "v"); err != nil {
		t.Fatalf("SET: %v", err)
	} else if s, _ := reply.Text(); s != "OK" {
		t.Fatalf("SET = %q", s)
	}
	if reply, err := c.Do(ctx, "GET", "k"); err != nil {
		t.Fatalf("GET: %v", err)
	} else if s, _ := reply.Text(); s != "v" {
		t.Fatalf("GET = %q", s)
	}
	if reply, err := c.Do(ctx, "GET", "absent"); err != nil {
		t.Fatalf("GET absent: %v", err)
	} else if !reply.Null {
		t.Fatalf("a missing key must be a nil bulk, got %+v", reply)
	}

	if _, err := c.Do(ctx, "HSET", "h", "f1", "v1", "f2", "v2"); err != nil {
		t.Fatalf("HSET: %v", err)
	}
	reply, err := c.Do(ctx, "HGETALL", "h")
	if err != nil {
		t.Fatalf("HGETALL: %v", err)
	}
	flat, err := reply.Strings()
	if err != nil {
		t.Fatalf("HGETALL decode: %v", err)
	}
	if len(flat) != 4 || flat[0] != "f1" || flat[1] != "v1" || flat[2] != "f2" || flat[3] != "v2" {
		t.Fatalf("HGETALL = %v", flat)
	}

	if _, err := c.Do(ctx, "ZADD", "z", "1", "a", "2", "b", "3", "c"); err != nil {
		t.Fatalf("ZADD: %v", err)
	}
	reply, err = c.Do(ctx, "ZRANGE", "z", "0", "-1", "REV", "WITHSCORES")
	if err != nil {
		t.Fatalf("ZRANGE: %v", err)
	}
	got, err := reply.Strings()
	if err != nil {
		t.Fatalf("ZRANGE decode: %v", err)
	}
	if strings.Join(got, ",") != "c,3,b,2,a,1" {
		t.Fatalf("ZRANGE REV WITHSCORES = %v", got)
	}
	if _, err := c.Do(ctx, "ZREMRANGEBYRANK", "z", "1", "-1"); err != nil {
		t.Fatalf("ZREMRANGEBYRANK: %v", err)
	}
	reply, _ = c.Do(ctx, "ZRANGE", "z", "0", "-1")
	got, _ = reply.Strings()
	if strings.Join(got, ",") != "a" {
		t.Fatalf("after trimming to the oldest entry, zset = %v", got)
	}
}

func TestClientPipelineReturnsRepliesInOrder(t *testing.T) {
	srv := startServer(t, mockredis.Options{})
	c := newClient(t, srv)
	ctx := context.Background()

	replies, err := c.Pipeline(ctx, [][]string{
		{"SET", "a", "1"},
		{"EVAL", "return 1", "0"},
		{"GET", "a"},
		{"PING"},
	})
	if err != nil {
		t.Fatalf("Pipeline: %v", err)
	}
	if len(replies) != 4 {
		t.Fatalf("got %d replies for 4 commands", len(replies))
	}
	if s, _ := replies[0].Text(); s != "OK" {
		t.Fatalf("reply 0 = %q", s)
	}
	// EVAL is not implemented by the test server on purpose — the quota store
	// deliberately relies on single-command atomicity rather than scripting. The
	// point here is that an error reply in the middle of a pipeline is returned
	// in its own slot and does not desynchronise the rest.
	if replies[1].Kind != '-' {
		t.Fatalf("reply 1 must be the unknown-command error, got %+v", replies[1])
	}
	if s, _ := replies[2].Text(); s != "1" {
		t.Fatalf("reply 2 = %q (pipelined replies must stay aligned)", s)
	}
	if s, _ := replies[3].Text(); s != "PONG" {
		t.Fatalf("reply 3 = %q", s)
	}
}

func TestClientHandlesTLSStyleExpiry(t *testing.T) {
	srv := startServer(t, mockredis.Options{})
	c := newClient(t, srv)
	ctx := context.Background()

	if _, err := c.Do(ctx, "HSET", "scope", "k", "v"); err != nil {
		t.Fatalf("HSET: %v", err)
	}
	if reply, err := c.Do(ctx, "EXPIRE", "scope", "1"); err != nil || reply.Int != 1 {
		t.Fatalf("EXPIRE = %+v, %v", reply, err)
	}
	if reply, err := c.Do(ctx, "TTL", "scope"); err != nil || reply.Int < 0 {
		t.Fatalf("TTL = %+v, %v", reply, err)
	}
	// The deadline must apply to the hash as well as to its index, otherwise a
	// store would read an index entry whose data is gone.
	if _, err := c.Do(ctx, "HSET", "idx", "m", "1"); err != nil {
		t.Fatalf("HSET: %v", err)
	}
	if _, err := c.Do(ctx, "EXPIRE", "idx", "60"); err != nil {
		t.Fatalf("EXPIRE: %v", err)
	}
	reply, err := c.Do(ctx, "TTL", "idx")
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if reply.Int < 50 || reply.Int > 60 {
		t.Fatalf("TTL = %d, want about 60", reply.Int)
	}
	if reply, err := c.Do(ctx, "TTL", "nope"); err != nil || reply.Int != -2 {
		t.Fatalf("TTL of a missing key = %+v, %v (want -2)", reply, err)
	}
}

func TestClientAuthHandshake(t *testing.T) {
	srv := startServer(t, mockredis.Options{RequirePass: "hunter2"})
	ctx := context.Background()

	bad := newClient(t, srv, func(o *Options) { o.Password = "wrong" })
	if err := bad.Ping(ctx); err == nil {
		t.Fatal("a wrong password must fail at handshake time, not at first command")
	}

	good := newClient(t, srv, func(o *Options) { o.Password = "hunter2" })
	if err := good.Ping(ctx); err != nil {
		t.Fatalf("Ping with the right password: %v", err)
	}
}

func TestClientErrorReplyIsAValueNotATransportFailure(t *testing.T) {
	srv := startServer(t, mockredis.Options{})
	c := newClient(t, srv)
	reply, err := c.Do(context.Background(), "NOPE")
	if err != nil {
		t.Fatalf("an error reply must return a reply, not a transport error: %v", err)
	}
	if reply.Kind != '-' {
		t.Fatalf("reply = %+v", reply)
	}
	if err := reply.AsError(); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("AsError = %v", err)
	}
}

func TestClientReportsAConnectionFailure(t *testing.T) {
	srv := startServer(t, mockredis.Options{})
	c := newClient(t, srv)
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	// Simulating "Redis went away" while the gateway keeps running is the whole
	// reason the cache must degrade to a miss instead of failing requests.
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Ping(ctx); err == nil {
		t.Fatal("Ping must fail once the server is gone")
	}
	if _, err := c.Do(ctx, "GET", "k"); err == nil {
		t.Fatal("a command against a dead server must return an error")
	}
	if s := c.Stats(); s.Errors == 0 {
		t.Fatal("the error counter must record the failure so an operator can see a dead Redis")
	}
}

func TestClientIsSafeUnderConcurrency(t *testing.T) {
	srv := startServer(t, mockredis.Options{})
	c := newClient(t, srv, func(o *Options) { o.PoolSize = 4 })
	ctx := context.Background()

	const workers, perWorker = 16, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				key := fmt.Sprintf("k:%d:%d", w, i)
				if _, err := c.Do(ctx, "SET", key, "v"); err != nil {
					errs <- err
					return
				}
				reply, err := c.Do(ctx, "GET", key)
				if err != nil {
					errs <- err
					return
				}
				if s, _ := reply.Text(); s != "v" {
					errs <- fmt.Errorf("GET %s = %q", key, s)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent client: %v", err)
	}
	// The pool must have been reused, not rebuilt per command: a client that
	// dials per request would show one dial per command.
	if st := c.Stats(); st.Dials > int64(workers) {
		t.Fatalf("dials = %d for %d workers; connections are not being pooled", st.Dials, workers)
	}
}

func TestClientRespectsContextDeadline(t *testing.T) {
	// No server on this port: the dial has to honour the context rather than sit
	// in a two-second TCP timeout while a client request waits.
	c := NewClient(Options{Addr: "127.0.0.1:1", DialTimeout: 5 * time.Second})
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Do(ctx, "PING"); err == nil {
		t.Fatal("dialling a closed port must fail")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the context deadline was ignored: took %v", elapsed)
	}
}

func TestIsNotFoundMatchesTheRedisMessage(t *testing.T) {
	if !IsNotFound(&Error{Message: "ERR no such key"}) {
		t.Fatal("IsNotFound must recognise a missing key")
	}
	if IsNotFound(&Error{Message: "WRONGTYPE Operation against a key holding the wrong kind of value"}) {
		t.Fatal("a type error is not a missing key; conflating them hides a real bug")
	}
	if IsNotFound(nil) {
		t.Fatal("nil is not a missing key")
	}
}

func TestClientCloseIsIdempotentAndRefusesNewWork(t *testing.T) {
	srv := startServer(t, mockredis.Options{})
	c := newClient(t, srv)
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := c.Do(ctx, "PING"); err == nil {
		t.Fatal("a closed client must refuse work")
	}
}

func TestClientHandlesManyEntriesInOneHash(t *testing.T) {
	// The store's lookup reads a whole scope in one HGETALL, so the reply path
	// has to survive a large array: 256 entries of ~2.7 KiB is the documented
	// default working set.
	srv := startServer(t, mockredis.Options{})
	c := newClient(t, srv)
	ctx := context.Background()
	payload := strings.Repeat("x", 2700)
	cmds := make([][]string, 0, 256)
	for i := 0; i < 256; i++ {
		cmds = append(cmds, []string{"HSET", "scope", fmt.Sprintf("k%03d", i), payload})
	}
	if _, err := c.Pipeline(ctx, cmds); err != nil {
		t.Fatalf("pipeline of 256 HSETs: %v", err)
	}
	reply, err := c.Do(ctx, "HGETALL", "scope")
	if err != nil {
		t.Fatalf("HGETALL: %v", err)
	}
	flat, err := reply.Strings()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(flat) != 512 {
		t.Fatalf("HGETALL returned %d elements, want 512", len(flat))
	}
	// A 32 KiB read buffer must grow for a ~700 KiB reply; a naive fixed read
	// would truncate it into a protocol error.
	if len(flat[1]) != len(payload) {
		t.Fatalf("payload length %d != %d", len(flat[1]), len(payload))
	}
}

func TestClientSelectsADatabase(t *testing.T) {
	srv := startServer(t, mockredis.Options{})
	ctx := context.Background()
	c := newClient(t, srv, func(o *Options) { o.DB = 3 })
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping after SELECT: %v", err)
	}
	replies, err := c.Pipeline(ctx, [][]string{{"SET", "k", "v"}, {"GET", "k"}})
	if err != nil {
		t.Fatalf("Pipeline: %v", err)
	}
	if s, _ := replies[1].Text(); s != "v" {
		t.Fatalf("GET after SELECT = %q", s)
	}
}

func TestStatsCountReuse(t *testing.T) {
	srv := startServer(t, mockredis.Options{})
	c := newClient(t, srv, func(o *Options) { o.PoolSize = 1 })
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := c.Do(ctx, "PING"); err != nil {
			t.Fatalf("PING %d: %v", i, err)
		}
	}
	st := c.Stats()
	if st.Reused < 4 {
		t.Fatalf("pool reuse = %d after 5 sequential commands; the pool is not being reused", st.Reused)
	}
	if st.Open != 1 {
		t.Fatalf("open connections = %d, want 1", st.Open)
	}
}

// ---------------------------------------------------------------------------
// Counter and claim commands (M3 quota primitives)
// ---------------------------------------------------------------------------

// TestClientRoundTripsTheCounterCommands covers the primitives a quota store
// needs and the cache store never used. They are tested through the real client
// over a real socket because their value is atomicity, and atomicity is a
// property of the server's command dispatch — a test that called the mock's
// methods directly would prove nothing about what a gateway can rely on.
func TestClientRoundTripsTheCounterCommands(t *testing.T) {
	srv := startServer(t, mockredis.Options{})
	c := newClient(t, srv)
	ctx := context.Background()

	// INCR creates the key at 1, and INCRBY/DECRBY move it.
	for _, want := range []int64{1, 4, 2} {
		var cmd []string
		switch want {
		case 1:
			cmd = []string{"INCR", "count"}
		case 4:
			cmd = []string{"INCRBY", "count", "3"}
		case 2:
			cmd = []string{"DECRBY", "count", "2"}
		}
		reply, err := c.Do(ctx, cmd...)
		if err != nil {
			t.Fatalf("%v: %v", cmd, err)
		}
		if reply.Int != want {
			t.Fatalf("%v = %d, want %d", cmd, reply.Int, want)
		}
	}
	if reply, err := c.Do(ctx, "DECR", "count"); err != nil {
		t.Fatalf("DECR: %v", err)
	} else if reply.Int != 1 {
		t.Fatalf("DECR = %d, want 1", reply.Int)
	}

	// A counter is a string, and a non-numeric value must be an error reply,
	// not a panic or a silent zero.
	if _, err := c.Do(ctx, "SET", "notanumber", "abc"); err != nil {
		t.Fatalf("SET: %v", err)
	}
	if reply, err := c.Do(ctx, "INCR", "notanumber"); err != nil {
		t.Fatalf("INCR on a non-numeric value returned a transport error: %v", err)
	} else if verr := reply.AsError(); verr == nil || !strings.Contains(verr.Error(), "not an integer") {
		t.Fatalf("INCR on a non-numeric value = %+v, want a value error", reply)
	}

	// SETNX is the only mutual-exclusion primitive available, so its answer must
	// distinguish "I claimed it" from "someone else has it".
	if reply, err := c.Do(ctx, "SETNX", "lock", "holder-a"); err != nil {
		t.Fatalf("SETNX: %v", err)
	} else if reply.Int != 1 {
		t.Fatalf("first SETNX = %d, want 1", reply.Int)
	}
	if reply, err := c.Do(ctx, "SETNX", "lock", "holder-b"); err != nil {
		t.Fatalf("SETNX: %v", err)
	} else if reply.Int != 0 {
		t.Fatalf("second SETNX = %d, want 0", reply.Int)
	}
	if reply, _ := c.Do(ctx, "GET", "lock"); true {
		if s, _ := reply.Text(); s != "holder-a" {
			t.Fatalf("SETNX overwrote the holder: GET = %q", s)
		}
	}

	// SETEX must make the key disappear on its own; a window counter that
	// outlives its window is a quota that never resets. The window is a minute
	// rather than a second because the property under test is that INCR leaves
	// the deadline alone, and a one-second window would expire mid-assertion.
	if _, err := c.Do(ctx, "SETEX", "window", "60", "5"); err != nil {
		t.Fatalf("SETEX: %v", err)
	}
	ttlBefore, err := c.Do(ctx, "TTL", "window")
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttlBefore.Int <= 0 || ttlBefore.Int > 60 {
		t.Fatalf("TTL after SETEX 60 = %d, want (0,60]", ttlBefore.Int)
	}
	// An existing TTL survives INCR: a hot counter must not extend its window.
	if reply, err := c.Do(ctx, "INCR", "window"); err != nil {
		t.Fatalf("INCR on a key with a TTL: %v", err)
	} else if reply.Int != 6 {
		t.Fatalf("INCR of the string \"5\" = %d, want 6", reply.Int)
	}
	ttlAfter, err := c.Do(ctx, "TTL", "window")
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	// TTL rounds down, so a second passing is expected; what must never happen is
	// the deadline moving AWAY (which is what a reset-to-full-window would look
	// like) or the key losing its expiry entirely.
	if ttlAfter.Int > ttlBefore.Int || ttlAfter.Int <= 0 {
		t.Fatalf("INCR moved the TTL from %d to %d; a counter must not extend its own window", ttlBefore.Int, ttlAfter.Int)
	}

	// MGET returns one reply per key, in order, with a nil for a missing one.
	if _, err := c.Do(ctx, "SET", "b", "2"); err != nil {
		t.Fatalf("SET: %v", err)
	}
	reply, err := c.Do(ctx, "MGET", "b", "missing", "count")
	if err != nil {
		t.Fatalf("MGET: %v", err)
	}
	if len(reply.Array) != 3 {
		t.Fatalf("MGET returned %d replies, want 3", len(reply.Array))
	}
	if s, _ := reply.Array[0].Text(); s != "2" {
		t.Fatalf("MGET[0] = %q, want 2", s)
	}
	if !reply.Array[1].Null {
		t.Fatalf("MGET[1] = %+v, want a nil bulk", reply.Array[1])
	}
	if s, _ := reply.Array[2].Text(); s != "1" {
		t.Fatalf("MGET[2] = %q, want 1", s)
	}

	// HINCRBY and ZINCRBY: the per-field and scored forms.
	if reply, err := c.Do(ctx, "HINCRBY", "quota", "day", "100"); err != nil {
		t.Fatalf("HINCRBY: %v", err)
	} else if reply.Int != 100 {
		t.Fatalf("HINCRBY = %d, want 100", reply.Int)
	}
	if reply, err := c.Do(ctx, "HINCRBY", "quota", "day", "50"); err != nil {
		t.Fatalf("HINCRBY: %v", err)
	} else if reply.Int != 150 {
		t.Fatalf("HINCRBY = %d, want 150", reply.Int)
	}
	if reply, err := c.Do(ctx, "ZINCRBY", "cost", "0.25", "tenant-a"); err != nil {
		t.Fatalf("ZINCRBY: %v", err)
	} else if s, _ := reply.Text(); s != "0.25" {
		t.Fatalf("ZINCRBY = %q, want 0.25", s)
	}
	if reply, err := c.Do(ctx, "ZINCRBY", "cost", "0.5", "tenant-a"); err != nil {
		t.Fatalf("ZINCRBY: %v", err)
	} else if s, _ := reply.Text(); s != "0.75" {
		t.Fatalf("ZINCRBY = %q, want 0.75", s)
	}
	if reply, err := c.Do(ctx, "ZSCORE", "cost", "tenant-a"); err != nil {
		t.Fatalf("ZSCORE: %v", err)
	} else if s, _ := reply.Text(); s != "0.75" {
		t.Fatalf("ZSCORE = %q, want 0.75", s)
	}
}

// readReply is exercised through a real socket above; this guards the one
// property the whole cache depends on and is easy to lose in a refactor: a bulk
// string is read as exactly N bytes, never split on the first newline.
func TestBulkStringIsByteExact(t *testing.T) {
	payload := "line1\r\nline2\r\n\r\n"
	raw := fmt.Sprintf("$%d\r\n%s\r\n", len(payload), payload)
	got, err := readReply(bufio.NewReader(bytes.NewReader([]byte(raw))), 0)
	if err != nil {
		t.Fatalf("readReply: %v", err)
	}
	if got.Str != payload {
		t.Fatalf("got %q, want %q", got.Str, payload)
	}
}
