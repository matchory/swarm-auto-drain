package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/hostclient"
)

// defaultResumeTimeoutSecs bounds the wait for this node to be back in
// service: boot, the swarm marking the node Ready, the leader's next
// reactivation tick, the coordinator task being rescheduled here and
// starting up.
const defaultResumeTimeoutSecs = 900

// socketDirMode matches the mode the daemon itself stamps on the socket
// directory (run.go socketParentDirMode).
const socketDirMode = 0o750

// resumeWaitInterval is the gap between /healthz attempts. A var so tests
// can shorten it.
var resumeWaitInterval = 2 * time.Second

// resumeCmd is the `resume` subcommand, fired by the boot-time oneshot after
// a coordinated reboot. The node comes back drained, with no coordinator
// replica (Swarm stops global tasks on a drained node), and a worker cannot
// undo its own drain. The leader's replica reactivates the node and
// releases the lock once it sees the node back; resume only waits for that.
//
// A replica answering on the local socket proves the node is Active again:
// Swarm schedules the global coordinator task only on an Active node. On
// success resume best-effort releases the lock (normally already released
// by the leader: 404) and clears RESUME_SENTINEL so the unit's
// ConditionPathExists gate won't re-fire on later boots. On timeout it exits
// 1 and keeps the sentinel; the node stays drained for the operator.
func resumeCmd(_ []string) int {
	socket := envOr("COORDINATOR_SOCKET", defaultCoordinatorSocket)
	resumeSentinel := envOr("RESUME_SENTINEL", defaultResumeSentinel)
	lockTimeout := envSecondsOr("LOCK_TIMEOUT_SECONDS", defaultLockTimeoutSecs)
	resumeTimeout := envSecondsOr("RESUME_TIMEOUT_SECONDS", defaultResumeTimeoutSecs)

	// The socket directory is the replica's bind-mount source. /var/run is
	// a tmpfs, so after a reboot it is gone, and Swarm refuses to start a
	// task whose bind source does not exist.
	if err := os.MkdirAll(filepath.Dir(socket), socketDirMode); err != nil {
		slog.Error("create socket directory", "path", filepath.Dir(socket), "err", err)
		return 1
	}

	client := hostclient.New(socket)
	if err := waitForHealthy(client, resumeTimeout, resumeWaitInterval); err != nil {
		slog.Error("node not back in service: coordinator replica never answered",
			"err", err, "socket", socket, "timeout", resumeTimeout)
		return 1
	}
	slog.Info("node active again")

	releaseAfterResume(client, lockTimeout)

	if err := os.Remove(resumeSentinel); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("remove resume sentinel", "path", resumeSentinel, "err", err)
		return 1
	}
	return 0
}

// releaseAfterResume releases this node's lock in case the leader has not
// yet. Its outcome does not decide resume's: the node is already Active,
// and a lock still held is released by the leader or reclaimed after
// LOCK_TTL.
func releaseAfterResume(client *hostclient.Client, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	status, err := client.ReleaseLock(ctx)
	switch {
	case err != nil:
		slog.Warn("release lock", "err", err, "status", status)
	case status == http.StatusNoContent:
		slog.Info("lock released")
	default:
		// 404: the leader already released it. 403: a peer holds it now.
		slog.Info("lock already released", "status", status)
	}
}

// waitForHealthy polls /healthz until success or deadline. Returns nil
// on the first 2xx, or the last observed error after timeout. Callers
// log the path/socket alongside our error.
func waitForHealthy(client *hostclient.Client, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), interval)
		err := client.GetHealthz(ctx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(interval)
	}
	if lastErr == nil {
		return errors.New("healthz never attempted within timeout")
	}
	return lastErr
}
