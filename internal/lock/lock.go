// Package lock arbitrates the cluster-wide reboot mutex. The lock document
// lives in a Docker Config; "create-fails-on-exists" is the CAS primitive.
package lock

import (
	"context"
	"time"
)

type Lock struct {
	Holder     string `json:"holder"`
	AcquiredAt int64  `json:"acquired_at"` // unix seconds UTC
	TTLSeconds int    `json:"ttl_seconds"`
}

// Status is the outcome of an Acquire call.
type Status int

const (
	// Acquired means the caller now holds the lock (no prior holder).
	Acquired Status = iota
	// AcquiredViaSteal means the prior lock was stale; caller stole it.
	AcquiredViaSteal
	// HeldByOther means a non-stale lock is held by someone else.
	HeldByOther
)

// AcquireResult bundles Status with the current Lock state.
type AcquireResult struct {
	Status  Status
	Current *Lock
}

// Store is the storage primitive for the lock document.
type Store interface {
	Acquire(ctx context.Context, holder string, ttl time.Duration) (AcquireResult, error)
	Release(ctx context.Context, holder string) error
	Inspect(ctx context.Context) (*Lock, error)
}

type errStr string

func (e errStr) Error() string { return string(e) }

const (
	ErrNotHolder errStr = "lock not held by caller"
	ErrNoLock    errStr = "no lock exists"
)
