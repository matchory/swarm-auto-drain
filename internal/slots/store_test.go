package slots

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

type fakeDocker struct {
	configs map[string][]byte
	// raceOn names a config that a "peer" creates just before our create
	// call lands, to exercise the EEXIST path.
	raceOn     string
	inspectErr error
	mu         sync.Mutex
}

func newFake() *fakeDocker { return &fakeDocker{configs: map[string][]byte{}} }

func (f *fakeDocker) ConfigCreate(_ context.Context, name string, data []byte, _ map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name == f.raceOn {
		f.configs[name] = []byte(`{"holder":"peer","acquired_at":1}`)
		f.raceOn = ""
	}
	if _, ok := f.configs[name]; ok {
		return "", errors.New("Error response from daemon: rpc error: code = AlreadyExists desc = name conflicts with an existing object")
	}
	f.configs[name] = append([]byte(nil), data...)
	return "id-" + name, nil
}

func (f *fakeDocker) ConfigInspect(_ context.Context, name string) ([]byte, map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return nil, nil, f.inspectErr
	}
	data, ok := f.configs[name]
	if !ok {
		return nil, nil, errors.New("Error: No such config: " + name)
	}
	return append([]byte(nil), data...), nil, nil
}

func (f *fakeDocker) ConfigRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.configs[name]; !ok {
		return errors.New("Error: No such config: " + name)
	}
	delete(f.configs, name)
	return nil
}

func newStore(f *fakeDocker, workerSlots int) *Store {
	return &Store{
		Docker:      f,
		Prefix:      "reboot-slot",
		WorkerSlots: workerSlots,
		Now:         func() time.Time { return time.Unix(1000, 0) },
		Metrics:     metrics.NewNoop().Slots,
	}
}

func mustAcquire(t *testing.T, s *Store, holder string, kind Kind) *Slot {
	t.Helper()
	slot, _, err := s.Acquire(context.Background(), holder, kind)
	if err != nil {
		t.Fatalf("Acquire(%s): %v", holder, err)
	}
	return slot
}

func TestAcquire_WorkerTakesFirstFreeSlot(t *testing.T) {
	s := newStore(newFake(), 2)
	got := mustAcquire(t, s, "w1", Worker)
	if got == nil || got.Name != "reboot-slot-1" || got.Holder != "w1" || got.AcquiredAt != 1000 {
		t.Fatalf("slot = %+v, want reboot-slot-1 held by w1 at 1000", got)
	}
	got = mustAcquire(t, s, "w2", Worker)
	if got == nil || got.Name != "reboot-slot-2" {
		t.Fatalf("second worker slot = %+v, want reboot-slot-2", got)
	}
}

func TestAcquire_StoredDataHasNoName(t *testing.T) {
	f := newFake()
	mustAcquire(t, newStore(f, 1), "w1", Worker)
	if got := string(f.configs["reboot-slot-1"]); got != `{"holder":"w1","acquired_at":1000}` {
		t.Fatalf("stored = %s", got)
	}
}

func TestAcquire_FullReportsHolders(t *testing.T) {
	s := newStore(newFake(), 1)
	mustAcquire(t, s, "w1", Worker)
	slot, holders, err := s.Acquire(context.Background(), "w2", Worker)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if slot != nil {
		t.Fatalf("slot = %+v, want nil (full)", slot)
	}
	if len(holders) != 1 || holders[0] != "w1" {
		t.Fatalf("holders = %v, want [w1]", holders)
	}
}

func TestAcquire_SameHolderGetsItsSlotBack(t *testing.T) {
	f := newFake()
	s := newStore(f, 2)
	first := mustAcquire(t, s, "w1", Worker)
	again := mustAcquire(t, s, "w1", Worker)
	if again == nil || again.Name != first.Name {
		t.Fatalf("re-acquire = %+v, want %s", again, first.Name)
	}
	if len(f.configs) != 1 {
		t.Fatalf("configs = %d, want 1 (no second slot)", len(f.configs))
	}
}

func TestAcquire_ManagersShareOneSlotApartFromWorkers(t *testing.T) {
	s := newStore(newFake(), 1)
	mustAcquire(t, s, "w1", Worker)
	m := mustAcquire(t, s, "m1", Manager)
	if m == nil || m.Name != "reboot-slot-manager" {
		t.Fatalf("manager slot = %+v, want reboot-slot-manager", m)
	}
	slot, holders, err := s.Acquire(context.Background(), "m2", Manager)
	if err != nil || slot != nil {
		t.Fatalf("second manager: slot=%+v err=%v, want full", slot, err)
	}
	if len(holders) != 1 || holders[0] != "m1" {
		t.Fatalf("holders = %v, want [m1]", holders)
	}
}

func TestAcquire_LostCreateRaceMovesToNextSlot(t *testing.T) {
	f := newFake()
	f.raceOn = "reboot-slot-1"
	s := newStore(f, 2)
	got := mustAcquire(t, s, "w1", Worker)
	if got == nil || got.Name != "reboot-slot-2" {
		t.Fatalf("slot = %+v, want reboot-slot-2 after losing slot-1", got)
	}
}

func TestAcquire_InspectErrorFails(t *testing.T) {
	f := newFake()
	f.inspectErr = errors.New("connection refused")
	s := newStore(f, 1)
	if _, _, err := s.Acquire(context.Background(), "w1", Worker); err == nil {
		t.Fatal("Acquire: want error when the store cannot be read")
	}
}

func TestRelease(t *testing.T) {
	f := newFake()
	s := newStore(f, 1)
	mustAcquire(t, s, "w1", Worker)

	released, err := s.Release(context.Background(), "w2")
	if err != nil || released {
		t.Fatalf("Release(non-holder) = %v, %v; want false, nil", released, err)
	}
	if len(f.configs) != 1 {
		t.Fatal("non-holder release removed a slot")
	}
	released, err = s.Release(context.Background(), "w1")
	if err != nil || !released {
		t.Fatalf("Release(holder) = %v, %v; want true, nil", released, err)
	}
	if len(f.configs) != 0 {
		t.Fatal("holder release left the slot in place")
	}
}

func TestHeldBy(t *testing.T) {
	s := newStore(newFake(), 2)
	if got, err := s.HeldBy(context.Background(), "w1"); err != nil || got != nil {
		t.Fatalf("HeldBy before acquire = %+v, %v; want nil, nil", got, err)
	}
	mustAcquire(t, s, "w2", Worker)
	mustAcquire(t, s, "w1", Worker)
	got, err := s.HeldBy(context.Background(), "w1")
	if err != nil || got == nil || got.Name != "reboot-slot-2" {
		t.Fatalf("HeldBy = %+v, %v; want reboot-slot-2", got, err)
	}
}
