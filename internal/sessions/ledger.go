// Package sessions keeps a per-conversation ledger of what the gateway served.
//
// The unit is the caller's X-InferGate-Session, not the process and not the
// upstream: an agent conversation is many requests against several providers,
// and the questions that matter in an agent deployment ("what did this
// conversation cost?", "which model did it actually use?", "did the retry get
// replayed or did we pay twice?") are all answered per conversation, across
// providers. A tenant that never sends a session id still gets an entry under
// the empty tenant so the ledger never silently drops a request.
//
// The ledger is deliberately lossy: it holds the most recent sessions in a true
// LRU bounded by Capacity, drops them after TTL, and keeps only the last
// RecentPerSession requests per session. Those requests are evidence for
// troubleshooting, not an audit log - the durable record is the request log and
// the trace store.
package sessions

import (
	"container/list"
	"sort"
	"strings"
	"sync"
	"time"
)

// Request is one accounted request inside a session.
type Request struct {
	At        time.Time `json:"at"`
	RequestID string    `json:"request_id,omitempty"`
	Route     string    `json:"route,omitempty"`
	Upstream  string    `json:"upstream,omitempty"`
	// Model is the model that actually ran the request. It is the key of the
	// session's per-model rollup and of the cost calculation, because a
	// provider reports the snapshot it served and an alias may resolve to a
	// differently priced one.
	Model string `json:"model,omitempty"`
	// RequestedModel is what the caller asked for, kept because it can differ
	// from Model: a tiered route rewrites the name for a local box, and a
	// provider echoes its own snapshot. Without it, "the conversation used
	// four models" and "the caller asked for one" are indistinguishable.
	RequestedModel   string    `json:"requested_model,omitempty"`
	Status           int       `json:"status"`
	Outcome          string    `json:"outcome,omitempty"`
	Stream           bool      `json:"stream,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CachedTokens     int       `json:"cached_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	ElapsedMS        float64   `json:"elapsed_ms,omitempty"`
	FirstTokenMS     float64   `json:"first_token_ms,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	Tried            []string  `json:"tried,omitempty"`
	Cache            string    `json:"cache,omitempty"`
	Replay           bool      `json:"idempotent_replay,omitempty"`
	Reason           string    `json:"reason,omitempty"`
}

// ModelTotals aggregates one model inside a session.
type ModelTotals struct {
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Session is the per-conversation rollup plus a bounded window of its recent
// requests.
type Session struct {
	Tenant           string                 `json:"tenant,omitempty"`
	ID               string                 `json:"id"`
	FirstSeen        time.Time              `json:"first_seen"`
	LastSeen         time.Time              `json:"last_seen"`
	Requests         int64                  `json:"requests"`
	Ok               int64                  `json:"ok"`
	Failed           int64                  `json:"failed"`
	Replays          int64                  `json:"idempotent_replays"`
	CacheHits        int64                  `json:"cache_hits"`
	PromptTokens     int64                  `json:"prompt_tokens"`
	CompletionTokens int64                  `json:"completion_tokens"`
	CachedTokens     int64                  `json:"cached_tokens"`
	CostUSD          float64                `json:"cost_usd"`
	Models           map[string]ModelTotals `json:"models,omitempty"`
	Upstreams        map[string]int64       `json:"upstreams,omitempty"`
	Recent           []Request              `json:"recent"`
	RecentDropped    int64                  `json:"recent_dropped"`
	ExpiresAt        time.Time              `json:"expires_at"`

	// key is the composite LRU key, kept on the value so eviction does not
	// have to search the map for the element it just took off the list.
	key string
}

// TenantTotal is a tenant rolled up across its sessions.
type TenantTotal struct {
	Tenant           string  `json:"tenant,omitempty"`
	Sessions         int     `json:"sessions"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Stats are the counters /admin/sessions and /metrics report.
type Stats struct {
	// Recorded counts every accounted request, including ones dropped from a
	// session's Recent window.
	Recorded      uint64 `json:"recorded"`
	NewSessions   uint64 `json:"new_sessions"`
	NoSessionID   uint64 `json:"no_session_id"`
	RecentDropped uint64 `json:"recent_dropped"`
	Evicted       uint64 `json:"evicted"`
	Expired       uint64 `json:"expired"`
	Flushes       uint64 `json:"flushes"`
}

// Ledger is the in-process session ledger. It mirrors internal/cache's memory
// store (true LRU, a janitor for footprint rather than correctness) because the
// access pattern is a hot working set of live conversations and a long tail of
// finished ones.
type Ledger struct {
	mu       sync.Mutex
	sessions map[string]*list.Element // composite key -> element holding *Session
	lru      *list.List               // front = most recently active

	capacity         int
	ttl              time.Duration
	recentPerSession int
	now              func() time.Time
	stats            Stats

	closeCh chan struct{}
	once    sync.Once
}

// Options configures a Ledger.
type Options struct {
	// Capacity is the LRU bound over sessions. Zero means DefaultCapacity.
	Capacity int
	// TTL is how long a session stays in the ledger after its last request.
	// Zero means DefaultTTL.
	TTL time.Duration
	// RecentPerSession bounds the per-session request window. Zero means
	// DefaultRecentPerSession.
	RecentPerSession int
	// Now overrides the clock, for tests.
	Now func() time.Time
	// SweepInterval controls the janitor. Zero means one minute; a negative
	// value disables it.
	SweepInterval time.Duration
}

// Defaults for Options.
const (
	DefaultCapacity         = 4096
	DefaultTTL              = 24 * time.Hour
	DefaultRecentPerSession = 8
)

// New builds a ledger and starts its janitor.
func New(opts Options) *Ledger {
	l := &Ledger{
		sessions:         make(map[string]*list.Element),
		lru:              list.New(),
		capacity:         opts.Capacity,
		ttl:              opts.TTL,
		recentPerSession: opts.RecentPerSession,
		now:              opts.Now,
		closeCh:          make(chan struct{}),
	}
	if l.capacity <= 0 {
		l.capacity = DefaultCapacity
	}
	if l.ttl <= 0 {
		l.ttl = DefaultTTL
	}
	if l.recentPerSession <= 0 {
		l.recentPerSession = DefaultRecentPerSession
	}
	if l.now == nil {
		l.now = time.Now
	}
	interval := opts.SweepInterval
	if interval == 0 {
		interval = time.Minute
	}
	if interval > 0 {
		go l.janitor(interval)
	}
	return l
}

func (l *Ledger) janitor(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-l.closeCh:
			return
		case <-t.C:
			l.sweep()
		}
	}
}

func (l *Ledger) sweep() {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, el := range l.sessions {
		s := el.Value.(*Session)
		if !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt) {
			l.remove(el)
			l.stats.Expired++
		}
	}
}

// keyOf builds the composite key. A NUL separator cannot appear in an HTTP
// header value, so "acme" + "x" and "acmex" + "" cannot collide.
func keyOf(tenant, id string) string { return tenant + "\x00" + id }

func (l *Ledger) remove(el *list.Element) {
	s := el.Value.(*Session)
	l.lru.Remove(el)
	delete(l.sessions, s.key)
}

// Record accounts one request against (tenant, id).
//
// The caller passes the session id as it arrived; an empty id is counted in
// NoSessionID and deliberately NOT turned into a synthetic session, because a
// ledger full of one-request pseudo-sessions would hide the fact that the
// caller never sent the header.
func (l *Ledger) Record(tenant, id string, req Request) {
	if strings.TrimSpace(id) == "" {
		l.mu.Lock()
		l.stats.NoSessionID++
		l.mu.Unlock()
		return
	}
	now := l.now()
	if req.At.IsZero() {
		req.At = now
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.stats.Recorded++
	key := keyOf(tenant, id)
	el, ok := l.sessions[key]
	var s *Session
	if ok {
		s = el.Value.(*Session)
	} else {
		s = &Session{Tenant: tenant, ID: id, FirstSeen: req.At, key: key}
		el = l.lru.PushFront(s)
		l.sessions[key] = el
		l.stats.NewSessions++
	}
	l.lru.MoveToFront(el)

	s.LastSeen = req.At
	s.Requests++
	if req.Status >= 400 {
		s.Failed++
	} else {
		s.Ok++
	}
	if req.Replay {
		s.Replays++
	}
	if strings.HasPrefix(req.Cache, "hit") {
		s.CacheHits++
	}
	s.PromptTokens += int64(req.PromptTokens)
	s.CompletionTokens += int64(req.CompletionTokens)
	s.CachedTokens += int64(req.CachedTokens)
	s.CostUSD += req.CostUSD
	if req.Model != "" {
		if s.Models == nil {
			s.Models = make(map[string]ModelTotals, 2)
		}
		mt := s.Models[req.Model]
		mt.Requests++
		mt.PromptTokens += int64(req.PromptTokens)
		mt.CompletionTokens += int64(req.CompletionTokens)
		mt.CostUSD += req.CostUSD
		s.Models[req.Model] = mt
	}
	if req.Upstream != "" {
		if s.Upstreams == nil {
			s.Upstreams = make(map[string]int64, 2)
		}
		s.Upstreams[req.Upstream]++
	}
	s.Recent = append(s.Recent, req)
	if len(s.Recent) > l.recentPerSession {
		drop := len(s.Recent) - l.recentPerSession
		s.Recent = append([]Request(nil), s.Recent[drop:]...)
		s.RecentDropped += int64(drop)
		l.stats.RecentDropped += uint64(drop)
	}
	s.ExpiresAt = now.Add(l.ttl)

	for len(l.sessions) > l.capacity {
		back := l.lru.Back()
		if back == nil {
			break
		}
		l.remove(back)
		l.stats.Evicted++
	}
}

// Get returns one session.
func (l *Ledger) Get(tenant, id string) (Session, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.sessions[keyOf(tenant, id)]
	if !ok {
		return Session{}, false
	}
	return clone(el.Value.(*Session)), true
}

// Find returns every session with this id, most recently active first. A bare
// session id is ambiguous when two tenants used the same one, and the caller
// (the admin endpoint) is the one that can ask for a tenant disambiguator.
func (l *Ledger) Find(id string) []Session {
	l.mu.Lock()
	out := make([]Session, 0, 2)
	for _, el := range l.sessions {
		s := el.Value.(*Session)
		if s.ID == id {
			out = append(out, clone(s))
		}
	}
	l.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

// List returns sessions, most recently active first, capped at limit.
func (l *Ledger) List(limit int) []Session {
	l.mu.Lock()
	out := make([]Session, 0, l.lru.Len())
	for _, el := range l.sessions {
		out = append(out, clone(el.Value.(*Session)))
	}
	l.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen.Equal(out[j].LastSeen) {
			if out[i].Tenant == out[j].Tenant {
				return out[i].ID < out[j].ID
			}
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Tenants rolls every live session up per tenant, biggest spender first.
func (l *Ledger) Tenants() []TenantTotal {
	l.mu.Lock()
	byTenant := make(map[string]*TenantTotal, 4)
	for _, el := range l.sessions {
		s := el.Value.(*Session)
		tt, ok := byTenant[s.Tenant]
		if !ok {
			tt = &TenantTotal{Tenant: s.Tenant}
			byTenant[s.Tenant] = tt
		}
		tt.Sessions++
		tt.Requests += s.Requests
		tt.PromptTokens += s.PromptTokens
		tt.CompletionTokens += s.CompletionTokens
		tt.CostUSD += s.CostUSD
	}
	l.mu.Unlock()
	out := make([]TenantTotal, 0, len(byTenant))
	for _, tt := range byTenant {
		out = append(out, *tt)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostUSD == out[j].CostUSD {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].CostUSD > out[j].CostUSD
	})
	return out
}

// Flush drops every session and returns how many were held.
func (l *Ledger) Flush() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.sessions)
	l.sessions = make(map[string]*list.Element)
	l.lru = list.New()
	l.stats.Flushes++
	return n
}

// Len counts live sessions.
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.sessions)
}

// Stats returns the counters.
func (l *Ledger) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats
}

// Capacity, TTL and RecentPerSession expose the effective bounds.
func (l *Ledger) Capacity() int         { return l.capacity }
func (l *Ledger) TTL() time.Duration    { return l.ttl }
func (l *Ledger) RecentPerSession() int { return l.recentPerSession }

// Close stops the janitor. It is idempotent.
func (l *Ledger) Close() error {
	l.once.Do(func() { close(l.closeCh) })
	return nil
}

func clone(s *Session) Session {
	out := *s
	if s.Models != nil {
		out.Models = make(map[string]ModelTotals, len(s.Models))
		for k, v := range s.Models {
			out.Models[k] = v
		}
	}
	if s.Upstreams != nil {
		out.Upstreams = make(map[string]int64, len(s.Upstreams))
		for k, v := range s.Upstreams {
			out.Upstreams[k] = v
		}
	}
	if s.Recent != nil {
		out.Recent = make([]Request, len(s.Recent))
		for i, r := range s.Recent {
			if r.Tried != nil {
				r.Tried = append([]string(nil), r.Tried...)
			}
			out.Recent[i] = r
		}
	}
	return out
}
