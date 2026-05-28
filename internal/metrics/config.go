package metrics

import "time"

// Config drives metrics.New. Both ListenAddr and OTLPEndpoint default
// to "" — empty means the corresponding reader is not registered. With
// both empty, New returns a noop-backed Metrics.
type Config struct {
	// ListenAddr is the bind address for the Prometheus exporter's
	// HTTP /metrics handler. Empty disables the Prom reader.
	ListenAddr string
	// OTLPEndpoint is the standard OTLP collector endpoint. Empty
	// disables the OTLP push reader. Other OTLP env vars
	// (OTEL_EXPORTER_OTLP_HEADERS, _INSECURE, etc.) are honored by
	// the SDK's default-from-env constructor.
	OTLPEndpoint string
	// ServiceVersion populates the service.version resource attr.
	ServiceVersion string
	// NodeHostname populates the host.name resource attr.
	NodeHostname string
	// InitialRole populates the swarm.role resource attr (snapshot at
	// startup; the role can flip later — see spec §3.6).
	InitialRole string
	// PushInterval is the cadence for the OTLP PeriodicReader.
	// Ignored if OTLPEndpoint is empty.
	PushInterval time.Duration
}
