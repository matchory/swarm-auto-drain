package lock

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

type fakeDocker struct {
	mu     sync.Mutex
	data   []byte
	exists bool
}

func (f *fakeDocker) ConfigCreate(_ context.Context, _ string, data []byte, _ map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exists {
		return "", errors.New(
			"Error response from daemon: rpc error: code = AlreadyExists " +
				"desc = name conflicts with an existing object",
		)
	}
	f.data = append([]byte(nil), data...)
	f.exists = true
	return "fake-id", nil
}

func (f *fakeDocker) ConfigInspect(_ context.Context, _ string) ([]byte, map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists {
		return nil, nil, errors.New("Error: no such config")
	}
	return append([]byte(nil), f.data...), nil, nil
}

func (f *fakeDocker) ConfigRemove(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists {
		return errors.New("Error: no such config")
	}
	f.exists = false
	f.data = nil
	return nil
}

func newStore(f *fakeDocker) *ConfigStore {
	return &ConfigStore{
		Docker:     f,
		ConfigName: "lock",
		Now:        func() time.Time { return time.Unix(1000, 0) },
		Metrics:    metrics.NewNoop().Lock,
	}
}

func TestAcquire_FreshSuccess(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	r, err := s.Acquire(context.Background(), "node-a", 30*time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if r.Status != Acquired {
		t.Errorf("Status = %v, want Acquired", r.Status)
	}
	if r.Current == nil ||
		r.Current.Holder != "node-a" ||
		r.Current.AcquiredAt != 1000 ||
		r.Current.TTLSeconds != 1800 {
		t.Errorf("Current = %+v", r.Current)
	}
}

func TestAcquire_HeldByOther_Fresh(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	_, _ = s.Acquire(context.Background(), "node-a", 30*time.Minute)

	r, err := s.Acquire(context.Background(), "node-b", 30*time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if r.Status != HeldByOther {
		t.Errorf("Status = %v, want HeldByOther", r.Status)
	}
	if r.Current.Holder != "node-a" {
		t.Errorf("Current.Holder = %q", r.Current.Holder)
	}
}

func TestAcquire_StaleSteal(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	_, _ = s.Acquire(context.Background(), "node-a", 30*time.Minute)

	s.Now = func() time.Time { return time.Unix(1000+1800+1, 0) }

	r, err := s.Acquire(context.Background(), "node-b", 30*time.Minute)
	if err != nil {
		t.Fatalf("Acquire after stale: %v", err)
	}
	if r.Status != AcquiredViaSteal {
		t.Errorf("Status = %v, want AcquiredViaSteal", r.Status)
	}
	if r.Current.Holder != "node-b" {
		t.Errorf("Current.Holder = %q, want node-b", r.Current.Holder)
	}
}

// clobberDocker simulates a concurrent peer that removes our freshly
// recreated lock and writes its own in the non-atomic remove→recreate window:
// once we recreate, every subsequent inspect reports the peer as holder.
type clobberDocker struct {
	stale  []byte // the expired holder we observe before stealing
	peer   []byte // the holder a racing peer writes over our recreate
	exists bool
	stole  bool
}

func (c *clobberDocker) ConfigCreate(_ context.Context, _ string, _ []byte, _ map[string]string) (string, error) {
	if c.exists {
		return "", errors.New("rpc error: code = AlreadyExists desc = name conflicts")
	}
	c.exists = true
	c.stole = true // our recreate succeeded — but a peer is about to clobber it.
	return "id", nil
}

func (c *clobberDocker) ConfigInspect(_ context.Context, _ string) ([]byte, map[string]string, error) {
	if !c.exists {
		return nil, nil, errors.New("Error: no such config")
	}
	if c.stole {
		return append([]byte(nil), c.peer...), nil, nil
	}
	return append([]byte(nil), c.stale...), nil, nil
}

func (c *clobberDocker) ConfigRemove(_ context.Context, _ string) error {
	c.exists = false
	return nil
}

func TestAcquire_StaleSteal_PeerClobbersRecreate(t *testing.T) {
	stale, _ := json.Marshal(Lock{Holder: "node-a", AcquiredAt: 1000, TTLSeconds: 1800})
	peer, _ := json.Marshal(Lock{Holder: "node-c", AcquiredAt: 4000, TTLSeconds: 1800})
	c := &clobberDocker{stale: stale, peer: peer, exists: true}
	s := &ConfigStore{
		Docker:     c,
		ConfigName: "lock",
		Now:        func() time.Time { return time.Unix(1000+1800+1, 0) },
		Metrics:    metrics.NewNoop().Lock,
	}

	r, err := s.Acquire(context.Background(), "node-b", 30*time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Post-recreate verification must detect the peer's clobber and refuse to
	// claim ownership, rather than returning AcquiredViaSteal with a false
	// belief while node-c also believes it holds the cluster mutex.
	if r.Status != HeldByOther {
		t.Errorf("Status = %v, want HeldByOther (peer clobbered our recreate)", r.Status)
	}
	if r.Current == nil || r.Current.Holder != "node-c" {
		t.Errorf("Current = %+v, want holder node-c", r.Current)
	}
}

func TestRelease_HolderOK(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	_, _ = s.Acquire(context.Background(), "node-a", 30*time.Minute)
	if err := s.Release(context.Background(), "node-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got, err := s.Inspect(context.Background())
	if got != nil {
		t.Errorf("lock still present after release: %+v", got)
	}
	if !errors.Is(err, ErrNoLock) {
		t.Errorf("Inspect after release: err=%v, want ErrNoLock", err)
	}
}

func TestRelease_NotHolder(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	_, _ = s.Acquire(context.Background(), "node-a", 30*time.Minute)
	err := s.Release(context.Background(), "node-b")
	if err == nil || !errors.Is(err, ErrNotHolder) {
		t.Fatalf("Release wrong-holder: err=%v, want ErrNotHolder", err)
	}
}

func TestRelease_NoLock(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	err := s.Release(context.Background(), "node-a")
	if err == nil || !errors.Is(err, ErrNoLock) {
		t.Fatalf("Release no-lock: err=%v, want ErrNoLock", err)
	}
}

func TestInspect_None(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	got, err := s.Inspect(context.Background())
	if !errors.Is(err, ErrNoLock) {
		t.Fatalf("Inspect: err=%v, want ErrNoLock", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestInspect_Roundtrip(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	_, _ = s.Acquire(context.Background(), "node-a", 30*time.Minute)
	got, err := s.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want lock")
	}
	if got.Holder != "node-a" {
		t.Errorf("Holder = %q", got.Holder)
	}
	var l Lock
	if unmarshalErr := json.Unmarshal(f.data, &l); unmarshalErr != nil {
		t.Errorf("stored bytes not valid Lock JSON: %v", unmarshalErr)
	}
}

func TestIsNotFoundString(t *testing.T) {
	// Both capitalizations of the daemon message must classify — dockerd
	// has emitted both across versions, and custom Docker SDK wrappers
	// may normalize either way. Lock the case-insensitive contract.
	cases := []string{
		"Error: no such config",
		"Error response from daemon: No such config: swarm-auto-drain-lock",
		"no such config: foo",
		"No such config: foo",
	}
	for _, msg := range cases {
		if !isNotFound(errors.New(msg)) {
			t.Errorf("isNotFound(%q) = false, want true", msg)
		}
	}
	if !isAlreadyExists(errors.New("name conflicts with an existing object")) {
		t.Error("isAlreadyExists did not match")
	}
	if isNotFound(errors.New("unrelated")) || isAlreadyExists(errors.New("unrelated")) {
		t.Error("false positive")
	}
	_ = strings.Contains
}

func TestConfigStore_Acquire_MetricsAcquired(t *testing.T) {
	reader, m := metrics.NewManualHarness(t)
	f := &fakeDocker{}
	s := &ConfigStore{
		Docker:     f,
		Now:        func() time.Time { return time.Unix(1000, 0) },
		ConfigName: "lock",
		Metrics:    m.Lock,
	}

	if _, err := s.Acquire(context.Background(), "node-a", time.Minute); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	points := metrics.FindCounter(t, reader, "swarm_auto_drain.lock.acquire")
	dp := metrics.PointWithAttrs(t, points, "outcome", "acquired")
	if dp.Value != 1 {
		t.Errorf("acquire counter = %d, want 1", dp.Value)
	}
	held := metrics.FindGauge(t, reader, "swarm_auto_drain.lock.held")
	if len(held) == 0 || held[0].Value != 1 {
		t.Errorf("held gauge = %+v, want value 1", held)
	}
}

func TestConfigStore_Acquire_MetricsEexist(t *testing.T) {
	reader, m := metrics.NewManualHarness(t)
	f := &fakeDocker{}
	s := &ConfigStore{
		Docker:     f,
		Now:        func() time.Time { return time.Unix(1000, 0) },
		ConfigName: "lock",
		Metrics:    m.Lock,
	}
	if _, err := s.Acquire(context.Background(), "node-a", 30*time.Minute); err != nil {
		t.Fatalf("seed Acquire: %v", err)
	}

	if _, err := s.Acquire(context.Background(), "node-b", 30*time.Minute); err != nil {
		t.Fatalf("contended Acquire: %v", err)
	}

	points := metrics.FindCounter(t, reader, "swarm_auto_drain.lock.acquire")
	dp := metrics.PointWithAttrs(t, points, "outcome", "eexist")
	if dp.Value != 1 {
		t.Errorf("eexist counter = %d, want 1", dp.Value)
	}
}

func TestConfigStore_Acquire_MetricsStolen(t *testing.T) {
	reader, m := metrics.NewManualHarness(t)
	now := time.Unix(10_000, 0)
	stale := Lock{Holder: "node-a", AcquiredAt: now.Add(-time.Hour).Unix(), TTLSeconds: 10}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	f := &fakeDocker{data: data, exists: true}
	s := &ConfigStore{
		Docker:     f,
		Now:        func() time.Time { return now },
		ConfigName: "lock",
		Metrics:    m.Lock,
	}

	r, err := s.Acquire(context.Background(), "node-b", time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if r.Status != AcquiredViaSteal {
		t.Fatalf("status = %v, want AcquiredViaSteal", r.Status)
	}

	acquire := metrics.FindCounter(t, reader, "swarm_auto_drain.lock.acquire")
	dp := metrics.PointWithAttrs(t, acquire, "outcome", "stolen")
	if dp.Value != 1 {
		t.Errorf("stolen counter = %d, want 1", dp.Value)
	}
	steals := metrics.FindCounter(t, reader, "swarm_auto_drain.lock.steals")
	sdp := metrics.PointWithAttrs(t, steals, "reason", "ttl_expired")
	if sdp.Value != 1 {
		t.Errorf("steals counter = %d, want 1", sdp.Value)
	}
	held := metrics.FindGauge(t, reader, "swarm_auto_drain.lock.held")
	if len(held) == 0 || held[0].Value != 1 {
		t.Errorf("held gauge = %+v, want value 1", held)
	}
}

func TestConfigStore_Release_MetricsReleased(t *testing.T) {
	reader, m := metrics.NewManualHarness(t)
	f := &fakeDocker{}
	tNow := time.Unix(1000, 0)
	s := &ConfigStore{
		Docker:     f,
		Now:        func() time.Time { return tNow },
		ConfigName: "lock",
		Metrics:    m.Lock,
	}
	if _, err := s.Acquire(context.Background(), "node-a", time.Minute); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Advance the clock so the hold histogram observes a non-zero duration.
	tNow = tNow.Add(5 * time.Second)

	if err := s.Release(context.Background(), "node-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	rel := metrics.FindCounter(t, reader, "swarm_auto_drain.lock.release")
	dp := metrics.PointWithAttrs(t, rel, "outcome", "released")
	if dp.Value != 1 {
		t.Errorf("released counter = %d, want 1", dp.Value)
	}
	if count := metrics.FindHistogramCount(t, reader, "swarm_auto_drain.lock.hold"); count != 1 {
		t.Errorf("hold histogram count = %d, want 1", count)
	}
	held := metrics.FindGauge(t, reader, "swarm_auto_drain.lock.held")
	// Latest gauge observation should be 0 after release.
	if len(held) == 0 || held[len(held)-1].Value != 0 {
		t.Errorf("held gauge after release = %+v, want value 0", held)
	}
}

func TestConfigStore_Release_MetricsNotHolder(t *testing.T) {
	reader, m := metrics.NewManualHarness(t)
	f := &fakeDocker{}
	s := &ConfigStore{
		Docker:     f,
		Now:        func() time.Time { return time.Unix(1000, 0) },
		ConfigName: "lock",
		Metrics:    m.Lock,
	}
	if _, err := s.Acquire(context.Background(), "node-a", time.Minute); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if err := s.Release(context.Background(), "node-b"); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("Release: err=%v, want ErrNotHolder", err)
	}

	rel := metrics.FindCounter(t, reader, "swarm_auto_drain.lock.release")
	dp := metrics.PointWithAttrs(t, rel, "outcome", "not_holder")
	if dp.Value != 1 {
		t.Errorf("not_holder counter = %d, want 1", dp.Value)
	}
}
