// Package metrics: see metrics.go.
package metrics

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/metric/noop"
)

// ShutdownFunc flushes both exporters with a caller-supplied budget.
// A nil ShutdownFunc means there is nothing to flush.
type ShutdownFunc func(context.Context) error

// New constructs the metric provider, attaches whichever readers Config
// asks for, and returns the resulting Metrics, the HTTP handler for the
// Prometheus exporter (nil if disabled), and a shutdown func.
//
// With both ListenAddr and OTLPEndpoint empty, falls back to a noop
// provider — domain code records as usual and the values go nowhere.
func New(ctx context.Context, cfg Config) (Metrics, http.Handler, ShutdownFunc, error) {
	if cfg.ListenAddr == "" && cfg.OTLPEndpoint == "" {
		m, err := build(noop.NewMeterProvider().Meter("swarm-auto-drain"))
		if err != nil {
			return Metrics{}, nil, nil, fmt.Errorf("build noop: %w", err)
		}
		return m, nil, nil, nil
	}
	// Real-provider construction is wired in subsequent tasks.
	return Metrics{}, nil, nil, fmt.Errorf("real provider not yet implemented")
}
