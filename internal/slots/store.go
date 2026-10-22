// Package slots hands out the cluster's reboot slots. Each slot is a Docker
// Config; "create fails if the name exists" is the compare-and-swap, so any
// manager can hand out slots without two of them giving away the same one.
package slots

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/errdefs"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

// docker is the subset of internal/docker.Client the store needs, declared
// here so this package does not import internal/docker.
type docker interface {
	ConfigCreate(ctx context.Context, name string, data []byte, labels map[string]string) (string, error)
	ConfigInspect(ctx context.Context, name string) (data []byte, labels map[string]string, err error)
	ConfigRemove(ctx context.Context, name string) error
}

// holderLabel repeats the holder on the Config so `docker config ls` shows it.
const holderLabel = "com.matchory.swarm-auto-drain.holder"

// Kind says which slots a caller draws from.
type Kind int

const (
	// Worker slots: <Prefix>-1 … <Prefix>-N.
	Worker Kind = iota
	// Manager slot: <Prefix>-manager, one for all managers.
	Manager
)

func (k Kind) String() string {
	if k == Manager {
		return "manager"
	}
	return "worker"
}

// Slot is one held reboot slot. Name is the Config's name, not part of its
// data.
type Slot struct {
	Name       string `json:"name,omitempty"`
	Holder     string `json:"holder"`
	AcquiredAt int64  `json:"acquired_at"` // Unix seconds.
}

// Store hands out WorkerSlots worker slots (<Prefix>-1 … <Prefix>-N) and one
// manager slot (<Prefix>-manager). Slots never expire.
type Store struct {
	Docker      docker
	Now         func() time.Time // nil → time.Now.
	Prefix      string
	Metrics     metrics.SlotMetrics
	WorkerSlots int
	// mu serializes this process's acquires, so a caller retrying against
	// the same manager cannot end up holding two slots.
	mu sync.Mutex
}

// Acquire returns the slot holder already holds, or takes the first free
// slot of kind. A nil slot means every slot of that kind is taken; holders
// then lists who has them.
func (s *Store) Acquire(ctx context.Context, holder string, kind Kind) (*Slot, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	names := s.workerNames()
	if kind == Manager {
		names = []string{s.managerName()}
	}

	existing, err := s.HeldBy(ctx, holder)
	if err != nil {
		s.recordAcquire(ctx, kind, "error")
		return nil, nil, err
	}
	if existing != nil {
		s.recordAcquire(ctx, kind, "held")
		return existing, nil, nil
	}

	doc := Slot{Holder: holder, AcquiredAt: s.now().Unix()}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal slot: %w", err)
	}
	var holders []string
	for _, name := range names {
		_, createErr := s.Docker.ConfigCreate(ctx, name, data, map[string]string{holderLabel: holder})
		if createErr == nil {
			doc.Name = name
			s.recordAcquire(ctx, kind, "acquired")
			return &doc, nil, nil
		}
		if !isAlreadyExists(createErr) {
			s.recordAcquire(ctx, kind, "error")
			return nil, nil, fmt.Errorf("create %s: %w", name, createErr)
		}
		taken, loadErr := s.load(ctx, name)
		if loadErr != nil {
			s.recordAcquire(ctx, kind, "error")
			return nil, nil, loadErr
		}
		if taken != nil {
			holders = append(holders, taken.Holder)
		}
	}
	s.recordAcquire(ctx, kind, "full")
	return nil, holders, nil
}

// Release frees the slot holder holds. It reports false, and changes
// nothing, when holder holds none.
func (s *Store) Release(ctx context.Context, holder string) (bool, error) {
	slot, err := s.HeldBy(ctx, holder)
	if err != nil {
		s.recordRelease(ctx, "error")
		return false, err
	}
	if slot == nil {
		s.recordRelease(ctx, "not_held")
		return false, nil
	}
	if removeErr := s.Docker.ConfigRemove(ctx, slot.Name); removeErr != nil && !isNotFound(removeErr) {
		s.recordRelease(ctx, "error")
		return false, fmt.Errorf("remove %s: %w", slot.Name, removeErr)
	}
	s.recordRelease(ctx, "released")
	s.Metrics.Hold.Record(ctx, float64(s.now().Unix()-slot.AcquiredAt))
	return true, nil
}

// HeldBy returns the slot holder holds, of either kind, or nil.
func (s *Store) HeldBy(ctx context.Context, holder string) (*Slot, error) {
	for _, name := range append([]string{s.managerName()}, s.workerNames()...) {
		slot, err := s.load(ctx, name)
		if err != nil {
			return nil, err
		}
		if slot != nil && slot.Holder == holder {
			return slot, nil
		}
	}
	return nil, nil //nolint:nilnil // "holds no slot" is not an error
}

// load reads one slot, or nil if the Config does not exist.
func (s *Store) load(ctx context.Context, name string) (*Slot, error) {
	data, _, err := s.Docker.ConfigInspect(ctx, name)
	if err != nil {
		if isNotFound(err) {
			return nil, nil //nolint:nilnil // a free slot is not an error
		}
		return nil, fmt.Errorf("inspect %s: %w", name, err)
	}
	var slot Slot
	if unmarshalErr := json.Unmarshal(data, &slot); unmarshalErr != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", name, unmarshalErr)
	}
	slot.Name = name
	return &slot, nil
}

func (s *Store) workerNames() []string {
	names := make([]string, s.WorkerSlots)
	for i := range names {
		names[i] = s.Prefix + "-" + strconv.Itoa(i+1)
	}
	return names
}

func (s *Store) managerName() string { return s.Prefix + "-manager" }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) recordAcquire(ctx context.Context, kind Kind, outcome string) {
	s.Metrics.Acquire.Add(ctx, 1, otelmetric.WithAttributes(
		attribute.String("kind", kind.String()), attribute.String("outcome", outcome)))
}

func (s *Store) recordRelease(ctx context.Context, outcome string) {
	s.Metrics.Release.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("outcome", outcome)))
}

// isNotFound checks the SDK's errdefs first, then a narrow substring the
// real client's wrapper and dockerd's CLI-facing message both use.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errdefs.IsNotFound(err) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "no such config")
}

// isAlreadyExists mirrors isNotFound for create: Docker reports a name clash
// as Conflict, rendered as "AlreadyExists" (gRPC) or "name conflicts".
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
