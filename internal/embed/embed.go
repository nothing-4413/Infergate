// Package embed turns text into vectors for the semantic cache.
//
// Two implementations exist, and the reason is environmental rather than
// architectural: a semantic cache is only useful if paraphrases land close
// together, but the gateway must also run with no network and no embedding
// service (this host has neither an API key nor a local model server). So the
// default is a deterministic, offline embedder, and a real embedding endpoint is
// a configuration change rather than a code change.
//
//	HashingEmbedder  feature hashing over words and word bigrams; no network,
//	                 no model file, identical output in every process.
//	HTTPEmbedder     POST /embeddings to any OpenAI-compatible endpoint
//	                 (vLLM, Ollama, OpenAI, a local sentence-transformers server).
//
// The offline embedder is honest about what it is: it measures LEXICAL overlap.
// It catches reordering, insertion, deletion and punctuation changes, and it
// will not catch a synonym substitution. That limit is measured rather than
// hand-waved: scripts/measure-m2.ps1 sweeps the similarity threshold over a
// labelled paraphrase set and records both the hit rate and the false-hit rate,
// which is what makes the threshold a measured number instead of a guess.
package embed

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Embedder converts texts into unit-length vectors.
//
// Implementations must return one vector per input text, in the same order, and
// every vector must have the same length. Callers rely on both: the cache pairs
// vector i with text i, and a mismatched length is a silent scoring bug.
type Embedder interface {
	// Embed returns one vector per text. It must respect ctx cancellation: an
	// embedding call is on the critical path of a client request, and a stuck
	// embedder must not hold a request open past its deadline.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dims is the vector length this embedder produces.
	Dims() int

	// Name identifies the embedder in logs and in /admin/cache. It is part of
	// nothing security-sensitive; it exists so an operator can tell which
	// embedder filled a cache.
	Name() string
}

// ---------------------------------------------------------------------------
// Offline embedder: feature hashing
// ---------------------------------------------------------------------------

// HashingEmbedder is the offline default.
//
// The algorithm is the classic "hashing trick" with signed features:
//
//  1. Tokenise: lowercase, split on anything that is not a letter or a digit,
//     and additionally emit character bigrams for CJK runs (Chinese has no
//     spaces, so a whole sentence would otherwise be a single token and any two
//     Chinese sentences would be orthogonal).
//  2. Features: word unigrams plus word bigrams, so word order matters at the
//     scale of a phrase ("dog bites man" != "man bites dog") without turning the
//     vector into a pure n-gram soup.
//  3. Hash each feature to a bucket with FNV-1a and accumulate a signed,
//     sublinearly weighted count (1 + ln tf). The sign is a second hash bit:
//     without it, collisions only ever ADD similarity, which inflates scores
//     between unrelated texts.
//  4. L2-normalise, so cosine similarity is a dot product and a long document
//     is not automatically "closer" to everything than a short one.
//
// FNV-1a is used deliberately instead of hash/maphash: maphash is seeded per
// process, so two gateway replicas would compute different vectors for the same
// text and a shared Redis cache would never serve a cross-replica hit. The hash
// must be stable across processes and across restarts, which is exactly what a
// cache key needs.
type HashingEmbedder struct {
	dims int
}

// DefaultHashingDims is the vector width of the offline embedder.
//
// 512 is a compromise with a measured cost: an entry's vector is stored as
// base64 float32 (2 KiB for 512 dims, 2.7 KiB encoded). With the default
// 256 entries per scope, one lookup reads roughly 700 KiB from the store, which
// is fine for a local Redis and would need a real vector index at a larger
// scale. Narrowing to 256 dims halves that at some cost in discrimination.
const DefaultHashingDims = 512

// NewHashingEmbedder returns an offline embedder of the given width. A width of
// zero selects DefaultHashingDims; a negative width is clamped, because a zero
// or negative modulus is a panic in the accumulator rather than a config error
// the caller could recover from.
func NewHashingEmbedder(dims int) *HashingEmbedder {
	if dims <= 0 {
		dims = DefaultHashingDims
	}
	return &HashingEmbedder{dims: dims}
}

// Dims implements Embedder.
func (h *HashingEmbedder) Dims() int { return h.dims }

// Name implements Embedder.
func (h *HashingEmbedder) Name() string { return fmt.Sprintf("hashing-%d", h.dims) }

// Embed implements Embedder. It cannot fail (no IO) and never returns a nil
// error, but it keeps the signature so callers need no type switch.
func (h *HashingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = h.vector(t)
	}
	return out, nil
}

func (h *HashingEmbedder) vector(text string) []float32 {
	v := make([]float32, h.dims)
	counts := map[string]int{}
	for _, tok := range Features(text) {
		counts[tok]++
	}
	for tok, n := range counts {
		idx, sign := hashFeature(tok, h.dims)
		// Sublinear term frequency: a word repeated ten times is not ten times
		// as important, otherwise a padded or repetitive prompt dominates the
		// vector and drifts away from its own paraphrase.
		w := float32(1 + math.Log(float64(n)))
		v[idx] += sign * w
	}
	Normalize(v)
	return v
}

// Features returns the tokens the offline embedder hashes, in document order.
//
// It is exported because the thresholds that make the semantic cache correct are
// only defensible if the tokenisation is inspectable: /admin/cache/lookup shows
// the features of a probe prompt, so a surprising similarity can be read off the
// token list instead of guessed at.
func Features(text string) []string {
	words := tokenize(text)
	feats := make([]string, 0, len(words)*2)
	feats = append(feats, words...)
	for i := 0; i+1 < len(words); i++ {
		feats = append(feats, words[i]+" "+words[i+1])
	}
	return feats
}

// tokenize lowercases the text and splits it into words, expanding CJK runs into
// character bigrams.
func tokenize(text string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		tok := string(cur)
		cur = cur[:0]
		if isCJK(tok) {
			// A Chinese sentence has no spaces, so treat it as a bag of
			// character bigrams: "重置我的密码" -> 重置/置我/我的/的密/密码.
			// Single characters are dropped as features because they are too
			// common to discriminate (的, 了, 是).
			r := []rune(tok)
			for i := 0; i+1 < len(r); i++ {
				words = append(words, string(r[i:i+2]))
			}
			return
		}
		words = append(words, tok)
	}

	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
			continue
		}
		flush()
	}
	flush()
	return words
}

func isCJK(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

// hashFeature maps a feature to a bucket and a sign.
//
// Two independent hashes rather than one: reusing a single hash for the index
// and for the sign correlates them, so a collision systematically points in one
// direction. The sign is taken from the top bit of an FNV-1a over a different
// prefix, which is enough for a cache heuristic and keeps the code readable.
func hashFeature(feature string, dims int) (int, float32) {
	idx := fnvHash("i:" + feature)
	sign := fnvHash("s:" + feature)
	// #nosec G115 -- dims is positive (validated in the constructor) and the
	// modulo result is non-negative, so the conversion cannot overflow.
	i := int(idx % uint64(dims))
	if sign&1 == 0 {
		return i, 1
	}
	return i, -1
}

func fnvHash(s string) uint64 {
	h := fnv.New64a()
	// hash.Hash.Write never returns an error for fnv, and the input is a string.
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// Normalize scales v to unit length in place. A zero vector is left as-is
// (dividing by its norm would produce NaNs, and a NaN cosine compares false
// against every threshold, which would silently turn a cache hit into a miss).
func Normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}

// Cosine returns the cosine similarity of a and b.
//
// Unit-length inputs make this a dot product; the norms are computed anyway so
// the function is correct for unnormalised vectors too (an entry stored by an
// older version, or a vector from an HTTP embedder that does not normalise).
// A length mismatch returns 0: 0 is below every usable threshold, so a
// dimension change turns into cache misses rather than into wrong hits.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// IsZero reports whether v carries no direction: it is empty or every component
// is zero.
//
// A zero query vector is not "similar to nothing", it is meaningless, and the
// arithmetic agrees: Cosine returns 0 for it, so against a threshold of exactly
// 0 every stored entry would qualify. A caller that failed to compute an
// embedding would then get an arbitrary entry back - a wrong answer dressed up
// as a cache hit. Stores therefore reject a zero query vector outright and
// report no matches, which is the same outcome as a cold cache.
func IsZero(v []float32) bool {
	if len(v) == 0 {
		return true
	}
	for _, x := range v {
		if x != 0 {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Vector wire format
// ---------------------------------------------------------------------------

// EncodeVector serialises a vector as base64 of little-endian float32.
//
// Storing vectors as a JSON array of numbers was the obvious first choice and it
// is wrong twice over: it is roughly twice the size (up to 8 characters per
// float), and text→float64→float32 round-tripping is lossy, so reading an entry
// back gives a vector that is close to, but not equal to, the one that was
// written. For a cache whose entire job is comparing vectors, "close" is a bug
// waiting to happen. Raw float32 round-trips exactly.
func EncodeVector(v []float32) string {
	buf := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(x))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// DecodeVector reverses EncodeVector.
func DecodeVector(s string) ([]float32, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("embed: decode vector: %w", err)
	}
	if len(raw)%4 != 0 {
		return nil, fmt.Errorf("embed: decode vector: %d bytes is not a whole number of float32", len(raw))
	}
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// HTTP embedder
// ---------------------------------------------------------------------------

// HTTPOptions configures an OpenAI-compatible embedding endpoint.
type HTTPOptions struct {
	// BaseURL is the API root, e.g. "http://127.0.0.1:8000/v1". The embedder
	// appends "/embeddings".
	BaseURL string

	// Model is the embedding model name sent in the request body.
	Model string

	// APIKey is sent as "Authorization: Bearer <key>" when non-empty. Empty is
	// the normal case for a local vLLM or Ollama server.
	APIKey string

	// Timeout bounds one embedding HTTP call. Embedding is on the request path,
	// so this must be small: a slow embedder is worse than a cache miss.
	Timeout time.Duration

	// MaxInputChars truncates each text before sending. An embedding endpoint
	// has its own token limit and a 413 from the embedder would turn into a
	// failed client request; truncation degrades the cache to a miss instead.
	// Zero selects DefaultMaxInputChars.
	MaxInputChars int

	// Client is optional; a client with a sane transport is created otherwise.
	Client *http.Client
}

// DefaultMaxInputChars bounds one embedded text.
const DefaultMaxInputChars = 8000

// HTTPEmbedder calls an OpenAI-compatible /embeddings endpoint.
type HTTPEmbedder struct {
	opts   HTTPOptions
	client *http.Client
	dims   int
}

// NewHTTPEmbedder validates the options and returns the embedder. Dims is
// unknown until the first call, so it reports 0 until then; a cache that needs
// a fixed width must learn it from the first response (see Cache.observeDims).
func NewHTTPEmbedder(opts HTTPOptions) (*HTTPEmbedder, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, fmt.Errorf("embed: http embedder needs a base_url")
	}
	if strings.TrimSpace(opts.Model) == "" {
		return nil, fmt.Errorf("embed: http embedder needs a model")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.MaxInputChars <= 0 {
		opts.MaxInputChars = DefaultMaxInputChars
	}
	c := opts.Client
	if c == nil {
		c = &http.Client{Timeout: opts.Timeout}
	}
	return &HTTPEmbedder{opts: opts, client: c}, nil
}

// Dims implements Embedder. Zero means "not yet observed".
func (e *HTTPEmbedder) Dims() int { return e.dims }

// Name implements Embedder.
func (e *HTTPEmbedder) Name() string { return "http:" + e.opts.Model }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Embed implements Embedder.
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	inputs := make([]string, len(texts))
	for i, t := range texts {
		inputs[i] = truncateRunes(t, e.opts.MaxInputChars)
	}
	body, err := json.Marshal(embedRequest{Model: e.opts.Model, Input: inputs})
	if err != nil {
		return nil, fmt.Errorf("embed: marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, e.opts.Timeout)
	defer cancel()

	url := strings.TrimRight(e.opts.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.opts.APIKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: call %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap the read: an embedding response is a vector table, and a misconfigured
	// URL pointed at a chat endpoint could otherwise stream megabytes into
	// memory on the request path.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("embed: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: %s returned %d: %s", url, resp.StatusCode, snippet(raw))
	}

	var parsed embedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("embed: parse response: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("embed: provider error: %s", parsed.Error.Message)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("embed: asked for %d vectors, got %d", len(texts), len(parsed.Data))
	}

	// The API contract says `index` identifies the position; some servers
	// return them out of order, and pairing the wrong vector with the wrong
	// text would poison the cache with misleading similarities.
	ordered := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(ordered) {
			return nil, fmt.Errorf("embed: response index %d out of range", d.Index)
		}
		ordered[d.Index] = d.Embedding
	}
	for i, v := range ordered {
		if len(v) == 0 {
			return nil, fmt.Errorf("embed: response is missing the vector for input %d", i)
		}
		Normalize(v)
	}
	e.dims = len(ordered[0])
	return ordered, nil
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// Fallback
// ---------------------------------------------------------------------------

// Fallback tries Primary and, on any error, falls back to Secondary.
//
// The direction that matters is which one is primary. A gateway that cannot
// embed must not fail the client request: with a local hashing embedder as the
// fallback, an embedding outage degrades the cache to lexical matching instead
// of turning every chat completion into a 500. The reverse order would make a
// network dependency load-bearing for a pure optimisation.
type Fallback struct {
	Primary   Embedder
	Secondary Embedder

	// OnFallback is called with the primary's error. It exists so the caller can
	// count degraded embeddings without this package importing metrics.
	OnFallback func(err error)
}

// Embed implements Embedder.
func (f *Fallback) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vecs, err := f.Primary.Embed(ctx, texts)
	if err == nil {
		return vecs, nil
	}
	if f.OnFallback != nil {
		f.OnFallback(err)
	}
	if f.Secondary == nil {
		return nil, err
	}
	return f.Secondary.Embed(ctx, texts)
}

// Dims implements Embedder.
func (f *Fallback) Dims() int {
	if d := f.Primary.Dims(); d > 0 {
		return d
	}
	return f.Secondary.Dims()
}

// Name implements Embedder.
func (f *Fallback) Name() string {
	if f.Secondary == nil {
		return f.Primary.Name()
	}
	return f.Primary.Name() + "|" + f.Secondary.Name()
}

// SortedBySimilarity is a small helper used by the admin lookup endpoint.
type Scored struct {
	Key        string
	Similarity float64
	Model      string
	PromptHash string
	CreatedAt  time.Time
}

// Rank returns the entries sorted by descending similarity. It exists so the
// admin surface and the tests agree on one definition of "nearest".
func Rank(scores []Scored) []Scored {
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].Similarity > scores[j].Similarity })
	return scores
}
