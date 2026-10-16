// Package lock arbitrates the cluster-wide reboot mutex. The lock document
// lives in a Docker Config; "create-fails-on-exists" is the CAS primitive.
package lock

import (
	"context"
	"time"
)

// Lock is the document stored in Docker Config that represents the cluster
// reboot mutex.
type Lock struct {
	Holder     string `json:"holder"`
	AcquiredAt int64  `json:"acquired_at"` // Unix seconds UTC.
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
// Pointer field first to keep the GC pointer-scan window minimal.
type AcquireResult struct {
	Current *Lock
	Status  Status
}

// Store is the storage primitive for the lock document.
type Store interface {
	Acquire(ctx context.Context, holder string, ttl time.Duration) (AcquireResult, error)
	Release(ctx context.Context, holder string) error
	Inspect(ctx context.Context) (*Lock, error)
	// MarkDrained records that the coordinator is draining `holder` (node
	// `nodeID`) for a reboot, so the leader may reactivate it once it is
	// back. ErrNoLock / ErrNotHolder when the caller does not hold the lock.
	MarkDrained(ctx context.Context, holder, nodeID string) error
}

// sentinelError is a constant-friendly error type for package-level sentinels.
type sentinelError string

func (e sentinelError) Error() string { return string(e) }

// Sentinel errors returned by Store implementations.
const (
	ErrNotHolder sentinelError = "lock not held by caller"
	ErrNoLock    sentinelError = "no lock exists"
)
