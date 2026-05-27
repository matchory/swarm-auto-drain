package metrics_test

import (
	"context"
	"testing"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

func TestNew_NoReadersConfigured_ReturnsNoop(t *testing.T) {
	m, handler, shutdown, err := metrics.New(context.Background(), metrics.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if handler != nil {
		t.Errorf("handler should be nil when no Prom listener configured, got %T", handler)
	}
	if shutdown != nil {
		t.Errorf("shutdown should be nil when no readers configured")
	}
	if m.Lock.Acquire == nil {
		t.Error("Metrics still must be populated (noop-backed)")
	}
}
