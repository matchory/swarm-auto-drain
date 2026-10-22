package metrics_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

func TestManualHarness_RecordsAndFinds(t *testing.T) {
	reader, m := metrics.NewManualHarness(t)
	m.Slots.Acquire.Add(
		context.Background(),
		1,
		metric.WithAttributes(attribute.String("outcome", "acquired")),
	)

	points := metrics.FindCounter(t, reader, "swarm_auto_drain.slots.acquire")
	if len(points) == 0 {
		t.Fatal("expected at least one data point")
	}

	dp := metrics.PointWithAttrs(t, points, "outcome", "acquired")
	if dp.Value != 1 {
		t.Errorf("expected value 1, got %d", dp.Value)
	}
}
