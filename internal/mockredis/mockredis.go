// Package mockredis is an in-process RESP2 server: enough of Redis to run and
// test the gateway's Redis-backed stores without a Redis installation.
//
// Why this exists rather than a test-only stub: the store's real risk is not
// "does HSET round-trip" but "does the wire protocol survive a socket", and a
// store that can only be exercised with `docker run redis` on a developer
// machine is a store that is never exercised. This server speaks RESP2 over TCP,
// so the client under test is the production client, unchanged.
//
// The parser here is written INDEPENDENTLY of internal/redis's decoder. Sharing
// one implementation would let a wrong length prefix or a missing CRLF pass on
// both sides of the conversation, which is exactly the class of bug a
// protocol-level test double is supposed to catch.
//
// What is implemented: strings with EX/PX/NX/XX plus SETNX/SETEX, integer
// counters (INCR/INCRBY/DECR/DECRBY, and HINCRBY/ZINCRBY for the per-field and
// scored forms quotas need), MGET, hashes, sorted sets (rank and score ranges),
// expiry, key inspection (TYPE/KEYS/DBSIZE/INFO), SELECT, AUTH when a password
// is configured, FLUSHDB/FLUSHALL, and optional LRU eviction under a key cap —
// which is the one Redis policy a cache actually depends on.
// What is not: scripting (EVAL), MULTI/EXEC, WATCH, pub/sub, pipelining limits,
// replication, module commands, and RESP3.
//
// The counter commands are deliberately the atomic substitute for EVAL:
// internal/redis pools a connection per call, so a MULTI/EXEC sequence would
// span several connections and be worse than useless, while a single command is
// atomic on a real server. A quota store built on INCRBY alone is therefore
// honest about what it can and cannot guarantee.
package mockredis

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stats counts what the server has served, so an acceptance check can prove the
// cache actually talked to it (an empty Redis and a broken cache look identical
// from the outside otherwise).
type Stats struct {
	Commands    int64 `json:"commands"`
	Connections int64 `json:"connections"`
	Keys        int64 `json:"keys"`
	Evictions   int64 `json:"evictions"`
	Expired     int64 `json:"expired"`
}

// Options configures a Server.
type Options struct {
	// Addr is the listen address. ":0" picks an ephemeral port, which is what
	// tests want; the default is ":6399" so a real Redis on 6379 is not
	// shadowed.
	Addr string

	// RequirePass, when set, makes AUTH mandatory (and rejects everything else
	// with NOAUTH), so the client's handshake path is exercised.
	RequirePass string

	// MaxKeys turns on approximate LRU eviction above this many keys. Redis does
	// this with maxmemory-policy; the cache cares because a cap is what keeps a
	// scope from growing without bound.
	MaxKeys int

	// Logger, when set, logs every command at debug level. Debug, not info: a
	// cache at load is a firehose.
	Logger *slog.Logger
}

// Server is an in-memory RESP2 server.
type Server struct {
	opts Options

	ln     net.Listener
	done   chan struct{}
	wg     sync.WaitGroup
	closed bool

	mu     sync.Mutex
	strs   map[string]*stringVal
	hashes map[string]map[string]string
	zsets  map[string]map[string]float64
	// expires holds deadlines for hashes and zsets. Strings carry their own
	// deadline (stringVal.expire) because that mirrors how Redis stores it: the
	// deadline belongs to the key, and a cache that expires the hash but not its
	// index (or the reverse) leaves an index pointing at absent data.
	expires map[string]time.Time
	// access is the LRU clock, only maintained when MaxKeys > 0.
	access  map[string]int64
	clock   int64
	stats   Stats
	clients map[net.Conn]struct{}
	// authOK tracks per-connection authentication.
	authOK map[net.Conn]bool
}

type stringVal struct {
	value  string
	expire time.Time
}

// New returns an unstarted server.
func New(opts Options) *Server {
	if opts.Addr == "" {
		opts.Addr = ":6399"
	}
	return &Server{
		opts:    opts,
		done:    make(chan struct{}),
		strs:    map[string]*stringVal{},
		hashes:  map[string]map[string]string{},
		zsets:   map[string]map[string]float64{},
		expires: map[string]time.Time{},
		access:  map[string]int64{},
		clients: map[net.Conn]struct{}{},
		authOK:  map[net.Conn]bool{},
	}
}

// Start binds the listener and begins serving.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("mockredis: listen: %w", err)
	}
	s.ln = ln
	s.wg.Add(2)
	go s.serve()
	go s.sweepLoop()
	return nil
}

// sweepLoop drops expired keys on a timer, the way Redis's active expiry cycle
// does. Without it a key that is written once and never read again occupies
// memory forever.
func (s *Server) sweepLoop() {
	defer s.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.sweepExpired()
		}
	}
}

// Addr returns the bound address (useful with ":0").
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.opts.Addr
	}
	return s.ln.Addr().String()
}

// Stats returns a snapshot of the counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.Keys = int64(len(s.strs) + len(s.hashes) + len(s.zsets))
	st.Connections = int64(s.stats.Connections)
	return st
}

// Close stops the listener and drops every client connection, which is how a
// test simulates "Redis went away" while the gateway keeps running.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.ln
	clients := make([]net.Conn, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	close(s.done)
	for _, c := range clients {
		_ = c.Close()
	}
	if ln != nil {
		_ = ln.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.mu.Lock()
		s.stats.Connections++
		s.clients[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.clients, c)
		delete(s.authOK, c)
		s.mu.Unlock()
		_ = c.Close()
	}()

	r := bufio.NewReaderSize(c, 32<<10)
	w := bufio.NewWriterSize(c, 32<<10)
	for {
		args, err := readCommand(r)
		if err != nil {
			if err != io.EOF {
				// A malformed command is answered, not ignored: the client must
				// see an error reply rather than a hang.
				_ = writeError(w, "ERR Protocol error: "+err.Error())
				_ = w.Flush()
			}
			return
		}
		if len(args) == 0 {
			continue
		}
		if s.opts.Logger != nil {
			s.opts.Logger.Debug("mockredis command", slog.String("cmd", strings.ToUpper(args[0])), slog.String("args", strings.Join(args[1:], " ")))
		}
		reply := s.dispatch(c, args)
		if reply == "" {
			continue
		}
		if _, err := w.WriteString(reply); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

// dispatch executes one command and returns its encoded reply.
func (s *Server) dispatch(c net.Conn, args []string) string {
	cmd := strings.ToUpper(args[0])

	s.mu.Lock()
	s.stats.Commands++
	s.clock++
	var authed bool
	if s.opts.RequirePass != "" {
		authed = s.authOK[c]
	}
	s.mu.Unlock()

	if cmd == "AUTH" {
		return s.cmdAuth(c, args)
	}
	if s.opts.RequirePass != "" && !authed {
		return encodeError("NOAUTH Authentication required.")
	}
	if cmd == "QUIT" {
		return encodeSimple("OK")
	}

	switch cmd {
	case "PING":
		if len(args) > 1 {
			return encodeBulk(args[1])
		}
		return encodeSimple("PONG")
	case "ECHO":
		if len(args) != 2 {
			return arity(cmd)
		}
		return encodeBulk(args[1])
	case "SELECT":
		// Single-database server: accepting SELECT keeps the client's handshake
		// path working. A real deployment with several logical databases is a
		// configuration concern, not a cache one.
		return encodeSimple("OK")
	case "CONFIG":
		return encodeArray(nil)
	case "COMMAND":
		return encodeArray(nil)
	case "INFO":
		return encodeBulk(s.infoText())
	case "DBSIZE":
		s.mu.Lock()
		n := len(s.strs) + len(s.hashes) + len(s.zsets)
		s.mu.Unlock()
		return encodeInt(int64(n))
	case "FLUSHDB", "FLUSHALL":
		s.mu.Lock()
		s.strs = map[string]*stringVal{}
		s.hashes = map[string]map[string]string{}
		s.zsets = map[string]map[string]float64{}
		s.expires = map[string]time.Time{}
		s.access = map[string]int64{}
		s.mu.Unlock()
		return encodeSimple("OK")
	}

	// The commands below are the ones the cache store uses.
	switch cmd {
	case "DEL":
		return s.cmdDel(args)
	case "EXISTS":
		return s.cmdExists(args)
	case "EXPIRE", "PEXPIRE":
		return s.cmdExpire(cmd, args)
	case "TTL", "PTTL":
		return s.cmdTTL(cmd, args)
	case "TYPE":
		return s.cmdType(args)
	case "KEYS":
		return s.cmdKeys(args)
	case "GET":
		return s.cmdGet(args)
	case "SET":
		return s.cmdSet(args)
	case "SETNX":
		return s.cmdSetNX(args)
	case "SETEX":
		return s.cmdSetEX(args)
	case "INCR", "INCRBY", "DECR", "DECRBY":
		return s.cmdIncr(cmd, args)
	case "MGET":
		return s.cmdMGet(args)
	case "HSET", "HMSET":
		return s.cmdHSet(args)
	case "HGET":
		return s.cmdHGet(args)
	case "HINCRBY":
		return s.cmdHIncrBy(args)
	case "HDEL":
		return s.cmdHDel(args)
	case "HGETALL":
		return s.cmdHGetAll(args)
	case "HLEN":
		return s.cmdHLen(args)
	case "HKEYS":
		return s.cmdHKeys(args)
	case "HVALS":
		return s.cmdHVals(args)
	case "ZADD":
		return s.cmdZAdd(args)
	case "ZCARD":
		return s.cmdZCard(args)
	case "ZRANGE", "ZREVRANGE":
		return s.cmdZRange(cmd, args)
	case "ZRANGEBYSCORE":
		return s.cmdZRangeByScore(args)
	case "ZSCORE":
		return s.cmdZScore(args)
	case "ZINCRBY":
		return s.cmdZIncrBy(args)
	case "ZREM":
		return s.cmdZRem(args)
	case "ZREMRANGEBYRANK":
		return s.cmdZRemRangeByRank(args)
	case "ZREMRANGEBYSCORE":
		return s.cmdZRemRangeByScore(args)
	}
	return encodeError("ERR unknown command '" + args[0] + "'")
}

func (s *Server) cmdAuth(c net.Conn, args []string) string {
	if s.opts.RequirePass == "" {
		return encodeError("ERR Client sent AUTH, but no password is set")
	}
	if len(args) != 2 {
		return arity("AUTH")
	}
	if args[1] != s.opts.RequirePass {
		return encodeError("WRONGPASS invalid username-password pair or user is disabled.")
	}
	s.mu.Lock()
	s.authOK[c] = true
	s.mu.Unlock()
	return encodeSimple("OK")
}

// ---------------------------------------------------------------------------
// Expiry helpers. Every read goes through getString/getHash/getZSet so a lazy
// expiry is impossible to forget on one code path and not another.
//
// The collection accessors return COPIES. Returning the live map would hand a
// caller a reference that outlives the lock, and two connections reading and
// writing the same hash would then trip Go's concurrent-map-write check — a
// crash inside the test double that looks exactly like the gateway's bug.
// ---------------------------------------------------------------------------

func (s *Server) expired(expire time.Time) bool {
	return !expire.IsZero() && time.Now().After(expire)
}

func (s *Server) getString(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.strs[key]
	if !ok {
		return "", false
	}
	if s.expired(v.expire) {
		delete(s.strs, key)
		delete(s.access, key)
		s.stats.Expired++
		return "", false
	}
	s.touch(key)
	return v.value, true
}

// getHash returns a copy of the hash, or false when it is absent or expired.
func (s *Server) getHash(key string) (map[string]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(key)
	h, ok := s.hashes[key]
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out, true
}

// getZSet returns a copy of the sorted set, or false when it is absent or
// expired.
func (s *Server) getZSet(key string) (map[string]float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(key)
	z, ok := s.zsets[key]
	if !ok {
		return nil, false
	}
	out := make(map[string]float64, len(z))
	for k, v := range z {
		out[k] = v
	}
	return out, true
}

// expireLocked drops a hash or zset whose deadline has passed. Called with the
// lock held.
func (s *Server) expireLocked(key string) {
	at, ok := s.expires[key]
	if !ok || !s.expired(at) {
		return
	}
	_, hadH := s.hashes[key]
	_, hadZ := s.zsets[key]
	delete(s.hashes, key)
	delete(s.zsets, key)
	delete(s.expires, key)
	delete(s.access, key)
	if hadH || hadZ {
		s.stats.Expired++
	}
}

// sweepExpired drops every expired key. Redis does this with a background
// sample; a cache server without it would hold expired entries until they are
// read, which for a write-once scope means forever.
func (s *Server) sweepExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.strs {
		if !v.expire.IsZero() && now.After(v.expire) {
			delete(s.strs, k)
			delete(s.access, k)
			s.stats.Expired++
		}
	}
	for k, at := range s.expires {
		if now.After(at) {
			s.expireLocked(k)
		}
	}
}

// touch advances the LRU clock for a key. Called with the lock held.
func (s *Server) touch(key string) {
	if s.opts.MaxKeys > 0 {
		s.access[key] = s.clock
	}
}

// evictLocked drops the least recently used keys until the cap holds. It runs
// after a write, which is where Redis runs its own eviction.
func (s *Server) evictLocked() {
	if s.opts.MaxKeys <= 0 {
		return
	}
	total := len(s.strs) + len(s.hashes) + len(s.zsets)
	if total <= s.opts.MaxKeys {
		return
	}
	type kv struct {
		key string
		at  int64
	}
	all := make([]kv, 0, len(s.access))
	for k, at := range s.access {
		all = append(all, kv{k, at})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at < all[j].at })
	for _, e := range all {
		if total <= s.opts.MaxKeys {
			break
		}
		removed := false
		if _, ok := s.strs[e.key]; ok {
			delete(s.strs, e.key)
			removed = true
		}
		if _, ok := s.hashes[e.key]; ok {
			delete(s.hashes, e.key)
			removed = true
		}
		if _, ok := s.zsets[e.key]; ok {
			delete(s.zsets, e.key)
			removed = true
		}
		delete(s.access, e.key)
		if removed {
			total--
			s.stats.Evictions++
		}
	}
}

// ---------------------------------------------------------------------------
// Strings
// ---------------------------------------------------------------------------

func (s *Server) cmdGet(args []string) string {
	if len(args) != 2 {
		return arity("GET")
	}
	v, ok := s.getString(args[1])
	if !ok {
		return encodeNilBulk()
	}
	return encodeBulk(v)
}

func (s *Server) cmdSet(args []string) string {
	if len(args) < 3 {
		return arity("SET")
	}
	key, val := args[1], args[2]
	var (
		expire time.Time
		nx, xx bool
	)
	for i := 3; i < len(args); i++ {
		switch strings.ToUpper(args[i]) {
		case "EX":
			if i+1 >= len(args) {
				return encodeError("ERR syntax error")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return encodeError("ERR value is not an integer or out of range")
			}
			expire = time.Now().Add(time.Duration(n) * time.Second)
			i++
		case "PX":
			if i+1 >= len(args) {
				return encodeError("ERR syntax error")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return encodeError("ERR value is not an integer or out of range")
			}
			expire = time.Now().Add(time.Duration(n) * time.Millisecond)
			i++
		case "NX":
			nx = true
		case "XX":
			xx = true
		case "KEEPTTL":
			// Accepted and ignored: nothing in the cache relies on preserving a
			// TTL across a write, and silently accepting is better than failing a
			// command the store might one day send.
		default:
			return encodeError("ERR syntax error")
		}
	}
	s.mu.Lock()
	_, exists := s.strs[key]
	if !exists {
		// A key could still exist as a hash or a zset; EXISTS would say so.
		if _, ok := s.hashes[key]; ok {
			exists = true
		}
		if _, ok := s.zsets[key]; ok {
			exists = true
		}
	}
	if (nx && exists) || (xx && !exists) {
		s.mu.Unlock()
		return encodeNilBulk()
	}
	s.strs[key] = &stringVal{value: val, expire: expire}
	delete(s.hashes, key)
	delete(s.zsets, key)
	s.touch(key)
	s.evictLocked()
	s.mu.Unlock()
	return encodeSimple("OK")
}

// cmdSetNX implements SETNX key value: an all-or-nothing claim on a key, which
// is the only mutual-exclusion primitive this server offers (there is no
// MULTI/EXEC or WATCH). Expiry is honoured, so a dead key is claimable.
func (s *Server) cmdSetNX(args []string) string {
	if len(args) != 3 {
		return arity("SETNX")
	}
	key, val := args[1], args[2]
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(key)
	if s.keyExistsLocked(key) {
		return encodeInt(0)
	}
	s.strs[key] = &stringVal{value: val}
	delete(s.hashes, key)
	delete(s.zsets, key)
	s.touch(key)
	s.evictLocked()
	return encodeInt(1)
}

// cmdSetEX implements SETEX key seconds value, the command a quota store uses
// for a window counter that must not outlive its window.
func (s *Server) cmdSetEX(args []string) string {
	if len(args) != 4 {
		return arity("SETEX")
	}
	n, err := strconv.Atoi(args[2])
	if err != nil {
		return encodeError("ERR value is not an integer or out of range")
	}
	if n <= 0 {
		return encodeError("ERR invalid expire time in 'setex' command")
	}
	key, val := args[1], args[3]
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strs[key] = &stringVal{value: val, expire: time.Now().Add(time.Duration(n) * time.Second)}
	delete(s.hashes, key)
	delete(s.zsets, key)
	s.touch(key)
	s.evictLocked()
	return encodeSimple("OK")
}

// cmdIncr implements INCR/INCRBY/DECR/DECRBY. Redis performs these on a single
// key under its own lock, so they are the atomic counter primitive a quota
// store needs without a scripting engine: the whole read-modify-write happens
// here while the server lock is held, and a real Redis gives the same guarantee
// for the same reason (a command is atomic, a script is not required).
//
// TTL semantics match Redis: incrementing an existing key leaves its deadline
// alone, so a window counter's expiry does not slide forward under load — a
// sliding window would never reset exactly when traffic is heaviest.
func (s *Server) cmdIncr(cmd string, args []string) string {
	needsArg := cmd == "INCRBY" || cmd == "DECRBY"
	want := 2
	if needsArg {
		want = 3
	}
	if len(args) != want {
		return arity(cmd)
	}
	delta := int64(1)
	if needsArg {
		n, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return encodeError("ERR value is not an integer or out of range")
		}
		delta = n
	}
	if cmd == "DECR" || cmd == "DECRBY" {
		delta = -delta
	}
	key := args[1]
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(key)
	if s.keyExistsLocked(key) {
		if _, ok := s.strs[key]; !ok {
			return encodeError("WRONGTYPE Operation against a key holding the wrong kind of value")
		}
	}
	var cur int64
	var expire time.Time
	if v, ok := s.strs[key]; ok && !s.expired(v.expire) {
		n, err := strconv.ParseInt(v.value, 10, 64)
		if err != nil {
			return encodeError("ERR value is not an integer or out of range")
		}
		cur = n
		expire = v.expire
	}
	next := cur + delta
	if (delta > 0 && next < cur) || (delta < 0 && next > cur) {
		return encodeError("ERR increment or decrement would overflow")
	}
	s.strs[key] = &stringVal{value: strconv.FormatInt(next, 10), expire: expire}
	delete(s.hashes, key)
	delete(s.zsets, key)
	s.touch(key)
	s.evictLocked()
	return encodeInt(next)
}

// cmdMGet implements MGET, the batched read a quota check uses to fetch every
// window of one tenant in a single round trip.
func (s *Server) cmdMGet(args []string) string {
	if len(args) < 2 {
		return arity("MGET")
	}
	parts := make([]string, 0, len(args)-1)
	for _, k := range args[1:] {
		s.mu.Lock()
		s.expireLocked(k)
		v, ok := s.strs[k]
		if ok && s.expired(v.expire) {
			delete(s.strs, k)
			delete(s.access, k)
			s.stats.Expired++
			ok = false
		}
		if ok {
			s.touch(k)
		}
		s.mu.Unlock()
		if !ok {
			parts = append(parts, encodeNilBulk())
			continue
		}
		parts = append(parts, encodeBulk(v.value))
	}
	return encodeArrayRaw(parts)
}

// keyExistsLocked reports whether a live key of any type holds this name.
// Called with the lock held; it lazily expires a stale string the way GET does.
func (s *Server) keyExistsLocked(key string) bool {
	if v, ok := s.strs[key]; ok {
		if s.expired(v.expire) {
			delete(s.strs, key)
			delete(s.access, key)
			s.stats.Expired++
		} else {
			return true
		}
	}
	if _, ok := s.hashes[key]; ok {
		return true
	}
	if _, ok := s.zsets[key]; ok {
		return true
	}
	return false
}

func (s *Server) cmdDel(args []string) string {
	if len(args) < 2 {
		return arity("DEL")
	}
	s.mu.Lock()
	var n int64
	for _, k := range args[1:] {
		removed := false
		if _, ok := s.strs[k]; ok {
			delete(s.strs, k)
			removed = true
		}
		if _, ok := s.hashes[k]; ok {
			delete(s.hashes, k)
			removed = true
		}
		if _, ok := s.zsets[k]; ok {
			delete(s.zsets, k)
			removed = true
		}
		delete(s.access, k)
		if removed {
			n++
		}
	}
	s.mu.Unlock()
	return encodeInt(n)
}

func (s *Server) cmdExists(args []string) string {
	if len(args) < 2 {
		return arity("EXISTS")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, k := range args[1:] {
		if _, ok := s.strs[k]; ok {
			n++
			continue
		}
		if _, ok := s.hashes[k]; ok {
			n++
			continue
		}
		if _, ok := s.zsets[k]; ok {
			n++
		}
	}
	return encodeInt(n)
}

func (s *Server) cmdExpire(cmd string, args []string) string {
	if len(args) != 3 {
		return arity(cmd)
	}
	n, err := strconv.Atoi(args[2])
	if err != nil {
		return encodeError("ERR value is not an integer or out of range")
	}
	d := time.Duration(n) * time.Second
	if cmd == "PEXPIRE" {
		d = time.Duration(n) * time.Millisecond
	}
	at := time.Now().Add(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.strs[args[1]]; ok {
		v.expire = at
		return encodeInt(1)
	}
	// Hashes and zsets expire together, mirroring how the store writes them:
	// one entry lives in two keys, and a half-expired pair would leave an index
	// pointing at absent data.
	_, h := s.hashes[args[1]]
	_, z := s.zsets[args[1]]
	if h || z {
		s.expires[args[1]] = at
		return encodeInt(1)
	}
	return encodeInt(0)
}

func (s *Server) cmdTTL(cmd string, args []string) string {
	if len(args) != 2 {
		return arity(cmd)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var expire time.Time
	switch {
	case s.strs[args[1]] != nil:
		expire = s.strs[args[1]].expire
	case s.hashes[args[1]] != nil, s.zsets[args[1]] != nil:
		expire = s.expires[args[1]]
	default:
		return encodeInt(-2)
	}
	if expire.IsZero() {
		return encodeInt(-1)
	}
	left := time.Until(expire)
	if left < 0 {
		return encodeInt(-2)
	}
	if cmd == "PTTL" {
		return encodeInt(left.Milliseconds())
	}
	return encodeInt(int64(left.Seconds()))
}

func (s *Server) cmdType(args []string) string {
	if len(args) != 2 {
		return arity("TYPE")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.strs[args[1]]; ok {
		return encodeSimple("string")
	}
	if _, ok := s.hashes[args[1]]; ok {
		return encodeSimple("hash")
	}
	if _, ok := s.zsets[args[1]]; ok {
		return encodeSimple("zset")
	}
	return encodeSimple("none")
}

func (s *Server) cmdKeys(args []string) string {
	if len(args) != 2 {
		return arity("KEYS")
	}
	pattern := args[1]
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	add := func(k string) {
		if strings.HasPrefix(k, "\x00expire:") {
			return
		}
		if matchGlob(pattern, k) {
			out = append(out, k)
		}
	}
	for k := range s.strs {
		add(k)
	}
	for k := range s.hashes {
		add(k)
	}
	for k := range s.zsets {
		add(k)
	}
	sort.Strings(out)
	return encodeArray(out)
}

// matchGlob supports only the "*" wildcard, which is all a diagnostic KEYS
// needs and all the tests use.
func matchGlob(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	if i := strings.Index(pattern, "*"); i >= 0 {
		return strings.HasPrefix(s, pattern[:i]) && strings.HasSuffix(s, pattern[i+1:])
	}
	return pattern == s
}

// ---------------------------------------------------------------------------
// Hashes
// ---------------------------------------------------------------------------

func (s *Server) cmdHSet(args []string) string {
	if len(args) < 4 || len(args)%2 != 0 {
		return arity("HSET")
	}
	key := args[1]
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hashes[key]
	if !ok {
		h = map[string]string{}
		s.hashes[key] = h
	}
	var added int64
	for i := 2; i+1 < len(args); i += 2 {
		if _, exists := h[args[i]]; !exists {
			added++
		}
		h[args[i]] = args[i+1]
	}
	s.touch(key)
	s.evictLocked()
	return encodeInt(added)
}

func (s *Server) cmdHGet(args []string) string {
	if len(args) != 3 {
		return arity("HGET")
	}
	h, ok := s.getHash(args[1])
	if !ok {
		return encodeNilBulk()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := h[args[2]]
	if !ok {
		return encodeNilBulk()
	}
	return encodeBulk(v)
}

// cmdHIncrBy implements HINCRBY, the counter-per-field form a quota store uses
// when one hash holds every window of a tenant and each field must increment
// independently.
func (s *Server) cmdHIncrBy(args []string) string {
	if len(args) != 4 {
		return arity("HINCRBY")
	}
	delta, err := strconv.ParseInt(args[3], 10, 64)
	if err != nil {
		return encodeError("ERR value is not an integer or out of range")
	}
	key, field := args[1], args[2]
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(key)
	if _, ok := s.strs[key]; ok {
		return encodeError("WRONGTYPE Operation against a key holding the wrong kind of value")
	}
	h, ok := s.hashes[key]
	if !ok {
		h = map[string]string{}
		s.hashes[key] = h
	}
	var cur int64
	if raw, ok := h[field]; ok {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return encodeError("ERR hash value is not an integer")
		}
		cur = n
	}
	next := cur + delta
	if (delta > 0 && next < cur) || (delta < 0 && next > cur) {
		return encodeError("ERR increment or decrement would overflow")
	}
	h[field] = strconv.FormatInt(next, 10)
	s.touch(key)
	s.evictLocked()
	return encodeInt(next)
}

func (s *Server) cmdHDel(args []string) string {
	if len(args) < 3 {
		return arity("HDEL")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hashes[args[1]]
	if !ok {
		return encodeInt(0)
	}
	var n int64
	for _, f := range args[2:] {
		if _, ok := h[f]; ok {
			delete(h, f)
			n++
		}
	}
	return encodeInt(n)
}

func (s *Server) cmdHGetAll(args []string) string {
	if len(args) != 2 {
		return arity("HGETALL")
	}
	h, ok := s.getHash(args[1])
	if !ok {
		return encodeArray(nil)
	}
	// HGETALL returns a flat field, value, field, value, ... array, so the only
	// thing that may be sorted is the FIELD list. Sorting the flattened array
	// would move all the values behind all the fields and silently pair every
	// field with the wrong value - a caller reading pairs positionally would
	// then store a similarity under a key it does not belong to. Redis leaves
	// the order unspecified; sorting the keys just makes the double
	// deterministic for tests.
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	flat := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		flat = append(flat, k, h[k])
	}
	return encodeArray(flat)
}

func (s *Server) cmdHLen(args []string) string {
	if len(args) != 2 {
		return arity("HLEN")
	}
	h, ok := s.getHash(args[1])
	if !ok {
		return encodeInt(0)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return encodeInt(int64(len(h)))
}

func (s *Server) cmdHKeys(args []string) string {
	if len(args) != 2 {
		return arity("HKEYS")
	}
	h, ok := s.getHash(args[1])
	if !ok {
		return encodeArray(nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	sort.Strings(out)
	return encodeArray(out)
}

func (s *Server) cmdHVals(args []string) string {
	if len(args) != 2 {
		return arity("HVALS")
	}
	h, ok := s.getHash(args[1])
	if !ok {
		return encodeArray(nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(h))
	for _, k := range keys {
		out = append(out, h[k])
	}
	return encodeArray(out)
}

// ---------------------------------------------------------------------------
// Sorted sets
// ---------------------------------------------------------------------------

func (s *Server) cmdZAdd(args []string) string {
	if len(args) < 4 || len(args)%2 != 0 {
		return arity("ZADD")
	}
	key := args[1]
	start := 2
	// ZADD key [NX|XX|GT|LT|CH] score member ...
	for start < len(args)-1 {
		switch strings.ToUpper(args[start]) {
		case "NX", "XX", "GT", "LT", "CH":
			start++
			continue
		}
		break
	}
	if (len(args)-start)%2 != 0 {
		return arity("ZADD")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	z, ok := s.zsets[key]
	if !ok {
		z = map[string]float64{}
		s.zsets[key] = z
	}
	var added int64
	for i := start; i+1 < len(args); i += 2 {
		score, err := strconv.ParseFloat(args[i], 64)
		if err != nil {
			return encodeError("ERR value is not a valid float")
		}
		if _, exists := z[args[i+1]]; !exists {
			added++
		}
		z[args[i+1]] = score
	}
	s.touch(key)
	s.evictLocked()
	return encodeInt(added)
}

// cmdZIncrBy implements ZINCRBY, the scored-counter form used for weighted
// cost windows (a request's cost is a float, its count is not).
func (s *Server) cmdZIncrBy(args []string) string {
	if len(args) != 4 {
		return arity("ZINCRBY")
	}
	delta, err := strconv.ParseFloat(args[2], 64)
	if err != nil {
		return encodeError("ERR value is not a valid float")
	}
	key, member := args[1], args[3]
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(key)
	z, ok := s.zsets[key]
	if !ok {
		z = map[string]float64{}
		s.zsets[key] = z
	}
	next := z[member] + delta
	z[member] = next
	s.touch(key)
	s.evictLocked()
	return encodeBulk(formatFloat(next))
}

func (s *Server) cmdZCard(args []string) string {
	if len(args) != 2 {
		return arity("ZCARD")
	}
	z, ok := s.getZSet(args[1])
	if !ok {
		return encodeInt(0)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return encodeInt(int64(len(z)))
}

// cmdZRange implements the rank form, including REV and WITHSCORES.
func (s *Server) cmdZRange(cmd string, args []string) string {
	if len(args) < 4 {
		return arity(cmd)
	}
	start, err1 := strconv.Atoi(args[2])
	stop, err2 := strconv.Atoi(args[3])
	if err1 != nil || err2 != nil {
		return encodeError("ERR value is not an integer or out of range")
	}
	rev := cmd == "ZREVRANGE"
	withScores := false
	for _, a := range args[4:] {
		switch strings.ToUpper(a) {
		case "WITHSCORES":
			withScores = true
		case "REV":
			rev = true
		default:
			return encodeError("ERR syntax error")
		}
	}
	z, ok := s.getZSet(args[1])
	if !ok {
		return encodeArray(nil)
	}
	s.mu.Lock()
	members := sortedMembers(z, rev)
	s.mu.Unlock()

	members, ok2 := sliceRange(members, start, stop)
	if !ok2 {
		return encodeArray(nil)
	}
	out := make([]string, 0, len(members)*2)
	for _, m := range members {
		out = append(out, m)
		if withScores {
			out = append(out, formatFloat(z[m]))
		}
	}
	return encodeArray(out)
}

func (s *Server) cmdZRangeByScore(args []string) string {
	if len(args) < 4 {
		return arity("ZRANGEBYSCORE")
	}
	min, err1 := parseScoreBound(args[2])
	max, err2 := parseScoreBound(args[3])
	if err1 != nil || err2 != nil {
		return encodeError("ERR min or max is not a float")
	}
	withScores := false
	for _, a := range args[4:] {
		if strings.EqualFold(a, "WITHSCORES") {
			withScores = true
		}
	}
	z, ok := s.getZSet(args[1])
	if !ok {
		return encodeArray(nil)
	}
	s.mu.Lock()
	members := sortedMembers(z, false)
	s.mu.Unlock()
	out := make([]string, 0, len(members))
	for _, m := range members {
		if z[m] >= min && z[m] <= max {
			out = append(out, m)
			if withScores {
				out = append(out, formatFloat(z[m]))
			}
		}
	}
	return encodeArray(out)
}

func (s *Server) cmdZScore(args []string) string {
	if len(args) != 3 {
		return arity("ZSCORE")
	}
	z, ok := s.getZSet(args[1])
	if !ok {
		return encodeNilBulk()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	score, ok := z[args[2]]
	if !ok {
		return encodeNilBulk()
	}
	return encodeBulk(formatFloat(score))
}

func (s *Server) cmdZRem(args []string) string {
	if len(args) < 3 {
		return arity("ZREM")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	z, ok := s.zsets[args[1]]
	if !ok {
		return encodeInt(0)
	}
	var n int64
	for _, m := range args[2:] {
		if _, ok := z[m]; ok {
			delete(z, m)
			n++
		}
	}
	return encodeInt(n)
}

func (s *Server) cmdZRemRangeByRank(args []string) string {
	if len(args) != 4 {
		return arity("ZREMRANGEBYRANK")
	}
	start, err1 := strconv.Atoi(args[2])
	stop, err2 := strconv.Atoi(args[3])
	if err1 != nil || err2 != nil {
		return encodeError("ERR value is not an integer or out of range")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	z, ok := s.zsets[args[1]]
	if !ok {
		return encodeInt(0)
	}
	members, ok2 := sliceRange(sortedMembers(z, false), start, stop)
	if !ok2 {
		return encodeInt(0)
	}
	for _, m := range members {
		delete(z, m)
	}
	return encodeInt(int64(len(members)))
}

func (s *Server) cmdZRemRangeByScore(args []string) string {
	if len(args) != 4 {
		return arity("ZREMRANGEBYSCORE")
	}
	min, err1 := parseScoreBound(args[2])
	max, err2 := parseScoreBound(args[3])
	if err1 != nil || err2 != nil {
		return encodeError("ERR min or max is not a float")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	z, ok := s.zsets[args[1]]
	if !ok {
		return encodeInt(0)
	}
	var n int64
	for m, score := range z {
		if score >= min && score <= max {
			delete(z, m)
			n++
		}
	}
	return encodeInt(n)
}

func sortedMembers(z map[string]float64, rev bool) []string {
	members := make([]string, 0, len(z))
	for m := range z {
		members = append(members, m)
	}
	sort.Slice(members, func(i, j int) bool {
		if z[members[i]] == z[members[j]] {
			// Redis orders equal scores lexicographically, which makes the
			// ordering deterministic — a cache that depends on "the newest
			// entry" must not get a random answer when two writes share a
			// millisecond.
			return members[i] < members[j]
		}
		if rev {
			return z[members[i]] > z[members[j]]
		}
		return z[members[i]] < z[members[j]]
	})
	return members
}

// sliceRange applies Redis's inclusive-start, inclusive-stop indices with
// negative offsets counted from the end.
func sliceRange(items []string, start, stop int) ([]string, bool) {
	n := len(items)
	if n == 0 {
		return nil, false
	}
	if start < 0 {
		start = n + start
		if start < 0 {
			start = 0
		}
	}
	if stop < 0 {
		stop = n + stop
	}
	if stop >= n {
		stop = n - 1
	}
	if start > stop || start >= n {
		return nil, false
	}
	return items[start : stop+1], true
}

func parseScoreBound(s string) (float64, error) {
	s = strings.TrimPrefix(s, "(")
	return strconv.ParseFloat(s, 64)
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func (s *Server) infoText() string {
	st := s.Stats()
	return fmt.Sprintf("# Server\r\nredis_version:7.0.0-mockredis\r\n# Keyspace\r\ndb0:keys=%d,evictions=%d,expired=%d,commands=%d\r\n",
		st.Keys, st.Evictions, st.Expired, st.Commands)
}

// ---------------------------------------------------------------------------
// RESP2 encoding and decoding (server side)
// ---------------------------------------------------------------------------

func encodeSimple(s string) string { return "+" + s + "\r\n" }

func encodeError(s string) string { return "-" + s + "\r\n" }

func encodeInt(n int64) string { return ":" + strconv.FormatInt(n, 10) + "\r\n" }

func encodeBulk(s string) string {
	return "$" + strconv.Itoa(len(s)) + "\r\n" + s + "\r\n"
}

func encodeNilBulk() string { return "$-1\r\n" }

func encodeArray(items []string) string {
	var b strings.Builder
	b.WriteString("*")
	b.WriteString(strconv.Itoa(len(items)))
	b.WriteString("\r\n")
	for _, it := range items {
		b.WriteString(encodeBulk(it))
	}
	return b.String()
}

// encodeArrayRaw frames elements that are ALREADY RESP-encoded. MGET needs it
// because its reply mixes bulk strings with nils, which encodeArray cannot
// express — it frames every element as a bulk string, so a nil would come back
// as the four characters "$-1".
func encodeArrayRaw(elems []string) string {
	var b strings.Builder
	b.WriteString("*")
	b.WriteString(strconv.Itoa(len(elems)))
	b.WriteString("\r\n")
	for _, e := range elems {
		b.WriteString(e)
	}
	return b.String()
}

func writeError(w *bufio.Writer, msg string) error {
	_, err := w.WriteString(encodeError(msg))
	return err
}

func arity(cmd string) string {
	return encodeError("ERR wrong number of arguments for '" + strings.ToLower(cmd) + "' command")
}

// readCommand parses one client command: a RESP2 array of bulk strings.
func readCommand(r *bufio.Reader) ([]string, error) {
	prefix, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if prefix != '*' {
		return nil, fmt.Errorf("expected an array, got %q", string(prefix))
	}
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("invalid array length %q", line)
	}
	if n > 1<<20 {
		return nil, fmt.Errorf("array of %d elements is too large", n)
	}
	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b != '$' {
			return nil, fmt.Errorf("expected a bulk string, got %q", string(b))
		}
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}
		l, err := strconv.Atoi(line)
		if err != nil || l < 0 {
			return nil, fmt.Errorf("invalid bulk length %q", line)
		}
		if l > 64<<20 {
			return nil, fmt.Errorf("bulk string of %d bytes is too large", l)
		}
		buf := make([]byte, l+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		if buf[l] != '\r' || buf[l+1] != '\n' {
			// The strict check is the point of an independent parser: a client
			// that forgets the trailing CRLF would otherwise be tolerated here
			// and rejected by a real Redis.
			return nil, fmt.Errorf("bulk string is not terminated by CRLF")
		}
		args = append(args, string(buf[:l]))
	}
	return args, nil
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(line, "\r\n") {
		return "", fmt.Errorf("line is not terminated by CRLF")
	}
	return strings.TrimSuffix(line, "\r\n"), nil
}
