// Package drain wraps swarm node drain/active operations and the
// manager-quorum precondition.
package drain

import (
	"context"
	"time"
)

// QuorumReport is what the quorum check returns to a caller.
type QuorumReport struct {
	TotalManagers     int
	ReachableManagers int // including caller if reachable
	OthersReachable   int // excluding caller
	Required          int // floor(N/2)+1
}

// Result of a drain operation.
type Result struct {
	TasksRemaining int
	Elapsed        time.Duration
	TimedOut       bool
}

type Drainer interface {
	QuorumOK(ctx context.Context, self string) (bool, QuorumReport, error)
	Drain(ctx context.Context, node string, timeout, pollInterval time.Duration) (Result, error)
	Activate(ctx context.Context, node string) error
}
