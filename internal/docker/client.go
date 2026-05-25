// Package docker wraps the Docker SDK for the coordinator's use.
package docker

import (
	"context"
	"errors"

	"github.com/docker/docker/api/types/system"
	dockerclient "github.com/docker/docker/client"
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

var errNotYetImplemented = errors.New("docker.Client: method not yet implemented (filled in by Task 11)")

func (d *dockerClient) ConfigCreate(context.Context, string, []byte, map[string]string) (string, error) {
	return "", errNotYetImplemented
}
func (d *dockerClient) ConfigInspect(context.Context, string) ([]byte, map[string]string, error) {
	return nil, nil, errNotYetImplemented
}
func (d *dockerClient) ConfigRemove(context.Context, string) error { return errNotYetImplemented }
func (d *dockerClient) NodeList(context.Context, map[string][]string) ([]NodeSummary, error) {
	return nil, errNotYetImplemented
}
func (d *dockerClient) NodeUpdate(context.Context, string, string) error { return errNotYetImplemented }
func (d *dockerClient) NodeTaskCount(context.Context, string) (int, error) {
	return 0, errNotYetImplemented
}
