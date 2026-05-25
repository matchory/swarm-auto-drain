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
	ReachableManagers int `json:"reachable_managers"` // including caller if reachable
	OthersReachable   int `json:"others_reachable"`   // excluding caller
	Required          int `json:"required"`           // floor(N/2)+1
}

// ManagersHealth is the diagnostics shape returned by /managers/health. The
// JSON keys mirror the spec contract (§3.2): `{"reachable":N,"total":M,"leader":"<host>"}`.
type ManagersHealth struct {
	Reachable int    `json:"reachable"`
	Total     int    `json:"total"`
	Leader    string `json:"leader"`
}

// Result of a drain operation.
type Result struct {
	TasksRemaining int
	Elapsed        time.Duration
	TimedOut       bool
}

type Drainer interface {
	QuorumOK(ctx context.Context, self string) (bool, QuorumReport, error)
	Managers(ctx context.Context) (ManagersHealth, error)
	Drain(ctx context.Context, node string, timeout, pollInterval time.Duration) (Result, error)
	Activate(ctx context.Context, node string) error
}
