package embed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The offline embedder is the default, so its behaviour IS the default cache
// behaviour. These tests pin the three properties the cache depends on:
// identical text is exactly identical, near-duplicates beat unrelated text, and
// the result does not depend on anything process-local.

func TestIdenticalTextIsExactlyIdentical(t *testing.T) {
	e := NewHashingEmbedder(0)
	got, err := e.Embed(context.Background(), []string{
		"how do I reset my password?",
		"how do I reset my password?",
		"  HOW do I reset   my password!!!  ",
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if c := Cosine(got[0], got[1]); c != 1 {
		t.Fatalf("same text must score exactly 1.0, got %v", c)
	}
	// Case, extra whitespace and punctuation are tokenisation noise, not
	// meaning: a cache that misses on them reports a hit rate that is lower
	// than reality and teaches the operator to distrust the metric.
	if c := Cosine(got[0], got[2]); math.Abs(c-1) > 1e-9 {
		t.Fatalf("case/space/punctuation must normalise away, got %v", c)
	}
}

func TestNearDuplicatesBeatUnrelatedText(t *testing.T) {
	e := NewHashingEmbedder(0)
	ctx := context.Background()
	base := "how do I reset my password on the admin console"

	vec := func(s string) []float32 {
		v, err := e.Embed(ctx, []string{s})
		if err != nil {
			t.Fatalf("Embed(%q): %v", s, err)
		}
		return v[0]
	}

	reference := vec(base)
	cases := map[string]float64{
		"insertion":  Cosine(reference, vec("how do I quickly reset my password on the admin console")),
		"deletion":   Cosine(reference, vec("how do I reset my password")),
		"rephrased":  Cosine(reference, vec("how can I reset the password on the admin console")),
		"unrelated":  Cosine(reference, vec("what is the capital of peru")),
		"off-topic2": Cosine(reference, vec("the build fails with a linker error on windows")),
	}
	// The ordering is the contract; the absolute values are a property of this
	// embedder and are asserted loosely so a tuning change does not fail the
	// suite for the wrong reason.
	for _, near := range []string{"insertion", "deletion", "rephrased"} {
		if cases[near] <= cases["unrelated"] {
			t.Errorf("%s similarity %.4f must exceed unrelated %.4f", near, cases[near], cases["unrelated"])
		}
		if cases[near] < 0.5 {
			t.Errorf("%s similarity %.4f is too low for a paraphrase; the threshold sweep in measure-m2 would have no usable range", near, cases[near])
		}
	}
	if cases["unrelated"] > 0.3 {
		t.Errorf("unrelated texts score %.4f; a threshold that catches paraphrases would also catch noise", cases["unrelated"])
	}
}

func TestWordOrderMattersThroughBigrams(t *testing.T) {
	e := NewHashingEmbedder(0)
	ctx := context.Background()
	got, err := e.Embed(ctx, []string{"dog bites man", "man bites dog", "dog bites man bites dog"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	reversed := Cosine(got[0], got[1])
	if reversed >= 1 {
		t.Fatalf("reversing two words must not be a perfect match (bigrams exist to prevent this), got %v", reversed)
	}
	if repeated := Cosine(got[0], got[2]); repeated <= reversed {
		t.Fatalf("a superset phrase must stay closer (%.4f) than a word-order reversal (%.4f)", repeated, reversed)
	}
}

func TestChineseUsesCharacterBigrams(t *testing.T) {
	e := NewHashingEmbedder(0)
	ctx := context.Background()
	got, err := e.Embed(ctx, []string{
		"请帮我重置密码",
		"怎么重置密码",
		"今天天气不错适合出门散步",
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	related := Cosine(got[0], got[1])
	unrelated := Cosine(got[0], got[2])
	if related <= unrelated {
		t.Fatalf("two password sentences (%.4f) must score above an unrelated sentence (%.4f); without CJK bigrams a space-less sentence is one token", related, unrelated)
	}
	if related < 0.4 {
		t.Fatalf("CJK bigram overlap is only %.4f; the Chinese path of the cache would never hit", related)
	}
}

func TestVectorIsIndependentOfProcessState(t *testing.T) {
	// Two instances stand in for two replicas. A per-process seed (hash/maphash)
	// would make a shared Redis cache unreachable across replicas, so this is
	// the test that protects the cross-replica hit path.
	a, b := NewHashingEmbedder(64), NewHashingEmbedder(64)
	ctx := context.Background()
	va, err := a.Embed(ctx, []string{"shared cache entry"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	vb, err := b.Embed(ctx, []string{"shared cache entry"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if EncodeVector(va[0]) != EncodeVector(vb[0]) {
		t.Fatal("two embedders produced different vectors for the same text: the hash is not stable across processes")
	}
}

func TestDimsAndName(t *testing.T) {
	if d := NewHashingEmbedder(0).Dims(); d != DefaultHashingDims {
		t.Fatalf("zero dims must select the default %d, got %d", DefaultHashingDims, d)
	}
	if d := NewHashingEmbedder(-5).Dims(); d != DefaultHashingDims {
		t.Fatalf("negative dims must be clamped, got %d", d)
	}
	if n := NewHashingEmbedder(128).Name(); n != "hashing-128" {
		t.Fatalf("Name = %q, want hashing-128", n)
	}
	got, err := NewHashingEmbedder(128).Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want one vector per input, got %d", len(got))
	}
	for i, v := range got {
		if len(v) != 128 {
			t.Fatalf("vector %d has %d dims, want 128", i, len(v))
		}
	}
}

func TestEmptyTextHasNoFeaturesAndNoNaNs(t *testing.T) {
	if f := Features("   \t\n  "); len(f) != 0 {
		t.Fatalf("whitespace must produce no features, got %v", f)
	}
	v, err := NewHashingEmbedder(16).Embed(context.Background(), []string{"", "!!!"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for i, vec := range v {
		for j, x := range vec {
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				t.Fatalf("vector %d component %d is %v; Normalize divided by a zero norm", i, j, x)
			}
		}
		if c := Cosine(vec, vec); c != 0 {
			t.Fatalf("an empty text must not match itself (cosine %v); it has no features to compare", c)
		}
	}
}

func TestFeaturesIncludeUnigramsAndBigrams(t *testing.T) {
	f := Features("Reset the Password")
	joined := strings.Join(f, "|")
	for _, want := range []string{"reset", "the", "password", "reset the", "the password"} {
		if !strings.Contains(joined, want) {
			t.Errorf("features %q must contain %q", joined, want)
		}
	}
	// Three words make two bigrams, never a trigram: a trigram would make the
	// vector brittle to a single inserted word.
	if strings.Contains(joined, "reset the password") {
		t.Errorf("features %q must not contain a trigram", joined)
	}
}

func TestCosineEdgeCases(t *testing.T) {
	if c := Cosine(nil, nil); c != 0 {
		t.Fatalf("empty vectors must score 0, got %v", c)
	}
	if c := Cosine([]float32{1, 2}, []float32{1, 2, 3}); c != 0 {
		t.Fatalf("dimension mismatch must score 0 (a miss), got %v", c)
	}
	if c := Cosine([]float32{0, 0}, []float32{1, 1}); c != 0 {
		t.Fatalf("a zero vector must score 0, got %v", c)
	}
	// Unnormalised inputs still compare correctly, because the norms are
	// computed rather than assumed.
	if c := Cosine([]float32{2, 0}, []float32{5, 0}); math.Abs(c-1) > 1e-12 {
		t.Fatalf("parallel unnormalised vectors must score 1, got %v", c)
	}
	if c := Cosine([]float32{1, 0}, []float32{0, 1}); math.Abs(c) > 1e-12 {
		t.Fatalf("orthogonal vectors must score 0, got %v", c)
	}
}

func TestNormalizeHandlesZeroVector(t *testing.T) {
	v := []float32{0, 0, 0}
	Normalize(v)
	for i, x := range v {
		if x != 0 {
			t.Fatalf("component %d became %v; a zero vector must stay zero", i, x)
		}
	}
}

func TestVectorEncodingRoundTripsExactly(t *testing.T) {
	in := []float32{0, 1, -1, 0.1, 1e-8, -3.4028235e38, math.MaxFloat32, 1.0 / 3.0}
	out, err := DecodeVector(EncodeVector(in))
	if err != nil {
		t.Fatalf("DecodeVector: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("length %d != %d", len(out), len(in))
	}
	for i := range in {
		if math.Float32bits(out[i]) != math.Float32bits(in[i]) {
			t.Fatalf("component %d: got %v, want %v (base64 float32 must round-trip bit-exactly)", i, out[i], in[i])
		}
	}
	if v, err := DecodeVector(""); err != nil || v != nil {
		t.Fatalf("empty string must decode to nil, nil; got %v, %v", v, err)
	}
	if _, err := DecodeVector("not base64!!"); err == nil {
		t.Fatal("invalid base64 must be an error")
	}
	if _, err := DecodeVector(base64.StdEncoding.EncodeToString([]byte{1, 2, 3})); err == nil {
		t.Fatal("a byte length that is not a multiple of 4 must be an error")
	}
}

// ---------------------------------------------------------------------------
// HTTP embedder
// ---------------------------------------------------------------------------

func TestHTTPEmbedderPairsVectorsByIndex(t *testing.T) {
	// The handler runs on the httptest server's goroutine and the assertions run
	// on the test goroutine, so everything it publishes is guarded. Responding
	// over a socket is not a happens-before edge the race detector honours, and
	// this file previously had no synchronization at all.
	var mu sync.Mutex
	var gotReq embedRequest
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		gotPath, gotAuth, gotReq = r.URL.Path, r.Header.Get("Authorization"), req
		mu.Unlock()
		// Deliberately out of order and deliberately unnormalised: the embedder
		// must sort by index and normalise, because a mismatched pairing writes
		// a vector next to the wrong text and the cache then "remembers" a
		// similarity that never existed.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","usage":{"prompt_tokens":7},"data":[
			{"index":1,"embedding":[0,3,4]},
			{"index":0,"embedding":[0,0,2]}]}`))
	}))
	defer srv.Close()

	e, err := NewHTTPEmbedder(HTTPOptions{BaseURL: srv.URL + "/v1/", Model: "m", APIKey: "secret", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("NewHTTPEmbedder: %v", err)
	}
	got, err := e.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	mu.Lock()
	path, auth, req := gotPath, gotAuth, gotReq
	mu.Unlock()
	if path != "/v1/embeddings" {
		t.Fatalf("path = %q, want /v1/embeddings (the base URL's trailing slash must not double up)", path)
	}
	if auth != "Bearer secret" {
		t.Fatalf("Authorization = %q", auth)
	}
	if req.Model != "m" || len(req.Input) != 2 || req.Input[1] != "second" {
		t.Fatalf("request body = %+v", req)
	}
	if math.Abs(Cosine(got[0], []float32{0, 0, 1})-1) > 1e-6 {
		t.Fatalf("input 0 got the wrong vector: %v (want [0,0,1] after normalisation)", got[0])
	}
	if math.Abs(Cosine(got[1], []float32{0, 0.6, 0.8})-1) > 1e-6 {
		t.Fatalf("input 1 got the wrong vector: %v", got[1])
	}
	if e.Dims() != 3 {
		t.Fatalf("Dims must be learned from the first response, got %d", e.Dims())
	}
	if e.Name() != "http:m" {
		t.Fatalf("Name = %q", e.Name())
	}
}

func TestHTTPEmbedderTruncatesLongInput(t *testing.T) {
	var mu sync.Mutex
	var gotReq embedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		gotReq = req
		mu.Unlock()
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer srv.Close()

	e, err := NewHTTPEmbedder(HTTPOptions{BaseURL: srv.URL, Model: "m", MaxInputChars: 10})
	if err != nil {
		t.Fatalf("NewHTTPEmbedder: %v", err)
	}
	if _, err := e.Embed(context.Background(), []string{strings.Repeat("中", 40)}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	mu.Lock()
	req := gotReq
	mu.Unlock()
	if n := len([]rune(req.Input[0])); n != 10 {
		t.Fatalf("truncated to %d runes, want 10 (a 413 from the embedder must not become a failed client request)", n)
	}
}

func TestHTTPEmbedderErrorsAreExplicit(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"status": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`upstream is down`))
		},
		"provider error": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"error":{"message":"model not found","type":"invalid_request_error"}}`))
		},
		"count mismatch": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2]}]}`))
		},
		"index out of range": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2]},{"index":9,"embedding":[1,2]}]}`))
		},
		"missing vector": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[]},{"index":1,"embedding":[1,2]}]}`))
		},
		"not json": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<html>proxy error</html>`))
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()
			e, err := NewHTTPEmbedder(HTTPOptions{BaseURL: srv.URL, Model: "m", Timeout: 2 * time.Second})
			if err != nil {
				t.Fatalf("NewHTTPEmbedder: %v", err)
			}
			if _, err := e.Embed(context.Background(), []string{"a", "b"}); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

func TestHTTPEmbedderHonoursContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer func() { close(release); srv.Close() }()

	e, err := NewHTTPEmbedder(HTTPOptions{BaseURL: srv.URL, Model: "m", Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewHTTPEmbedder: %v", err)
	}
	start := time.Now()
	if _, err := e.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("want a timeout error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the timeout must be the configured one, took %v", elapsed)
	}
}

func TestNewHTTPEmbedderValidatesOptions(t *testing.T) {
	if _, err := NewHTTPEmbedder(HTTPOptions{Model: "m"}); err == nil {
		t.Fatal("a missing base_url must be rejected at construction, not at first use")
	}
	if _, err := NewHTTPEmbedder(HTTPOptions{BaseURL: "http://x"}); err == nil {
		t.Fatal("a missing model must be rejected")
	}
}

// ---------------------------------------------------------------------------
// Fallback
// ---------------------------------------------------------------------------

type stubEmbedder struct {
	name string
	dims int
	vec  []float32
	err  error
}

func (s *stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = s.vec
	}
	return out, nil
}
func (s *stubEmbedder) Dims() int    { return s.dims }
func (s *stubEmbedder) Name() string { return s.name }

func TestFallbackDegradesInsteadOfFailing(t *testing.T) {
	boom := errors.New("embedding endpoint unreachable")
	var seen []error
	f := &Fallback{
		Primary:    &stubEmbedder{name: "http", dims: 3, err: boom},
		Secondary:  &stubEmbedder{name: "hashing", dims: 16, vec: []float32{1, 0}},
		OnFallback: func(err error) { seen = append(seen, err) },
	}
	got, err := f.Embed(context.Background(), []string{"a"})
	if err != nil {
		t.Fatalf("a cache optimisation must never fail the request: %v", err)
	}
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("the fallback's vector must be returned, got %v", got)
	}
	if len(seen) != 1 || !errors.Is(seen[0], boom) {
		t.Fatalf("the hook must observe the primary's error exactly once, got %v", seen)
	}
	if f.Dims() != 3 {
		t.Fatalf("Dims prefers the primary when it reports one, got %d", f.Dims())
	}
	if f.Name() != "http|hashing" {
		t.Fatalf("Name = %q", f.Name())
	}
}

func TestFallbackDoesNotCallTheHookOnSuccess(t *testing.T) {
	called := false
	f := &Fallback{
		Primary:    &stubEmbedder{name: "ok", dims: 2, vec: []float32{1, 0}},
		Secondary:  &stubEmbedder{name: "never", dims: 2, err: errors.New("must not be used")},
		OnFallback: func(error) { called = true },
	}
	if _, err := f.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if called {
		t.Fatal("the fallback hook fired although the primary succeeded")
	}
}

func TestFallbackWithNoSecondaryReturnsThePrimaryError(t *testing.T) {
	boom := errors.New("down")
	f := &Fallback{Primary: &stubEmbedder{name: "http", err: boom}}
	if _, err := f.Embed(context.Background(), []string{"a"}); !errors.Is(err, boom) {
		t.Fatalf("without a secondary the error must surface, got %v", err)
	}
}

func TestRankSortsByDescendingSimilarity(t *testing.T) {
	in := []Scored{{Key: "a", Similarity: 0.5}, {Key: "b", Similarity: 0.9}, {Key: "c", Similarity: 0.1}}
	got := Rank(in)
	if got[0].Key != "b" || got[1].Key != "a" || got[2].Key != "c" {
		t.Fatalf("Rank order = %v", []string{got[0].Key, got[1].Key, got[2].Key})
	}
}
