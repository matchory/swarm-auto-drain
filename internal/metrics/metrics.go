// Package metrics owns the coordinator's OpenTelemetry metric provider
// and exports the typed instrument handles that domain packages record
// against. See docs/specs/2026-05-27-metrics-design.md.
package metrics

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// Metrics is the full set of instruments exposed by the coordinator.
// Each sub-struct corresponds to a subsystem; domain packages receive
// only the sub-struct they need so the dependency surface stays narrow.
type Metrics struct {
	Slots    SlotMetrics
	Drain    DrainMetrics
	Identity IdentityMetrics
	HTTP     HTTPMetrics
}

// SlotMetrics covers reboot-slot outcomes.
type SlotMetrics struct {
	Acquire metric.Int64Counter     // attrs: kind, outcome
	Release metric.Int64Counter     // attrs: outcome
	Hold    metric.Float64Histogram // unit: seconds, acquire to release
}

// DrainMetrics covers node-drain outcomes and duration.
type DrainMetrics struct {
	Total          metric.Int64Counter     // attrs: outcome
	Duration       metric.Float64Histogram // unit: seconds
	TasksRemaining metric.Int64Gauge
	QuorumCheck    metric.Int64Counter // attrs: result
}

// IdentityMetrics covers caller-identity resolution.
type IdentityMetrics struct {
	Resolutions metric.Int64Counter // attrs: source, outcome
}

// HTTPMetrics covers per-request RED metrics.
type HTTPMetrics struct {
	Requests metric.Int64Counter     // attrs: method, route, status
	Duration metric.Float64Histogram // attrs: method, route; unit: seconds
}

// build wires a Metrics struct from a meter provider. Used by both the
// production constructor (New) and the test/noop constructors.
func build(meter metric.Meter) (Metrics, error) {
	var m Metrics
	var err error
	if m.Slots.Acquire, err = meter.Int64Counter("swarm_auto_drain.slots.acquire", metric.WithDescription("Slot acquire attempts by kind and outcome.")); err != nil {
		return Metrics{}, fmt.Errorf("slots.acquire: %w", err)
	}
	if m.Slots.Release, err = meter.Int64Counter("swarm_auto_drain.slots.release", metric.WithDescription("Slot releases by outcome.")); err != nil {
		return Metrics{}, fmt.Errorf("slots.release: %w", err)
	}
	if m.Slots.Hold, err = meter.Float64Histogram("swarm_auto_drain.slots.hold", metric.WithUnit("s"), metric.WithDescription("Time a slot was held, acquire to release.")); err != nil {
		return Metrics{}, fmt.Errorf("slots.hold: %w", err)
	}

	if m.Drain.Total, err = meter.Int64Counter("swarm_auto_drain.drain.total", metric.WithDescription("Drain attempts by outcome.")); err != nil {
		return Metrics{}, fmt.Errorf("drain.total: %w", err)
	}
	if m.Drain.Duration, err = meter.Float64Histogram("swarm_auto_drain.drain.duration", metric.WithUnit("s"), metric.WithDescription("Drain duration in seconds.")); err != nil {
		return Metrics{}, fmt.Errorf("drain.duration: %w", err)
	}
	if m.Drain.TasksRemaining, err = meter.Int64Gauge("swarm_auto_drain.drain.tasks_remaining", metric.WithDescription("Tasks remaining on the node during an active drain.")); err != nil {
		return Metrics{}, fmt.Errorf("drain.tasks_remaining: %w", err)
	}
	if m.Drain.QuorumCheck, err = meter.Int64Counter("swarm_auto_drain.drain.quorum_check", metric.WithDescription("Pre-drain quorum checks by result.")); err != nil {
		return Metrics{}, fmt.Errorf("drain.quorum_check: %w", err)
	}

	if m.Identity.Resolutions, err = meter.Int64Counter("swarm_auto_drain.identity.resolutions", metric.WithDescription("Identity resolutions by source and outcome.")); err != nil {
		return Metrics{}, fmt.Errorf("identity.resolutions: %w", err)
	}

	if m.HTTP.Requests, err = meter.Int64Counter("swarm_auto_drain.http.requests", metric.WithDescription("HTTP requests by method/route/status.")); err != nil {
		return Metrics{}, fmt.Errorf("http.requests: %w", err)
	}
	if m.HTTP.Duration, err = meter.Float64Histogram("swarm_auto_drain.http.duration", metric.WithUnit("s"), metric.WithDescription("HTTP request duration.")); err != nil {
		return Metrics{}, fmt.Errorf("http.duration: %w", err)
	}

	return m, nil
}
