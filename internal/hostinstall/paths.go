package hostinstall

import "path/filepath"

// BinaryPath is the operator-visible install path for the binary. The
// embedded systemd units reference this string literally, so changing
// it here without updating the embedded units (or templating them) will
// produce a broken install.
const BinaryPath = "/usr/local/sbin/swarm-auto-drain"

// EnvFile is the env file systemd's EnvironmentFile= directive loads.
const EnvFile = "/etc/default/swarm-auto-drain"

// SystemdDir is the system-wide systemd unit directory.
const SystemdDir = "/etc/systemd/system"

// CoordinatorGroup is the name of the host-local group whose members
// can talk to the coordinator socket.
const CoordinatorGroup = "swarm-coord"

// Unit file names — bare names so callers can join with SystemdDir.
const (
	UnitTriggerService = "swarm-auto-drain-trigger.service"
	UnitTriggerTimer   = "swarm-auto-drain-trigger.timer"
	UnitResumeService  = "swarm-auto-drain-resume.service"
)

// SocketPath is the default coordinator socket path. Surfaced in the
// install summary so operators know what to chmod/bind-mount against.
const SocketPath = "/var/run/swarm-auto-drain/coord.sock"

func rootedEnvFile(root string) string {
	return filepath.Join(root, EnvFile)
}

func rootedSystemdDir(root string) string {
	return filepath.Join(root, SystemdDir)
}
