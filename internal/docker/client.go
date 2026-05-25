// Package docker wraps the Docker SDK for the coordinator's use.
package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

// Client is the subset of the Docker SDK we use, exposed as an interface so
// other packages can be tested with fakes.
type Client interface {
	Info(ctx context.Context) (system.Info, error)
	Close() error

	// Used by the lock store.
	ConfigCreate(ctx context.Context, name string, data []byte, labels map[string]string) (id string, err error)
	ConfigInspect(ctx context.Context, name string) (data []byte, labels map[string]string, err error)
	ConfigRemove(ctx context.Context, name string) error

	// Used by the drainer.
	NodeList(ctx context.Context, filters map[string][]string) ([]NodeSummary, error)
	NodeUpdate(ctx context.Context, nodeName, availability string) error
	NodeTaskCount(ctx context.Context, nodeName string) (int, error)
}

// NodeSummary is a tiny projection of swarm.Node for our needs.
type NodeSummary struct {
	Hostname     string
	Role         string // "manager" | "worker"
	Reachability string // "reachable" | "unreachable" | "unknown" | ""
	Leader       bool
	Availability string // "active" | "pause" | "drain"
}

// New constructs a real Docker client wrapping the given socket path.
func New(socketPath string) (Client, error) {
	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.WithHost("unix://"+socketPath),
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, err
	}
	return &dockerClient{c: cli}, nil
}

type dockerClient struct {
	c *dockerclient.Client
}

func (d *dockerClient) Info(ctx context.Context) (system.Info, error) {
	return d.c.Info(ctx)
}

func (d *dockerClient) Close() error { return d.c.Close() }

func (d *dockerClient) ConfigCreate(ctx context.Context, name string, data []byte, labels map[string]string) (string, error) {
	spec := swarm.ConfigSpec{
		Annotations: swarm.Annotations{Name: name, Labels: labels},
		Data:        data,
	}
	resp, err := d.c.ConfigCreate(ctx, spec)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (d *dockerClient) ConfigInspect(ctx context.Context, name string) ([]byte, map[string]string, error) {
	cfg, _, err := d.c.ConfigInspectWithRaw(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil, fmt.Errorf("No such config: %s: %w", name, err)
		}
		return nil, nil, err
	}
	return cfg.Spec.Data, cfg.Spec.Labels, nil
}

func (d *dockerClient) ConfigRemove(ctx context.Context, name string) error {
	err := d.c.ConfigRemove(ctx, name)
	if err != nil && errdefs.IsNotFound(err) {
		return fmt.Errorf("No such config: %s: %w", name, err)
	}
	return err
}

func (d *dockerClient) NodeList(ctx context.Context, fs map[string][]string) ([]NodeSummary, error) {
	args := filters.NewArgs()
	for k, vs := range fs {
		for _, v := range vs {
			args.Add(k, v)
		}
	}
	nodes, err := d.c.NodeList(ctx, types.NodeListOptions{Filters: args})
	if err != nil {
		return nil, err
	}
	out := make([]NodeSummary, 0, len(nodes))
	for _, n := range nodes {
		s := NodeSummary{
			Hostname:     n.Description.Hostname,
			Role:         string(n.Spec.Role),
			Availability: string(n.Spec.Availability),
		}
		if n.ManagerStatus != nil {
			s.Reachability = string(n.ManagerStatus.Reachability)
			s.Leader = n.ManagerStatus.Leader
		}
		out = append(out, s)
	}
	return out, nil
}

func (d *dockerClient) NodeUpdate(ctx context.Context, nodeName, availability string) error {
	nodes, err := d.c.NodeList(ctx, types.NodeListOptions{
		Filters: filters.NewArgs(filters.Arg("name", nodeName)),
	})
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("node %q not found", nodeName)
	}
	n := nodes[0]
	n.Spec.Availability = swarm.NodeAvailability(availability)
	return d.c.NodeUpdate(ctx, n.ID, n.Version, n.Spec)
}

func (d *dockerClient) NodeTaskCount(ctx context.Context, nodeName string) (int, error) {
	args := filters.NewArgs(
		filters.Arg("node", nodeName),
		filters.Arg("desired-state", "running"),
	)
	tasks, err := d.c.TaskList(ctx, types.TaskListOptions{Filters: args})
	if err != nil {
		return 0, err
	}
	return len(tasks), nil
}
