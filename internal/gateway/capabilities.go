package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/upstream"
)

// ModelInfo is the per-model metadata the gateway cannot observe on the wire.
//
// The openai-compatible dialect reports what a MODEL is called and what an
// upstream SERVES; nothing in it says how large the context is, how much the
// model can emit, or which optional request shapes it accepts. An agent runtime
// that has to decide whether to compress a conversation needs the first number,
// and an operator debugging "why did vision fail on this backend" needs the
// third. Both come from configuration and are served back unchanged.
type ModelInfo struct {
	// ContextWindow is the model's total context in tokens, 0 when unknown.
	ContextWindow int

	// MaxOutputTokens is the largest completion the model accepts, 0 when
	// unknown.
	MaxOutputTokens int

	// Capabilities are tags this model supports on its own, unioned with what
	// the serving upstreams declare.
	Capabilities []string

	// Notes is free text for the operator's own bookkeeping.
	Notes string
}

// CapabilityReport is the declarative answer to "what can this gateway serve?".
//
// It performs no I/O: everything here is either configuration or this process's
// own health window. That is the point of separating it from ProbeCapabilities,
// which spends money and can therefore only be run deliberately.
type CapabilityReport struct {
	GeneratedAt  time.Time            `json:"generated_at"`
	Capabilities []string             `json:"capabilities"`
	Models       []ModelCapability    `json:"models"`
	Upstreams    []UpstreamCapability `json:"upstreams"`
}

// ModelCapability is one model and where it can be served from.
type ModelCapability struct {
	Model string `json:"model"`

	// Capabilities is the union of what the upstreams serving this model
	// declare and what the model's own metadata declares.
	Capabilities []string `json:"capabilities"`

	ContextWindow   int    `json:"context_window,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	Notes           string `json:"notes,omitempty"`

	// Upstreams are the backends that can serve this model, in routing order.
	Upstreams []string `json:"upstreams"`

	// Available reports whether at least one serving backend is not currently
	// circuit-broken. A model with no available backend is still listed: the
	// caller needs to know it exists and that it is down, which is a different
	// answer from "unknown model".
	Available bool `json:"available"`
}

// UpstreamCapability is one backend's declared surface and its current state.
type UpstreamCapability struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	BaseURL      string   `json:"base_url"`
	Capabilities []string `json:"capabilities"`
	Models       []string `json:"models"`
	CatchAll     bool     `json:"catch_all"`
	Priority     int      `json:"priority"`
	Tier         string   `json:"tier"`
	State        string   `json:"state"`
}

// CapabilityReport assembles the declarative capability surface.
func (p *Proxy) CapabilityReport() CapabilityReport {
	rep := CapabilityReport{GeneratedAt: time.Now().UTC()}

	targets := p.sortedTargets()
	rep.Upstreams = make([]UpstreamCapability, 0, len(targets))

	// served maps a model name to the backends that claim it, and caps maps it
	// to the union of their capability tags.
	served := make(map[string][]*upstream.Target)
	caps := make(map[string]map[string]struct{})
	addCap := func(model string, tags []string) {
		if caps[model] == nil {
			caps[model] = make(map[string]struct{})
		}
		for _, t := range tags {
			if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
				caps[model][t] = struct{}{}
			}
		}
	}

	for _, t := range targets {
		state := p.upstreamState(t.Name)
		rep.Upstreams = append(rep.Upstreams, UpstreamCapability{
			Name:         t.Name,
			Kind:         t.Kind,
			BaseURL:      t.BaseURL,
			Capabilities: t.Capabilities(),
			Models:       t.ModelPatterns(),
			CatchAll:     t.IsCatchAll(),
			Priority:     t.Priority(),
			Tier:         t.Tier(),
			State:        state,
		})
		for _, m := range t.ModelPatterns() {
			if m == "/" {
				continue
			}
			served[m] = append(served[m], t)
			addCap(m, t.Capabilities())
		}
	}

	// A model named only in configuration still exists: it is declared here so
	// that its context window and capability tags are served back before any
	// upstream has seen a request for it.
	for model := range p.models {
		if _, ok := served[model]; !ok {
			served[model] = nil
		}
		if _, ok := caps[model]; !ok {
			caps[model] = make(map[string]struct{})
		}
		addCap(model, p.models[model].Capabilities)
	}

	allCaps := make(map[string]struct{})
	for model := range caps {
		info := p.models[model]

		names := make([]*upstream.Target, 0, len(served[model]))
		names = append(names, served[model]...)
		// A catch-all backend serves every model, including one it never named.
		for _, t := range targets {
			if t.IsCatchAll() {
				names = append(names, t)
				addCap(model, t.Capabilities())
			}
		}

		upNames := make([]string, 0, len(names))
		available := false
		seen := make(map[string]struct{}, len(names))
		for _, t := range names {
			if _, dup := seen[t.Name]; dup {
				continue
			}
			seen[t.Name] = struct{}{}
			upNames = append(upNames, t.Name)
			if p.upstreamState(t.Name) != "open" {
				available = true
			}
		}

		modelCaps := setNames(caps[model])
		for _, c := range modelCaps {
			allCaps[c] = struct{}{}
		}

		rep.Models = append(rep.Models, ModelCapability{
			Model:           model,
			Capabilities:    modelCaps,
			ContextWindow:   info.ContextWindow,
			MaxOutputTokens: info.MaxOutputTokens,
			Notes:           info.Notes,
			Upstreams:       upNames,
			Available:       available,
		})
	}

	sort.Slice(rep.Models, func(i, j int) bool { return rep.Models[i].Model < rep.Models[j].Model })
	rep.Capabilities = setNames(allCaps)
	if rep.Models == nil {
		rep.Models = []ModelCapability{}
	}
	return rep
}

// sortedTargets lists the backends in routing order (priority, then name) so
// that two reports of the same fleet read identically.
func (p *Proxy) sortedTargets() []*upstream.Target {
	if p.upstreams == nil {
		return nil
	}
	targets := p.upstreams.Targets()
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].Priority() != targets[j].Priority() {
			return targets[i].Priority() < targets[j].Priority()
		}
		return targets[i].Name < targets[j].Name
	})
	return targets
}

// upstreamState reports a backend's circuit state, defaulting to "closed" when
// no breaker group is configured (M0/M1 behaviour: everything is healthy).
func (p *Proxy) upstreamState(name string) string {
	if p.breakers == nil {
		return "closed"
	}
	return string(p.breakers.Get(name).Report().State)
}

// CapabilityProbeRequest selects what a live probe should check.
type CapabilityProbeRequest struct {
	// Capabilities are the tags to probe. Empty means every capability this
	// gateway declares, which is the expensive choice and the honest default.
	Capabilities []string `json:"capabilities"`

	// Model is the model to probe with. Empty means each backend is probed with
	// the first model it declares, and a backend that declares only the
	// catch-all "/" is skipped (a probe needs a concrete model name to send).
	Model string `json:"model"`

	// Upstreams restricts the probe to named backends. Empty means all.
	Upstreams []string `json:"upstreams"`
}

// CapabilityProbe is the outcome of one live check against one backend.
type CapabilityProbe struct {
	Capability string  `json:"capability"`
	Upstream   string  `json:"upstream"`
	Model      string  `json:"model"`
	Status     int     `json:"status"`
	Accepted   bool    `json:"accepted"`
	Outcome    string  `json:"outcome"`
	Error      string  `json:"error,omitempty"`
	ElapsedMS  float64 `json:"elapsed_ms"`
	Path       string  `json:"path"`
}

// Probe outcomes. They are deliberately more than a boolean: "the provider
// refused this request shape" and "the provider is rate limiting me" are
// different answers, and only the first one is about capability.
const (
	ProbeAccepted      = "accepted"
	ProbeRejected      = "rejected"
	ProbeUnauthorized  = "unauthorized"
	ProbeIndeterminate = "indeterminate"
	ProbeUnreachable   = "unreachable"
	ProbeSkipped       = "skipped"
)

// probePathChat and probePathEmbeddings are the two endpoints a probe uses.
const (
	probePathChat       = "/v1/chat/completions"
	probePathEmbeddings = "/v1/embeddings"
)

// ErrUnknownCapability marks a probe request naming a capability this gateway
// has no shape for. It is a caller error, and the handlers translate it into a
// 400 rather than a 500.
var ErrUnknownCapability = errors.New("unknown capability")

// probeCapabilities lists every capability this gateway knows how to probe, in
// the order the report lists them.
func probeCapabilities() []string {
	return []string{"chat", "stream", "tools", "vision", "json", "embeddings"}
}

// probeTinyPNG is a 1x1 transparent PNG. A vision probe has to send an image
// and the request must not depend on the network fetching a URL, so it is
// inlined as a data URL. The model is not expected to describe it: the probe
// asks whether the provider ACCEPTS the content part at all.
const probeTinyPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// ProbeCapabilities spends real upstream calls to find out which capabilities a
// backend actually accepts.
//
// # What a probe can and cannot establish
//
// A probe sends the smallest request that exercises a capability and judges the
// answer by ACCEPTANCE: a 2xx or any status that is not a client rejection
// means the provider understood the shape. It does not establish that the model
// uses the capability well -- a tool call is not required to come back -- and it
// says nothing about quality. The claim it supports is the one an operator
// needs before trusting a backend for a workload: this endpoint does not reject
// the request shape.
//
// It deliberately bypasses the cache, the budget, the breaker and the retry
// chain: a probe is a diagnostic, not customer traffic, and it must not consume
// a tenant's budget, be answered from cache, or trip a breaker.
func (p *Proxy) ProbeCapabilities(ctx context.Context, req CapabilityProbeRequest) ([]CapabilityProbe, error) {
	wanted := req.Capabilities
	if len(wanted) == 0 {
		wanted = probeCapabilities()
	}
	known := make(map[string]struct{}, len(probeCapabilities()))
	for _, c := range probeCapabilities() {
		known[c] = struct{}{}
	}
	for i, c := range wanted {
		c = strings.ToLower(strings.TrimSpace(c))
		if _, ok := known[c]; !ok {
			return nil, fmt.Errorf("%w %q: probe one of %s", ErrUnknownCapability, c, strings.Join(probeCapabilities(), ", "))
		}
		wanted[i] = c
	}

	restrict := make(map[string]struct{}, len(req.Upstreams))
	for _, n := range req.Upstreams {
		if n = strings.TrimSpace(n); n != "" {
			restrict[n] = struct{}{}
		}
	}

	timeout := p.upstreamTTL
	if timeout <= 0 || timeout > 20*time.Second {
		timeout = 20 * time.Second
	}

	out := make([]CapabilityProbe, 0, len(wanted)*len(p.sortedTargets()))
	for _, target := range p.sortedTargets() {
		if len(restrict) > 0 {
			if _, ok := restrict[target.Name]; !ok {
				continue
			}
		}
		model, ok := probeModel(target, req.Model)
		for _, capability := range wanted {
			if !ok {
				out = append(out, CapabilityProbe{
					Capability: capability,
					Upstream:   target.Name,
					Outcome:    ProbeSkipped,
					Error:      "backend declares no concrete model name to probe with",
				})
				continue
			}
			out = append(out, p.probeOne(ctx, target, capability, model, timeout))
		}
	}
	return out, nil
}

// probeModel picks the model name to send. An explicit request wins; otherwise
// the first concrete model the backend declares is used. The catch-all "/" is
// not a model name a provider accepts, so a backend that declares only that
// cannot be probed without being told what to send.
func probeModel(t *upstream.Target, requested string) (string, bool) {
	if requested = strings.TrimSpace(requested); requested != "" {
		return requested, true
	}
	for _, m := range t.ModelPatterns() {
		if m != "/" && strings.TrimSpace(m) != "" {
			return m, true
		}
	}
	return "", false
}

// probeOne performs one probe and classifies the answer.
func (p *Proxy) probeOne(ctx context.Context, target *upstream.Target, capability, model string, timeout time.Duration) CapabilityProbe {
	probe := CapabilityProbe{Capability: capability, Upstream: target.Name, Model: model}

	path, body, err := probeBody(capability, model)
	if err != nil {
		probe.Outcome = ProbeSkipped
		probe.Error = err.Error()
		return probe
	}
	probe.Path = path

	// A synthetic inbound request gives buildRequest everything it needs (the
	// path, the method, a context) while keeping the real caller's headers and
	// traceparent out of a diagnostic: buildRequest is the one place that knows
	// how a backend is addressed and credentialed, and re-implementing that
	// here is how a probe starts testing a different code path than the one
	// customers use.
	synthetic := httptest.NewRequest(http.MethodPost, path, nil)
	synthetic.Header.Set("Content-Type", "application/json")
	rec := &record{requestID: "probe-" + target.Name, path: path}

	req, client, cleanup, err := p.buildRequest(synthetic, target, body, timeout, rec)
	if err != nil {
		probe.Outcome = ProbeUnreachable
		probe.Error = err.Error()
		return probe
	}
	defer cleanup()
	req = req.WithContext(ctx)

	start := time.Now()
	resp, err := client.Do(req)
	probe.ElapsedMS = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		probe.Outcome = ProbeUnreachable
		probe.Error = err.Error()
		return probe
	}
	defer resp.Body.Close()

	// The answer is read but never kept: a probe needs the status and the
	// content type, and a streamed answer would otherwise be an unbounded read.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	probe.Status = resp.StatusCode
	probe.Accepted, probe.Outcome = classifyProbe(capability, resp.StatusCode, resp.Header)
	if !probe.Accepted && probe.Outcome == ProbeRejected {
		probe.Error = "provider rejected the request shape"
	}
	return probe
}

// classifyProbe turns a status into an outcome. Only a client-side rejection is
// about capability: a rate limit or an upstream error says nothing about
// whether the backend supports the shape, and reporting it as "not supported"
// would make a probe lie during exactly the incident it is run in.
func classifyProbe(capability string, status int, header http.Header) (bool, string) {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return false, ProbeUnauthorized
	case status == http.StatusTooManyRequests || status == http.StatusRequestTimeout:
		return false, ProbeIndeterminate
	case status >= 500:
		return false, ProbeIndeterminate
	case status >= 400:
		return false, ProbeRejected
	}
	if capability == "stream" {
		// A provider that answers a streaming request with a buffered body has
		// accepted the parameter but not the mode, which is worth knowing.
		if !strings.Contains(strings.ToLower(header.Get("Content-Type")), "text/event-stream") {
			return false, ProbeRejected
		}
	}
	return true, ProbeAccepted
}

// probeBody builds the smallest request that exercises one capability.
func probeBody(capability, model string) (string, []byte, error) {
	switch capability {
	case "embeddings":
		return probePathEmbeddings, mustJSON(map[string]any{"model": model, "input": "ping"}), nil
	case "chat":
		return probePathChat, mustJSON(map[string]any{
			"model":      model,
			"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
			"max_tokens": 1,
		}), nil
	case "stream":
		return probePathChat, mustJSON(map[string]any{
			"model":      model,
			"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
			"max_tokens": 1,
			"stream":     true,
		}), nil
	case "tools":
		return probePathChat, mustJSON(map[string]any{
			"model":    model,
			"messages": []any{map[string]any{"role": "user", "content": "ping"}},
			"tools": []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "infergate_probe",
					"description": "no-op probe function",
					"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			}},
			"max_tokens": 1,
		}), nil
	case "vision":
		return probePathChat, mustJSON(map[string]any{
			"model": model,
			"messages": []any{map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "ping"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": probeTinyPNG}},
				},
			}},
			"max_tokens": 1,
		}), nil
	case "json":
		return probePathChat, mustJSON(map[string]any{
			"model":           model,
			"messages":        []any{map[string]any{"role": "user", "content": "{\"ok\":true}"}},
			"response_format": map[string]any{"type": "json_object"},
			"max_tokens":      1,
		}), nil
	default:
		return "", nil, fmt.Errorf("no probe shape for capability %q", capability)
	}
}

// mustJSON marshals a probe body. The values are literals, so a failure is a
// programming error rather than something a caller can cause.
func mustJSON(v any) []byte {
	buf, err := json.Marshal(v)
	if err != nil {
		panic("gateway: probe body: " + err.Error())
	}
	return buf
}

// setNames turns a set into a sorted slice, which keeps reports stable and
// diffable across restarts.
func setNames(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ProbeJSONReader is a small helper for tests and handlers that want to decode a
// probe request leniently: an absent body means "probe everything declared".
func ProbeJSONReader(body []byte) (CapabilityProbeRequest, error) {
	var req CapabilityProbeRequest
	if len(bytes.TrimSpace(body)) == 0 {
		return req, nil
	}
	err := json.Unmarshal(body, &req)
	return req, err
}
