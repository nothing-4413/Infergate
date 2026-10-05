// Command mockcollector is a throwaway OTLP/HTTP trace sink for the acceptance
// scripts.
//
// The Go acceptance gate (cmd/verify-m5) already proves the exporter against an
// httptest collector inside the same process, which is the right place to assert
// the payload's shape. This binary exists so the curl-level gate can prove the
// same thing across a real process boundary: a gateway started by
// scripts/verify-m5.ps1 posts to this listener over a real socket, and the
// script then asks the collector what it received.
//
// It accepts any path (so a wrong-path defect is observable rather than a 404
// the script would have to guess about), records the path, content type, byte
// count and SHA-256 of every request body, and answers:
//
//	GET  /healthz   200 ok
//	GET  /requests  {"count":N,"requests":[{path,content_type,bytes,sha256}]}
//	GET  /dump      the recorded bodies, separated by a line of dashes
//
// The payload bodies are kept in memory (a trace export is kilobytes, and the
// gates run for seconds), and /dump is what a failing assertion prints to show
// what actually arrived.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type recorded struct {
	Path        string              `json:"path"`
	ContentType string              `json:"content_type"`
	Bytes       int                 `json:"bytes"`
	SHA256      string              `json:"sha256"`
	Header      map[string][]string `json:"header"`
	body        string
}

type collector struct {
	mu      sync.Mutex
	records []recorded
}

func (c *collector) add(path string, header http.Header, contentType, body string) recorded {
	sum := sha256.Sum256([]byte(body))
	rec := recorded{
		Path:        path,
		ContentType: contentType,
		Bytes:       len(body),
		SHA256:      hex.EncodeToString(sum[:]),
		Header:      map[string][]string(header.Clone()),
		body:        body,
	}
	c.mu.Lock()
	c.records = append(c.records, rec)
	c.mu.Unlock()
	return rec
}

func (c *collector) snapshot() []recorded {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recorded(nil), c.records...)
}

func main() {
	listen := flag.String("listen", "127.0.0.1:19999", "address to listen on")
	name := flag.String("name", "mockcollector", "name used in log lines")
	maxBody := flag.Int64("max-body-bytes", 8<<20, "largest body accepted")
	quiet := flag.Bool("quiet", false, "log only errors")
	flag.Parse()

	logger := log.New(os.Stderr, "collector "+*name+" ", log.LstdFlags|log.Lmicroseconds)
	c := &collector{}

	write := func(w http.ResponseWriter, code int, contentType, body string) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, "application/json", `{"status":"ok"}`)
	})
	mux.HandleFunc("/requests", func(w http.ResponseWriter, r *http.Request) {
		recs := c.snapshot()
		out := struct {
			Count    int        `json:"count"`
			Requests []recorded `json:"requests"`
		}{Count: len(recs), Requests: recs}
		if out.Requests == nil {
			out.Requests = []recorded{}
		}
		raw, err := json.Marshal(out)
		if err != nil {
			write(w, http.StatusInternalServerError, "application/json", `{"error":"marshal"}`)
			return
		}
		write(w, http.StatusOK, "application/json", string(raw))
	})
	mux.HandleFunc("/dump", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		for i, rec := range c.snapshot() {
			if i > 0 {
				b.WriteString("\n--------------------------------------------------\n")
			}
			b.WriteString(rec.body)
		}
		write(w, http.StatusOK, "text/plain; charset=utf-8", b.String())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, *maxBody+1))
		if err != nil {
			write(w, http.StatusBadRequest, "application/json", `{"error":"read body"}`)
			return
		}
		if int64(len(raw)) > *maxBody {
			write(w, http.StatusRequestEntityTooLarge, "application/json", `{"error":"body too large"}`)
			return
		}
		rec := c.add(r.URL.Path, r.Header, r.Header.Get("Content-Type"), string(raw))
		if !*quiet {
			logger.Printf("POST %s %d bytes sha256=%s", rec.Path, rec.Bytes, rec.SHA256[:12])
		}
		// The OTLP/HTTP response body; a collector that answers 200 with an
		// empty body is also legal, but {"partialSuccess":{}} is what the
		// specification shows and what a real collector returns.
		write(w, http.StatusOK, "application/json", `{"partialSuccess":{}}`)
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		logger.Printf("shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	if !*quiet {
		logger.Printf("listening on %s", *listen)
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "collector: %v\n", err)
		os.Exit(1)
	}
}
