package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matchory/swarm-auto-drain/internal/api"
	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

func TestMetricsMiddleware_KnownRouteCounted(t *testing.T) {
	reader, m := metrics.NewManualHarness(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	h := api.Metrics(inner, m.HTTP)

	req := httptest.NewRequest(http.MethodPost, "/acquire", http.NoBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status passthrough broken: %d", w.Code)
	}
	points := metrics.FindCounter(t, reader, "swarm_auto_drain.http.requests")
	dp := metrics.PointWithAttrs(t, points, "method", "POST", "route", "/acquire", "status", "201")
	if dp.Value != 1 {
		t.Errorf("expected count 1, got %d", dp.Value)
	}
}

func TestMetricsMiddleware_UnknownRouteBucketsToOther(t *testing.T) {
	reader, m := metrics.NewManualHarness(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := api.Metrics(inner, m.HTTP)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/garbage", http.NoBody))

	points := metrics.FindCounter(t, reader, "swarm_auto_drain.http.requests")
	_ = metrics.PointWithAttrs(t, points, "route", "other")
}

func TestMetricsMiddleware_DurationObserved(t *testing.T) {
	reader, m := metrics.NewManualHarness(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := api.Metrics(inner, m.HTTP)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/status", http.NoBody))

	count := metrics.FindHistogramCount(t, reader, "swarm_auto_drain.http.duration")
	if count == 0 {
		t.Fatal("expected at least one histogram observation")
	}
}
