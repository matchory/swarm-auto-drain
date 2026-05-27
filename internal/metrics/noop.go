package metrics

import "go.opentelemetry.io/otel/metric/noop"

// NewNoop returns a Metrics whose instrument handles are backed by an
// OpenTelemetry noop provider. Recording is cheap and goes nowhere.
// Used when no readers are configured and as the default in tests that
// don't care about metric values.
func NewNoop() Metrics {
	m, err := build(noop.NewMeterProvider().Meter("swarm-auto-drain"))
	if err != nil {
		// Noop provider does not return errors from instrument
		// creation; if build ever does, that's a programmer bug we
		// want to find immediately.
		panic("metrics: build against noop provider failed: " + err.Error())
	}
	return m
}
