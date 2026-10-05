// Package quota enforces per-tenant token, cost and request budgets.
//
// # Reserve, then settle
//
// A gateway cannot know what a request will cost before the provider answers:
// the completion length is the model's choice, and a tool-calling agent can
// emit a hundred tokens or ten thousand from the same prompt. So the budget is
// enforced in two steps, which is the only scheme that both rejects a request
// that has no budget left and charges the request that actually happened:
//
//  1. RESERVE an estimate before the call. The estimate is deliberately
//     pessimistic (the whole body's length plus the requested completion
//     budget), because a limit that is only checked afterwards cannot stop
//     anything - by the time the answer arrives, the money is spent.
//  2. SETTLE the difference after the call, once the provider reported the real
//     token counts. Integer counters mean settle is a single atomic add of
//     (actual - reserved), so a request that came in under its estimate returns
//     budget and one that came in over it pays the difference.
//
// # Why INCRBY and not Lua
//
// Every check-and-increment here is one Redis command. A MULTI/EXEC block would
// be the textbook way to make "read the total, compare, write the total" atomic,
// but this gateway's Redis client pools a connection per call, so a transaction
// would span several connections and be neither atomic nor isolated. A single
// INCRBY is genuinely atomic on a real Redis, and it is enough: the counter
// already contains every in-flight reservation, so a request that cannot afford
// its own estimate sees a total above the limit and rolls its reservation back.
// The visible consequence is that the limit is enforced against ESTIMATED usage
// and can be crossed by a request that underestimated itself - see Overshoot in
// Stats. That is a property of the mechanism, not a bug, and it is reported.
//
// # What a cache hit costs
//
// Nothing. The budget exists to bound what the operator pays a provider, and a
// served cache hit pays nobody. A hit therefore settles its token and cost
// reservation back to zero while keeping its request count. That is also the
// reason admission happens before the cache lookup rather than after it: the
// ordering does not matter to the numbers, but doing it once, in one place,
// keeps the accounting honest for every path through the proxy.
package quota

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Window names. They are part of the counter key, not a duration.
const (
	WindowDay     = "day"
	WindowMinute  = "minute"
	WindowSession = "session"
)

// Counter names, also part of the key. Cost is kept in MICRO-dollars as an
// integer: a float counter cannot be incremented atomically in Redis and would
// accumulate rounding error across millions of INCRBYs.
const (
	CounterTokens   = "tokens"
	CounterCost     = "cost_micros"
	CounterRequests = "requests"
)

// Decision actions.
const (
	// ActionAllow admits the request unchanged.
	ActionAllow = "allow"
	// ActionDegrade admits the request with a cheaper shape applied: a
	// downgraded model, a capped completion budget, or both.
	ActionDegrade = "degrade"
	// ActionReject refuses the request with 429.
	ActionReject = "reject"
)

// Reasons, reported in headers, logs and metrics. They are low-cardinality by
// construction: dimension names and a handful of fixed strings, never a tenant.
const (
	ReasonDisabled      = "disabled"
	ReasonNoStore       = "no-store"
	ReasonWithinBudget  = "within-budget"
	ReasonStoreError    = "store-error"
	ReasonAnomaly       = "anomaly"
	ReasonDailyTokens   = "tokens_per_day"
	ReasonDailyCost     = "cost_per_day_usd"
	ReasonSessionTokens = "tokens_per_session"
	ReasonMinuteRPM     = "requests_per_minute"
)

// Policy is the effective budget for one tenant. A zero limit means "this
// dimension is not budgeted", never "the budget is zero"; there is no way to
// express a tenant that may spend nothing, because that tenant is better
// expressed by not being in the file at all.
type Policy struct {
	Tenant            string
	TokensPerDay      int64
	CostPerDayMicros  int64
	RequestsPerMinute int64
	TokensPerSession  int64
	OnExceed          string
	DowngradeModel    string
	MaxTokensCap      int
	AnomalyRatio      float64
	SessionTTL        time.Duration
}

// Budgeted reports whether any dimension is limited.
//
// It is the single source of truth for "is this tenant bounded at all": there
// is deliberately no separate unbounded flag, because a second answer to the
// same question is a second answer that can be wrong. A policy with every
// dimension at zero short-circuits admission - nothing to reserve against, so
// it costs one map lookup per request and no Redis round trips.
func (p Policy) Budgeted() bool {
	return p.TokensPerDay > 0 || p.CostPerDayMicros > 0 ||
		p.RequestsPerMinute > 0 || p.TokensPerSession > 0
}

// Config configures a Manager. It mirrors the `quota:` config section.
type Config struct {
	// Enabled turns enforcement on. When false every request is admitted with
	// ReasonDisabled and no counter is touched.
	Enabled bool

	// Prefix is the Redis key prefix.
	Prefix string

	// FailOpen admits traffic when the store itself fails. The default is
	// false: a budget that cannot be read is not a budget, and a gateway that
	// silently stops enforcing spend during a Redis blip is a gateway that
	// spends without limit exactly when nobody is watching. FailOpen exists for
	// the deployment where availability outranks cost control, and it is
	// always reported in the response headers.
	FailOpen bool

	// DefaultPolicy applies to a tenant with no entry of its own.
	DefaultPolicy Policy

	// Tenants are the named policies, keyed by tenant.
	Tenants map[string]Policy

	// Now overrides the clock, for tests.
	Now func() time.Time

	// Log receives anomalies and store failures. Nil disables logging.
	Log Logger
}

// Logger is the slice of slog that the manager uses.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// Estimate is what the gateway expects a request to consume. It is reserved
// before the call.
type Estimate struct {
	PromptTokens     int
	CompletionTokens int
	CostMicros       int64
	Requests         int64
}

// Tokens is the total the estimate reserves.
func (e Estimate) Tokens() int64 { return int64(e.PromptTokens + e.CompletionTokens) }

// Usage is what a request actually consumed, as reported by the provider.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	CostMicros       int64
	Requests         int64
}

// Tokens is the total the usage charges.
func (u Usage) Tokens() int64 { return int64(u.PromptTokens + u.CompletionTokens) }

// Decision is the outcome of admission.
type Decision struct {
	// Allowed is false when the request must be refused.
	Allowed bool

	// Action is ActionAllow, ActionDegrade or ActionReject.
	Action string

	// Reason names the dimension that decided it, or a fixed string.
	Reason string

	// Decision is what the caller must tell the client, in the response headers.
	DowngradeModel string
	MaxTokensCap   int

	// Limit, Used and Requested describe the breached dimension, for the error
	// message and the headers. Used is the total AFTER this request's
	// reservation was refused and rolled back, so it is the number the client
	// can compare against the limit.
	Limit     int64
	Used      int64
	Requested int64

	// RetryAfter is how long until the breached window resets. It is set only
	// on a rejection.
	RetryAfter time.Duration

	// Reservation carries the reserved counters. Nil when nothing was reserved
	// (a disabled, unbounded or rejected request).
	Reservation *Reservation
}

// Reservation is a set of reserved counters, settled or released once.
//
// It is not safe for concurrent use, and it does not need to be: it belongs to
// exactly one request.
type Reservation struct {
	Tenant  string
	Session string
	entries []entry
	settled bool
}

// entry is one reserved counter.
type entry struct {
	key      string
	window   string
	bucket   string
	counter  string
	ttl      time.Duration
	reserved int64
}

// Stats is the manager's running tally, for /admin/quota and the metrics
// endpoint.
type Stats struct {
	Allowed     uint64
	Degraded    uint64
	Rejected    uint64
	StoreErrors uint64
	Alerts      uint64
	// OvershootTokens counts tokens settled above the reserved estimate. It is
	// the honest measure of how much the limit is exceeded by, and it is the
	// number that tells an operator to raise the completion estimate.
	OvershootTokens    uint64
	OvershootCostMicro int64
	// ReservedTokens is what admission pre-charged, SettledTokens what the
	// provider actually reported, ReleasedTokens the part of the reservation
	// that came back. They are three views of one ledger rather than three
	// independent counters, so reserved - released + overshoot == settled.
	//
	// All three are counted per reserved ENTRY, so a tenant metered on both a
	// daily and a per-session token budget contributes its tokens twice - once
	// per budget it consumed. The identity is what makes that readable: the
	// numbers describe budget consumed, not requests admitted.
	ReservedTokens uint64
	SettledTokens  uint64
	ReleasedTokens uint64
	// ReleasedCostMicro is the money side of ReleasedTokens. It is kept
	// separate for the same reason the two are separate in the store: summing
	// micro-dollars into a token count produced a "released tokens" figure that
	// had nothing to do with tokens.
	ReleasedCostMicro int64
}

// Report is a tenant's current standing, for /admin/quota.
type Report struct {
	Tenant string
	Policy Policy

	Day            string
	TokensToday    int64
	CostTodayMicro int64

	Minute             string
	RequestsThisMinute int64

	Session       string
	SessionTokens int64

	// BaselineTokens is the mean of the previous days that had traffic, and
	// Ratio is today against it. Alerting is true when today crossed the
	// policy's anomaly ratio.
	BaselineTokens float64
	Ratio          float64
	Alerting       bool
}

// Manager enforces policies against a Store. The zero value is not usable;
// call New.
type Manager struct {
	cfg   Config
	store Store
	now   func() time.Time
	log   Logger

	stats atomics

	mu        sync.Mutex
	anomalies map[string]*anomalyState
}

// atomics holds the counters that every request touches, so that the hot path
// never takes the manager's mutex.
type atomics struct {
	allowed       atomic.Uint64
	degraded      atomic.Uint64
	rejected      atomic.Uint64
	storeErrors   atomic.Uint64
	alerts        atomic.Uint64
	overshoot     atomic.Uint64
	overshootCost atomic.Int64
	reservedTok   atomic.Uint64
	settledTok    atomic.Uint64
	releasedTok   atomic.Uint64
	releasedCost  atomic.Int64
}

// New builds a Manager. A nil store with Enabled true is a configuration
// mistake: it is reported by Admit as a store error rather than panicking,
// because a gateway that panics on one request serves none.
func New(cfg Config, store Store) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "ig:quota"
	}
	return &Manager{
		cfg:       cfg,
		store:     store,
		now:       cfg.Now,
		log:       cfg.Log,
		anomalies: make(map[string]*anomalyState),
	}
}

// Enabled reports whether the manager enforces anything.
func (m *Manager) Enabled() bool { return m.cfg.Enabled }

// Policy returns the effective policy for a tenant.
func (m *Manager) Policy(tenant string) Policy {
	if p, ok := m.cfg.Tenants[tenant]; ok {
		return p
	}
	return m.cfg.DefaultPolicy
}

// Admit reserves the estimate against every limited dimension and reports
// whether the request may proceed.
//
// The reservation is all-or-nothing: if any dimension refuses, every counter
// already reserved is given back before returning. A partial reservation would
// charge a rejected request for the dimensions that happened to be checked
// first, and the lease would leak - nothing would ever settle it.
func (m *Manager) Admit(ctx context.Context, tenant, session string, est Estimate) (*Decision, error) {
	if !m.cfg.Enabled {
		return &Decision{Allowed: true, Action: ActionAllow, Reason: ReasonDisabled}, nil
	}
	policy := m.Policy(tenant)
	if !policy.Budgeted() {
		m.stats.allowed.Add(1)
		return &Decision{Allowed: true, Action: ActionAllow, Reason: ReasonWithinBudget}, nil
	}
	if m.store == nil {
		return m.unavailable(errors.New("quota: no store configured"))
	}

	now := m.now().UTC()
	checks := m.checks(policy, tenant, session, now, est)

	// Reserve every dimension, remembering the FIRST one that goes over. A
	// degrade policy keeps reserving after a breach (a degraded request is still
	// a request, and it still settles), while a reject policy stops there.
	degrade := policy.OnExceed == ActionDegrade &&
		(policy.DowngradeModel != "" || policy.MaxTokensCap > 0)

	res := &Reservation{Tenant: tenant, Session: session}
	var breach *check
	var breachTotal int64
	for _, c := range checks {
		total, err := m.store.Add(ctx, c.key, c.delta, c.ttl)
		if err != nil {
			// Whatever was reserved before this failure is still ours; give it
			// back before deciding, so a store blip cannot leak budget.
			m.releaseEntries(ctx, res.entries)
			res.settled = true
			return m.unavailable(fmt.Errorf("quota: reserve %s: %w", c.dimension, err))
		}
		res.entries = append(res.entries, entry{
			key: c.key, window: c.window, bucket: c.bucket, counter: c.counter,
			ttl: c.ttl, reserved: c.delta,
		})
		// The token ledger is kept per reserved ENTRY, not per request. A tenant
		// can be metered on two token dimensions (day and session), and counting
		// the request once here while settling its entries one by one made
		// "reserved - released + overshoot == settled" false for exactly those
		// tenants - the ones whose accounting is hardest to reason about.
		// Crediting each entry as it is written keeps the identity per entry, so
		// it also holds for the sum.
		if c.counter == CounterTokens {
			m.stats.reservedTok.Add(uint64(c.delta))
		}
		if total > c.limit && breach == nil {
			breached := c
			breach = &breached
			breachTotal = total
			if !degrade {
				break
			}
		}
	}

	if breach != nil {
		// breachTotal includes this request's delta; the tenant's standing is
		// what it had before, and reporting the inflated number would make the
		// error message contradict itself.
		used := breachTotal - breach.delta
		if degrade {
			// The ladder's whole point: keep serving, but smaller. The
			// reservation stays as it was, so settlement returns whatever the
			// cheaper shape did not use.
			m.stats.degraded.Add(1)
			return &Decision{
				Allowed:        true,
				Action:         ActionDegrade,
				Reason:         breach.dimension,
				DowngradeModel: policy.DowngradeModel,
				MaxTokensCap:   policy.MaxTokensCap,
				Limit:          breach.limit,
				Used:           used,
				Requested:      breach.delta,
				Reservation:    res,
			}, nil
		}
		m.releaseEntries(ctx, res.entries)
		res.settled = true
		m.stats.rejected.Add(1)
		return &Decision{
			Allowed:    false,
			Action:     ActionReject,
			Reason:     breach.dimension,
			Limit:      breach.limit,
			Used:       used,
			Requested:  breach.delta,
			RetryAfter: breach.retryAfter,
		}, nil
	}
	m.stats.allowed.Add(1)
	return &Decision{Allowed: true, Action: ActionAllow, Reason: ReasonWithinBudget, Reservation: res}, nil
}

// Settle charges the real usage and returns whatever the estimate over-reserved.
//
// Settling an already settled or released reservation is a no-op rather than an
// error: the proxy settles from a deferred block, which can run on a path that
// already released, and a double charge would be worse than a missed one.
func (m *Manager) Settle(ctx context.Context, res *Reservation, usage Usage) error {
	if res == nil || res.settled {
		return nil
	}
	res.settled = true
	if m.store == nil {
		return nil
	}
	var firstErr error
	for _, e := range res.entries {
		actual := actualFor(e.counter, usage)
		diff := actual - e.reserved
		if _, err := m.store.Add(ctx, e.key, diff, e.ttl); err != nil {
			m.stats.storeErrors.Add(1)
			m.warn("quota: settle failed", "tenant", res.Tenant, "counter", e.counter, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if diff > 0 {
			// The request outgrew its estimate. The counter now sits above the
			// limit, so the next request from this tenant is the one that gets
			// refused - which is the correct direction to be wrong in.
			switch e.counter {
			case CounterTokens:
				m.stats.overshoot.Add(uint64(diff))
			case CounterCost:
				m.stats.overshootCost.Add(diff)
			}
		}
		// The audit counters are split by DIMENSION, not just by sign: a cost
		// entry's credit is micro-dollars, and a live run showed the two being
		// summed into one "released tokens" number (1014 tokens released for a
		// request that reserved 279), which is a unit error that reads as a
		// budget anomaly. Tokens are counted where tokens live, money where
		// money lives, and the two never share a counter.
		switch e.counter {
		case CounterTokens:
			// Settled is what was really charged, so reserved - released +
			// overshoot == settled holds for every reservation, and "settled" is
			// no longer a second name for "overshoot".
			if actual < 0 {
				actual = 0
			}
			m.stats.settledTok.Add(uint64(actual))
			if diff < 0 {
				m.stats.releasedTok.Add(uint64(-diff))
			}
		case CounterCost:
			if diff < 0 {
				m.stats.releasedCost.Add(-diff)
			}
		}
	}
	m.checkAnomaly(ctx, res.Tenant, usage)
	return firstErr
}

// Release gives every reserved counter back, including the request count. It is
// what a request that never reached a provider settles with.
//
// The refund is credited per entry and per dimension by releaseEntries: a
// reservation can span tokens AND money, and crediting only the token side left
// a refunded request's spend rolled back in the store but missing from the audit
// counters.
func (m *Manager) Release(ctx context.Context, res *Reservation) error {
	if res == nil || res.settled {
		return nil
	}
	res.settled = true
	if m.store == nil {
		return nil
	}
	return m.releaseEntries(ctx, res.entries)
}

// releaseEntries is the shared rollback. It reports the first error but always
// tries every entry: leaving one counter charged is a permanent leak.
func (m *Manager) releaseEntries(ctx context.Context, entries []entry) error {
	var firstErr error
	for _, e := range entries {
		if _, err := m.store.Add(ctx, e.key, -e.reserved, e.ttl); err != nil {
			m.stats.storeErrors.Add(1)
			m.warn("quota: release failed", "counter", e.counter, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// Credited only once the store really gave the amount back: a counter
		// that is still charged must never be reported as released.
		if e.reserved <= 0 {
			continue
		}
		switch e.counter {
		case CounterTokens:
			m.stats.releasedTok.Add(uint64(e.reserved))
		case CounterCost:
			m.stats.releasedCost.Add(e.reserved)
		}
	}
	return firstErr
}

// unavailable applies the fail-open policy to a store failure.
//
// The returned error is always non-nil: the caller must log it either way. What
// differs is whether the request is admitted.
func (m *Manager) unavailable(err error) (*Decision, error) {
	m.stats.storeErrors.Add(1)
	m.warn("quota: store unavailable", "err", err, "fail_open", m.cfg.FailOpen)
	if m.cfg.FailOpen {
		// A fail-open admission is still an admission. Leaving it out of the
		// decision counters made allowed+degraded+rejected undercount the
		// requests the gateway actually served, exactly when the store was down
		// and the numbers were being watched.
		m.stats.allowed.Add(1)
		return &Decision{Allowed: true, Action: ActionAllow, Reason: ReasonStoreError}, err
	}
	m.stats.rejected.Add(1)
	return &Decision{Allowed: false, Action: ActionReject, Reason: ReasonStoreError}, err
}

// Stats returns the running tally.
func (m *Manager) Stats() Stats {
	return Stats{
		Allowed:            m.stats.allowed.Load(),
		Degraded:           m.stats.degraded.Load(),
		Rejected:           m.stats.rejected.Load(),
		StoreErrors:        m.stats.storeErrors.Load(),
		Alerts:             m.stats.alerts.Load(),
		OvershootTokens:    m.stats.overshoot.Load(),
		OvershootCostMicro: m.stats.overshootCost.Load(),
		ReservedTokens:     m.stats.reservedTok.Load(),
		SettledTokens:      m.stats.settledTok.Load(),
		ReleasedTokens:     m.stats.releasedTok.Load(),
		ReleasedCostMicro:  m.stats.releasedCost.Load(),
	}
}

// Report reads a tenant's counters. It is for the admin endpoint, so it costs
// the round trips that the request path avoids.
func (m *Manager) Report(ctx context.Context, tenant, session string) (Report, error) {
	policy := m.Policy(tenant)
	now := m.now().UTC()
	rep := Report{
		Tenant:  tenant,
		Policy:  policy,
		Day:     dayBucket(now),
		Minute:  minuteBucket(now),
		Session: session,
	}
	if m.store == nil {
		return rep, nil
	}
	var err error
	if rep.TokensToday, err = m.store.Get(ctx, m.key(tenant, WindowDay, rep.Day, CounterTokens)); err != nil {
		return rep, err
	}
	if rep.CostTodayMicro, err = m.store.Get(ctx, m.key(tenant, WindowDay, rep.Day, CounterCost)); err != nil {
		return rep, err
	}
	if rep.RequestsThisMinute, err = m.store.Get(ctx, m.key(tenant, WindowMinute, rep.Minute, CounterRequests)); err != nil {
		return rep, err
	}
	if session != "" {
		if rep.SessionTokens, err = m.store.Get(ctx, m.key(tenant, WindowSession, session, CounterTokens)); err != nil {
			return rep, err
		}
	}
	baseline, err := m.baseline(ctx, tenant, now, policy)
	if err != nil {
		return rep, err
	}
	rep.BaselineTokens = baseline
	if baseline > 0 {
		rep.Ratio = float64(rep.TokensToday) / baseline
		rep.Alerting = policy.AnomalyRatio > 0 && rep.Ratio >= policy.AnomalyRatio
	}
	return rep, nil
}

// Close releases the store.
func (m *Manager) Close() error {
	if m.store == nil {
		return nil
	}
	return m.store.Close()
}

// check is one dimension's reservation attempt.
type check struct {
	dimension  string
	key        string
	window     string
	bucket     string
	counter    string
	ttl        time.Duration
	delta      int64
	limit      int64
	retryAfter time.Duration
}

// checks builds the reservation list for a policy.
//
// Every limited dimension is reserved, including the ones the request is
// nowhere near: skipping a dimension because its current total is low would
// mean reading it first, and the read-then-write is exactly the race the single
// INCRBY avoids.
func (m *Manager) checks(p Policy, tenant, session string, now time.Time, est Estimate) []check {
	var out []check
	day := dayBucket(now)
	dayTTL := untilDayEnd(now)
	if p.TokensPerDay > 0 {
		out = append(out, check{
			dimension:  ReasonDailyTokens,
			key:        m.key(tenant, WindowDay, day, CounterTokens),
			window:     WindowDay,
			bucket:     day,
			counter:    CounterTokens,
			ttl:        dayTTL,
			delta:      est.Tokens(),
			limit:      p.TokensPerDay,
			retryAfter: dayTTL,
		})
	}
	if p.CostPerDayMicros > 0 {
		out = append(out, check{
			dimension:  ReasonDailyCost,
			key:        m.key(tenant, WindowDay, day, CounterCost),
			window:     WindowDay,
			bucket:     day,
			counter:    CounterCost,
			ttl:        dayTTL,
			delta:      est.CostMicros,
			limit:      p.CostPerDayMicros,
			retryAfter: dayTTL,
		})
	}
	if p.RequestsPerMinute > 0 {
		minute := minuteBucket(now)
		out = append(out, check{
			dimension:  ReasonMinuteRPM,
			key:        m.key(tenant, WindowMinute, minute, CounterRequests),
			window:     WindowMinute,
			bucket:     minute,
			counter:    CounterRequests,
			ttl:        minuteTTL,
			delta:      max64(est.Requests, 1),
			limit:      p.RequestsPerMinute,
			retryAfter: time.Minute - time.Duration(now.Second())*time.Second,
		})
	}
	if p.TokensPerSession > 0 && session != "" {
		ttl := p.SessionTTL
		if ttl <= 0 {
			ttl = DefaultSessionTTL
		}
		out = append(out, check{
			dimension:  ReasonSessionTokens,
			key:        m.key(tenant, WindowSession, session, CounterTokens),
			window:     WindowSession,
			bucket:     session,
			counter:    CounterTokens,
			ttl:        ttl,
			delta:      est.Tokens(),
			limit:      p.TokensPerSession,
			retryAfter: ttl,
		})
	}
	return out
}

// key builds a counter key. The tenant and the session id are sanitised so a
// caller cannot use a crafted value to write outside its own namespace: the
// tenant header is client-controlled, and "acme:day:20261005:tokens" as a
// tenant name would otherwise let one caller charge another's budget.
func (m *Manager) key(tenant, window, bucket, counter string) string {
	return strings.Join([]string{
		m.cfg.Prefix,
		sanitise(tenant),
		window,
		sanitise(bucket),
		counter,
	}, ":")
}

// sanitise encodes every character that could act as a separator or confuse a
// Redis key, so that two different names can never share a counter.
//
// The encoding is injective, which is the property that matters here: the tenant
// name arrives in a request header, so a caller controls it, and a many-to-one
// mapping ("acme:inc" and "acme_inc" collapsing to one key) would let a crafted
// name charge another tenant's budget. A tenant configured under an odd name is
// therefore its own tenant rather than a second name for someone else.
//
// Safe characters pass through, "_" doubles ("__"), and anything else becomes
// "_x" plus exactly six hex digits. The fixed width is what makes the encoding
// unambiguous: without it, rune U+10FFF followed by "ff" and rune U+10FFFF
// followed by "f" would both encode to "_x10fffff".
func sanitise(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '.':
			b.WriteRune(r)
		case r == '_':
			b.WriteString("__")
		default:
			fmt.Fprintf(&b, "_x%06x", r)
		}
	}
	return b.String()
}

// DefaultSessionTTL bounds a session counter. It is also the upper bound on how
// long an abandoned session keeps its budget, so it is a day rather than an hour:
// a long agent run should not have its budget reset mid-run.
const DefaultSessionTTL = 24 * time.Hour

// minuteTTL is the life of a minute bucket. It has to outlive the bucket: a key
// created at 10:00:59 is still relevant until 10:01:00.
const minuteTTL = 2 * time.Minute

func dayBucket(t time.Time) string    { return t.Format("20060102") }
func minuteBucket(t time.Time) string { return t.Format("200601021504") }

// untilDayEnd is how long a day bucket must live: until just past the next UTC
// midnight. An hour of slack means a replica whose clock is behind still reads
// the bucket it wrote through the end of the day.
//
// The caller's instant is converted to UTC first. Reading the calendar date off
// a local time would build the boundary from the wrong day for any caller that
// passed a zoned time, which is a silent off-by-one-day in a budget window.
func untilDayEnd(t time.Time) time.Duration {
	utc := t.UTC()
	next := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	d := next.Sub(utc)
	if d < time.Minute {
		return time.Minute
	}
	return d + time.Hour
}

// actualFor maps a counter to its settled value.
func actualFor(counter string, u Usage) int64 {
	switch counter {
	case CounterTokens:
		return u.Tokens()
	case CounterCost:
		return u.CostMicros
	case CounterRequests:
		// The request happened; settlement never changes the count.
		return max64(u.Requests, 1)
	}
	return 0
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// anomalyState remembers the last anomaly evaluation for a tenant so that the
// baseline reads happen at most once a minute per tenant, and so that an alert
// fires on the transition rather than on every request.
type anomalyState struct {
	day      string
	baseline float64
	ratio    float64
	alerting bool
	checked  time.Time
}

// anomalyCheckInterval throttles the baseline reads. They are three GETs per
// tenant, which is nothing at the admin endpoint and everything on the request
// path.
const anomalyCheckInterval = time.Minute

// anomalyBaselineDays is how many previous days the baseline averages.
const anomalyBaselineDays = 3

// baseline returns the mean daily tokens of the previous days that had traffic.
//
// Days with no traffic are skipped rather than counted as zero: a tenant that
// was quiet last Tuesday has not "averaged" down to half its usual spend, and
// averaging in zeroes would make the first busy Monday look like an anomaly.
func (m *Manager) baseline(ctx context.Context, tenant string, now time.Time, p Policy) (float64, error) {
	if p.AnomalyRatio <= 0 {
		return 0, nil
	}
	var sum, days float64
	for i := 1; i <= anomalyBaselineDays; i++ {
		bucket := dayBucket(now.AddDate(0, 0, -i))
		v, err := m.store.Get(ctx, m.key(tenant, WindowDay, bucket, CounterTokens))
		if err != nil {
			return 0, err
		}
		if v > 0 {
			sum += float64(v)
			days++
		}
	}
	if days == 0 {
		return 0, nil
	}
	return sum / days, nil
}

// checkAnomaly evaluates the anomaly ratio after a settle and logs the
// transition into and out of an alert.
//
// This is alerting only: nothing here refuses a request. A spend spike is
// usually a real workload doing something new, and a gateway that starts
// rejecting traffic because it looked unusual is a gateway that turns a
// customer's launch into an outage.
func (m *Manager) checkAnomaly(ctx context.Context, tenant string, u Usage) {
	policy := m.Policy(tenant)
	if policy.AnomalyRatio <= 0 || m.store == nil {
		return
	}
	now := m.now().UTC()
	day := dayBucket(now)

	m.mu.Lock()
	st := m.anomalies[tenant]
	if st == nil {
		st = &anomalyState{}
		m.anomalies[tenant] = st
	}
	if st.day == day && now.Sub(st.checked) < anomalyCheckInterval {
		m.mu.Unlock()
		return
	}
	st.day = day
	st.checked = now
	m.mu.Unlock()

	today, err := m.store.Get(ctx, m.key(tenant, WindowDay, day, CounterTokens))
	if err != nil {
		m.stats.storeErrors.Add(1)
		m.warn("quota: anomaly read failed", "tenant", tenant, "err", err)
		return
	}
	base, err := m.baseline(ctx, tenant, now, policy)
	if err != nil {
		m.stats.storeErrors.Add(1)
		m.warn("quota: anomaly baseline failed", "tenant", tenant, "err", err)
		return
	}

	ratio := 0.0
	if base > 0 {
		ratio = float64(today) / base
	}
	alerting := base > 0 && ratio >= policy.AnomalyRatio

	m.mu.Lock()
	was := st.alerting
	st.baseline, st.ratio, st.alerting = base, ratio, alerting
	m.mu.Unlock()

	switch {
	case alerting && !was:
		m.stats.alerts.Add(1)
		if m.log != nil {
			m.log.Warn("quota: spend anomaly",
				"tenant", tenant,
				"tokens_today", today,
				"baseline_tokens", base,
				"ratio", ratio,
				"threshold", policy.AnomalyRatio,
			)
		}
	case !alerting && was:
		if m.log != nil {
			m.log.Info("quota: spend back to normal",
				"tenant", tenant, "tokens_today", today, "baseline_tokens", base, "ratio", ratio)
		}
	}
}

func (m *Manager) warn(msg string, args ...any) {
	if m.log != nil {
		m.log.Warn(msg, args...)
	}
}
