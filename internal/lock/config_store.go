package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/errdefs"
)

// docker is the subset of internal/docker.Client that ConfigStore needs.
// Defined locally so this package doesn't import internal/docker — the
// real Client satisfies it implicitly.
type docker interface {
	ConfigCreate(ctx context.Context, name string, data []byte, labels map[string]string) (string, error)
	ConfigInspect(ctx context.Context, name string) (data []byte, labels map[string]string, err error)
	ConfigRemove(ctx context.Context, name string) error
}

// ConfigStore implements Store on top of the Docker SDK's Config primitives.
// Field order puts pointer/interface fields first to shrink the GC pointer scan.
type ConfigStore struct {
	Docker     docker
	Now        func() time.Time // Injectable for tests; nil → time.Now.
	ConfigName string
}

// Acquire takes the cluster lock for `holder` with the given TTL.
func (s *ConfigStore) Acquire(ctx context.Context, holder string, ttl time.Duration) (AcquireResult, error) {
	return s.tryAcquire(ctx, holder, ttl, false)
}

// Release drops the lock if `holder` currently owns it.
func (s *ConfigStore) Release(ctx context.Context, holder string) error {
	existing, err := s.loadCurrent(ctx)
	if errors.Is(err, ErrNoLock) {
		return ErrNoLock
	}
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	if existing.Holder != holder {
		return ErrNotHolder
	}
	if removeErr := s.Docker.ConfigRemove(ctx, s.ConfigName); removeErr != nil && !isNotFound(removeErr) {
		return fmt.Errorf("remove: %w", removeErr)
	}
	return nil
}

// Inspect returns the current Lock document, or ErrNoLock if no lock is held.
func (s *ConfigStore) Inspect(ctx context.Context) (*Lock, error) {
	return s.loadCurrent(ctx)
}

// tryAcquire is the recursive worker. `retried` prevents an infinite loop
// when a peer keeps releasing the lock between our create attempt and our
// follow-up inspect. The flow is intentionally written as one linear method
// so the stale-steal race protection (step 5) reads top-to-bottom alongside
// the surrounding CAS steps — see the block comment at step 5.
//
//nolint:gocognit,gocyclo,cyclop,revive // stale-steal race protection must stay linear (see step 5).
func (s *ConfigStore) tryAcquire(
	ctx context.Context, holder string, ttl time.Duration, retried bool,
) (AcquireResult, error) {
	doc := Lock{Holder: holder, AcquiredAt: s.now().UTC().Unix(), TTLSeconds: int(ttl.Seconds())}
	data, err := json.Marshal(doc)
	if err != nil {
		return AcquireResult{}, fmt.Errorf("marshal lock: %w", err)
	}

	// 1. Try a fresh create.
	_, createErr := s.Docker.ConfigCreate(ctx, s.ConfigName, data, nil)
	if createErr == nil {
		return AcquireResult{Current: &doc, Status: Acquired}, nil
	}
	if !isAlreadyExists(createErr) {
		return AcquireResult{}, fmt.Errorf("create: %w", createErr)
	}

	// 2. EEXIST — read existing holder.
	existing, inspectErr := s.loadCurrent(ctx)
	if inspectErr != nil && !errors.Is(inspectErr, ErrNoLock) {
		return AcquireResult{}, fmt.Errorf("inspect after EEXIST: %w", inspectErr)
	}
	if existing == nil {
		// Vanished between create and inspect (peer released). Retry the
		// fresh-create path once; if it disappears again we treat the slot
		// as contested rather than spin forever.
		if retried {
			return AcquireResult{Current: nil, Status: HeldByOther}, nil
		}
		return s.tryAcquire(ctx, holder, ttl, true)
	}

	// 3. Same holder retrying — idempotent re-acquire.
	if existing.Holder == holder {
		return AcquireResult{Current: existing, Status: Acquired}, nil
	}

	// 4. Held by someone else, still fresh.
	if s.now().UTC().Unix()-existing.AcquiredAt <= int64(existing.TTLSeconds) {
		return AcquireResult{Current: existing, Status: HeldByOther}, nil
	}

	// 5. Stale steal. Re-inspect right before remove to catch the case where
	// a peer already completed its own steal+create cycle in the gap; without
	// this check we'd delete the peer's fresh lock and both nodes would
	// believe they hold the cluster-wide mutex.
	confirmed, confirmErr := s.loadCurrent(ctx)
	if confirmErr != nil && !errors.Is(confirmErr, ErrNoLock) {
		return AcquireResult{}, fmt.Errorf("re-inspect before steal: %w", confirmErr)
	}
	if confirmed == nil {
		if retried {
			return AcquireResult{Current: nil, Status: HeldByOther}, nil
		}
		return s.tryAcquire(ctx, holder, ttl, true)
	}
	if confirmed.Holder != existing.Holder || confirmed.AcquiredAt != existing.AcquiredAt {
		// Lock was replaced between inspect and re-inspect. Report whoever
		// now holds it instead of risking a wrong remove.
		return AcquireResult{Current: confirmed, Status: HeldByOther}, nil
	}

	// 6. Confirmed stale — remove + recreate.
	if removeErr := s.Docker.ConfigRemove(ctx, s.ConfigName); removeErr != nil && !isNotFound(removeErr) {
		return AcquireResult{}, fmt.Errorf("remove stale: %w", removeErr)
	}
	_, recreateErr := s.Docker.ConfigCreate(ctx, s.ConfigName, data, nil)
	if recreateErr == nil {
		return AcquireResult{Current: &doc, Status: AcquiredViaSteal}, nil
	}
	if !isAlreadyExists(recreateErr) {
		return AcquireResult{}, fmt.Errorf("create after steal: %w", recreateErr)
	}
	// A peer beat us to the recreate. Best-effort report of the winner; an
	// inspect failure here is non-fatal — caller still gets HeldByOther.
	winner, winnerErr := s.loadCurrent(ctx)
	if winnerErr != nil && !errors.Is(winnerErr, ErrNoLock) {
		winner = nil
	}
	return AcquireResult{Current: winner, Status: HeldByOther}, nil
}

func (s *ConfigStore) loadCurrent(ctx context.Context) (*Lock, error) {
	data, _, err := s.Docker.ConfigInspect(ctx, s.ConfigName)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNoLock
		}
		return nil, fmt.Errorf("config inspect: %w", err)
	}
	var doc Lock
	if unmarshalErr := json.Unmarshal(data, &doc); unmarshalErr != nil {
		return nil, fmt.Errorf("unmarshal lock: %w", unmarshalErr)
	}
	return &doc, nil
}

func (s *ConfigStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// isNotFound checks the Docker SDK errdefs first (the canonical signal), then
// falls back to a specific substring used by the real client's wrapper and by
// the test fakes. The fallback is intentionally narrow to avoid mis-classifying
// unrelated daemon errors that happen to contain the words "not found".
//
// Case-insensitive: dockerd has emitted both "No such config" (CLI-facing)
// and "no such config" (API-facing) across versions, and custom Docker SDK
// wrappers may normalize either way. The fallback only fires when errdefs
// has already failed, so the false-positive risk is bounded.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errdefs.IsNotFound(err) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "no such config")
}

// isAlreadyExists mirrors isNotFound for the create-EEXIST path. Docker
// returns AlreadyExists as a Conflict per errdefs; the substring fallback
// matches the gRPC-style ("AlreadyExists") and daemon-style ("name conflicts")
// renderings.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	if errdefs.IsConflict(err) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "name conflicts") || strings.Contains(msg, "AlreadyExists")
}
