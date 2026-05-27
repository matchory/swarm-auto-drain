package metrics_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestNew_PromListener_ReturnsHandler(t *testing.T) {
	cfg := metrics.Config{
		ListenAddr:     ":0", // not actually bound here; just a non-empty marker
		ServiceVersion: "v0-test",
		NodeHostname:   "node-a",
		InitialRole:    "manager",
	}
	m, handler, shutdown, err := metrics.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if handler == nil {
		t.Fatal("handler must be non-nil when Prom listener configured")
	}
	if shutdown == nil {
		t.Fatal("shutdown must be non-nil when any reader is attached")
	}
	if m.Lock.Acquire == nil {
		t.Fatal("Metrics must be populated")
	}

	// Sanity: handler serves Prometheus text format.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") && !strings.HasPrefix(ct, "application/openmetrics-text") {
		t.Errorf("unexpected content-type %q", ct)
	}

	if shutdownErr := shutdown(context.Background()); shutdownErr != nil {
		t.Errorf("shutdown: %v", shutdownErr)
	}
}

func TestNew_OTLPOnly_ReturnsShutdown(t *testing.T) {
	cfg := metrics.Config{
		OTLPEndpoint:   "127.0.0.1:1", // unreachable; we never push during the test
		PushInterval:   60 * time.Second,
		ServiceVersion: "v0-test",
		NodeHostname:   "node-a",
	}
	m, handler, shutdown, err := metrics.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if handler != nil {
		t.Error("handler should be nil when no Prom listener configured")
	}
	if shutdown == nil {
		t.Fatal("shutdown must be non-nil when OTLP reader attached")
	}
	if m.Drain.Total == nil {
		t.Error("Metrics must be populated")
	}
	// Cancel quickly so the shutdown's PeriodicReader.ForceFlush doesn't
	// dial the unreachable endpoint.
	sCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_ = shutdown(sCtx) // error from unreachable endpoint is expected and tolerated
}

func TestNew_RuntimeMetricsRegisteredOnRealProvider(t *testing.T) {
	cfg := metrics.Config{ListenAddr: ":0"}
	_, handler, shutdown, err := metrics.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	// Contrib's goroutine metric has been renamed across versions:
	// process_runtime_go_goroutines (oldest), runtime_go_goroutines, and
	// go_goroutine_count (v0.68+). Accept any of them.
	if !strings.Contains(body, "process_runtime_go_goroutines") &&
		!strings.Contains(body, "runtime_go_goroutines") &&
		!strings.Contains(body, "go_goroutine_count") {
		end := len(body)
		if end > 500 {
			end = 500
		}
		t.Errorf("expected runtime metrics in /metrics output; got:\n%s", body[:end])
	}
}
