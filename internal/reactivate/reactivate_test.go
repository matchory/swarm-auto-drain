package reactivate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/lock"
	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

// fakeConfigs is a Swarm Config store with real identity semantics (fresh ID
// per create, lookup by name or current ID, version-checked label updates),
// so the tests drive the real lock.ConfigStore.
type fakeConfigs struct {
	mu        sync.Mutex
	data      []byte
	labels    map[string]string
	id        string
	creates   int
	version   uint64
	exists    bool
	removeErr error
}

const errNoSuchConfig = "no such config"

func (f *fakeConfigs) ConfigCreate(_ context.Context, _ string, data []byte, _ map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exists {
		return "", errors.New("rpc error: code = AlreadyExists desc = name conflicts")
	}
	f.creates++
	f.data, f.labels, f.id, f.version, f.exists = data, nil, fmt.Sprintf("cfg-%d", f.creates), 1, true
	return f.id, nil
}

func (f *fakeConfigs) resolves(ref string) bool { return f.exists && (ref == "lock" || ref == f.id) }

func (f *fakeConfigs) ConfigInspect(_ context.Context, ref string) ([]byte, map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.resolves(ref) {
		return nil, nil, errors.New(errNoSuchConfig)
	}
	return f.data, maps.Clone(f.labels), nil
}

func (f *fakeConfigs) ConfigRemove(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		err := f.removeErr
		f.removeErr = nil
		return err
	}
	if !f.resolves(ref) {
		return errors.New(errNoSuchConfig)
	}
	f.exists = false
	return nil
}

func (f *fakeConfigs) ConfigMeta(_ context.Context, ref string) (string, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.resolves(ref) {
		return "", 0, errors.New(errNoSuchConfig)
	}
	return f.id, f.version, nil
}

func (f *fakeConfigs) ConfigUpdateLabels(_ context.Context, id string, version uint64, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists || id != f.id {
		return errors.New(errNoSuchConfig)
	}
	if version != f.version {
		return errors.New("update out of sequence")
	}
	f.labels = maps.Clone(labels)
	f.version++
	return nil
}

// fakeCluster is a mutable swarm view. Activation follows Swarm's rule:
// only drain→active changes anything.
type fakeCluster struct {
	nodes     map[string]*Node
	activated []string
}

func (c *fakeCluster) Node(_ context.Context, id string) (Node, error) {
	n, ok := c.nodes[id]
	if !ok {
		return Node{}, ErrNodeGone
	}
	return *n, nil
}

func (c *fakeCluster) ActivateIfDrained(_ context.Context, id string) (bool, error) {
	n, ok := c.nodes[id]
	if !ok {
		return false, ErrNodeGone
	}
	if n.Availability != "drain" {
		return false, nil
	}
	n.Availability = "active"
	c.activated = append(c.activated, id)
	return true, nil
}

// selfCluster gives one reconciler its own node identity over a shared
// cluster, so two replicas can race through leadership changes.
type selfCluster struct {
	*fakeCluster

	self string
}

func (s selfCluster) SelfNodeID(context.Context) (string, error) { return s.self, nil }

type harness struct {
	t       *testing.T
	configs *fakeConfigs
	store   *lock.ConfigStore
	cluster *fakeCluster
	clock   int64
}

// newHarness builds a cluster of two managers (m1 leader, m2) and a worker
// w1, with w1 holding the lock and drained by the coordinator.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, configs: &fakeConfigs{}, clock: 1000}
	h.store = &lock.ConfigStore{
		Docker:     h.configs,
		ConfigName: "lock",
		Now:        func() time.Time { return time.Unix(h.clock, 0) },
		Metrics:    metrics.NewNoop().Lock,
	}
	h.cluster = &fakeCluster{nodes: map[string]*Node{
		"m1": {ID: "m1", Hostname: "mgr-1", State: stateReady, Availability: "active", Reachability: "reachable", Leader: true},
		"m2": {ID: "m2", Hostname: "mgr-2", State: stateReady, Availability: "active", Reachability: "reachable"},
		"w1": {ID: "w1", Hostname: "worker-1", State: stateReady, Availability: "active"},
	}}
	return h
}

func (h *harness) reconciler(self string) *Reconciler {
	return &Reconciler{
		Cluster:   selfCluster{fakeCluster: h.cluster, self: self},
		Store:     h.store,
		IsManager: func() bool { return true },
		Now:       func() time.Time { return time.Unix(h.clock, 0) },
		Metrics:   metrics.NewNoop().Reactivate,
		Interval:  time.Second,
	}
}

// drainForReboot does what the /drain handler does for the lock holder.
func (h *harness) drainForReboot(hostname, id string) {
	h.t.Helper()
	ctx := context.Background()
	if res, err := h.store.Acquire(ctx, hostname, 30*time.Minute); err != nil || res.Status != lock.Acquired {
		h.t.Fatalf("acquire: %+v %v", res, err)
	}
	if err := h.store.MarkDrained(ctx, hostname, id); err != nil {
		h.t.Fatalf("mark: %v", err)
	}
	h.cluster.nodes[id].Availability = "drain"
}

func (h *harness) tick(r *Reconciler, want string) {
	h.t.Helper()
	h.clock += 5
	got, err := r.Tick(context.Background())
	if err != nil {
		h.t.Fatalf("Tick: %v", err)
	}
	if got != want {
		h.t.Fatalf("Tick = %q, want %q", got, want)
	}
}

func (h *harness) state(id, state string) { h.cluster.nodes[id].State = state }

func (h *harness) assertLockHeldBy(holder string) {
	h.t.Helper()
	cur, err := h.store.Inspect(context.Background())
	if err != nil || cur.Holder != holder {
		h.t.Fatalf("lock = %+v, %v; want held by %s", cur, err, holder)
	}
}

func (h *harness) assertNoLock() {
	h.t.Helper()
	if cur, err := h.store.Inspect(context.Background()); !errors.Is(err, lock.ErrNoLock) {
		h.t.Fatalf("lock = %+v, %v; want released", cur, err)
	}
}

func TestTick_RebootCycle(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler("m1")
	h.drainForReboot("worker-1", "w1")

	h.tick(r, OutcomeAwaitingDown) // still draining / about to reboot
	h.state("w1", stateDisconnected)
	h.tick(r, OutcomeDownSeen)
	h.state("w1", stateDown)
	h.tick(r, OutcomeAwaitingReturn)
	h.state("w1", "unknown")
	h.tick(r, OutcomeAwaitingReturn)
	h.assertLockHeldBy("worker-1")
	if h.cluster.nodes["w1"].Availability != "drain" {
		t.Fatal("activated before the node was back")
	}

	h.state("w1", stateReady)
	h.tick(r, OutcomeReleased)
	if got := h.cluster.nodes["w1"].Availability; got != "active" {
		t.Errorf("availability = %q, want active", got)
	}
	h.assertNoLock()
	h.tick(r, OutcomeIdle)
	if len(h.cluster.activated) != 1 {
		t.Errorf("activations = %v, want exactly one", h.cluster.activated)
	}
}

func TestTick_ManagerHolderUnreachableCountsAsDown(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler("m1")
	h.drainForReboot("mgr-2", "m2")

	h.tick(r, OutcomeAwaitingDown)
	// The holder was the old leader: after the election every node is
	// "unknown", which is not evidence; raft reachability is.
	h.state("m2", "unknown")
	h.tick(r, OutcomeAwaitingDown)
	h.cluster.nodes["m2"].Reachability = unreachable
	h.tick(r, OutcomeDownSeen)
	h.cluster.nodes["m2"].Reachability = "reachable"
	h.state("m2", stateReady)
	h.tick(r, OutcomeReleased)
	if got := h.cluster.nodes["m2"].Availability; got != "active" {
		t.Errorf("availability = %q, want active", got)
	}
}

func TestTick_ManualDrainIgnored(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler("m1")
	// worker-1 holds the lock but was never drained by the coordinator; an
	// operator drained it by hand and it went through a down/up cycle.
	if _, err := h.store.Acquire(context.Background(), "worker-1", 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	h.cluster.nodes["w1"].Availability = "drain"
	h.state("w1", stateDown)
	h.tick(r, OutcomeIdle)
	h.state("w1", stateReady)
	h.tick(r, OutcomeIdle)

	if h.cluster.nodes["w1"].Availability != "drain" || len(h.cluster.activated) != 0 {
		t.Error("manually drained node was reactivated")
	}
	h.assertLockHeldBy("worker-1")
}

func TestTick_ManuallyDrainedNonHolderIgnored(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler("m1")
	h.cluster.nodes["m2"].Availability = "drain"
	h.state("m2", stateDown)
	h.tick(r, OutcomeIdle)
	h.state("m2", stateReady)
	h.tick(r, OutcomeIdle)
	if len(h.cluster.activated) != 0 {
		t.Errorf("activated %v without any lock", h.cluster.activated)
	}
}

func TestTick_OnlyLeaderActs(t *testing.T) {
	h := newHarness(t)
	follower := h.reconciler("m2")
	h.drainForReboot("worker-1", "w1")
	h.state("w1", stateDown)
	h.tick(follower, OutcomeNotLeader)

	worker := h.reconciler("w1")
	worker.IsManager = func() bool { return false }
	h.tick(worker, OutcomeNotLeader)

	snap, _ := h.store.Snapshot(context.Background())
	if snap.Mark.DownSeenAt != 0 {
		t.Error("a non-leader recorded progress")
	}
}

func TestTick_LeaderChangeMidway(t *testing.T) {
	h := newHarness(t)
	r1, r2 := h.reconciler("m1"), h.reconciler("m2")
	h.drainForReboot("worker-1", "w1")

	h.state("w1", stateDown)
	h.tick(r1, OutcomeDownSeen)

	// m1 loses leadership while w1 is still down; the election marks w1
	// "unknown". m2's replica must continue from the persisted progress.
	h.cluster.nodes["m1"].Leader, h.cluster.nodes["m2"].Leader = false, true
	h.state("w1", "unknown")
	h.tick(r1, OutcomeNotLeader)
	h.tick(r2, OutcomeAwaitingReturn)
	h.state("w1", stateReady)
	h.tick(r2, OutcomeReleased)

	if h.cluster.nodes["w1"].Availability != "active" {
		t.Error("node not reactivated after the leader change")
	}
	h.assertNoLock()
}

func TestTick_LeaderChangeBetweenActivateAndRelease(t *testing.T) {
	h := newHarness(t)
	r1, r2 := h.reconciler("m1"), h.reconciler("m2")
	h.drainForReboot("worker-1", "w1")
	h.state("w1", stateDown)
	h.tick(r1, OutcomeDownSeen)
	h.state("w1", stateReady)

	// m1 activates and records it, then its release fails (it lost the
	// manager API mid-step).
	h.configs.removeErr = errors.New("this node is not a swarm manager")
	h.clock += 5
	if _, err := r1.Tick(context.Background()); err == nil {
		t.Fatal("expected the release error to surface")
	}
	if h.cluster.nodes["w1"].Availability != "active" {
		t.Fatal("activation step did not run")
	}
	h.assertLockHeldBy("worker-1")

	h.cluster.nodes["m1"].Leader, h.cluster.nodes["m2"].Leader = false, true
	h.tick(r2, OutcomeReleased)
	h.assertNoLock()
	if len(h.cluster.activated) != 1 {
		t.Errorf("activations = %v, want one", h.cluster.activated)
	}
}

func TestTick_NodeNeverReturns(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler("m1")
	h.drainForReboot("worker-1", "w1")
	h.state("w1", stateDown)
	h.tick(r, OutcomeDownSeen)
	for range 10 {
		h.tick(r, OutcomeAwaitingReturn)
	}
	h.assertLockHeldBy("worker-1")

	// After LOCK_TTL a peer steals the lock as stale. The new lock carries
	// no mark, so the dead node stays drained.
	h.clock += 31 * 60
	res, err := h.store.Acquire(context.Background(), "mgr-2", 30*time.Minute)
	if err != nil || res.Status != lock.AcquiredViaSteal {
		t.Fatalf("steal: %+v %v", res, err)
	}
	h.tick(r, OutcomeIdle)
	h.state("w1", stateReady)
	h.tick(r, OutcomeIdle)
	if h.cluster.nodes["w1"].Availability != "drain" {
		t.Error("node reactivated after it lost the lock")
	}
	h.assertLockHeldBy("mgr-2")
}

func TestTick_RebootFasterThanPollIsNotGuessed(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler("m1")
	h.drainForReboot("worker-1", "w1")
	// The node went down and came back between two ticks: no down state
	// was ever observed. The loop must not guess; the node stays drained
	// and the host's resume times out visibly.
	for range 5 {
		h.tick(r, OutcomeAwaitingDown)
	}
	if h.cluster.nodes["w1"].Availability != "drain" {
		t.Error("reactivated without evidence of a reboot")
	}
}

func TestTick_HolderMismatchIgnored(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler("m1")
	h.drainForReboot("worker-1", "w1")
	// The node ID now belongs to a node with another hostname.
	h.cluster.nodes["w1"].Hostname = "renamed"
	h.state("w1", stateDown)
	h.tick(r, OutcomeHolderMismatch)
	h.state("w1", stateReady)
	h.tick(r, OutcomeHolderMismatch)
	if len(h.cluster.activated) != 0 {
		t.Error("acted on a node that is not the holder")
	}
}

func TestTick_NodeRemovedKeepsLock(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler("m1")
	h.drainForReboot("worker-1", "w1")
	delete(h.cluster.nodes, "w1")
	h.tick(r, OutcomeNodeGone)
	h.assertLockHeldBy("worker-1")
}

func TestTick_OperatorTookOverAvailability(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler("m1")
	h.drainForReboot("worker-1", "w1")
	h.state("w1", stateDown)
	h.tick(r, OutcomeDownSeen)
	h.cluster.nodes["w1"].Availability = "pause"
	h.state("w1", stateReady)
	h.tick(r, OutcomeReleased)
	if got := h.cluster.nodes["w1"].Availability; got != "pause" {
		t.Errorf("availability = %q, want the operator's pause kept", got)
	}
	h.assertNoLock()
}

func TestTick_LockReleasedElsewhere(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler("m1")
	h.drainForReboot("worker-1", "w1")
	h.state("w1", stateDown)
	h.tick(r, OutcomeDownSeen)
	h.state("w1", stateReady)
	// The lock is released elsewhere (the host's own DELETE /lock, or an
	// operator) before the leader gets to it: nothing left to act on.
	snap, _ := h.store.Snapshot(context.Background())
	_ = h.store.ReleaseSnapshot(context.Background(), snap)
	h.tick(r, OutcomeIdle)
	if h.cluster.nodes["w1"].Availability != "drain" {
		t.Error("acted without a lock")
	}
}

func TestTick_RecordsStepMetrics(t *testing.T) {
	reader, m := metrics.NewManualHarness(t)
	h := newHarness(t)
	r := h.reconciler("m1")
	r.Metrics = m.Reactivate
	h.drainForReboot("worker-1", "w1")
	h.state("w1", stateDown)
	h.tick(r, OutcomeDownSeen)
	h.state("w1", stateReady)
	h.tick(r, OutcomeReleased)

	points := metrics.FindCounter(t, reader, "swarm_auto_drain.reactivate.steps")
	for _, step := range []string{"down_seen", "activate", "release"} {
		var found bool
		for _, dp := range points {
			s, _ := dp.Attributes.Value("step")
			o, _ := dp.Attributes.Value("outcome")
			if s.AsString() == step && o.AsString() == "ok" && dp.Value == 1 {
				found = true
			}
		}
		if !found {
			t.Errorf("no steps{step=%s,outcome=ok}=1 point in %+v", step, points)
		}
	}
}
