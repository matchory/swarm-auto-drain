package lock

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMarkDrained_HolderRecordsMark(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	ctx := context.Background()
	_, _ = s.Acquire(ctx, "node-a", 30*time.Minute)

	if err := s.MarkDrained(ctx, "node-a", "id-a"); err != nil {
		t.Fatalf("MarkDrained: %v", err)
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	want := DrainMark{NodeID: "id-a", DrainedAt: 1000}
	if snap.Mark == nil || *snap.Mark != want {
		t.Errorf("Mark = %+v, want %+v", snap.Mark, want)
	}
	if snap.Lock.Holder != "node-a" {
		t.Errorf("Holder = %q", snap.Lock.Holder)
	}
}

func TestMarkDrained_NotHolderWritesNothing(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	ctx := context.Background()
	_, _ = s.Acquire(ctx, "node-a", 30*time.Minute)

	if err := s.MarkDrained(ctx, "node-b", "id-b"); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("err = %v, want ErrNotHolder", err)
	}
	if f.labels != nil {
		t.Errorf("labels written for a non-holder: %v", f.labels)
	}
}

func TestMarkDrained_NoLock(t *testing.T) {
	s := newStore(&fakeDocker{})
	if err := s.MarkDrained(context.Background(), "node-a", "id-a"); !errors.Is(err, ErrNoLock) {
		t.Fatalf("err = %v, want ErrNoLock", err)
	}
}

func TestSnapshot_UnmarkedLock(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	_, _ = s.Acquire(context.Background(), "node-a", 30*time.Minute)
	snap, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Mark != nil {
		t.Errorf("Mark = %+v, want nil", snap.Mark)
	}
}

func TestSnapshot_MalformedMarkIsAbsent(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	_, _ = s.Acquire(context.Background(), "node-a", 30*time.Minute)
	f.labels = map[string]string{labelNodeID: "id-a", labelDrainedAt: "yesterday"}
	snap, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Mark != nil {
		t.Errorf("Mark = %+v, want nil for a malformed mark", snap.Mark)
	}
}

func TestUpdateMark_StaleVersionRejected(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	ctx := context.Background()
	_, _ = s.Acquire(ctx, "node-a", 30*time.Minute)
	_ = s.MarkDrained(ctx, "node-a", "id-a")
	stale, _ := s.Snapshot(ctx)

	fresh, _ := s.Snapshot(ctx)
	fresh.Mark.DownSeenAt = 1100
	if err := s.UpdateMark(ctx, fresh, *fresh.Mark); err != nil {
		t.Fatalf("UpdateMark fresh: %v", err)
	}
	stale.Mark.ActivatedAt = 1200
	if err := s.UpdateMark(ctx, stale, *stale.Mark); err == nil {
		t.Fatal("UpdateMark with a stale version succeeded")
	}
	got, _ := s.Snapshot(ctx)
	if got.Mark.DownSeenAt != 1100 || got.Mark.ActivatedAt != 0 {
		t.Errorf("Mark = %+v, want the fresh write only", got.Mark)
	}
}

func TestReleaseSnapshot_LeavesNewerInstanceAlone(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	ctx := context.Background()
	_, _ = s.Acquire(ctx, "node-a", 30*time.Minute)
	old, _ := s.Snapshot(ctx)

	// node-a releases and node-b takes the lock before our remove lands.
	_ = s.Release(ctx, "node-a")
	_, _ = s.Acquire(ctx, "node-b", 30*time.Minute)

	if err := s.ReleaseSnapshot(ctx, old); !errors.Is(err, ErrNoLock) {
		t.Fatalf("err = %v, want ErrNoLock", err)
	}
	cur, err := s.Inspect(ctx)
	if err != nil || cur.Holder != "node-b" {
		t.Errorf("lock = %+v, %v; want node-b's lock untouched", cur, err)
	}
}

func TestReleaseSnapshot_RemovesThatInstance(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	ctx := context.Background()
	_, _ = s.Acquire(ctx, "node-a", 30*time.Minute)
	snap, _ := s.Snapshot(ctx)
	if err := s.ReleaseSnapshot(ctx, snap); err != nil {
		t.Fatalf("ReleaseSnapshot: %v", err)
	}
	if _, err := s.Inspect(ctx); !errors.Is(err, ErrNoLock) {
		t.Errorf("Inspect err = %v, want ErrNoLock", err)
	}
}
