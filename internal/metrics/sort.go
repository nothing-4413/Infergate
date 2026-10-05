package metrics

import "sort"

// sortRequests orders snapshots deterministically so that /metrics output and
// test assertions do not depend on Go's randomised map iteration order.
func sortRequests(rs []RequestSnapshot) {
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.Route != b.Route {
			return a.Route < b.Route
		}
		if a.Upstream != b.Upstream {
			return a.Upstream < b.Upstream
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.Status != b.Status {
			return a.Status < b.Status
		}
		return a.Outcome < b.Outcome
	})
}
