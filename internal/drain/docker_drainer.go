package drain

import (
	"context"
	"fmt"
	"time"
)

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
	NodeUpdate(ctx context.Context, node string, availability string) error
	NodeTaskCount(ctx context.Context, node string) (int, error)
}

type DockerDrainer struct {
	Docker docker
}

func (d *DockerDrainer) QuorumOK(ctx context.Context, self string) (bool, QuorumReport, error) {
	nodes, err := d.Docker.NodeList(ctx, map[string][]string{"role": {"manager"}})
	if err != nil {
		return false, QuorumReport{}, fmt.Errorf("list managers: %w", err)
	}

	// Worker is calling? Always OK — workers don't participate in raft.
	isManager := false
	for _, n := range nodes {
		if n.Hostname == self {
			isManager = true
			break
		}
	}
	if !isManager {
		return true, QuorumReport{TotalManagers: len(nodes)}, nil
	}

	others := 0
	reachable := 0
	for _, n := range nodes {
		if n.Reachability == "reachable" {
			reachable++
		}
		if n.Hostname == self {
			continue
		}
		if n.Reachability == "reachable" {
			others++
		}
	}
	rep := QuorumReport{
		TotalManagers:     len(nodes),
		ReachableManagers: reachable,
		OthersReachable:   others,
		Required:          (len(nodes) / 2) + 1,
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
	h := ManagersHealth{Total: len(nodes)}
	for _, n := range nodes {
		if n.Reachability == "reachable" {
			h.Reachable++
		}
		if n.Leader {
			h.Leader = n.Hostname
		}
	}
	return h, nil
}

func (d *DockerDrainer) Drain(ctx context.Context, node string, timeout, pollInterval time.Duration) (Result, error) {
	if err := d.Docker.NodeUpdate(ctx, node, "drain"); err != nil {
		return Result{}, fmt.Errorf("set drain: %w", err)
	}
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		n, err := d.Docker.NodeTaskCount(ctx, node)
		if err != nil {
			return Result{}, fmt.Errorf("poll tasks: %w", err)
		}
		if n == 0 {
			return Result{TasksRemaining: 0, Elapsed: time.Since(start)}, nil
		}
		if time.Now().After(deadline) {
			return Result{TasksRemaining: n, Elapsed: time.Since(start), TimedOut: true}, nil
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (d *DockerDrainer) Activate(ctx context.Context, node string) error {
	return d.Docker.NodeUpdate(ctx, node, "active")
}
