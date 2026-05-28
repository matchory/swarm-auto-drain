package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/hostclient"
	"github.com/matchory/swarm-auto-drain/internal/hostinstall"
	"github.com/matchory/swarm-auto-drain/internal/policy"
)

// Host-side defaults. These mirror the env file written by `install` and
// stay in sync via the embedded template's expected values. Operators can
// override via /etc/default/swarm-auto-drain which systemd loads before
// ExecStart.
const (
	// defaultCoordinatorSocket reuses the single source of truth in
	// hostinstall so the daemon, the host CLI, and the installed env file
	// cannot drift apart.
	defaultCoordinatorSocket = hostinstall.SocketPath
	defaultDisabledFile      = "/etc/swarm-auto-drain/disabled"
	defaultSentinelFile      = "/var/run/reboot-required"
	defaultResumeSentinel    = "/var/lib/swarm-auto-drain/draining"
	defaultBlackoutFile      = "/etc/swarm-auto-drain/blackout"
	defaultLockTimeoutSecs   = 60
	defaultDrainTimeoutSecs  = 600
	resumeSentinelDirMode    = 0o755
	resumeSentinelFileMode   = 0o644
	// drainContextSlack widens the client's drain context past the
	// daemon-side DrainTimeout so a slow drain returns 408 from the
	// daemon rather than a client-side deadline (which would surface as
	// a bare network error, hiding the operational reason).
	drainContextSlack = 30 * time.Second
)

// Reason values emitted on the structured `reason=` log field. Stable;
// operators grep on these.
const (
	reasonLockHeldByPeer = "lock-held-by-peer"
	reasonLockFailed     = "lock-failed"
	reasonDrainTimeout   = "drain-timeout"
	reasonQuorumGuard    = "quorum-guard"
	reasonDrainFailed    = "drain-failed"
	reasonRebooting      = "rebooting"
)

// systemctlReboot execs `systemctl reboot`. Tests replace it to observe
// the call without rebooting the test runner.
var systemctlReboot = func() error {
	return exec.Command("systemctl", "reboot").Run()
}

// triggerNow returns the current time. Tests replace it to drive window
// and blackout checks deterministically.
var triggerNow = time.Now

// triggerCmd is the `trigger` subcommand: evaluate host-local reboot
// policy, then if the policy says go, acquire the cluster lock, drain
// this node, and reboot. Exit 0 covers both "we did it" and every
// expected skip (disabled, no reboot needed, outside window, blackout,
// peer holds the lock, quorum would be lost). Exit 1 is a real failure.
func triggerCmd(_ []string) int {
	socket := envOr("COORDINATOR_SOCKET", defaultCoordinatorSocket)
	resumeSentinel := envOr("RESUME_SENTINEL", defaultResumeSentinel)
	lockTimeout := envSecondsOr("LOCK_TIMEOUT_SECONDS", defaultLockTimeoutSecs)
	drainTimeout := envSecondsOr("DRAIN_TIMEOUT_SECONDS", defaultDrainTimeoutSecs)

	cfg, err := loadTriggerPolicy()
	if err != nil {
		slog.Error("policy config", "err", err)
		return 1
	}

	decision, err := policy.ShouldRebootNow(cfg, triggerNow())
	if err != nil {
		slog.Error("evaluate policy", "err", err)
		return 1
	}
	if !decision.Reboot {
		slog.Info("skip", "reason", decision.Reason)
		return 0
	}

	client := hostclient.New(socket)

	// Daemon readiness + version handshake.
	ctxRole, cancelRole := context.WithTimeout(context.Background(), 5*time.Second)
	role, err := client.GetRole(ctxRole)
	cancelRole()
	if err != nil {
		slog.Error("get role", "err", err, "socket", socket)
		return 1
	}
	if majorSkew(role.Version, version) {
		slog.Warn("daemon version skew",
			"daemon", role.Version,
			"host", version,
			"hint", "upgrade host binary")
	}

	ctx := context.Background()
	exitCode := runReboot(ctx, client, lockTimeout, drainTimeout, resumeSentinel)
	return exitCode
}

// runReboot owns the sentinel → lock → drain → reboot sequence.
//
// Two design rules keep the cluster invariant "lock held ⇔ a node is
// draining or has committed to reboot" intact:
//
//  1. The resume sentinel is written BEFORE we acquire the lock, so a
//     successful drain is always paired with a sentinel on disk.
//     Otherwise a sentinel-write failure after drain would leave the
//     node drained with no resume.service trigger, requiring manual
//     intervention to re-activate.
//  2. After a successful drain we DO NOT release the lock on the
//     "reboot failed" path. Releasing would let a peer believe the
//     cluster is free while this node is actually drained; on the next
//     boot resume.service releases the lock via DELETE /lock. Errors
//     before drain success do release the lock — we hadn't yet
//     committed cluster-visible state.
//
// The 503 quorum-guard path explicitly releases the lock because we
// never drained — staying held would stall the fleet for the full TTL.
func runReboot(
	ctx context.Context,
	client *hostclient.Client,
	lockTimeout, drainTimeout time.Duration,
	resumeSentinel string,
) int {
	if err := writeResumeSentinel(resumeSentinel); err != nil {
		slog.Error("write resume sentinel", "path", resumeSentinel, "err", err)
		return 1
	}

	ctxLock, cancel := context.WithTimeout(ctx, lockTimeout)
	res, err := client.AcquireLock(ctxLock)
	cancel()
	if err != nil {
		slog.Error("acquire lock", "err", err, "reason", reasonLockFailed)
		return 1
	}
	switch res.Status {
	case http.StatusCreated, http.StatusGone:
		slog.Info("lock acquired", "status", res.Status)
	case http.StatusConflict:
		slog.Info("skip", "reason", reasonLockHeldByPeer, "holder", res.Lock.Holder)
		return 0
	default:
		slog.Error("unexpected lock status",
			"status", res.Status, "reason", reasonLockFailed)
		return 1
	}

	ctxDrain, cancelDrain := context.WithTimeout(ctx, drainTimeout+drainContextSlack)
	status, body, err := client.Drain(ctxDrain)
	cancelDrain()
	if err != nil {
		slog.Error("drain", "err", err, "reason", reasonDrainFailed)
		releaseBestEffort(client, lockTimeout)
		return 1
	}
	switch status {
	case http.StatusOK:
		slog.Info("drained", "body", string(body))
	case http.StatusRequestTimeout:
		slog.Error("drain timed out",
			"body", string(body), "reason", reasonDrainTimeout)
		releaseBestEffort(client, lockTimeout)
		return 1
	case http.StatusServiceUnavailable:
		slog.Info("skip", "reason", reasonQuorumGuard, "body", string(body))
		releaseBestEffort(client, lockTimeout)
		return 0
	default:
		slog.Error("drain status",
			"status", status, "body", string(body), "reason", reasonDrainFailed)
		releaseBestEffort(client, lockTimeout)
		return 1
	}

	// Past this point the node is drained; the lock now stays held
	// until resume.service releases it on the next boot. Any error here
	// is a deferred problem the operator handles, not a "release and
	// pretend it didn't happen" — releasing would expose the cluster to
	// a peer starting its own drain while we're still alive.
	slog.Info("rebooting", "reason", reasonRebooting)
	if rebootErr := systemctlReboot(); rebootErr != nil {
		slog.Error("systemctl reboot", "err", rebootErr)
		return 1
	}
	// `systemctl reboot` schedules the reboot and returns; the kernel
	// terminates us at its leisure during the shutdown sequence.
	return 0
}

// releaseBestEffort tries to release the cluster lock. Failures are
// logged but never propagated — the caller is already on an error path
// and the lock will eventually be reclaimed via the stale-TTL mechanism.
func releaseBestEffort(client *hostclient.Client, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	status, err := client.ReleaseLock(ctx)
	if err != nil {
		slog.Warn("best-effort lock release",
			"err", err.Error(),
			"status", status)
	}
}

// loadTriggerPolicy reads policy-related env vars and parses the window.
func loadTriggerPolicy() (policy.Config, error) {
	disabled := envOr("DISABLED_FILE", defaultDisabledFile)
	sentinel := envOr("SENTINEL_FILE", defaultSentinelFile)
	blackout := envOr("BLACKOUT_FILE", defaultBlackoutFile)

	w, err := policy.ParseWindow(os.Getenv("REBOOT_WINDOW"))
	if err != nil {
		return policy.Config{}, fmt.Errorf("REBOOT_WINDOW: %w", err)
	}
	return policy.Config{
		DisabledFile: disabled,
		SentinelFile: sentinel,
		Window:       w,
		BlackoutFile: blackout,
	}, nil
}

func writeResumeSentinel(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), resumeSentinelDirMode); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}
	if err := os.WriteFile(path, nil, resumeSentinelFileMode); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// envOr returns the env var if set and non-empty, otherwise fallback.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envSecondsOr parses an integer-seconds env var. Invalid values fall
// back silently rather than failing — operators' typos shouldn't strand
// the host out of policy. The default is conservative.
func envSecondsOr(key string, fallback int) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return time.Duration(fallback) * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("invalid env seconds; using default",
			"key", key, "value", v, "default", fallback)
		return time.Duration(fallback) * time.Second
	}
	return time.Duration(n) * time.Second
}

// majorSkew reports whether the SemVer major segments of a and b differ.
// Inputs with or without a leading "v" are accepted. Unparseable inputs
// return false: a warning we cannot justify is worse than no warning.
func majorSkew(a, b string) bool {
	majA, okA := parseMajor(a)
	majB, okB := parseMajor(b)
	if !okA || !okB {
		return false
	}
	return majA != majB
}

func parseMajor(v string) (int, bool) {
	if v == "" {
		return 0, false
	}
	if v[0] == 'v' || v[0] == 'V' {
		v = v[1:]
	}
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(v[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}
