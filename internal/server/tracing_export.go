package server

import (
	"fmt"

	"github.com/infergate/infergate/internal/config"
	"github.com/infergate/infergate/internal/gateway"
	"github.com/infergate/infergate/internal/traceexport"
)

// buildTraceExporters turns the tracing config into live exporters.
//
// Both sinks are optional and independent: a deployment may want a durable
// local JSONL for debugging, an OTLP collector for a shared dashboard, or both.
// Each one owns a background worker, so nothing here touches the network or the
// filesystem on the request path — Export() only enqueues.
//
// A configured sink that cannot be constructed is an error, not a warning: the
// alternative is a gateway that happily serves traffic while the trace pipeline
// the operator configured writes nothing at all.
func buildTraceExporters(tc config.TracingConfig, logger logAdapter) ([]gateway.TraceExporter, []namedCloser, []traceReporter, error) {
	var (
		exporters []gateway.TraceExporter
		closers   []namedCloser
		report    []traceReporter
	)

	if path := tc.JSONLPath; path != "" {
		exporter, err := traceexport.NewJSONL(traceexport.JSONLOptions{
			Path:   path,
			Logger: loggerFrom(logger),
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("server: tracing: jsonl export: %w", err)
		}
		exporters = append(exporters, exporter)
		closers = append(closers, namedCloser{name: "jsonl", close: exporter.Close})
		report = append(report, traceReporter{
			name: "jsonl",
			stats: func() map[string]any {
				return map[string]any{
					"path":    path,
					"written": exporter.Written(),
					"dropped": exporter.Dropped(),
					// Err() is the writer's last failure (a full disk, a closed
					// fd). Reported rather than logged per drop: a failing sink
					// fails in bursts and a line per drop is its own flood.
					"error": errString(exporter.Err()),
				}
			},
		})
	}

	if endpoint := tc.OTLP.Endpoint; endpoint != "" {
		exporter, err := traceexport.NewOTLP(traceexport.OTLPOptions{
			Endpoint:    endpoint,
			Timeout:     tc.OTLP.Timeout.Duration(),
			ServiceName: tc.OTLP.ServiceName,
			Headers:     tc.OTLP.Headers,
			Logger:      loggerFrom(logger),
		})
		if err != nil {
			// Close what already exists before giving up, or a JSONL worker
			// keeps running for a server that will never be built.
			for _, c := range closers {
				_ = c.close()
			}
			return nil, nil, nil, fmt.Errorf("server: tracing: otlp export: %w", err)
		}
		exporters = append(exporters, exporter)
		closers = append(closers, namedCloser{name: "otlp", close: exporter.Close})
		report = append(report, traceReporter{
			name: "otlp",
			stats: func() map[string]any {
				st := exporter.Stats()
				return map[string]any{
					"endpoint":    endpoint,
					"exported":    st.Exported,
					"failed":      st.Failed,
					"dropped":     st.Dropped,
					"last_error":  st.LastError,
					"last_status": st.LastStatus,
				}
			},
		})
	}

	return exporters, closers, report, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
