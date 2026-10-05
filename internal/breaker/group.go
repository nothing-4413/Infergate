package breaker

import (
	"sort"
	"sync"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/stats"
)

// Group owns one breaker and one shared window per upstream, created together
// so the router, the breaker and the admin endpoint always read the same
// numbers. Keeping them in one structure also removes the classic bug where a
// reload builds new breakers but leaves the router reading the old windows.
type Group struct {
	cfg config.HealthConfig

	mu       sync.RWMutex
	breakers map[string]*Breaker
	order    []string
}

// NewGroup builds a group from the configured upstream names.
func NewGroup(cfg config.HealthConfig, names []string) *Group {
	g := &Group{cfg: cfg, breakers: make(map[string]*Breaker, len(names))}
	for _, name := range names {
		window := stats.New(cfg.Window.Duration(), cfg.Buckets)
		g.breakers[name] = New(name, cfg, window)
		g.order = append(g.order, name)
	}
	return g
}

// Get returns the breaker for an upstream. It never returns nil: an unknown
// name yields a fresh closed breaker, because a missing breaker must not be
// able to panic the request path or silently reject traffic.
func (g *Group) Get(name string) *Breaker {
	g.mu.RLock()
	b, ok := g.breakers[name]
	g.mu.RUnlock()
	if ok {
		return b
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if b, ok := g.breakers[name]; ok {
		return b
	}
	b = New(name, g.cfg, nil)
	g.breakers[name] = b
	g.order = append(g.order, name)
	return b
}

// Stats returns the shared window for an upstream, creating it if needed.
func (g *Group) Stats(name string) *stats.Stats { return g.Get(name).Stats() }

// Names lists the upstreams in creation order.
func (g *Group) Names() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]string, len(g.order))
	copy(out, g.order)
	return out
}

// Reports snapshots every breaker, sorted by name for stable output.
func (g *Group) Reports() []Report {
	g.mu.RLock()
	names := make([]string, 0, len(g.breakers))
	for name := range g.breakers {
		names = append(names, name)
	}
	g.mu.RUnlock()
	sort.Strings(names)

	out := make([]Report, 0, len(names))
	for _, name := range names {
		out = append(out, g.Get(name).Report())
	}
	return out
}

// ResetAll returns every breaker to closed and clears every window. It exists
// for the admin API: once an operator has fixed a backend, waiting out the old
// window is pointless because the window describes the broken configuration.
func (g *Group) ResetAll() {
	for _, name := range g.Names() {
		g.Get(name).Reset()
	}
}
