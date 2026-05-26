package docker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
)

type fakeClient struct {
	mu        sync.Mutex
	info      system.Info
	infoErr   error
	infoCalls int
	// NextErr, if set, is returned from any manager-only method on the next
	// call. Used by decorator tests to inject the "not a swarm manager"
	// error.
	nextErr error
}

func (f *fakeClient) Info(_ context.Context) (system.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoCalls++
	return f.info, f.infoErr
}
func (*fakeClient) Close() error { return nil }
func (f *fakeClient) ConfigCreate(context.Context, string, []byte, map[string]string) (string, error) {
	return "", f.nextErr
}

func (f *fakeClient) ConfigInspect(context.Context, string) ([]byte, map[string]string, error) {
	return nil, nil, f.nextErr
}
func (f *fakeClient) ConfigRemove(context.Context, string) error { return f.nextErr }
func (f *fakeClient) NodeList(context.Context, map[string][]string) ([]NodeSummary, error) {
	return nil, f.nextErr
}
func (f *fakeClient) NodeUpdate(context.Context, string, string) error   { return f.nextErr }
func (f *fakeClient) NodeTaskCount(context.Context, string) (int, error) { return 0, f.nextErr }
func (f *fakeClient) SelfServiceName(context.Context) (string, error)    { return "", f.nextErr }
func (f *fakeClient) TasksByServiceWithIPs(context.Context, string) (map[string]string, error) {
	return nil, f.nextErr
}

func (f *fakeClient) SelfAttachedNetworks(context.Context) ([]string, error) {
	return nil, f.nextErr
}

func (f *fakeClient) NetworkInspect(context.Context, string) (NetworkSummary, error) {
	return NetworkSummary{}, f.nextErr
}

func managerInfo() system.Info {
	return system.Info{Swarm: swarm.Info{ControlAvailable: true, LocalNodeState: swarm.LocalNodeStateActive}}
}

func workerInfo() system.Info {
	return system.Info{Swarm: swarm.Info{ControlAvailable: false, LocalNodeState: swarm.LocalNodeStateActive}}
}

func TestRoleDetector_Initial(t *testing.T) {
	f := &fakeClient{info: managerInfo()}
	d := NewRoleDetector(f, time.Hour)

	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := d.Role(); got != RoleManager {
		t.Errorf("Role = %v, want manager", got)
	}
}

func TestRoleDetector_TransitionOnRefresh(t *testing.T) {
	f := &fakeClient{info: managerInfo()}
	d := NewRoleDetector(f, time.Hour)
	_ = d.Refresh(context.Background())

	f.mu.Lock()
	f.info = workerInfo()
	f.mu.Unlock()
	_ = d.Refresh(context.Background())

	if got := d.Role(); got != RoleWorker {
		t.Errorf("Role = %v, want worker after demote", got)
	}
}

func TestRoleDetector_RefreshErrorKeepsLastRole(t *testing.T) {
	f := &fakeClient{info: managerInfo()}
	d := NewRoleDetector(f, time.Hour)
	_ = d.Refresh(context.Background())

	f.mu.Lock()
	f.infoErr = errors.New("docker down")
	f.mu.Unlock()
	if err := d.Refresh(context.Background()); err == nil {
		t.Fatal("expected error from Refresh")
	}
	if got := d.Role(); got != RoleManager {
		t.Errorf("Role = %v, want manager (last known)", got)
	}
}
