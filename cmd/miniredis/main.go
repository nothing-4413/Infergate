// Command miniredis runs the in-repo RESP2 server as a standalone process, so
// the semantic cache's Redis backend can be verified end to end without a
// Redis installation.
//
// It exists because the M2 acceptance path must be runnable on a machine with
// no Docker daemon and no Redis service, while still exercising a real socket,
// a real request/response protocol and real serialisation. It is NOT a Redis
// replacement: it implements the subset of commands the cache store issues, and
// the docs say so. Pointing the gateway at a stock Redis is a configuration
// change (cache.redis.addr), and docker-compose.yml brings one up for anyone who
// wants the production dependency in the loop.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/infergate/infergate/internal/logging"
	"github.com/infergate/infergate/internal/mockredis"
)

func main() {
	var (
		listen      = flag.String("listen", ":6399", "address to listen on (6399, not 6379, so a real Redis on this host is never shadowed)")
		requirePass = flag.String("requirepass", "", "password to require; empty means no AUTH")
		maxKeys     = flag.Int("max-keys", 4096, "approximate key cap; beyond it the least recently used key is evicted")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn, error")
		logFormat   = flag.String("log-format", "text", "log format: text or json")
	)
	flag.Parse()

	logger, err := logging.New(os.Stderr, *logLevel, *logFormat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniredis: bad logging options: %v\n", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := mockredis.New(mockredis.Options{
		Addr:        *listen,
		RequirePass: *requirePass,
		MaxKeys:     *maxKeys,
		Logger:      logger,
	})
	if err := srv.Start(); err != nil {
		logger.Error("cannot start", slog.String("error", err.Error()), slog.String("listen", *listen))
		os.Exit(1)
	}
	logger.Info("miniredis listening",
		slog.String("addr", srv.Addr()),
		slog.Bool("auth", *requirePass != ""),
		slog.Int("max_keys", *maxKeys),
		// Stated at startup so nobody mistakes this for a full Redis.
		slog.String("note", "in-repo RESP2 subset for verification; not a Redis replacement"),
	)

	fmt.Printf("miniredis listening on %s\n", srv.Addr())

	<-ctx.Done()
	logger.Info("shutting down")
	if err := srv.Close(); err != nil {
		logger.Warn("close", slog.String("error", err.Error()))
	}

	st := srv.Stats()
	logger.Info("stopped",
		slog.Int64("commands", st.Commands),
		slog.Int64("connections", st.Connections),
		slog.Int64("keys", st.Keys),
		slog.Int64("evictions", st.Evictions),
		slog.Int64("expired", st.Expired),
	)
}
