package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/errdefs"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
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
	Metrics    metrics.LockMetrics
	holdStart  time.Time
	holdMu     sync.Mutex
}

// Acquire takes the cluster lock for `holder` with the given TTL.
func (s *ConfigStore) Acquire(ctx context.Context, holder string, ttl time.Duration) (AcquireResult, error) {
	return s.tryAcquire(ctx, holder, ttl, false)
}

// Release drops the lock if `holder` currently owns it.
func (s *ConfigStore) Release(ctx context.Context, holder string) error {
	existing, err := s.loadCurrent(ctx)
	if errors.Is(err, ErrNoLock) {
		s.recordReleaseOutcome(ctx, "not_found")
		return ErrNoLock
	}
	if err != nil {
		s.recordReleaseOutcome(ctx, "error")
		return fmt.Errorf("inspect: %w", err)
	}
	if existing.Holder != holder {
		s.recordReleaseOutcome(ctx, "not_holder")
		return ErrNotHolder
	}
	if removeErr := s.Docker.ConfigRemove(ctx, s.ConfigName); removeErr != nil && !isNotFound(removeErr) {
		s.recordReleaseOutcome(ctx, "error")
		return fmt.Errorf("remove: %w", removeErr)
	}
	s.recordReleaseSuccess(ctx)
	return nil
}

// recordAcquireOutcome increments the lock.acquire counter with the
// outcome attribute. Centralized so every branch in tryAcquire records
// with the same attribute shape.
func (s *ConfigStore) recordAcquireOutcome(ctx context.Context, outcome string) {
	s.Metrics.Acquire.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("outcome", outcome)))
}

// recordReleaseOutcome increments the lock.release counter with the
// outcome attribute, for the error/not-found/not-holder branches.
func (s *ConfigStore) recordReleaseOutcome(ctx context.Context, outcome string) {
	s.Metrics.Release.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("outcome", outcome)))
}

// markHeld records the moment this replica took the lock. Paired with
// recordReleaseSuccess to compute hold duration without observable
// callbacks.
func (s *ConfigStore) markHeld(at time.Time) {
	s.holdMu.Lock()
	s.holdStart = at
	s.holdMu.Unlock()
}

// recordReleaseSuccess emits the released counter, the held=0 gauge,
// and — if we have a recorded hold start — the hold-duration histogram.
func (s *ConfigStore) recordReleaseSuccess(ctx context.Context) {
	s.holdMu.Lock()
	start := s.holdStart
	s.holdStart = time.Time{}
	s.holdMu.Unlock()
	if !start.IsZero() {
		s.Metrics.Hold.Record(ctx, time.Since(start).Seconds())
	}
	s.recordReleaseOutcome(ctx, "released")
	s.Metrics.Held.Record(ctx, 0)
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
//nolint:gocognit,gocyclo,cyclop,revive,funlen // stale-steal race protection must stay linear (see step 5).
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
		s.markHeld(s.now())
		s.recordAcquireOutcome(ctx, "acquired")
		s.Metrics.Held.Record(ctx, 1)
		return AcquireResult{Current: &doc, Status: Acquired}, nil
	}
	if !isAlreadyExists(createErr) {
		s.recordAcquireOutcome(ctx, "error")
		return AcquireResult{}, fmt.Errorf("create: %w", createErr)
	}

	// 2. EEXIST — read existing holder.
	existing, inspectErr := s.loadCurrent(ctx)
	if inspectErr != nil && !errors.Is(inspectErr, ErrNoLock) {
		s.recordAcquireOutcome(ctx, "error")
		return AcquireResult{}, fmt.Errorf("inspect after EEXIST: %w", inspectErr)
	}
	if existing == nil {
		// Vanished between create and inspect (peer released). Retry the
		// fresh-create path once; if it disappears again we treat the slot
		// as contested rather than spin forever.
		if retried {
			s.recordAcquireOutcome(ctx, "eexist")
			return AcquireResult{Current: nil, Status: HeldByOther}, nil
		}
		return s.tryAcquire(ctx, holder, ttl, true)
	}

	// 3. Same holder retrying — idempotent re-acquire.
	if existing.Holder == holder {
		s.recordAcquireOutcome(ctx, "acquired")
		return AcquireResult{Current: existing, Status: Acquired}, nil
	}

	// 4. Held by someone else, still fresh.
	if s.now().UTC().Unix()-existing.AcquiredAt <= int64(existing.TTLSeconds) {
		s.recordAcquireOutcome(ctx, "eexist")
		return AcquireResult{Current: existing, Status: HeldByOther}, nil
	}

	// 5. Stale steal. Re-inspect right before remove to catch the case where
	// a peer already completed its own steal+create cycle in the gap; without
	// this check we'd delete the peer's fresh lock and both nodes would
	// believe they hold the cluster-wide mutex. Docker Configs have no atomic
	// compare-and-delete, so this re-inspect SHRINKS but cannot fully close
	// the remove→recreate window — a peer that confirms staleness at the same
	// instant can still remove our fresh lock after our recreate. Step 6's
	// post-recreate ownership check catches every interleaving where that
	// peer's cycle lands before our recreate returns.
	confirmed, confirmErr := s.loadCurrent(ctx)
	if confirmErr != nil && !errors.Is(confirmErr, ErrNoLock) {
		s.recordAcquireOutcome(ctx, "error")
		return AcquireResult{}, fmt.Errorf("re-inspect before steal: %w", confirmErr)
	}
	if confirmed == nil {
		if retried {
			s.recordAcquireOutcome(ctx, "eexist")
			return AcquireResult{Current: nil, Status: HeldByOther}, nil
		}
		return s.tryAcquire(ctx, holder, ttl, true)
	}
	if confirmed.Holder != existing.Holder || confirmed.AcquiredAt != existing.AcquiredAt {
		// Lock was replaced between inspect and re-inspect. Report whoever
		// now holds it instead of risking a wrong remove.
		s.recordAcquireOutcome(ctx, "eexist")
		return AcquireResult{Current: confirmed, Status: HeldByOther}, nil
	}

	// 6. Confirmed stale — remove + recreate.
	if removeErr := s.Docker.ConfigRemove(ctx, s.ConfigName); removeErr != nil && !isNotFound(removeErr) {
		s.recordAcquireOutcome(ctx, "error")
		return AcquireResult{}, fmt.Errorf("remove stale: %w", removeErr)
	}
	_, recreateErr := s.Docker.ConfigCreate(ctx, s.ConfigName, data, nil)
	if recreateErr == nil {
		// Confirm we actually own the lock we just recreated. A peer that
		// confirmed the same stale doc can remove our fresh lock (its
		// ConfigRemove is by-name and version-blind) and recreate its own in
		// the non-atomic window; if its create lands before this inspect, the
		// stored holder is the peer's, not ours, and claiming AcquiredViaSteal
		// would give us a false belief of ownership while the peer also
		// believes it holds the cluster mutex.
		owner, ownErr := s.loadCurrent(ctx)
		if ownErr == nil && owner != nil &&
			(owner.Holder != holder || owner.AcquiredAt != doc.AcquiredAt) {
			s.recordAcquireOutcome(ctx, "eexist")
			return AcquireResult{Current: owner, Status: HeldByOther}, nil
		}
		s.markHeld(s.now())
		s.recordAcquireOutcome(ctx, "stolen")
		s.Metrics.Steals.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("reason", "ttl_expired")))
		s.Metrics.Held.Record(ctx, 1)
		return AcquireResult{Current: &doc, Status: AcquiredViaSteal}, nil
	}
	if !isAlreadyExists(recreateErr) {
		s.recordAcquireOutcome(ctx, "error")
		return AcquireResult{}, fmt.Errorf("create after steal: %w", recreateErr)
	}
	// A peer beat us to the recreate. Best-effort report of the winner; an
	// inspect failure here is non-fatal — caller still gets HeldByOther.
	winner, winnerErr := s.loadCurrent(ctx)
	if winnerErr != nil && !errors.Is(winnerErr, ErrNoLock) {
		winner = nil
	}
	s.recordAcquireOutcome(ctx, "eexist")
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
