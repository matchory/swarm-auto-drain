package docker

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker/errdefs"
)

// roleRefreshTimeout bounds an out-of-band role refresh triggered by the
// decorator. Decoupled from the inbound request's context (which may be
// canceled by a client disconnect or have a short deadline) so the cache
// flip still happens even when the request that surfaced the error is
// already dead.
const roleRefreshTimeout = 5 * time.Second

// IsNotSwarmManager reports whether err is the Docker daemon's "not a swarm
// manager" error, returned from manager-only APIs when this daemon was
// demoted (or never was a manager). Mirrors internal/lock's classification
// pattern: prefer the canonical errdefs class (the daemon wraps this as
// Unavailable since the swarm control plane is the unreachable resource),
// with a narrow substring fallback for non-canonical wrappers and test
// fakes that bypass errdefs. False positives are harmless — they only
// trigger an extra role refresh, which is cheap.
func IsNotSwarmManager(err error) bool {
	if err == nil {
		return false
	}
	if errdefs.IsUnavailable(err) {
		return true
	}
	return strings.Contains(err.Error(), "not a swarm manager")
}

// RoleRefresher is the minimal surface the decorator needs. Satisfied by
// *RoleDetector; declared as an interface so the decorator can be tested
// with a fake.
type RoleRefresher interface {
	Refresh(ctx context.Context) error
}

// WithRoleRefresh wraps a Client so that any manager-only API call which
// returns "not a swarm manager" triggers a synchronous role refresh before
// the error bubbles back. This is the lazy-on-error path that complements
// the periodic poll: a demoted manager flips its cached role faster than
// the next 30s tick. The in-flight request that surfaced the error still
// returns its error (the per-request role dispatch in main.go was
// evaluated at request entry and is not re-run); the *next* request from
// the same caller — including forward.Forwarder's retries — sees the
// updated role and dispatches to the worker handler.
//
// The wrapper only overrides methods that hit manager-only Docker APIs —
// Info, ContainerInspect (via SelfServiceName), and Close pass through
// the embedded Client. RoleDetector itself must be constructed with the
// unwrapped client so its periodic Refresh doesn't recurse.
func WithRoleRefresh(inner Client, refresher RoleRefresher) Client {
	return &roleAwareClient{Client: inner, refresher: refresher}
}

type roleAwareClient struct {
	Client

	refresher RoleRefresher
}

// ConfigCreate forwards to the embedded Client and triggers a role refresh on demotion errors.
func (c *roleAwareClient) ConfigCreate(
	ctx context.Context, name string, data []byte, labels map[string]string,
) (string, error) {
	id, err := c.Client.ConfigCreate(ctx, name, data, labels)
	c.check(ctx, err)
	return id, err
}

// ConfigInspect forwards to the embedded Client and triggers a role refresh on demotion errors.
func (c *roleAwareClient) ConfigInspect(ctx context.Context, name string) ([]byte, map[string]string, error) {
	d, l, err := c.Client.ConfigInspect(ctx, name)
	c.check(ctx, err)
	return d, l, err
}

// ConfigRemove forwards to the embedded Client and triggers a role refresh on demotion errors.
func (c *roleAwareClient) ConfigRemove(ctx context.Context, name string) error {
	err := c.Client.ConfigRemove(ctx, name)
	c.check(ctx, err)
	return err
}

// NodeList forwards to the embedded Client and triggers a role refresh on demotion errors.
func (c *roleAwareClient) NodeList(ctx context.Context, fs map[string][]string) ([]NodeSummary, error) {
	out, err := c.Client.NodeList(ctx, fs)
	c.check(ctx, err)
	return out, err
}

// NodeUpdate forwards to the embedded Client and triggers a role refresh on demotion errors.
func (c *roleAwareClient) NodeUpdate(ctx context.Context, nodeName, availability string) error {
	err := c.Client.NodeUpdate(ctx, nodeName, availability)
	c.check(ctx, err)
	return err
}

// NodeTaskCount forwards to the embedded Client and triggers a role refresh on demotion errors.
func (c *roleAwareClient) NodeTaskCount(ctx context.Context, nodeName string) (int, error) {
	n, err := c.Client.NodeTaskCount(ctx, nodeName)
	c.check(ctx, err)
	return n, err
}

// TasksByServiceWithIPs forwards to the embedded Client and triggers a role refresh on demotion errors.
func (c *roleAwareClient) TasksByServiceWithIPs(ctx context.Context, serviceName string) (map[string]string, error) {
	out, err := c.Client.TasksByServiceWithIPs(ctx, serviceName)
	c.check(ctx, err)
	return out, err
}

// check triggers a synchronous role refresh when err looks like the daemon's
// "not a swarm manager" signal. The ctx parameter is accepted to satisfy
// contextcheck but a fresh, timeout-bounded context is used so a canceled
// inbound request doesn't abort the refresh.
func (c *roleAwareClient) check(_ context.Context, err error) {
	if !IsNotSwarmManager(err) {
		return
	}
	// Use a fresh context, not the inbound request's: a client disconnect
	// or short request deadline would otherwise cancel the Refresh and the
	// role cache would stay stale exactly when we most need it to flip.
	ctx, cancel := context.WithTimeout(context.Background(), roleRefreshTimeout)
	defer cancel()
	//nolint:contextcheck // intentional: refresh must outlive the inbound request's deadline
	if refreshErr := c.refresher.Refresh(ctx); refreshErr != nil {
		slog.Debug("lazy role refresh failed", "err", refreshErr)
	}
}
