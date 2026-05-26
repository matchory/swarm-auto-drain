package drain

import (
	"context"
	"fmt"
	"time"
)

// reachableState is the value Docker reports for a manager that's in raft
// contact with the leader. Extracted because the same literal drives both the
// quorum check and the /managers/health diagnostics.
const reachableState = "reachable"

// quorumDivisor is the raft majority denominator: floor(N/2)+1.
const quorumDivisor = 2

// NodeSummary mirrors internal/docker.NodeSummary. Duplicated here to avoid
// the import cycle between internal/drain and internal/docker. The wiring in
// cmd/swarm-auto-drain/main.go converts between the two.
type NodeSummary struct {
	Hostname     string
	Role         string
	Reachability string
	Availability string
	Leader       bool
}

type docker interface {
	NodeList(ctx context.Context, filters map[string][]string) ([]NodeSummary, error)
	NodeUpdate(ctx context.Context, node, availability string) error
	NodeTaskCount(ctx context.Context, node string) (int, error)
}

// DockerDrainer is the Drainer implementation backed by the Docker SDK.
type DockerDrainer struct {
	Docker docker
}

// QuorumOK reports whether draining `self` would preserve raft quorum and
// returns the QuorumReport detail used by the spec's /drain response shape.
func (d *DockerDrainer) QuorumOK(ctx context.Context, self string) (bool, QuorumReport, error) {
	nodes, err := d.Docker.NodeList(ctx, map[string][]string{"role": {"manager"}})
	if err != nil {
		return false, QuorumReport{}, fmt.Errorf("list managers: %w", err)
	}

	// Worker is calling? Always OK — workers don't participate in raft.
	isManager := false
	for _, node := range nodes {
		if node.Hostname == self {
			isManager = true
			break
		}
	}
	if !isManager {
		return true, QuorumReport{
			TotalManagers:     len(nodes),
			ReachableManagers: 0,
			OthersReachable:   0,
			Required:          0,
		}, nil
	}

	others := 0
	reachable := 0
	for _, node := range nodes {
		if node.Reachability == reachableState {
			reachable++
		}
		if node.Hostname == self {
			continue
		}
		if node.Reachability == reachableState {
			others++
		}
	}
	rep := QuorumReport{
		TotalManagers:     len(nodes),
		ReachableManagers: reachable,
		OthersReachable:   others,
		Required:          (len(nodes) / quorumDivisor) + 1,
	}
	// To preserve raft quorum after self drains, OthersReachable must be >= Required.
	ok := rep.OthersReachable >= rep.Required
	return ok, rep, nil
}

// Managers reports total/reachable manager counts and the raft leader's
// hostname. Computed without reference to "self" so /managers/health can be
// scraped by external monitoring.
func (d *DockerDrainer) Managers(ctx context.Context) (ManagersHealth, error) {
	nodes, err := d.Docker.NodeList(ctx, map[string][]string{"role": {"manager"}})
	if err != nil {
		return ManagersHealth{}, fmt.Errorf("list managers: %w", err)
	}
	health := ManagersHealth{Leader: "", Reachable: 0, Total: len(nodes)}
	for _, node := range nodes {
		if node.Reachability == reachableState {
			health.Reachable++
		}
		if node.Leader {
			health.Leader = node.Hostname
		}
	}
	return health, nil
}

// Drain sets the node's availability to "drain" and polls until task count
// hits zero or the timeout elapses.
func (d *DockerDrainer) Drain(ctx context.Context, node string, timeout, pollInterval time.Duration) (Result, error) {
	if err := d.Docker.NodeUpdate(ctx, node, "drain"); err != nil {
		return Result{}, fmt.Errorf("set drain: %w", err)
	}
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		count, err := d.Docker.NodeTaskCount(ctx, node)
		if err != nil {
			return Result{}, fmt.Errorf("poll tasks: %w", err)
		}
		if count == 0 {
			return Result{TasksRemaining: 0, Elapsed: time.Since(start), TimedOut: false}, nil
		}
		if time.Now().After(deadline) {
			return Result{TasksRemaining: count, Elapsed: time.Since(start), TimedOut: true}, nil
		}
		select {
		case <-ctx.Done():
			return Result{}, fmt.Errorf("drain context: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// Activate sets the node's availability back to "active".
func (d *DockerDrainer) Activate(ctx context.Context, node string) error {
	if err := d.Docker.NodeUpdate(ctx, node, "active"); err != nil {
		return fmt.Errorf("set active: %w", err)
	}
	return nil
}
