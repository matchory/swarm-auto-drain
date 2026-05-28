// Package hostinstall handles per-host bootstrap and teardown: creates
// the coordinator group, drops an env file and systemd units under the
// requested filesystem root, and (when root == "/") drives systemctl to
// enable the timer and resume oneshot. Symmetric Uninstall undoes the
// systemd-visible state but preserves operator-managed paths (env file,
// binary, group, sentinel-state dir) so re-install yields a clean state.
package hostinstall

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
)

//go:embed files
var embeddedFiles embed.FS

// CommandRunner runs an external command (groupadd, usermod, systemctl)
// and returns its combined output for logging. Replaced in tests.
type CommandRunner func(name string, args ...string) (output string, err error)

// GroupLookup looks up a system group by name. Replaced in tests so the
// suite never depends on the host's /etc/group. Production uses
// user.LookupGroup, whose contract — including user.UnknownGroupError —
// is what callers code against.
type GroupLookup func(name string) (*user.Group, error)

// defaultRunner shells out via exec.Command.
var defaultRunner CommandRunner = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// Options carries every input Install/Uninstall need plus optional
// test seams. Zero values for Runner and LookupGroup fall back to the
// real implementations.
type Options struct {
	Root        string
	GID         int
	Force       bool
	User        string
	Runner      CommandRunner
	LookupGroup GroupLookup

	// Out receives the operator-facing summary. nil → os.Stdout. Tests
	// pass a bytes.Buffer.
	Out io.Writer
}

const (
	envFileMode    = 0o644
	envDirMode     = 0o755
	unitDirMode    = 0o755
	unitFileMode   = 0o644
	embedFilesRoot = "files"
)

// Install executes the per-host bootstrap. See package doc.
func Install(opts Options) error {
	opts = applyInstallDefaults(opts)
	if err := validateInstall(opts); err != nil {
		return err
	}

	if err := ensureGroup(opts); err != nil {
		return fmt.Errorf("ensure group: %w", err)
	}

	if err := writeEnvFile(opts); err != nil {
		return fmt.Errorf("write env file: %w", err)
	}

	if err := writeUnits(opts); err != nil {
		return fmt.Errorf("write systemd units: %w", err)
	}

	if opts.Root == "/" {
		if err := runSystemctlEnable(opts); err != nil {
			return fmt.Errorf("systemctl: %w", err)
		}
	}

	if opts.User != "" {
		if out, err := opts.Runner("usermod", "-aG", CoordinatorGroup, opts.User); err != nil {
			return fmt.Errorf("usermod -aG %s %s: %w (%s)",
				CoordinatorGroup, opts.User, err, out)
		}
	}

	printInstallSummary(opts)
	return nil
}

// Uninstall reverses what Install installed, tolerantly. systemctl
// failures are logged and continued past — a partial state must still
// converge to "nothing systemd-visible".
func Uninstall(opts Options) error {
	opts = applyUninstallDefaults(opts)
	if opts.Root == "/" && os.Geteuid() != 0 {
		return errors.New("uninstall: must run as root (or pass --root <prefix>)")
	}

	if opts.Root == "/" {
		stopAndDisableUnits(opts)
	}

	for _, name := range []string{UnitTriggerService, UnitTriggerTimer, UnitResumeService} {
		path := filepath.Join(rootedSystemdDir(opts.Root), name)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	if opts.Root == "/" {
		if out, err := opts.Runner("systemctl", "daemon-reload"); err != nil {
			slog.Info("systemctl daemon-reload (best-effort)",
				"err", err, "out", out)
		}
	}

	printUninstallSummary(opts)
	return nil
}

func applyInstallDefaults(opts Options) Options {
	if opts.Root == "" {
		opts.Root = "/"
	}
	if opts.Runner == nil {
		opts.Runner = defaultRunner
	}
	if opts.LookupGroup == nil {
		opts.LookupGroup = user.LookupGroup
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	return opts
}

func validateInstall(opts Options) error {
	if opts.Root == "/" && os.Geteuid() != 0 {
		return errors.New("install: must run as root (or pass --root <prefix>)")
	}
	if opts.GID <= 0 {
		return errors.New("install: --gid must be a positive integer")
	}
	return nil
}

func applyUninstallDefaults(opts Options) Options {
	if opts.Root == "" {
		opts.Root = "/"
	}
	if opts.Runner == nil {
		opts.Runner = defaultRunner
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	return opts
}

func stopAndDisableUnits(opts Options) {
	for _, unit := range []string{UnitTriggerTimer, UnitResumeService} {
		if out, err := opts.Runner("systemctl", "stop", unit); err != nil {
			slog.Info("systemctl stop (best-effort)",
				"unit", unit, "err", err, "out", out)
		}
		if out, err := opts.Runner("systemctl", "disable", unit); err != nil {
			slog.Info("systemctl disable (best-effort)",
				"unit", unit, "err", err, "out", out)
		}
	}
}

func ensureGroup(opts Options) error {
	g, err := opts.LookupGroup(CoordinatorGroup)
	if err == nil {
		gid, convErr := strconv.Atoi(g.Gid)
		if convErr != nil {
			return fmt.Errorf("group %q has non-numeric gid %q",
				CoordinatorGroup, g.Gid)
		}
		if gid != opts.GID {
			return fmt.Errorf("group %q exists with gid %d, want %d",
				CoordinatorGroup, gid, opts.GID)
		}
		return nil
	}
	var notFound user.UnknownGroupError
	if !errors.As(err, &notFound) {
		return fmt.Errorf("lookup group %q: %w", CoordinatorGroup, err)
	}
	if opts.Root != "/" {
		// Test/staging root: skip the real groupadd so the suite is
		// hermetic. Tests assert on the Runner stub directly when they
		// want to observe creation behavior.
		if _, err := opts.Runner("groupadd", "-r", "-g",
			strconv.Itoa(opts.GID), CoordinatorGroup); err != nil {
			return err
		}
		return nil
	}
	if out, err := opts.Runner("groupadd", "-r", "-g",
		strconv.Itoa(opts.GID), CoordinatorGroup); err != nil {
		return fmt.Errorf("groupadd: %w (%s)", err, out)
	}
	return nil
}

func writeEnvFile(opts Options) error {
	path := rootedEnvFile(opts.Root)
	if err := os.MkdirAll(filepath.Dir(path), envDirMode); err != nil {
		return err
	}
	if !opts.Force {
		if _, err := os.Stat(path); err == nil {
			slog.Info("env file exists; preserving (use --force to overwrite)",
				"path", path)
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	data, err := embeddedFiles.ReadFile(filepath.Join(embedFilesRoot, "swarm-auto-drain.env"))
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, envFileMode)
}

func writeUnits(opts Options) error {
	dir := rootedSystemdDir(opts.Root)
	if err := os.MkdirAll(dir, unitDirMode); err != nil {
		return err
	}
	for _, name := range []string{UnitTriggerService, UnitTriggerTimer, UnitResumeService} {
		data, err := embeddedFiles.ReadFile(filepath.Join(embedFilesRoot, name))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, unitFileMode); err != nil {
			return err
		}
	}
	return nil
}

func runSystemctlEnable(opts Options) error {
	steps := [][]string{
		{"daemon-reload"},
		{"enable", "--now", UnitTriggerTimer},
		{"enable", UnitResumeService},
	}
	for _, args := range steps {
		if out, err := opts.Runner("systemctl", args...); err != nil {
			return fmt.Errorf("systemctl %v: %w (%s)", args, err, out)
		}
	}
	return nil
}

func printInstallSummary(opts Options) {
	fmt.Fprintf(opts.Out, `swarm-auto-drain installed.

  socket:  %s
  group:   %s (gid %d)
  env:     %s
  units:   %s, %s, %s in %s

next: deploy the stack with:
  swarm-auto-drain stack | docker stack deploy -c - swarm-auto-drain
`,
		SocketPath,
		CoordinatorGroup, opts.GID,
		rootedEnvFile(opts.Root),
		UnitTriggerService, UnitTriggerTimer, UnitResumeService,
		rootedSystemdDir(opts.Root),
	)
}

func printUninstallSummary(opts Options) {
	fmt.Fprintf(opts.Out, `swarm-auto-drain uninstalled (systemd state removed).

NOT removed (operator state — delete manually for a clean slate):
  - %s      (env file with operator customizations)
  - %s   (the binary itself)
  - group %s            (may still have member users)
  - /var/lib/swarm-auto-drain/  (resume sentinel state directory)
`,
		rootedEnvFile(opts.Root),
		BinaryPath,
		CoordinatorGroup,
	)
}
