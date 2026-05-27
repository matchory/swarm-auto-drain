package metrics_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
