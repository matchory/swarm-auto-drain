package lock

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// docker is the subset of internal/docker.Client that ConfigStore needs.
// Defined locally so this package doesn't import internal/docker — the
// real Client satisfies it implicitly.
type docker interface {
	ConfigCreate(ctx context.Context, name string, data []byte, labels map[string]string) (string, error)
	ConfigInspect(ctx context.Context, name string) (data []byte, labels map[string]string, err error)
	ConfigRemove(ctx context.Context, name string) error
}

type ConfigStore struct {
	Docker     docker
	ConfigName string
	Now        func() time.Time // injectable for tests; nil → time.Now
}

func (s *ConfigStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *ConfigStore) Acquire(ctx context.Context, holder string, ttl time.Duration) (AcquireResult, error) {
	l := Lock{Holder: holder, AcquiredAt: s.now().UTC().Unix(), TTLSeconds: int(ttl.Seconds())}
	data, err := json.Marshal(l)
	if err != nil {
		return AcquireResult{}, err
	}

	// 1. Try a fresh create.
	if _, err := s.Docker.ConfigCreate(ctx, s.ConfigName, data, nil); err == nil {
		return AcquireResult{Status: Acquired, Current: &l}, nil
	} else if !isAlreadyExists(err) {
		return AcquireResult{}, fmt.Errorf("create: %w", err)
	}

	// 2. EEXIST — read the existing holder.
	existing, err := s.inspect(ctx)
	if err != nil {
		return AcquireResult{}, fmt.Errorf("inspect after EEXIST: %w", err)
	}

	// 3. Same holder retrying? Treat as success (idempotent re-acquire).
	if existing != nil && existing.Holder == holder {
		return AcquireResult{Status: Acquired, Current: existing}, nil
	}

	// 4. Fresh and held by someone else.
	if existing != nil && s.now().UTC().Unix()-existing.AcquiredAt <= int64(existing.TTLSeconds) {
		return AcquireResult{Status: HeldByOther, Current: existing}, nil
	}

	// 5. Stale — remove and retry create exactly once.
	if err := s.Docker.ConfigRemove(ctx, s.ConfigName); err != nil && !isNotFound(err) {
		return AcquireResult{}, fmt.Errorf("remove stale: %w", err)
	}
	if _, err := s.Docker.ConfigCreate(ctx, s.ConfigName, data, nil); err == nil {
		return AcquireResult{Status: AcquiredViaSteal, Current: &l}, nil
	} else if !isAlreadyExists(err) {
		return AcquireResult{}, fmt.Errorf("create after steal: %w", err)
	}
	winner, _ := s.inspect(ctx)
	return AcquireResult{Status: HeldByOther, Current: winner}, nil
}

func (s *ConfigStore) Release(ctx context.Context, holder string) error {
	existing, err := s.inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	if existing == nil {
		return ErrNoLock
	}
	if existing.Holder != holder {
		return ErrNotHolder
	}
	if err := s.Docker.ConfigRemove(ctx, s.ConfigName); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove: %w", err)
	}
	return nil
}

func (s *ConfigStore) Inspect(ctx context.Context) (*Lock, error) {
	return s.inspect(ctx)
}

func (s *ConfigStore) inspect(ctx context.Context) (*Lock, error) {
	data, _, err := s.Docker.ConfigInspect(ctx, s.ConfigName)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("unmarshal lock: %w", err)
	}
	return &l, nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "No such config") || strings.Contains(s, "not found")
}

func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "name conflicts") || strings.Contains(s, "already exists") || strings.Contains(s, "AlreadyExists")
}
