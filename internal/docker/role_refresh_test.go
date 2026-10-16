package docker

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type fakeRefresher struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeRefresher) RefreshFromErrorRecovery(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

// The actual error string from the Docker daemon when a non-manager handles
// a manager-only call. Kept verbatim so the substring matcher is exercised
// against the real shape, not a paraphrase.
const notManagerErr = `Error response from daemon: This node is not a swarm manager. ` +
	`Use "docker swarm init" or "docker swarm join" to connect this node to swarm and try again.`

func TestIsNotSwarmManager(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("connection refused"), false},
		{"verbatim daemon error", errors.New(notManagerErr), true},
		{"wrapped", errors.Join(errors.New("config create"), errors.New(notManagerErr)), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsNotSwarmManager(c.err); got != c.want {
				t.Errorf("IsNotSwarmManager = %v, want %v", got, c.want)
			}
		})
	}
}

func TestWithRoleRefresh_TriggersOnNotManager(t *testing.T) {
	// Every manager-only method should trip the refresh when the inner
	// client returns the daemon's "not a swarm manager" error.
	ctx := context.Background()
	methods := map[string]func(Client) error{
		"ConfigCreate": func(c Client) error {
			_, e := c.ConfigCreate(ctx, "x", nil, nil)
			return e
		},
		"ConfigInspect": func(c Client) error {
			_, _, e := c.ConfigInspect(ctx, "x")
			return e
		},
		"ConfigRemove": func(c Client) error { return c.ConfigRemove(ctx, "x") },
		"ConfigMeta": func(c Client) error {
			_, _, e := c.ConfigMeta(ctx, "x")
			return e
		},
		"ConfigUpdateLabels": func(c Client) error { return c.ConfigUpdateLabels(ctx, "x", 1, nil) },
		"NodeInspect": func(c Client) error {
			_, e := c.NodeInspect(ctx, "n")
			return e
		},
		"NodeUpdateAvailabilityIf": func(c Client) error {
			_, e := c.NodeUpdateAvailabilityIf(ctx, "n", "drain", "active")
			return e
		},
		"NodeList": func(c Client) error {
			_, e := c.NodeList(ctx, nil)
			return e
		},
		"NodeUpdate":    func(c Client) error { return c.NodeUpdate(ctx, "n", "active") },
		"NodeTaskCount": func(c Client) error { _, e := c.NodeTaskCount(ctx, "n"); return e },
		"TasksByServiceWithIPs": func(c Client) error {
			_, e := c.TasksByServiceWithIPs(ctx, "s")
			return e
		},
	}
	for name, call := range methods {
		t.Run(name, func(t *testing.T) {
			inner := &fakeClient{nextErr: errors.New(notManagerErr)}
			refresher := &fakeRefresher{}
			wrapped := WithRoleRefresh(inner, refresher)
			if err := call(wrapped); err == nil {
				t.Fatal("expected error to propagate")
			}
			if refresher.calls != 1 {
				t.Errorf("refresh calls = %d, want 1", refresher.calls)
			}
		})
	}
}

func TestWithRoleRefresh_NoTriggerOnOtherError(t *testing.T) {
	inner := &fakeClient{nextErr: errors.New("disk full")}
	refresher := &fakeRefresher{}
	wrapped := WithRoleRefresh(inner, refresher)
	_, _ = wrapped.ConfigCreate(context.Background(), "x", nil, nil)
	if refresher.calls != 0 {
		t.Errorf("refresh calls = %d, want 0 (unrelated error)", refresher.calls)
	}
}

func TestWithRoleRefresh_NoTriggerOnSuccess(t *testing.T) {
	inner := &fakeClient{}
	refresher := &fakeRefresher{}
	wrapped := WithRoleRefresh(inner, refresher)
	_, _ = wrapped.ConfigCreate(context.Background(), "x", nil, nil)
	if refresher.calls != 0 {
		t.Errorf("refresh calls = %d, want 0 (no error)", refresher.calls)
	}
}

func TestWithRoleRefresh_PassThroughMethods(t *testing.T) {
	// Info / SelfServiceName / Close are not wrapped, so they hit the
	// inner client directly. Sanity-check by counting Info calls.
	inner := &fakeClient{}
	refresher := &fakeRefresher{}
	wrapped := WithRoleRefresh(inner, refresher)
	_, _ = wrapped.Info(context.Background())
	if inner.infoCalls != 1 {
		t.Errorf("Info calls = %d, want 1", inner.infoCalls)
	}
	if refresher.calls != 0 {
		t.Errorf("refresh should not fire for Info, got %d", refresher.calls)
	}
}
