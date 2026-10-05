package main

// M4 acceptance harness.
//
// The shape is deliberately the same as cmd/verify-m3's: a checker that records
// one line per assertion, an environment that starts a real server on a free
// port over in-process upstreams that count what they were asked for, and a
// stack whose Close unwinds everything.
//
// M4 forces one difference. Tiering is a decision about a REQUEST, not a
// property of the process, so the checks here need two things verify-m3 never
// did: the router's own plan -- read directly, because the assembled gateway
// never exposes it -- and two upstreams in one stack whose recordings can be
// compared against each other.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/infergate/infergate/internal/logging"
)

// ---------------------------------------------------------------------------
// Assertion checker
// ---------------------------------------------------------------------------

type checker struct {
	out     io.Writer
	pass    int
	fail    int
	loops   int
	details []string
}

// assert records one assertion: a message stating what was expected, and
// whether it held. It returns the outcome so a caller can skip assertions that
// only make sense when this one held.
func (c *checker) assert(ok bool, format string, args ...any) bool {
	msg := fmt.Sprintf(format, args...)
	if ok {
		c.pass++
		fmt.Fprintf(c.out, "   PASS  %s\n", msg)
		return true
	}
	c.fail++
	c.details = append(c.details, msg)
	fmt.Fprintf(c.out, "   FAIL  %s\n", msg)
	return false
}

// assertLoop is assert for an assertion issued from inside a loop whose length
// is a property of a fleet or of a grid of inputs rather than a hand-written
// claim. The distinction is only reporting: the RESULT line counts them all,
// and -v prints the split.
func (c *checker) assertLoop(ok bool, format string, args ...any) bool {
	c.loops++
	return c.assert(ok, format, args...)
}

// info reports evidence that is worth seeing but is not itself a pass or a
// fail, such as the plan a request produced.
func (c *checker) info(format string, args ...any) {
	fmt.Fprintf(c.out, "         %s\n", fmt.Sprintf(format, args...))
}

func (c *checker) tally() (int, int) { return c.pass + c.fail, c.fail }

// environment carries what every check needs: somewhere to write, and whether
// to narrate the requests it makes.
type environment struct {
	out     io.Writer
	verbose bool
}

func newLogger(c *checker) *slog.Logger {
	logger, err := logging.New(io.Discard, "error", "text")
	if !c.assert(err == nil, "harness: build the server logger (%v)", err) {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return logger
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

type result struct {
	status int
	body   string
	header http.Header
}

func post(c *checker, base, path, body string, hdr map[string]string) result {
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+path, strings.NewReader(body))
	if !c.assert(err == nil, "harness: build POST %s (%v)", path, err) {
		return result{status: -1}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if !c.assert(err == nil, "harness: POST %s reaches the gateway (%v)", path, err) {
		return result{status: -1}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	c.assert(err == nil, "harness: read the body of POST %s (%v)", path, err)
	return result{status: resp.StatusCode, body: string(raw), header: resp.Header}
}

func chat(c *checker, base, body string, hdr map[string]string) result {
	return post(c, base, "/v1/chat/completions", body, hdr)
}

func get(c *checker, url string) result {
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Get(url)
	if !c.assert(err == nil, "harness: GET %s (%v)", url, err) {
		return result{status: -1}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	c.assert(err == nil, "harness: read the body of GET %s (%v)", url, err)
	return result{status: resp.StatusCode, body: string(raw), header: resp.Header}
}

func getJSON(c *checker, url string, v any) bool {
	res := get(c, url)
	if !c.assert(res.status == http.StatusOK, "harness: GET %s answers 200 (got %d: %s)", url, res.status, truncate(res.body, 200)) {
		return false
	}
	if err := json.Unmarshal([]byte(res.body), v); err != nil {
		return c.assert(false, "harness: GET %s has a decodable JSON body (%v)", url, err)
	}
	return true
}

func chatBody(c *checker, model, prompt string) string {
	return chatBodyWithCeiling(c, model, prompt, 0)
}

// chatBodyWithCeiling builds a chat request. maxTokens <= 0 omits the ceiling
// entirely rather than sending zero: a request that never mentions
// max_tokens and a request that asks for zero completions are different
// requests, and only the first is what a normal caller sends.
func chatBodyWithCeiling(c *checker, model, prompt string, maxTokens int) string {
	payload := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	}
	if maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}
	raw, err := json.Marshal(payload)
	c.assert(err == nil, "harness: encode a chat request for %q (%v)", model, err)
	return string(raw)
}

// deadAddr returns a 127.0.0.1 host:port nothing is listening on: it binds a
// port, notes it, and closes the listener. A connection to it is refused
// immediately, which is the failure mode the failover checks need.
func deadAddr(c *checker) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if !c.assert(err == nil, "harness: reserve a dead address (%v)", err) {
		return "127.0.0.1:1"
	}
	addr := ln.Addr().String()
	c.assert(ln.Close() == nil, "harness: release the reserved dead address %s", addr)
	return addr
}

// repoRoot walks up from the working directory to the module root, so a check
// can read a shipped config by path without depending on where the gate is run
// from inside the tree.
func repoRoot(c *checker) string {
	dir, err := os.Getwd()
	if !c.assert(err == nil, "harness: read the working directory (%v)", err) {
		return "."
	}
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	c.assert(false, "harness: find the module root above the working directory")
	return "."
}

// ---------------------------------------------------------------------------
// Metrics text
// ---------------------------------------------------------------------------

var metricLine = regexp.MustCompile(`(?m)^([a-zA-Z_:][a-zA-Z0-9_:]*)\{([^}]*)\}\s+([0-9.eE+-]+)\s*$`)

// metricSum adds every sample of one series whose label set contains all of
// wanted. Labels are matched as substrings ("upstream=\"cloud\""), so the
// caller writes them the way the exposition format does and the order the
// gateway happens to emit them in does not matter.
func metricSum(text, series string, wanted ...string) float64 {
	total := 0.0
	for _, m := range metricLine.FindAllStringSubmatch(text, -1) {
		if m[1] != series {
			continue
		}
		matched := true
		for _, want := range wanted {
			if !strings.Contains(m[2], want) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		var value float64
		if _, err := fmt.Sscanf(m[3], "%g", &value); err == nil {
			total += value
		}
	}
	return total
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

// ---------------------------------------------------------------------------
// Recording upstream
// ---------------------------------------------------------------------------

type recordedCall struct {
	Model     string
	MaxTokens int
	Raw       string
}

type recordingUpstream struct {
	name       string
	srv        *httptest.Server
	mu         sync.Mutex
	seen       []recordedCall
	prompt     int
	completion int
}

// newRecorder starts an upstream that answers like an OpenAI-compatible
// provider and records what it was asked for. The gate's whole method is to
// compare what the gateway says it did against what the backends actually
// received, so the recorder is the source of truth, not the gateway.
func newRecorder(c *checker, name string, promptTokens, completionTokens int) *recordingUpstream {
	up := &recordingUpstream{name: name, prompt: promptTokens, completion: completionTokens}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", up.chat)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")
	})
	up.srv = httptest.NewServer(mux)
	return up
}

func (u *recordingUpstream) chat(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":{"message":"unreadable body","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	var req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.Model == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"model is required","type":"invalid_request_error"}}`)
		return
	}

	u.mu.Lock()
	u.seen = append(u.seen, recordedCall{Model: req.Model, MaxTokens: req.MaxTokens, Raw: string(raw)})
	u.mu.Unlock()

	body, err := json.Marshal(map[string]any{
		"id":     "chatcmpl-" + u.name,
		"object": "chat.completion",
		"model":  req.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": "served by " + u.name},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens":     u.prompt,
			"completion_tokens": u.completion,
			"total_tokens":      u.prompt + u.completion,
		},
	})
	if err != nil {
		http.Error(w, `{"error":{"message":"could not encode","type":"internal_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (u *recordingUpstream) URL() string { return u.srv.URL }

func (u *recordingUpstream) Close() { u.srv.Close() }

func (u *recordingUpstream) Calls() []recordedCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]recordedCall, len(u.seen))
	copy(out, u.seen)
	return out
}

func (u *recordingUpstream) Count() int { return len(u.Calls()) }

func (u *recordingUpstream) Last() (recordedCall, bool) {
	calls := u.Calls()
	if len(calls) == 0 {
		return recordedCall{}, false
	}
	return calls[len(calls)-1], true
}
