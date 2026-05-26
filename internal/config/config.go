// Package config loads the coordinator's runtime configuration from environment
// variables. All knobs documented in docs/specs/.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Default values for env-driven knobs.
const (
	defaultLockTTL             = 30 * time.Minute
	defaultDrainTimeout        = 5 * time.Minute
	defaultRoleRecheck         = 30 * time.Second
	defaultForwardTimeout      = 60 * time.Second
	defaultManagerDiscoveryTTL = 60 * time.Second
	// DefaultUnixSocketMode is intentionally group-readable/writable so
	// the host-side script's runtime user (typically a dedicated group
	// like `swarm-coord`) can call the coordinator without being root.
	// 0660 (rw-rw----) is the standard Unix-socket pattern for this.
	defaultUnixSocketMode os.FileMode = 0o660
	// DefaultUnixSocketPath sits under /var/run alongside docker.sock for
	// operational familiarity. The compose stack file bind-mounts the
	// parent directory from the host so the socket appears on the host
	// at the same path.
	defaultUnixSocketPath = "/var/run/swarm-auto-drain/coord.sock"
)

// ErrNodeHostnameRequired indicates NODE_HOSTNAME is unset.
var ErrNodeHostnameRequired = errors.New(
	"NODE_HOSTNAME is required (set via swarm template: NODE_HOSTNAME={{.Node.Hostname}})",
)

// ErrInvalidInt indicates an env var that should hold an integer did not parse.
var ErrInvalidInt = errors.New("not an integer")

// ErrInvalidSocketMode indicates UNIX_SOCKET_MODE was set but not parseable
// as an octal file mode (e.g. "0660").
var ErrInvalidSocketMode = errors.New("not a valid octal file mode")

// Config holds runtime configuration loaded from environment variables.
// Field order groups all string fields up front to keep the GC pointer-scan
// window compact, then sized fields ordered largest-to-smallest for
// alignment (Duration:8, int:8, FileMode:4, bool:1).
//
// UnixSocketGID is the numeric GID that owns the Unix socket and its
// parent directory. Zero (the default, i.e. "root") means only root on
// the host can call the API — operators almost certainly want to set
// this to the GID of a dedicated `swarm-coord` group that the
// safe-reboot script's runtime user belongs to. Numeric (not name)
// because the container has no view of host /etc/group.
type Config struct {
	NodeHostname            string
	ListenAddr              string
	UnixSocketPath          string
	DockerSocket            string
	LockConfigName          string
	LogLevel                string
	LockTTL                 time.Duration
	DrainTimeout            time.Duration
	RoleRecheck             time.Duration
	ForwardTimeout          time.Duration
	ManagerDiscoveryTTL     time.Duration
	UnixSocketGID           int
	UnixSocketMode          os.FileMode
	AllowUnencryptedOverlay bool
}

// Load reads configuration from environment variables. NODE_HOSTNAME is
// required; everything else has a default documented in the spec.
func Load() (*Config, error) {
	nodeHostname := os.Getenv("NODE_HOSTNAME")
	if nodeHostname == "" {
		return nil, ErrNodeHostnameRequired
	}

	lockTTL, err := envSecs("LOCK_TTL_SECONDS", defaultLockTTL)
	if err != nil {
		return nil, err
	}
	drainTimeout, err := envSecs("DRAIN_TIMEOUT_SECONDS", defaultDrainTimeout)
	if err != nil {
		return nil, err
	}
	roleRecheck, err := envSecs("ROLE_RECHECK_SECONDS", defaultRoleRecheck)
	if err != nil {
		return nil, err
	}
	forwardTimeout, err := envSecs("FORWARD_TIMEOUT_SECONDS", defaultForwardTimeout)
	if err != nil {
		return nil, err
	}
	managerDiscoveryTTL, err := envSecs("MANAGER_DISCOVERY_TTL_SECONDS", defaultManagerDiscoveryTTL)
	if err != nil {
		return nil, err
	}
	unixSocketMode, err := envOctal("UNIX_SOCKET_MODE", defaultUnixSocketMode)
	if err != nil {
		return nil, err
	}
	unixSocketGID, err := envInt("UNIX_SOCKET_GID", 0)
	if err != nil {
		return nil, err
	}

	return &Config{
		NodeHostname:            nodeHostname,
		ListenAddr:              envStr("LISTEN_ADDR", "0.0.0.0:7777"),
		UnixSocketPath:          envStr("UNIX_SOCKET_PATH", defaultUnixSocketPath),
		DockerSocket:            envStr("DOCKER_SOCKET", "/var/run/docker.sock"),
		LockConfigName:          envStr("LOCK_CONFIG_NAME", "swarm-auto-drain-lock"),
		LogLevel:                envStr("LOG_LEVEL", "info"),
		LockTTL:                 lockTTL,
		DrainTimeout:            drainTimeout,
		RoleRecheck:             roleRecheck,
		ForwardTimeout:          forwardTimeout,
		ManagerDiscoveryTTL:     managerDiscoveryTTL,
		UnixSocketMode:          unixSocketMode,
		UnixSocketGID:           unixSocketGID,
		AllowUnencryptedOverlay: envBool("ALLOW_UNENCRYPTED_OVERLAY"),
	}, nil
}

// envInt parses an env var as a decimal integer. Used for UNIX_SOCKET_GID.
func envInt(key string, def int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q: %w", key, raw, ErrInvalidInt)
	}
	return n, nil
}

// envBool returns true iff the env var is set to a recognized truthy
// value. Anything else (unset, "0", "false", typos) means false — the
// secure default. We don't want a typo'd "ture" to silently enable an
// override that disables an encryption gate.
func envBool(key string) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "True", "yes", "YES":
		return true
	default:
		return false
	}
}

// envOctal parses an env var as an octal file mode (e.g. "0660").
// Returning a parse error rather than silently falling back lets
// operators catch typos at startup instead of running with a wrong
// permission mask.
func envOctal(key string, def os.FileMode) (os.FileMode, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	parsed, err := strconv.ParseUint(raw, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: %q: %w", key, raw, ErrInvalidSocketMode)
	}
	return os.FileMode(parsed), nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envSecs(key string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q: %w", key, raw, ErrInvalidInt)
	}
	return time.Duration(secs) * time.Second, nil
}
