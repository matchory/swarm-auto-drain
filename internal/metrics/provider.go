// Package metrics: see metrics.go.
package metrics

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

const meterName = "swarm-auto-drain"

// defaultPushInterval is the OTLP PeriodicReader cadence used when
// Config.PushInterval is unset or non-positive.
const defaultPushInterval = 15 * time.Second

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
		m, err := build(noop.NewMeterProvider().Meter(meterName))
		if err != nil {
			return Metrics{}, nil, nil, fmt.Errorf("build noop: %w", err)
		}
		return m, nil, nil, nil
	}

	res, err := buildResource(cfg)
	if err != nil {
		return Metrics{}, nil, nil, fmt.Errorf("resource: %w", err)
	}

	providerOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}

	var promHandler http.Handler
	if cfg.ListenAddr != "" {
		// Use an explicit registry so the OTel Prometheus reader doesn't
		// register against the global default registry.
		reg := prometheus.NewRegistry()
		promReader, promErr := otelprom.New(otelprom.WithRegisterer(reg))
		if promErr != nil {
			return Metrics{}, nil, nil, fmt.Errorf("prometheus exporter: %w", promErr)
		}
		providerOpts = append(providerOpts, sdkmetric.WithReader(promReader))
		promHandler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	}

	if cfg.OTLPEndpoint != "" {
		// otlpmetrichttp.WithEndpoint expects host:port (no scheme); the
		// config layer is responsible for stripping any URL form.
		otlpExp, otlpErr := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpoint(cfg.OTLPEndpoint))
		if otlpErr != nil {
			return Metrics{}, nil, nil, fmt.Errorf("otlp exporter: %w", otlpErr)
		}
		interval := cfg.PushInterval
		if interval <= 0 {
			interval = defaultPushInterval
		}
		providerOpts = append(providerOpts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(otlpExp, sdkmetric.WithInterval(interval)),
		))
	}

	provider := sdkmetric.NewMeterProvider(providerOpts...)

	// Register Go runtime instrumentation against the real provider only.
	// The noop branch above returns before reaching this point — runtime
	// metrics spin up their own goroutine and timer; no point paying that
	// cost when the data goes nowhere.
	if rtErr := runtime.Start(
		runtime.WithMeterProvider(provider),
		runtime.WithMinimumReadMemStatsInterval(time.Second),
	); rtErr != nil {
		return Metrics{}, nil, nil, fmt.Errorf("runtime instrumentation: %w", rtErr)
	}

	m, err := build(provider.Meter(meterName))
	if err != nil {
		return Metrics{}, nil, nil, fmt.Errorf("build instruments: %w", err)
	}
	shutdown := func(sCtx context.Context) error {
		if sErr := provider.Shutdown(sCtx); sErr != nil {
			return fmt.Errorf("metric provider shutdown: %w", sErr)
		}
		return nil
	}
	return m, promHandler, shutdown, nil
}

func buildResource(cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(meterName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	}
	if cfg.NodeHostname != "" {
		attrs = append(attrs, semconv.HostName(cfg.NodeHostname))
	}
	if cfg.InitialRole != "" {
		attrs = append(attrs, attribute.String("swarm.role", cfg.InitialRole))
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes("", attrs...))
	if err != nil {
		return nil, fmt.Errorf("merge resource: %w", err)
	}
	return res, nil
}
