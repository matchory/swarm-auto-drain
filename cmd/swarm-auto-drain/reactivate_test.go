package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/docker/docker/errdefs"

	dockerpkg "github.com/matchory/swarm-auto-drain/internal/docker"
	"github.com/matchory/swarm-auto-drain/internal/reactivate"
)

// nodesClient implements the node calls the adapters use; any other
// dockerpkg.Client method panics via the nil embedded interface.
type nodesClient struct {
	dockerpkg.Client

	nodes []dockerpkg.NodeSummary
}

func (c nodesClient) NodeList(context.Context, map[string][]string) ([]dockerpkg.NodeSummary, error) {
	return c.nodes, nil
}

func (c nodesClient) NodeInspect(_ context.Context, id string) (dockerpkg.NodeSummary, error) {
	for _, n := range c.nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return dockerpkg.NodeSummary{}, fmt.Errorf("node inspect %q: %w", id, errdefs.NotFound(errors.New("no such node")))
}

func TestNodeIDByHostname_ExactMatch(t *testing.T) {
	// The name filter matches substrings: "web1" also returns "web10".
	c := nodesClient{nodes: []dockerpkg.NodeSummary{
		{ID: "id-10", Hostname: "web10"},
		{ID: "id-1", Hostname: "web1"},
	}}
	got, err := nodeIDByHostname(context.Background(), c, "web1")
	if err != nil || got != "id-1" {
		t.Errorf("nodeIDByHostname = %q, %v; want id-1", got, err)
	}
	if _, err := nodeIDByHostname(context.Background(), c, "web"); !errors.Is(err, dockerpkg.ErrNodeNotFound) {
		t.Errorf("err = %v, want ErrNodeNotFound", err)
	}
}

func TestReactivateAdapter_MissingNodeIsGone(t *testing.T) {
	a := reactivateAdapter{nodesClient{}}
	if _, err := a.Node(context.Background(), "nope"); !errors.Is(err, reactivate.ErrNodeGone) {
		t.Errorf("err = %v, want ErrNodeGone", err)
	}
}
