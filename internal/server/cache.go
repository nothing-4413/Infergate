package server

// M2 semantic cache: construction and the admin surface.
//
// The cache is built here rather than inside the gateway because it is the only
// component that talks to another process (Redis) and may own a background
// goroutine. The gateway receives an already-built, already-validated value and
// stays free of I/O, which is what keeps it constructible in tests.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/infergate/infergate/internal/cache"
	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/embed"
)

// buildCache constructs the configured cache.
//
// A disabled cache still gets an object, so that /admin/cache can report what
// IS configured (an operator asking "is the cache on, and with which store"
// must be able to get an answer without reading the config file). It is simply
// never handed to the gateway, so a disabled cache costs nothing per request.
func buildCache(cfg *config.Config, logger logAdapter) (*cache.Cache, error) {
	cc := cfg.Cache
	if !cc.Enabled {
		return cache.New(cache.Config{Enabled: false}, cache.NewMemoryStore(cache.MemoryOptions{
			MaxEntriesPerScope: cc.MaxEntriesPerScope,
		}), nil, loggerFrom(logger)), nil
	}

	// 1. The embedder decides what "similar" means, and therefore what the
	//    threshold is worth. The offline one is lexical; the HTTP one is a real
	//    embedding model and raises the ceiling.
	var emb embed.Embedder
	var embName string
	switch cc.Embedding.Provider {
	case config.EmbedProviderHashing:
		emb = embed.NewHashingEmbedder(cc.Embedding.Dims)
		embName = emb.Name()
	case config.EmbedProviderHTTP:
		primary, err := embed.NewHTTPEmbedder(embed.HTTPOptions{
			BaseURL:       cc.Embedding.BaseURL,
			Model:         cc.Embedding.Model,
			APIKey:        cc.Embedding.APIKey,
			Timeout:       cc.Embedding.Timeout.Duration(),
			MaxInputChars: cc.Embedding.MaxInputChars,
		})
		if err != nil {
			return nil, fmt.Errorf("server: cache embedding: %w", err)
		}
		// An embedding endpoint that is down must degrade the cache to lexical
		// matching, not disable it and not fail the request. The fallback is
		// built with the same dimensions as the primary so that the vectors
		// already stored under the primary are not silently mis-compared: they
		// simply stop matching, which is a miss.
		fallback := embed.NewHashingEmbedder(cc.Embedding.Dims)
		emb = &embed.Fallback{
			Primary:   primary,
			Secondary: fallback,
			OnFallback: func(err error) {
				logger.Warn("cache: embedding request failed, using lexical fallback", "error", err.Error())
			},
		}
		embName = primary.Name()
	case config.EmbedProviderNone:
		// Exact-match cache only. Still worth having: an agent loop re-asks the
		// same question verbatim far more often than people expect.
		embName = "none"
	default:
		return nil, fmt.Errorf("server: cache: unsupported embedding provider %q", cc.Embedding.Provider)
	}

	// 2. The store decides whether the cache survives a restart and whether it
	//    is shared between replicas.
	var store cache.Store
	var storeName string
	switch cc.Store {
	case config.CacheStoreRedis:
		rs, err := cache.NewRedisStore(cache.RedisOptions{
			Addr:               cc.Redis.Addr,
			Password:           cc.Redis.Password,
			DB:                 cc.Redis.DB,
			Prefix:             cc.Redis.Prefix,
			MaxEntriesPerScope: cc.MaxEntriesPerScope,
			DialTimeout:        cc.Redis.DialTimeout.Duration(),
			ReadTimeout:        cc.Redis.ReadTimeout.Duration(),
			WriteTimeout:       cc.Redis.WriteTimeout.Duration(),
			PoolSize:           cc.Redis.PoolSize,
		})
		if err != nil {
			// Failing to start is the honest outcome: a deployment configured
			// for a shared cache that silently ran on process memory would
			// serve different answers from different replicas, which is far
			// harder to diagnose than a refused start.
			return nil, fmt.Errorf("server: cache: redis at %s: %w", cc.Redis.Addr, err)
		}
		store, storeName = rs, rs.Name()
	default:
		store = cache.NewMemoryStore(cache.MemoryOptions{
			MaxEntriesPerScope: cc.MaxEntriesPerScope,
		})
		storeName = store.Name()
	}

	cfgCache := cache.Config{
		Enabled:               true,
		Threshold:             cc.Threshold,
		TTL:                   cc.TTL.Duration(),
		MaxEntriesPerScope:    cc.MaxEntriesPerScope,
		MinPromptChars:        cc.MinPromptChars,
		AllowNondeterministic: cc.AllowNondeterministic,
		AllowTools:            cc.AllowTools,
		Now:                   time.Now,
	}
	if logger != nil {
		logger.Info("semantic cache enabled",
			"store", storeName, "embedding", embName,
			"threshold", cc.Threshold, "ttl", cc.TTL.String())
	}
	return cache.New(cfgCache, store, emb, loggerFrom(logger)), nil
}

// handleCache reports the cache's configuration and counters.
func (s *Server) handleCache(w http.ResponseWriter, r *http.Request) {
	if s.cache == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	cfg := s.cache.Config()
	stats := s.cache.Stats()
	storeStats := s.cache.Store().Stats()

	mode := embedMode(s.cache)
	scopes := map[string]int{}
	if lister, ok := s.cache.Store().(interface {
		Scopes(context.Context) (map[string]int, error)
	}); ok {
		if got, err := lister.Scopes(r.Context()); err == nil {
			scopes = got
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": cfg.Enabled,
		"store":   s.cache.Store().Name(),
		"config": map[string]any{
			"threshold":              cfg.Threshold,
			"ttl":                    cfg.TTL.String(),
			"max_entries_per_scope":  cfg.MaxEntriesPerScope,
			"min_prompt_chars":       cfg.MinPromptChars,
			"allow_nondeterministic": cfg.AllowNondeterministic,
			"allow_tools":            cfg.AllowTools,
			"embedding":              mode,
		},
		"stats": map[string]any{
			"lookups":         stats.Lookups,
			"hits":            stats.Hits,
			"exact_hits":      stats.ExactHits,
			"semantic_hits":   stats.SemanticHits,
			"misses":          stats.Misses,
			"stores":          stats.Stores,
			"store_errors":    stats.StoreErrors,
			"lookup_errors":   stats.LookupErrors,
			"stale_evictions": stats.StaleEvictions,
			"collapsed":       stats.Collapsed,
			"hit_rate":        hitRate(stats),
			// Tokens a hit did not have to be generated again. Reported here
			// rather than folded into the upstream token totals, which must
			// keep describing what the providers actually produced.
			"saved_prompt_tokens":     stats.SavedPromptTokens,
			"saved_completion_tokens": stats.SavedCompletionTokens,
		},
		"store_stats": storeStats,
		"scopes":      scopes,
	})
}

// handleCacheFlush empties a scope, or every scope when none is named.
//
// A POST with an optional ?scope=: flushing everything on a GET would let a
// crawler or a link prefetch empty a shared cache.
func (s *Server) handleCacheFlush(w http.ResponseWriter, r *http.Request) {
	if s.cache == nil || !s.cache.Config().Enabled {
		writeJSON(w, http.StatusOK, map[string]any{"flushed": 0, "enabled": false})
		return
	}
	scope := r.URL.Query().Get("scope")
	n, err := s.cache.Flush(r.Context(), scope)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{
				"message": err.Error(),
				"type":    "infergate_cache_error",
			},
		})
		return
	}
	if scope == "" {
		s.log.Info("cache flushed", "scope", "all", "entries", n)
	} else {
		s.log.Info("cache flushed", "scope", scope, "entries", n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"flushed": n, "scope": scope})
}

// handleCacheLookup explains why a prompt would (or would not) hit.
//
// This exists because a semantic cache is otherwise impossible to debug from
// outside: "it missed" and "it matched the wrong entry" look the same in a
// response body, and the threshold is a number nobody can pick by reading the
// config file.
func (s *Server) handleCacheLookup(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prompt := q.Get("prompt")
	if prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": "prompt is required",
				"type":    "infergate_bad_request",
			},
		})
		return
	}
	if s.cache == nil || !s.cache.Config().Enabled {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "prompt": prompt})
		return
	}
	scope := q.Get("scope")
	emb := s.cache.Embedder()
	if emb == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": true, "prompt": prompt, "scope": scope,
			"features": embedFeatures(prompt),
			"matches":  []any{},
			"note":     "no embedder configured: this cache is exact-match only",
		})
		return
	}
	vecs, err := emb.Embed(r.Context(), []string{prompt})
	if err != nil || len(vecs) == 0 {
		msg := "embedder unavailable"
		if err != nil {
			msg = err.Error()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": true, "prompt": prompt, "scope": scope, "matches": []any{}, "note": msg,
		})
		return
	}
	threshold := s.cache.Config().Threshold
	if raw := q.Get("threshold"); raw != "" {
		if v, perr := parseFloat(raw); perr == nil {
			threshold = v
		}
	}
	matches, err := s.cache.Store().Search(r.Context(), scope, vecs[0], threshold, 5)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "infergate_cache_error"},
		})
		return
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Similarity > matches[j].Similarity })

	type matchView struct {
		Key        string  `json:"key"`
		Similarity float64 `json:"similarity"`
		Model      string  `json:"model"`
		Prompt     string  `json:"prompt"`
		AgeSeconds float64 `json:"age_seconds"`
		Hits       int64   `json:"hits"`
		ExpiresIn  string  `json:"expires_in"`
	}
	now := time.Now()
	views := make([]matchView, 0, len(matches))
	for _, m := range matches {
		views = append(views, matchView{
			Key:        m.Entry.Key,
			Similarity: m.Similarity,
			Model:      m.Entry.Model,
			Prompt:     m.Entry.Prompt,
			AgeSeconds: now.Sub(m.Entry.CreatedAt).Seconds(),
			Hits:       m.Entry.Hits,
			ExpiresIn:  time.Until(m.Entry.ExpiresAt).Round(time.Second).String(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":   true,
		"prompt":    prompt,
		"scope":     scope,
		"threshold": threshold,
		"features":  embedFeatures(prompt),
		"matches":   views,
		"note":      "matches are limited to one scope: a scope is a tenant plus a model",
	})
}

// Cache exposes the cache for tests and shutdown.
func (s *Server) Cache() *cache.Cache { return s.cache }

// CloseCache releases the cache's resources (a Redis pool, a janitor
// goroutine). It is called during shutdown after the listener has stopped, so
// no request is still using it.
func (s *Server) CloseCache() error {
	if s.cache == nil {
		return nil
	}
	return s.cache.Close()
}

// embedMode describes the embedding configuration for /admin/cache.
func embedMode(c *cache.Cache) map[string]any {
	cfg := c.Config()
	emb := c.Embedder()
	if emb == nil {
		return map[string]any{"provider": "none", "dims": 0, "lexical": true}
	}
	name := emb.Name()
	if wrapper, ok := emb.(*embed.Fallback); ok && wrapper.Primary != nil {
		return map[string]any{
			"provider": name,
			"dims":     emb.Dims(),
			"model":    wrapper.Primary.Name(),
			"fallback": "hashing",
			"note":     "an embedding outage degrades matching to lexical",
			"lexical":  false,
			"enabled":  cfg.Enabled,
		}
	}
	return map[string]any{"provider": name, "dims": emb.Dims(), "lexical": true}
}

// embedFeatures exposes the tokens/hashes the lexical embedder keys on, so a
// surprising similarity can be explained instead of argued about.
func embedFeatures(prompt string) []string {
	f := embed.Features(prompt)
	if len(f) > 64 {
		return f[:64]
	}
	return f
}

// hitRate is the share of lookups the cache answered.
func hitRate(s cache.Stats) float64 {
	if s.Lookups == 0 {
		return 0
	}
	return float64(s.Hits) / float64(s.Lookups)
}

// parseFloat parses a query parameter. A malformed value is reported so the
// handler can fall back to the configured value instead of to zero, which would
// silently turn every lookup into a match.
func parseFloat(raw string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(raw), 64)
}
