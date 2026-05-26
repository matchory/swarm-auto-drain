// Package docker wraps the Docker SDK for the coordinator's use.
package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

// swarmServiceNameLabel is the label the swarm orchestrator stamps onto every
// task container, naming the service that container belongs to.
const swarmServiceNameLabel = "com.docker.swarm.service.name"

// Sentinel errors for static error wrapping (err113).
var (
	// ErrServiceNameEmpty is returned when a service name argument is the empty string.
	ErrServiceNameEmpty = errors.New("service name is empty")
	// ErrServiceNameInvalid is returned when a service name violates the DNS-label shape we require.
	ErrServiceNameInvalid = errors.New(
		"service name is not a valid DNS label " +
			"(must be 1-63 chars, [A-Za-z0-9-], start/end alphanumeric)",
	)
	// ErrNodeNotFound is returned when a NodeList filter matches no nodes.
	ErrNodeNotFound = errors.New("node not found")
	// ErrNoContainerIDCandidates is returned when self-identification finds no candidate container IDs.
	ErrNoContainerIDCandidates = errors.New(
		"no container ID candidates (empty $HOSTNAME and unreadable /proc/self/cgroup)",
	)
	// ErrInspectNoConfig is returned when ContainerInspect returns a container with no Config block.
	ErrInspectNoConfig = errors.New("inspect: no config")
	// ErrContainerNoSwarmLabel is returned when a container lacks the swarm service-name label.
	ErrContainerNoSwarmLabel = errors.New("container has no swarm service-name label (not a swarm task)")
)

// validServiceNameRe restricts service names to a shape that is safe to
// use BOTH as a Docker filter argument and as a DNS label component
// ("tasks.<name>"). Docker swarm itself permits a broader set
// (underscores, dots) but those break Go's net.LookupIP on some
// resolvers and create multi-label hostnames that may shadow other
// services. LDH (letters-digits-hyphen) with a length bound matches
// RFC 1123 hostname rules.
var validServiceNameRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// ValidateServiceName reports whether a swarm service name is safe to use
// for our overlay-DNS forwarding and Docker filter paths. The check
// rejects names with characters Docker accepts but DNS doesn't handle
// uniformly (underscores), names containing dots (which create
// hostname-label boundaries and can shadow other zones), and names that
// exceed the 63-char DNS label limit.
func ValidateServiceName(name string) error {
	if name == "" {
		return ErrServiceNameEmpty
	}
	if !validServiceNameRe.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrServiceNameInvalid, name)
	}
	return nil
}

// Client is the subset of the Docker SDK we use, exposed as an interface so
// other packages can be tested with fakes. Wider than 10 methods is
// intentional — splitting per-domain interfaces would force every
// consumer to compose multiple interfaces just to express "I need the
// Docker SDK," which buys nothing over a single named seam.
//
//nolint:interfacebloat // single SDK facade — see comment
type Client interface {
	Info(ctx context.Context) (system.Info, error)
	Close() error

	// Used by the lock store.
	ConfigCreate(ctx context.Context, name string, data []byte, labels map[string]string) (id string, err error)
	ConfigInspect(ctx context.Context, name string) (data []byte, labels map[string]string, err error)
	ConfigRemove(ctx context.Context, name string) error

	// Used by the drainer.
	NodeList(ctx context.Context, fs map[string][]string) ([]NodeSummary, error)
	NodeUpdate(ctx context.Context, nodeName, availability string) error
	NodeTaskCount(ctx context.Context, nodeName string) (int, error)

	// Used by identity verification: discover our own service name from the
	// container's swarm orchestrator labels, and list overlay IPs of running
	// tasks of that service mapped to their node hostnames.
	SelfServiceName(ctx context.Context) (string, error)
	TasksByServiceWithIPs(ctx context.Context, serviceName string) (map[string]string, error)

	// Used by the startup overlay-encryption check: list our container's
	// attached networks, then inspect each to read its driver and the
	// `encrypted` option that Swarm sets when an overlay is created with
	// IPsec enabled.
	SelfAttachedNetworks(ctx context.Context) ([]string, error)
	NetworkInspect(ctx context.Context, name string) (NetworkSummary, error)
}

// NetworkSummary is the tiny projection of network.Inspect we need to
// reason about whether traffic on this network is confidential.
// Field order puts strings first to keep the GC pointer-scan window compact.
type NetworkSummary struct {
	Name      string
	Driver    string // "overlay" | "bridge" | "host" | "none" | etc.
	Encrypted bool   // True iff Options["encrypted"] == "true" (Swarm IPsec).
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

// NodeUpdate flips a node's availability (active / pause / drain).
func (d *dockerClient) NodeUpdate(ctx context.Context, nodeName, availability string) error {
	nodes, err := d.c.NodeList(ctx, types.NodeListOptions{
		Filters: filters.NewArgs(filters.Arg("name", nodeName)),
	})
	if err != nil {
		return fmt.Errorf("node list %q: %w", nodeName, err)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("%w: %q", ErrNodeNotFound, nodeName)
	}
	node := nodes[0]
	node.Spec.Availability = swarm.NodeAvailability(availability)
	if updateErr := d.c.NodeUpdate(ctx, node.ID, node.Version, node.Spec); updateErr != nil {
		return fmt.Errorf("node update %q: %w", nodeName, updateErr)
	}
	return nil
}

// NodeTaskCount returns the number of running tasks scheduled on the named node.
func (d *dockerClient) NodeTaskCount(ctx context.Context, nodeName string) (int, error) {
	args := filters.NewArgs(
		filters.Arg("node", nodeName),
		filters.Arg("desired-state", "running"),
	)
	tasks, err := d.c.TaskList(ctx, types.TaskListOptions{Filters: args})
	if err != nil {
		return 0, fmt.Errorf("task list %q: %w", nodeName, err)
	}
	return len(tasks), nil
}

// ContainerIDRe matches a 64-hex Docker container ID.
// Cgroup v1 paths look
// like `/docker/<id>` and v2 like `0::/system.slice/docker-<id>.scope` or
// `0::/docker/<id>` — both embed the full container ID, so a single regex
// over /proc/self/cgroup covers both.
var containerIDRe = regexp.MustCompile(`[0-9a-f]{64}`)

// candidateContainerIDs returns identifiers that might name our own
// container, in order of preference. The caller probes ContainerInspect
// on each in turn and uses the first one that resolves to a real
// container. Returning multiple candidates rather than a single best
// guess avoids brittleness across cgroup v1, v2-systemd, CRI-containerd,
// and rootless setups where a foreign 64-hex token may precede the real
// container ID in /proc/self/cgroup.
//
//  1. $HOSTNAME — Docker sets this to the short container ID by default.
//     Cheap, no FS access, works under any cgroup runtime. Only fails if
//     the operator sets `hostname:` in compose.
//  2. Any 64-hex tokens in /proc/self/cgroup, in document order.
func candidateContainerIDs() []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	add(os.Getenv("HOSTNAME"))
	if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, m := range containerIDRe.FindAllString(string(data), -1) {
			add(m)
		}
	}
	return out
}

// SelfServiceName discovers the swarm service name of the running container.
func (d *dockerClient) SelfServiceName(ctx context.Context) (string, error) {
	candidates := candidateContainerIDs()
	if len(candidates) == 0 {
		return "", ErrNoContainerIDCandidates
	}
	var lastErr error
	for _, cid := range candidates {
		info, err := d.c.ContainerInspect(ctx, cid)
		if err != nil {
			lastErr = fmt.Errorf("container inspect %s: %w", cid, err)
			continue
		}
		if info.Config == nil {
			lastErr = fmt.Errorf("%w: %s", ErrInspectNoConfig, cid)
			continue
		}
		name := info.Config.Labels[swarmServiceNameLabel]
		if name == "" {
			lastErr = fmt.Errorf("%w: %s (label %s)", ErrContainerNoSwarmLabel, cid, swarmServiceNameLabel)
			continue
		}
		return name, nil
	}
	return "", fmt.Errorf("self container inspect failed for candidates %v: %w", candidates, lastErr)
}

// SelfAttachedNetworks returns the names of every Docker network our own
// container is attached to. Uses the same candidate-ID probing as
// SelfServiceName, so it survives cgroup-v1/v2 variation. Result is
// suitable for feeding into NetworkInspect to check encryption.
func (d *dockerClient) SelfAttachedNetworks(ctx context.Context) ([]string, error) {
	candidates := candidateContainerIDs()
	if len(candidates) == 0 {
		return nil, ErrNoContainerIDCandidates
	}
	var lastErr error
	for _, cid := range candidates {
		info, err := d.c.ContainerInspect(ctx, cid)
		if err != nil {
			lastErr = fmt.Errorf("container inspect %s: %w", cid, err)
			continue
		}
		if info.NetworkSettings == nil {
			lastErr = fmt.Errorf("%w: %s (no NetworkSettings)", ErrInspectNoConfig, cid)
			continue
		}
		out := make([]string, 0, len(info.NetworkSettings.Networks))
		for name := range info.NetworkSettings.Networks {
			out = append(out, name)
		}
		return out, nil
	}
	return nil, fmt.Errorf("self container inspect failed for candidates %v: %w", candidates, lastErr)
}

// NetworkInspect returns the projection of the named Docker network that
// the encryption check cares about. The `encrypted` bool reads
// Options["encrypted"] — Swarm stamps this when the overlay was created
// with `--opt encrypted` (or `driver_opts: { encrypted: "true" }`).
func (d *dockerClient) NetworkInspect(ctx context.Context, name string) (NetworkSummary, error) {
	got, err := d.c.NetworkInspect(ctx, name, network.InspectOptions{})
	if err != nil {
		return NetworkSummary{}, fmt.Errorf("network inspect %q: %w", name, err)
	}
	return NetworkSummary{
		Name:      got.Name,
		Driver:    got.Driver,
		Encrypted: got.Options["encrypted"] == "true",
	}, nil
}

// TasksByServiceWithIPs maps overlay-network IPs of running tasks of the named service to node hostnames.
func (d *dockerClient) TasksByServiceWithIPs(ctx context.Context, serviceName string) (map[string]string, error) {
	args := filters.NewArgs(
		filters.Arg("service", serviceName),
		filters.Arg("desired-state", "running"),
	)
	tasks, err := d.c.TaskList(ctx, types.TaskListOptions{Filters: args})
	if err != nil {
		return nil, fmt.Errorf("task list: %w", err)
	}
	// Build NodeID → Hostname map in one shot rather than inspecting each node.
	nodes, err := d.c.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("node list: %w", err)
	}
	nodeHostname := make(map[string]string, len(nodes))
	for i := range nodes {
		nodeHostname[nodes[i].ID] = nodes[i].Description.Hostname
	}

	out := make(map[string]string)
	for i := range tasks {
		task := &tasks[i]
		host, ok := nodeHostname[task.NodeID]
		if !ok {
			continue
		}
		for j := range task.NetworksAttachments {
			att := &task.NetworksAttachments[j]
			for _, addr := range att.Addresses {
				// Addresses are CIDR strings ("10.0.1.5/24"); we only want the IP.
				ip, _, parseErr := net.ParseCIDR(addr)
				if parseErr != nil {
					continue
				}
				out[ip.String()] = host
			}
		}
	}
	return out, nil
}
