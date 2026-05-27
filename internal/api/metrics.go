package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// knownRoutes is the fixed allowlist of route templates. Unknown paths
// bucket to "other" so cardinality stays bounded regardless of probe
// or attack traffic.
//
//nolint:gochecknoglobals // immutable lookup table; const-equivalent for maps
var knownRoutes = map[string]string{
	"/healthz":         "/healthz",
	"/role":            "/role",
	"/managers/health": "/managers/health",
	"/lock":            "/lock",
	"/drain":           "/drain",
	"/active":          "/active",
	"/metrics":         "/metrics",
}

// Metrics wraps next with RED-style counter and histogram recording.
// Install OUTSIDE AuditLog so the middleware overhead is itself
// measured by the histogram.
func Metrics(next http.Handler, m metrics.HTTPMetrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sr, r)
		dur := time.Since(start).Seconds()

		route := knownRoutes[r.URL.Path]
		if route == "" {
			route = "other"
		}
		baseAttrs := []attribute.KeyValue{
			attribute.String("method", r.Method),
			attribute.String("route", route),
		}
		m.Requests.Add(r.Context(), 1, otelmetric.WithAttributes(
			append(baseAttrs, attribute.String("status", strconv.Itoa(sr.status)))...,
		))
		m.Duration.Record(r.Context(), dur, otelmetric.WithAttributes(baseAttrs...))
	})
}
