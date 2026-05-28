package metrics

// This file exposes helpers that other packages' tests import to wire a
// ManualReader-backed provider and inspect recorded data points. The helpers
// live in a regular .go file (not _test.go) because Go does not export
// _test.go symbols across packages.

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// NewForTesting builds a Metrics struct backed by the supplied
// MeterProvider. Use this when a test wires its own ManualReader and
// wants the Metrics struct attached to the same provider.
func NewForTesting(provider *sdkmetric.MeterProvider) Metrics {
	m, err := build(provider.Meter(meterName))
	if err != nil {
		// Programmer bug — test helpers fail loud.
		panic("metrics.NewForTesting: " + err.Error())
	}
	return m
}

// NewManualHarness returns a ManualReader and a Metrics struct wired to
// the same MeterProvider, ready for tests to record-then-collect.
// Registers a t.Cleanup that shuts the provider down so tests don't leak.
func NewManualHarness(tb testing.TB) (*sdkmetric.ManualReader, Metrics) {
	tb.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tb.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})
	return reader, NewForTesting(provider)
}

// FindCounter collects from reader and returns the named counter's data
// points. Fails the test if the metric isn't found. Pair with
// PointWithAttrs when you care about a specific attribute combination.
func FindCounter(tb testing.TB, reader *sdkmetric.ManualReader, name string) []metricdata.DataPoint[int64] {
	tb.Helper()
	return findInt64Points(tb, reader, name)
}

// FindHistogramCount returns the cumulative count of observations for
// the named histogram across all attribute sets. Use this for "at least
// one observation" assertions where exact attributes are tested
// separately. Fails the test if the metric isn't found.
func FindHistogramCount(tb testing.TB, reader *sdkmetric.ManualReader, name string) uint64 {
	tb.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		tb.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if d, ok := m.Data.(metricdata.Histogram[float64]); ok {
				var total uint64
				for _, dp := range d.DataPoints {
					total += dp.Count
				}
				return total
			}
		}
	}
	tb.Fatalf("metric %q not found or wrong type", name)
	return 0
}

// FindGauge returns the data points of the named int64 gauge. Fails the
// test if not found. Pair with PointWithAttrs to select a specific
// attribute set.
func FindGauge(tb testing.TB, reader *sdkmetric.ManualReader, name string) []metricdata.DataPoint[int64] {
	tb.Helper()
	return findInt64Points(tb, reader, name)
}

// findInt64Points returns the data points of any int64 sum (counter) or
// gauge with the given name.
func findInt64Points(tb testing.TB, reader *sdkmetric.ManualReader, name string) []metricdata.DataPoint[int64] {
	tb.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		tb.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				return d.DataPoints
			case metricdata.Gauge[int64]:
				return d.DataPoints
			}
		}
	}
	tb.Fatalf("metric %q not found", name)
	return nil
}

// PointWithAttrs returns the data point whose attribute set contains
// every supplied key=value pair, or fails the test. Pass attributes as
// flat key, value, key, value, ... strings.
func PointWithAttrs(tb testing.TB, points []metricdata.DataPoint[int64], kv ...string) metricdata.DataPoint[int64] {
	tb.Helper()
	if len(kv)%2 != 0 {
		tb.Fatalf("PointWithAttrs: kv must be pairs, got %d", len(kv))
	}
	for _, dp := range points {
		if matchAttrs(dp.Attributes.ToSlice(), kv) {
			return dp
		}
	}
	tb.Fatalf("no data point with attrs %v among %d points", kv, len(points))
	return metricdata.DataPoint[int64]{}
}

// matchAttrs reports whether every want key=value pair appears in have.
func matchAttrs(have []attribute.KeyValue, want []string) bool {
	for i := 0; i < len(want); i += 2 {
		key := want[i]
		val := want[i+1]
		found := false
		for _, kv := range have {
			if string(kv.Key) == key && kv.Value.AsString() == val {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
