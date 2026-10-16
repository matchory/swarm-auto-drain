package docker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
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
func (f *fakeClient) ConfigMeta(context.Context, string) (string, uint64, error) {
	return "", 0, f.nextErr
}

func (f *fakeClient) ConfigUpdateLabels(context.Context, string, uint64, map[string]string) error {
	return f.nextErr
}

func (f *fakeClient) NodeInspect(context.Context, string) (NodeSummary, error) {
	return NodeSummary{}, f.nextErr
}

func (f *fakeClient) NodeUpdateAvailabilityIf(context.Context, string, string, string) (bool, error) {
	return false, f.nextErr
}

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
	d.Metrics = metrics.NewNoop().Role

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
	d.Metrics = metrics.NewNoop().Role
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
	d.Metrics = metrics.NewNoop().Role
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

func TestRoleDetector_Refresh_RecordsOk(t *testing.T) {
	reader, m := metrics.NewManualHarness(t)
	f := &fakeClient{info: managerInfo()}
	d := NewRoleDetector(f, time.Hour)
	d.Metrics = m.Role

	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	points := metrics.FindCounter(t, reader, "swarm_auto_drain.role.refresh")
	dp := metrics.PointWithAttrs(t, points, "trigger", "manual", "outcome", "ok")
	if dp.Value != 1 {
		t.Errorf("refresh counter = %d, want 1", dp.Value)
	}
	gauge := metrics.FindGauge(t, reader, "swarm_auto_drain.role")
	if len(gauge) == 0 || gauge[0].Value != 1 {
		t.Errorf("role gauge = %+v, want value 1", gauge)
	}
}

func TestRoleDetector_Refresh_RecordsTransition(t *testing.T) {
	reader, m := metrics.NewManualHarness(t)
	f := &fakeClient{info: workerInfo()}
	d := NewRoleDetector(f, time.Hour)
	d.Metrics = m.Role

	// First refresh: RoleUnknown -> RoleWorker. Must NOT be counted as a transition.
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh 1: %v", err)
	}

	f.mu.Lock()
	f.info = managerInfo()
	f.mu.Unlock()

	// Second refresh: RoleWorker -> RoleManager. Counts as a transition.
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh 2: %v", err)
	}

	points := metrics.FindCounter(t, reader, "swarm_auto_drain.role.transitions")
	dp := metrics.PointWithAttrs(t, points, "from", "worker", "to", "manager")
	if dp.Value != 1 {
		t.Errorf("transition counter = %d, want 1", dp.Value)
	}
	// Total across all attribute sets must be exactly 1 — the first
	// RoleUnknown -> RoleWorker step is excluded by design.
	var total int64
	for _, p := range points {
		total += p.Value
	}
	if total != 1 {
		t.Errorf("total transitions = %d, want 1 (unknown->worker must not count)", total)
	}
}
