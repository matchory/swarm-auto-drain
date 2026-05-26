package lock

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
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
	return &ConfigStore{Docker: f, ConfigName: "lock", Now: func() time.Time { return time.Unix(1000, 0) }}
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
