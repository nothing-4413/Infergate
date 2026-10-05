// Command infergate is the InferGate gateway process.
//
// M0 surface: an OpenAI-compatible reverse proxy that speaks SSE, so any
// OpenAI SDK pointed at this address works unchanged. Later milestones add
// routing, semantic caching and cost governance behind the same listener
// without changing the wire contract.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/logging"
	"github.com/infergate/infergate/internal/server"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=1.2.3" ./cmd/infergate
var version = "0.1.0-m0-dev"

func main() {
	configPath := flag.String("config", "configs/infergate.yaml", "path to the YAML configuration file")
	check := flag.Bool("check", false, "validate the configuration and exit without serving")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("infergate", version)
		return
	}

	// Validate before anything else so a typo fails at startup with a clear
	// message instead of as a mysterious 502 on the first request.
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "infergate: configuration error: %v\n", err)
		os.Exit(1)
	}

	logger, err := logging.New(os.Stdout, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "infergate: logger error: %v\n", err)
		os.Exit(1)
	}

	if *check {
		fmt.Printf("configuration OK: %d upstream(s) from %s\n", len(cfg.Upstreams), *configPath)
		return
	}

	srv, err := server.NewServer(cfg, logger)
	if err != nil {
		logger.Error("startup failed", "err", err)
		os.Exit(1)
	}
	srv.SetVersion(version)

	// Shut down on SIGINT/SIGTERM, draining in-flight requests (including
	// streams) for the configured grace period.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		logger.Info("shutdown signal received", "timeout", cfg.Server.ShutdownTimeout.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Duration())
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "err", err)
		}
	}()

	if err := srv.ListenAndServe(); err != nil {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
	// The cache is closed after the listener has stopped, so no request can
	// still be holding a Redis connection from its pool.
	if err := srv.CloseCache(); err != nil {
		logger.Warn("cache close failed", "err", err)
	}
	// Same reasoning for the quota store: a settle that is still in flight must
	// be able to reach its counters.
	if err := srv.CloseQuota(); err != nil {
		logger.Warn("quota close failed", "err", err)
	}
	logger.Info("stopped cleanly")
	// Give the log line above a chance to reach the console before exit.
	time.Sleep(10 * time.Millisecond)
}
