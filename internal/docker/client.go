// Package docker wraps the Docker SDK for the coordinator's use.
package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

var (
	// ErrNodeNotFound is returned when no node has the requested hostname.
	ErrNodeNotFound = errors.New("node not found")
	// ErrAmbiguousNode is returned when several nodes share the hostname.
	ErrAmbiguousNode = errors.New("hostname is not unique")
)

// IsNotManager reports whether err is the daemon's refusal of a
// manager-only call on a node that is not a swarm manager.
func IsNotManager(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not a swarm manager")
}

// Client is the subset of the Docker SDK the coordinator uses, as an
// interface so other packages can be tested with fakes.
type Client interface {
	Info(ctx context.Context) (system.Info, error)
	Close() error

	// Slot store.
	ConfigCreate(ctx context.Context, name string, data []byte, labels map[string]string) (id string, err error)
	ConfigInspect(ctx context.Context, name string) (data []byte, labels map[string]string, err error)
	ConfigRemove(ctx context.Context, name string) error

	// Drainer and identity.
	NodeList(ctx context.Context, fs map[string][]string) ([]NodeSummary, error)
	NodeUpdate(ctx context.Context, nodeName, availability string) error
	NodeTaskCount(ctx context.Context, nodeName string) (int, error)
}

// NodeSummary is a tiny projection of swarm.Node for our needs.
// Strings grouped first, bool last, to keep the GC pointer-scan window compact.
type NodeSummary struct {
	Hostname     string
	Role         string // "manager" | "worker".
	Reachability string // "reachable" | "unreachable" | "unknown" | "".
	Availability string // "active" | "pause" | "drain".
	Leader       bool
}

// New constructs a real Docker client wrapping the given socket path.
func New(socketPath string) (Client, error) {
	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.WithHost("unix://"+socketPath),
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &dockerClient{c: cli}, nil
}

type dockerClient struct {
	c *dockerclient.Client
}

// Info returns the Docker daemon's Info response.
func (d *dockerClient) Info(ctx context.Context) (system.Info, error) {
	info, err := d.c.Info(ctx)
	if err != nil {
		return info, fmt.Errorf("docker info: %w", err)
	}
	return info, nil
}

// Close releases the underlying Docker SDK client.
func (d *dockerClient) Close() error {
	if err := d.c.Close(); err != nil {
		return fmt.Errorf("docker close: %w", err)
	}
	return nil
}

// ConfigCreate creates a swarm Config and returns its ID.
func (d *dockerClient) ConfigCreate(
	ctx context.Context, name string, data []byte, labels map[string]string,
) (string, error) {
	spec := swarm.ConfigSpec{
		Annotations: swarm.Annotations{Name: name, Labels: labels},
		Data:        data,
	}
	resp, err := d.c.ConfigCreate(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("config create %q: %w", name, err)
	}
	return resp.ID, nil
}

// ConfigInspect returns the data and labels of the named swarm Config.
func (d *dockerClient) ConfigInspect(ctx context.Context, name string) ([]byte, map[string]string, error) {
	cfg, _, err := d.c.ConfigInspectWithRaw(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil, fmt.Errorf("no such config %q: %w", name, err)
		}
		return nil, nil, fmt.Errorf("config inspect %q: %w", name, err)
	}
	return cfg.Spec.Data, cfg.Spec.Labels, nil
}

// ConfigRemove deletes the named swarm Config.
func (d *dockerClient) ConfigRemove(ctx context.Context, name string) error {
	err := d.c.ConfigRemove(ctx, name)
	if err != nil && errdefs.IsNotFound(err) {
		return fmt.Errorf("no such config %q: %w", name, err)
	}
	if err != nil {
		return fmt.Errorf("config remove %q: %w", name, err)
	}
	return nil
}

// NodeList returns a projection of the swarm nodes matching the given filters.
func (d *dockerClient) NodeList(ctx context.Context, fs map[string][]string) ([]NodeSummary, error) {
	args := filters.NewArgs()
	for k, vs := range fs {
		for _, v := range vs {
			args.Add(k, v)
		}
	}
	nodes, err := d.c.NodeList(ctx, types.NodeListOptions{Filters: args})
	if err != nil {
		return nil, fmt.Errorf("node list: %w", err)
	}
	out := make([]NodeSummary, 0, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		summary := NodeSummary{
			Hostname:     node.Description.Hostname,
			Role:         string(node.Spec.Role),
			Reachability: "",
			Availability: string(node.Spec.Availability),
			Leader:       false,
		}
		if node.ManagerStatus != nil {
			summary.Reachability = string(node.ManagerStatus.Reachability)
			summary.Leader = node.ManagerStatus.Leader
		}
		out = append(out, summary)
	}
	return out, nil
}

// nodeByHostname resolves a node by its exact hostname. The Docker `name`
// filter matches substrings, so a hostname that is a prefix of another node's
// (e.g. "web1" vs "web10") returns multiple nodes in unspecified order; we
// must select the exact match rather than trusting nodes[0], otherwise we
// could act on the wrong node.
func (d *dockerClient) nodeByHostname(ctx context.Context, nodeName string) (swarm.Node, error) {
	nodes, err := d.c.NodeList(ctx, types.NodeListOptions{
		Filters: filters.NewArgs(filters.Arg("name", nodeName)),
	})
	if err != nil {
		return swarm.Node{}, fmt.Errorf("node list %q: %w", nodeName, err)
	}
	var found []swarm.Node
	for i := range nodes {
		if nodes[i].Description.Hostname == nodeName {
			found = append(found, nodes[i])
		}
	}
	switch len(found) {
	case 0:
		return swarm.Node{}, fmt.Errorf("%w: %q", ErrNodeNotFound, nodeName)
	case 1:
		return found[0], nil
	default:
		return swarm.Node{}, fmt.Errorf("%w: %d nodes are named %q", ErrAmbiguousNode, len(found), nodeName)
	}
}

// nodeUpdateAttempts bounds retries of a node update that lost a race with
// another change to the node (swarm's optimistic version check).
const nodeUpdateAttempts = 3

// NodeUpdate flips a node's availability (active / pause / drain).
func (d *dockerClient) NodeUpdate(ctx context.Context, nodeName, availability string) error {
	var err error
	for range nodeUpdateAttempts {
		var node swarm.Node
		if node, err = d.nodeByHostname(ctx, nodeName); err != nil {
			return err
		}
		node.Spec.Availability = swarm.NodeAvailability(availability)
		if err = d.c.NodeUpdate(ctx, node.ID, node.Version, node.Spec); err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "update out of sequence") {
			break
		}
	}
	return fmt.Errorf("node update %q: %w", nodeName, err)
}

// NodeTaskCount returns the number of tasks on the named node whose actual
// state is not terminal, i.e. whose container may still be running. Desired
// state is no use here: a drain sets it to shutdown at once, long before the
// containers stop.
func (d *dockerClient) NodeTaskCount(ctx context.Context, nodeName string) (int, error) {
	// Resolve to the exact node ID first: the task `node` filter, like the
	// node `name` filter, matches by name OR ID with substring semantics, so
	// filtering by hostname could count a prefix-collision peer's tasks and
	// the drain poll would never observe count==0. A full node ID is exact.
	node, err := d.nodeByHostname(ctx, nodeName)
	if err != nil {
		return 0, err
	}
	tasks, err := d.c.TaskList(ctx, types.TaskListOptions{Filters: filters.NewArgs(filters.Arg("node", node.ID))})
	if err != nil {
		return 0, fmt.Errorf("task list %q: %w", nodeName, err)
	}
	count := 0
	for i := range tasks {
		if !terminalTaskStates[tasks[i].Status.State] {
			count++
		}
	}
	return count, nil
}

// terminalTaskStates are the actual task states with no container left.
var terminalTaskStates = map[swarm.TaskState]bool{
	swarm.TaskStateComplete: true,
	swarm.TaskStateShutdown: true,
	swarm.TaskStateFailed:   true,
	swarm.TaskStateRejected: true,
	swarm.TaskStateRemove:   true,
	swarm.TaskStateOrphaned: true,
}
