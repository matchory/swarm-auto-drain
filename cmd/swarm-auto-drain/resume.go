package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/hostclient"
)

// resumeWaitTimeout is the upper bound on /healthz polling at boot.
// Five minutes covers the typical case where the daemon is still
// finishing its initial Docker `Info` and overlay-encryption check.
const resumeWaitTimeout = 5 * time.Minute

// resumeWaitInterval is the gap between /healthz attempts. Cheap to
// retry and the worst-case latency we accept for "daemon is up".
const resumeWaitInterval = 2 * time.Second

// resumeCmd is the `resume` subcommand: fired by the boot-time oneshot
// after a coordinated reboot. Waits for the local daemon, marks this
// node active again, releases the lock (or notes a peer already did),
// and clears the RESUME_SENTINEL so the unit's ConditionPathExists
// gate won't re-fire on subsequent boots.
func resumeCmd(_ []string) int {
	socket := envOr("COORDINATOR_SOCKET", defaultCoordinatorSocket)
	resumeSentinel := envOr("RESUME_SENTINEL", defaultResumeSentinel)
	lockTimeout := envSecondsOr("LOCK_TIMEOUT_SECONDS", defaultLockTimeoutSecs)

	client := hostclient.New(socket)

	if err := waitForHealthy(client, resumeWaitTimeout, resumeWaitInterval); err != nil {
		slog.Error("daemon not healthy", "err", err, "socket", socket)
		return 1
	}

	ctxActive, cancelActive := context.WithTimeout(context.Background(), lockTimeout)
	if err := client.Activate(ctxActive); err != nil {
		cancelActive()
		slog.Error("activate node", "err", err)
		return 1
	}
	cancelActive()
	slog.Info("node marked active")

	ctxRel, cancelRel := context.WithTimeout(context.Background(), lockTimeout)
	status, err := client.ReleaseLock(ctxRel)
	cancelRel()
	if err != nil {
		slog.Error("release lock", "err", err, "status", status)
		return 1
	}
	switch status {
	case http.StatusNoContent:
		slog.Info("lock released")
	case http.StatusForbidden, http.StatusNotFound:
		// Stale-steal or operator intervention took over while we were
		// rebooting — that's the correct end state. Nothing to clean up
		// on the cluster side.
		slog.Info("lock already gone (peer took over)", "status", status)
	default:
		slog.Error("unexpected release status", "status", status)
		return 1
	}

	if err := os.Remove(resumeSentinel); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("remove resume sentinel", "path", resumeSentinel, "err", err)
		return 1
	}
	return 0
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
