package docker

import (
	"context"
	"sync"
	"time"

	"github.com/docker/docker/api/types/system"
)

type Role int

const (
	RoleUnknown Role = iota
	RoleWorker
	RoleManager
)

func (r Role) String() string {
	switch r {
	case RoleManager:
		return "manager"
	case RoleWorker:
		return "worker"
	default:
		return "unknown"
	}
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

	mu   sync.RWMutex
	role Role
}

func NewRoleDetector(c roleInfoer, interval time.Duration) *RoleDetector {
	return &RoleDetector{client: c, interval: interval}
}

func (d *RoleDetector) Role() Role {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.role
}

// Refresh queries Docker once and updates the cached role. On error, the
// previously-cached role is preserved (transient Docker outages should not
// force a worker to start thinking it's a manager, or vice versa).
func (d *RoleDetector) Refresh(ctx context.Context) error {
	info, err := d.client.Info(ctx)
	if err != nil {
		return err
	}
	r := RoleWorker
	if info.Swarm.ControlAvailable {
		r = RoleManager
	}
	d.mu.Lock()
	d.role = r
	d.mu.Unlock()
	return nil
}

// Run blocks until ctx is cancelled, refreshing on the configured interval.
// Errors are suppressed — the cached value persists across transient failures.
func (d *RoleDetector) Run(ctx context.Context) {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = d.Refresh(ctx)
		}
	}
}
