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
	Lock     LockMetrics
	Drain    DrainMetrics
	Role     RoleMetrics
	Forward  ForwardMetrics
	Identity IdentityMetrics
	HTTP     HTTPMetrics
}

// LockMetrics covers cluster-mutex outcomes.
type LockMetrics struct {
	Acquire metric.Int64Counter     // attrs: outcome
	Release metric.Int64Counter     // attrs: outcome
	Steals  metric.Int64Counter     // attrs: reason
	Hold    metric.Float64Histogram // unit: seconds
	Held    metric.Int64Gauge
}

// DrainMetrics covers node-drain outcomes and duration.
type DrainMetrics struct {
	Total          metric.Int64Counter     // attrs: outcome
	Duration       metric.Float64Histogram // unit: seconds
	TasksRemaining metric.Int64Gauge
	QuorumCheck    metric.Int64Counter // attrs: result
}

// RoleMetrics covers the role detector's view of swarm role.
type RoleMetrics struct {
	Current     metric.Int64Gauge   // 0=worker, 1=manager
	Refresh     metric.Int64Counter // attrs: trigger, outcome
	Transitions metric.Int64Counter // attrs: from, to
}

// ForwardMetrics covers worker→manager proxy behavior.
type ForwardMetrics struct {
	Attempts           metric.Int64Counter     // attrs: outcome
	Duration           metric.Float64Histogram // unit: seconds
	CacheInvalidations metric.Int64Counter
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
//
//nolint:gocyclo,cyclop,revive // one branch per instrument; flat is easier to audit than indirection.
func build(meter metric.Meter) (Metrics, error) {
	var m Metrics
	var err error
	if m.Lock.Acquire, err = meter.Int64Counter("swarm_auto_drain.lock.acquire", metric.WithDescription("Lock acquire attempts by outcome.")); err != nil {
		return Metrics{}, fmt.Errorf("lock.acquire: %w", err)
	}
	if m.Lock.Release, err = meter.Int64Counter("swarm_auto_drain.lock.release", metric.WithDescription("Lock release attempts by outcome.")); err != nil {
		return Metrics{}, fmt.Errorf("lock.release: %w", err)
	}
	if m.Lock.Steals, err = meter.Int64Counter("swarm_auto_drain.lock.steals", metric.WithDescription("Stale-lock steals by reason.")); err != nil {
		return Metrics{}, fmt.Errorf("lock.steals: %w", err)
	}
	if m.Lock.Hold, err = meter.Float64Histogram("swarm_auto_drain.lock.hold", metric.WithUnit("s"), metric.WithDescription("Lock hold duration in seconds.")); err != nil {
		return Metrics{}, fmt.Errorf("lock.hold: %w", err)
	}
	if m.Lock.Held, err = meter.Int64Gauge("swarm_auto_drain.lock.held", metric.WithDescription("1 if this replica currently holds a lock, 0 otherwise.")); err != nil {
		return Metrics{}, fmt.Errorf("lock.held: %w", err)
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

	if m.Role.Current, err = meter.Int64Gauge("swarm_auto_drain.role", metric.WithDescription("Current role: 0=worker, 1=manager.")); err != nil {
		return Metrics{}, fmt.Errorf("role: %w", err)
	}
	if m.Role.Refresh, err = meter.Int64Counter("swarm_auto_drain.role.refresh", metric.WithDescription("Role detector refresh outcomes.")); err != nil {
		return Metrics{}, fmt.Errorf("role.refresh: %w", err)
	}
	if m.Role.Transitions, err = meter.Int64Counter("swarm_auto_drain.role.transitions", metric.WithDescription("Role transitions.")); err != nil {
		return Metrics{}, fmt.Errorf("role.transitions: %w", err)
	}

	if m.Forward.Attempts, err = meter.Int64Counter("swarm_auto_drain.forward.attempts", metric.WithDescription("Forward attempts by outcome.")); err != nil {
		return Metrics{}, fmt.Errorf("forward.attempts: %w", err)
	}
	if m.Forward.Duration, err = meter.Float64Histogram("swarm_auto_drain.forward.duration", metric.WithUnit("s"), metric.WithDescription("Forward request duration including retries.")); err != nil {
		return Metrics{}, fmt.Errorf("forward.duration: %w", err)
	}
	if m.Forward.CacheInvalidations, err = meter.Int64Counter("swarm_auto_drain.forward.cache_invalidations", metric.WithDescription("Manager-URL cache invalidations.")); err != nil {
		return Metrics{}, fmt.Errorf("forward.cache_invalidations: %w", err)
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
