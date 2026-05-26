// Package drain wraps swarm node drain/active operations and the
// manager-quorum precondition.
package drain

import (
	"context"
	"time"
)

// QuorumReport is what the quorum check returns to a caller.
type QuorumReport struct {
	TotalManagers     int `json:"total_managers"`
	ReachableManagers int `json:"reachable_managers"` // Including caller if reachable.
	OthersReachable   int `json:"others_reachable"`   // Excluding caller.
	Required          int `json:"required"`           // Quorum target: floor(N/2)+1.
}

// ManagersHealth is the diagnostics shape returned by /managers/health. The
// JSON keys mirror the spec contract (§3.2): `{"reachable":N,"total":M,"leader":"<host>"}`.
// Field order puts the string field first to keep the GC pointer-scan window compact.
type ManagersHealth struct {
	Leader    string `json:"leader"`
	Reachable int    `json:"reachable"`
	Total     int    `json:"total"`
}

// Result of a drain operation.
type Result struct {
	TasksRemaining int
	Elapsed        time.Duration
	TimedOut       bool
}

// Drainer is the high-level interface used by the HTTP handlers to drain a
// node, reactivate it, and inspect manager-quorum health.
type Drainer interface {
	QuorumOK(ctx context.Context, self string) (bool, QuorumReport, error)
	Managers(ctx context.Context) (ManagersHealth, error)
	Drain(ctx context.Context, node string, timeout, pollInterval time.Duration) (Result, error)
	Activate(ctx context.Context, node string) error
}
