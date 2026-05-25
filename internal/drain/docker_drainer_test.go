package drain

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeNode struct {
	Hostname     string
	Role         string
	Reachability string
	Availability string
	Leader       bool
}

type fakeDocker struct {
	nodes       []fakeNode
	updateErr   error
	updateCalls []string
	tasks       []int
}

func (f *fakeDocker) NodeList(_ context.Context, filters map[string][]string) ([]NodeSummary, error) {
	var out []NodeSummary
	wantRole := ""
	if v, ok := filters["role"]; ok && len(v) > 0 {
		wantRole = v[0]
	}
	for _, n := range f.nodes {
		if wantRole != "" && n.Role != wantRole {
			continue
		}
		out = append(out, NodeSummary{
			Hostname: n.Hostname, Role: n.Role,
			Reachability: n.Reachability, Availability: n.Availability,
			Leader: n.Leader,
		})
	}
	return out, nil
}
func (f *fakeDocker) NodeUpdate(_ context.Context, node, avail string) error {
	f.updateCalls = append(f.updateCalls, node+":"+avail)
	return f.updateErr
}
func (f *fakeDocker) NodeTaskCount(_ context.Context, _ string) (int, error) {
	if len(f.tasks) == 0 {
		return 0, nil
	}
	n := f.tasks[0]
	f.tasks = f.tasks[1:]
	return n, nil
}

func TestQuorumOK_WorkerAlwaysPasses(t *testing.T) {
	f := &fakeDocker{nodes: []fakeNode{
		{Hostname: "m1", Role: "manager", Reachability: "reachable"},
		{Hostname: "m2", Role: "manager", Reachability: "unreachable"},
		{Hostname: "m3", Role: "manager", Reachability: "unreachable"},
		{Hostname: "w1", Role: "worker", Reachability: ""},
	}}
	d := &DockerDrainer{Docker: f}
	ok, _, err := d.QuorumOK(context.Background(), "w1")
	if err != nil || !ok {
		t.Fatalf("worker quorum: ok=%v err=%v", ok, err)
	}
}

func TestQuorumOK_ManagerWith2Others(t *testing.T) {
	f := &fakeDocker{nodes: []fakeNode{
		{Hostname: "m1", Role: "manager", Reachability: "reachable"},
		{Hostname: "m2", Role: "manager", Reachability: "reachable"},
		{Hostname: "m3", Role: "manager", Reachability: "reachable"},
	}}
	d := &DockerDrainer{Docker: f}
	ok, rep, err := d.QuorumOK(context.Background(), "m1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Errorf("expected OK with 2 other reachable managers, got rep=%+v", rep)
	}
	if rep.OthersReachable != 2 || rep.Required != 2 || rep.TotalManagers != 3 {
		t.Errorf("rep = %+v", rep)
	}
}

func TestQuorumOK_ManagerWithOnly1Other(t *testing.T) {
	f := &fakeDocker{nodes: []fakeNode{
		{Hostname: "m1", Role: "manager", Reachability: "reachable"},
		{Hostname: "m2", Role: "manager", Reachability: "reachable"},
		{Hostname: "m3", Role: "manager", Reachability: "unreachable"},
	}}
	d := &DockerDrainer{Docker: f}
	ok, rep, err := d.QuorumOK(context.Background(), "m1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Errorf("expected NOT OK with only 1 other reachable manager, got rep=%+v", rep)
	}
}

func TestDrain_HappyPath(t *testing.T) {
	f := &fakeDocker{tasks: []int{3, 1, 0}}
	d := &DockerDrainer{Docker: f}
	res, err := d.Drain(context.Background(), "w1", time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res.TimedOut || res.TasksRemaining != 0 {
		t.Errorf("res = %+v", res)
	}
	if len(f.updateCalls) != 1 || f.updateCalls[0] != "w1:drain" {
		t.Errorf("updateCalls = %v", f.updateCalls)
	}
}

func TestDrain_Timeout(t *testing.T) {
	f := &fakeDocker{tasks: []int{5, 5, 5, 5, 5, 5, 5, 5}}
	d := &DockerDrainer{Docker: f}
	res, err := d.Drain(context.Background(), "w1", 50*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !res.TimedOut || res.TasksRemaining != 5 {
		t.Errorf("res = %+v", res)
	}
}

func TestActivate(t *testing.T) {
	f := &fakeDocker{}
	d := &DockerDrainer{Docker: f}
	if err := d.Activate(context.Background(), "w1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(f.updateCalls) != 1 || f.updateCalls[0] != "w1:active" {
		t.Errorf("updateCalls = %v", f.updateCalls)
	}
}

func TestManagers(t *testing.T) {
	f := &fakeDocker{nodes: []fakeNode{
		{Hostname: "m1", Role: "manager", Reachability: "reachable", Leader: true},
		{Hostname: "m2", Role: "manager", Reachability: "reachable"},
		{Hostname: "m3", Role: "manager", Reachability: "unreachable"},
		{Hostname: "w1", Role: "worker", Reachability: ""},
	}}
	d := &DockerDrainer{Docker: f}
	h, err := d.Managers(context.Background())
	if err != nil {
		t.Fatalf("Managers: %v", err)
	}
	if h.Total != 3 || h.Reachable != 2 || h.Leader != "m1" {
		t.Errorf("Managers = %+v", h)
	}
}

func TestDrain_UpdateError(t *testing.T) {
	f := &fakeDocker{updateErr: errors.New("forbidden")}
	d := &DockerDrainer{Docker: f}
	_, err := d.Drain(context.Background(), "w1", time.Second, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected error when NodeUpdate fails")
	}
}
