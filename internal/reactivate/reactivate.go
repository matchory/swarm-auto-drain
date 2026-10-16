// Package reactivate brings a lock holder back into service after its
// coordinated reboot. Swarm stops every task on a drained node, the
// coordinator's own replica included, so a rebooted worker has no local
// replica and no manager API to undo its drain. The replica on the raft
// leader does it instead: it follows the drain mark on the lock, waits until
// the holder has gone down and come back Ready, sets it Active and releases
// the lock. All progress lives in the lock's labels, so a new leader picks
// up where the old one stopped.
package reactivate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	"github.com/matchory/swarm-auto-drain/internal/lock"
	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

// Swarm node states and the manager reachability value the loop reads.
const (
	stateReady        = "ready"
	stateDown         = "down"
	stateDisconnected = "disconnected"
	unreachable       = "unreachable"
)

// Outcomes of one Tick. Stable; they appear in logs.
const (
	OutcomeNotLeader      = "not_leader"
	OutcomeIdle           = "idle"            // no lock, or the holder was not drained by the coordinator
	OutcomeNodeGone       = "node_gone"       // the drained node no longer exists in the swarm
	OutcomeHolderMismatch = "holder_mismatch" // mark names a node whose hostname is not the holder's
	OutcomeAwaitingDown   = "awaiting_down"
	OutcomeDownSeen       = "down_seen"
	OutcomeAwaitingReturn = "awaiting_return"
	OutcomeReleased       = "released"
	OutcomeLockGone       = "lock_gone" // the lock went away while we worked on it
)

// ErrNodeGone is returned by Cluster.Node when no node has the given ID.
var ErrNodeGone = errors.New("node not found")

// Node is the projection of a swarm node the loop needs.
type Node struct {
	ID           string
	Hostname     string
	State        string // "ready" | "down" | "disconnected" | "unknown".
	Availability string
	Reachability string // managers only; "" for workers.
	Leader       bool
}

// Cluster is the swarm surface the loop uses. Manager-only.
type Cluster interface {
	SelfNodeID(ctx context.Context) (string, error)
	Node(ctx context.Context, id string) (Node, error)
	// ActivateIfDrained sets the node Active only while it is drained.
	ActivateIfDrained(ctx context.Context, id string) (changed bool, err error)
}

// Store is the lock surface the loop uses.
type Store interface {
	Snapshot(ctx context.Context) (*lock.Snapshot, error)
	UpdateMark(ctx context.Context, snap *lock.Snapshot, mark lock.DrainMark) error
	ReleaseSnapshot(ctx context.Context, snap *lock.Snapshot) error
}

// Reconciler is the leader-only reactivation loop. Field order puts
// pointer/interface fields first to shrink the GC pointer-scan window.
type Reconciler struct {
	Cluster   Cluster
	Store     Store
	IsManager func() bool
	Now       func() time.Time // nil → time.Now.
	Metrics   metrics.ReactivateMetrics
	Interval  time.Duration
}

// Run ticks every Interval until ctx is done. Errors are logged and retried
// on the next tick; every step is idempotent.
func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	last := ""
	for {
		outcome, err := r.Tick(ctx)
		switch {
		case err != nil:
			slog.Warn("reactivation tick failed", "err", err)
		case outcome != last:
			level := slog.LevelInfo
			if outcome == OutcomeNotLeader || outcome == OutcomeIdle {
				level = slog.LevelDebug
			}
			slog.Log(ctx, level, "reactivation state", "state", outcome)
			last = outcome
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick runs one reconciliation pass and reports where the lock holder is.
func (r *Reconciler) Tick(ctx context.Context) (string, error) {
	if !r.IsManager() {
		return OutcomeNotLeader, nil
	}
	leader, err := r.isLeader(ctx)
	if err != nil || !leader {
		return OutcomeNotLeader, err
	}
	return r.reconcile(ctx)
}

// reconcile advances the drained lock holder by at most one state.
func (r *Reconciler) reconcile(ctx context.Context) (string, error) {
	snap, err := r.Store.Snapshot(ctx)
	if errors.Is(err, lock.ErrNoLock) {
		return OutcomeIdle, nil
	}
	if err != nil {
		return "", fmt.Errorf("read lock: %w", err)
	}
	if snap.Mark == nil {
		return OutcomeIdle, nil
	}

	node, err := r.Cluster.Node(ctx, snap.Mark.NodeID)
	if errors.Is(err, ErrNodeGone) {
		return OutcomeNodeGone, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect node %s: %w", snap.Mark.NodeID, err)
	}
	if node.Hostname != snap.Lock.Holder {
		return OutcomeHolderMismatch, nil
	}

	switch {
	case snap.Mark.ActivatedAt != 0:
		return r.release(ctx, snap)
	case snap.Mark.DownSeenAt == 0:
		return r.observeDown(ctx, snap, node)
	case node.State != stateReady:
		return OutcomeAwaitingReturn, nil
	default:
		return r.activate(ctx, snap, node)
	}
}

func (r *Reconciler) isLeader(ctx context.Context) (bool, error) {
	self, err := r.Cluster.SelfNodeID(ctx)
	if err != nil {
		return false, fmt.Errorf("self node id: %w", err)
	}
	node, err := r.Cluster.Node(ctx, self)
	if err != nil {
		return false, fmt.Errorf("inspect self: %w", err)
	}
	return node.Leader, nil
}

// wentDown reports whether the node is visibly gone: its agent session
// ended (disconnected), its heartbeat expired (down), or, for a manager,
// raft lost it. "unknown" is deliberately not evidence: every node passes
// through it after a leader change, rebooted or not.
func wentDown(n Node) bool {
	return n.State == stateDown || n.State == stateDisconnected || n.Reachability == unreachable
}

// observeDown persists the first sighting of the drained holder being down.
// Until then a Ready holder is still draining or about to reboot.
func (r *Reconciler) observeDown(ctx context.Context, snap *lock.Snapshot, node Node) (string, error) {
	if !wentDown(node) {
		return OutcomeAwaitingDown, nil
	}
	mark := *snap.Mark
	mark.DownSeenAt = r.now().UTC().Unix()
	if err := r.Store.UpdateMark(ctx, snap, mark); err != nil {
		if errors.Is(err, lock.ErrNoLock) {
			return OutcomeLockGone, nil
		}
		r.record(ctx, "down_seen", "error")
		return "", fmt.Errorf("record down: %w", err)
	}
	r.record(ctx, "down_seen", "ok")
	slog.Info("reboot holder went down", "node", node.Hostname, "node_id", node.ID, "state", node.State)
	return OutcomeDownSeen, nil
}

// activate sets the returned holder Active, records that on the lock, then
// releases it. Activation only flips drain→active; any other availability
// means an operator took over and is left as is.
func (r *Reconciler) activate(ctx context.Context, snap *lock.Snapshot, node Node) (string, error) {
	changed, err := r.Cluster.ActivateIfDrained(ctx, node.ID)
	if err != nil {
		r.record(ctx, "activate", "error")
		return "", fmt.Errorf("activate %s: %w", node.Hostname, err)
	}
	if changed {
		r.record(ctx, "activate", "ok")
		slog.Info("reboot holder is back; set active", "node", node.Hostname, "node_id", node.ID)
	} else {
		r.record(ctx, "activate", "noop")
		slog.Info("reboot holder is back but not drained; availability left as is",
			"node", node.Hostname, "node_id", node.ID, "availability", node.Availability)
	}

	mark := *snap.Mark
	mark.ActivatedAt = r.now().UTC().Unix()
	if err := r.Store.UpdateMark(ctx, snap, mark); err != nil {
		if errors.Is(err, lock.ErrNoLock) {
			return OutcomeLockGone, nil
		}
		return "", fmt.Errorf("record activation: %w", err)
	}
	return r.release(ctx, snap)
}

// release removes exactly the lock instance in snap.
func (r *Reconciler) release(ctx context.Context, snap *lock.Snapshot) (string, error) {
	err := r.Store.ReleaseSnapshot(ctx, snap)
	switch {
	case err == nil:
		r.record(ctx, "release", "ok")
		slog.Info("released lock after reboot", "holder", snap.Lock.Holder)
		return OutcomeReleased, nil
	case errors.Is(err, lock.ErrNoLock):
		r.record(ctx, "release", "noop")
		return OutcomeLockGone, nil
	default:
		r.record(ctx, "release", "error")
		return "", fmt.Errorf("release lock: %w", err)
	}
}

func (r *Reconciler) record(ctx context.Context, step, outcome string) {
	r.Metrics.Steps.Add(ctx, 1, otelmetric.WithAttributes(
		attribute.String("step", step), attribute.String("outcome", outcome)))
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
