package docker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/docker/api/types/system"
	"github.com/matchory/swarm-auto-drain/internal/metrics"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// Role identifies whether the local node currently acts as a swarm manager or worker.
type Role int

// Role values for RoleDetector.
const (
	// RoleUnknown is the zero value, used before the first successful refresh.
	RoleUnknown Role = iota
	// RoleWorker indicates the local node is a swarm worker (no control plane).
	RoleWorker
	// RoleManager indicates the local node is a swarm manager (control plane available).
	RoleManager
)

// String returns the lowercase role name ("manager", "worker", "unknown").
func (r Role) String() string {
	switch r {
	case RoleManager:
		return "manager"
	case RoleWorker:
		return "worker"
	case RoleUnknown:
		return "unknown"
	}
	return "unknown"
}

// roleInfoer is the subset of Client that RoleDetector needs.
type roleInfoer interface {
	Info(ctx context.Context) (system.Info, error)
}

// RoleDetector caches the local node's swarm role and refreshes it on a
// timer so that `docker node promote` / `docker node demote` is picked up
// without restarting the coordinator.
type RoleDetector struct {
	client   roleInfoer
	interval time.Duration

	Metrics metrics.RoleMetrics

	mu   sync.RWMutex
	role Role
}

// NewRoleDetector constructs a RoleDetector that polls c on the given interval.
func NewRoleDetector(c roleInfoer, interval time.Duration) *RoleDetector {
	return &RoleDetector{
		client:   c,
		interval: interval,
		mu:       sync.RWMutex{},
		role:     RoleUnknown,
	}
}

// Role returns the most recently observed role.
func (d *RoleDetector) Role() Role {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.role
}

// Refresh queries Docker once and updates the cached role. On error, the
// previously-cached role is preserved (transient Docker outages should not
// force a worker to start thinking it's a manager, or vice versa).
func (d *RoleDetector) Refresh(ctx context.Context) error {
	return d.refreshWithTrigger(ctx, "manual")
}

// RefreshFromErrorRecovery is the entry point used by WithRoleRefresh's
// synchronous, error-triggered refresh path. Tagged with a distinct
// trigger so observers can distinguish lazy recovery from periodic polling.
func (d *RoleDetector) RefreshFromErrorRecovery(ctx context.Context) error {
	return d.refreshWithTrigger(ctx, "error_recovery")
}

func (d *RoleDetector) refreshWithTrigger(ctx context.Context, trigger string) error {
	info, err := d.client.Info(ctx)
	if err != nil {
		d.Metrics.Refresh.Add(ctx, 1, otelmetric.WithAttributes(
			attribute.String("trigger", trigger),
			attribute.String("outcome", "error"),
		))
		return fmt.Errorf("docker info: %w", err)
	}
	newRole := RoleWorker
	if info.Swarm.ControlAvailable {
		newRole = RoleManager
	}
	d.mu.Lock()
	prev := d.role
	d.role = newRole
	d.mu.Unlock()

	d.Metrics.Refresh.Add(ctx, 1, otelmetric.WithAttributes(
		attribute.String("trigger", trigger),
		attribute.String("outcome", "ok"),
	))
	d.Metrics.Current.Record(ctx, roleAsGaugeValue(newRole))
	if prev != RoleUnknown && prev != newRole {
		d.Metrics.Transitions.Add(ctx, 1, otelmetric.WithAttributes(
			attribute.String("from", prev.String()),
			attribute.String("to", newRole.String()),
		))
	}
	return nil
}

func roleAsGaugeValue(r Role) int64 {
	if r == RoleManager {
		return 1
	}
	return 0
}

// Run blocks until ctx is canceled, refreshing on the configured interval.
// Errors are suppressed — the cached value persists across transient failures.
func (d *RoleDetector) Run(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.refreshWithTrigger(ctx, "periodic"); err != nil {
				slog.Debug("role refresh failed", "err", err)
			}
		}
	}
}
