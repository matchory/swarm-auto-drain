package main

import (
	"context"
	"fmt"

	"github.com/docker/docker/errdefs"

	dockerpkg "github.com/matchory/swarm-auto-drain/internal/docker"
	"github.com/matchory/swarm-auto-drain/internal/reactivate"
)

// reactivateAdapter bridges internal/docker.Client to reactivate.Cluster.
type reactivateAdapter struct{ c dockerpkg.Client }

func (a reactivateAdapter) SelfNodeID(ctx context.Context) (string, error) {
	info, err := a.c.Info(ctx)
	if err != nil {
		return "", err
	}
	return info.Swarm.NodeID, nil
}

func (a reactivateAdapter) Node(ctx context.Context, id string) (reactivate.Node, error) {
	n, err := a.c.NodeInspect(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return reactivate.Node{}, fmt.Errorf("%w: %s", reactivate.ErrNodeGone, id)
		}
		return reactivate.Node{}, err
	}
	return reactivate.Node{
		ID:           n.ID,
		Hostname:     n.Hostname,
		State:        n.State,
		Availability: n.Availability,
		Reachability: n.Reachability,
		Leader:       n.Leader,
	}, nil
}

func (a reactivateAdapter) ActivateIfDrained(ctx context.Context, id string) (bool, error) {
	return a.c.NodeUpdateAvailabilityIf(ctx, id, "drain", "active")
}

// nodeIDByHostname resolves a hostname to its swarm node ID. The `name`
// filter matches substrings, so the exact hostname is picked from the result.
func nodeIDByHostname(ctx context.Context, c dockerpkg.Client, hostname string) (string, error) {
	nodes, err := c.NodeList(ctx, map[string][]string{"name": {hostname}})
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.Hostname == hostname {
			return n.ID, nil
		}
	}
	return "", fmt.Errorf("%w: %q", dockerpkg.ErrNodeNotFound, hostname)
}
