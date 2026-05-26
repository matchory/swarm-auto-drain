package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/api"
	"github.com/matchory/swarm-auto-drain/internal/config"
	dockerpkg "github.com/matchory/swarm-auto-drain/internal/docker"
	"github.com/matchory/swarm-auto-drain/internal/drain"
	"github.com/matchory/swarm-auto-drain/internal/forward"
	"github.com/matchory/swarm-auto-drain/internal/identity"
	"github.com/matchory/swarm-auto-drain/internal/lock"
	"github.com/matchory/swarm-auto-drain/internal/logging"
)

const (
	exitConfigErr      = 2
	exitRuntimeErr     = 1
	pollIntervalDrain  = 2 * time.Second
	managerProbePort   = 7777
	managerProbeClient = 3 * time.Second
	forwardRetries     = 2
	shutdownGraceExtra = 30 * time.Second
	// SocketParentDirMode is the permission mode the coordinator stamps
	// on the bind-mounted Unix-socket directory. 0750 lets the
	// UNIX_SOCKET_GID group enter the directory; rwx for owner is
	// needed because the coordinator (root) writes the socket file.
	socketParentDirMode os.FileMode = 0o750
)

// runCmd is the `run` subcommand entry point. Loads config, sets up
// logging, then hands off to the daemon's run() function.
func runCmd(_ []string) int {
	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return exitConfigErr
	}
	logging.Setup(cfg.LogLevel)
	slog.Info("starting", "version", version, "node", cfg.NodeHostname)

	if runErr := run(cfg); runErr != nil {
		slog.Error("fatal", "err", runErr)
		return exitRuntimeErr
	}
	slog.Info("shutdown complete")
	return 0
}

func run(cfg *config.Config) error {
	dcli, err := dockerpkg.New(cfg.DockerSocket)
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = dcli.Close() }()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	role := dockerpkg.NewRoleDetector(dcli, cfg.RoleRecheck)
	if refreshErr := role.Refresh(ctx); refreshErr != nil {
		return fmt.Errorf("initial docker info: %w", refreshErr)
	}
	go role.Run(ctx)

	// Self-discover our swarm service name. Required by identity verification
	// on manager replicas; consumers pick the service name in their stack
	// file so we cannot hard-code it. Fail-closed: if we cannot discover it
	// at startup, refuse to run rather than silently accepting spoofed
	// X-Origin-Node headers later.
	serviceName, err := dcli.SelfServiceName(ctx)
	if err != nil {
		return fmt.Errorf("self service name (must be deployed as a swarm service): %w", err)
	}
	if validateErr := dockerpkg.ValidateServiceName(serviceName); validateErr != nil {
		return fmt.Errorf("self service name is not DNS-safe (use only [A-Za-z0-9-]): %w", validateErr)
	}
	slog.Info("discovered service name", "service", serviceName)

	// Confidentiality gate: any overlay network we're attached to MUST be
	// IPsec-encrypted, otherwise inter-host coordinator traffic is plaintext
	// on the wire. ALLOW_UNENCRYPTED_OVERLAY downgrades the failure to a
	// loud warning for dev/test only.
	if encErr := dockerpkg.CheckOverlayEncryption(ctx, dcli); encErr != nil {
		if !cfg.AllowUnencryptedOverlay {
			return fmt.Errorf("overlay encryption check (set ALLOW_UNENCRYPTED_OVERLAY=true to override): %w", encErr)
		}
		dockerpkg.LogUnencryptedOverlayWarning(encErr)
	} else {
		slog.Info("overlay encryption verified")
	}

	// Wrap dcli so that "not a swarm manager" errors from any manager-only
	// Docker call trigger a synchronous role refresh. Self-heals a stale
	// role cache within the same request — the caller's retry then dispatches
	// to the worker handler and forwards to a peer manager. Constructed AFTER
	// RoleDetector so the detector keeps using the raw client (Info is
	// available on any node, so it never trips the refresh check).
	dcli = dockerpkg.WithRoleRefresh(dcli, role)

	// Now field intentionally left as the package's default (time.Now);
	// tests inject a fake via the package directly.
	//nolint:exhaustruct // Now defaults to time.Now
	store := &lock.ConfigStore{Docker: dcli, ConfigName: cfg.LockConfigName}
	drainer := &drain.DockerDrainer{Docker: dockerAdapter{dcli}}

	handlers := api.Handlers{
		Store:        store,
		Drainer:      drainer,
		LockTTL:      cfg.LockTTL,
		DrainTimeout: cfg.DrainTimeout,
		PollInterval: pollIntervalDrain,
		Role:         func() string { return role.Role().String() },
		Version:      version,
	}
	// TTL defaults to defaultResolverTTL (5s) inside the resolver.
	//nolint:exhaustruct // TTL has a sensible default
	resolver := &identity.SwarmResolver{Docker: dcli, ServiceName: serviceName}
	mw := identity.Middleware{
		NodeHostname: cfg.NodeHostname,
		PublicPaths:  []string{"/healthz", "/role"},
		Resolver:     resolver,
	}

	finder := &forward.ManagerFinder{
		ServiceDNS: "tasks." + serviceName,
		Port:       managerProbePort,
		Client:     &http.Client{Timeout: managerProbeClient},
		TTL:        cfg.ManagerDiscoveryTTL,
	}
	fwd := &forward.Forwarder{
		OriginNode:       cfg.NodeHostname,
		FindManagerURL:   finder.Find,
		OnAttemptFailure: finder.Invalidate,
		Client:           &http.Client{Timeout: cfg.ForwardTimeout},
		Retries:          forwardRetries,
	}

	// Two handler trees, one per listener — each carries its own trust
	// model so we never accidentally apply the wrong one to the wrong
	// traffic. Re-checking role per-request means promote/demote takes
	// effect on the next inbound call without a restart.
	//
	// Middleware order (outer to inner) in each manager-mode path:
	//   identity (HostTrust or Middleware)
	//     → AuditLog (sees resolved identity, observes handler status)
	//       → Router
	//
	// Overlay TCP listener: peer replicas only. Manager mode wraps the
	// router in the IP-derived identity middleware; worker mode forwards
	// outbound and never touches identity.
	overlayHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if role.Role() == dockerpkg.RoleManager {
			mw.Wrap(api.AuditLog(handlers.Router())).ServeHTTP(w, r)
		} else {
			buildWorkerHandler(handlers.Router(), fwd).ServeHTTP(w, r)
		}
	})
	// Host Unix-socket listener: filesystem permissions are the access
	// boundary, with the kernel-reported UID == 0 check via SO_PEERCRED
	// layered on top. HostTrust enforces that on BOTH manager and worker
	// replicas — the gate must be uniform across the cluster, otherwise
	// a non-root member of the swarm-coord group could drive /drain from
	// a worker (forwarded to a manager) but not from a manager directly.
	// In worker mode HostTrust still injects identity, but the forwarder
	// overwrites X-Origin-Node from cfg.NodeHostname before relaying.
	hostHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var inner http.Handler
		if role.Role() == dockerpkg.RoleManager {
			inner = api.AuditLog(handlers.Router())
		} else {
			inner = buildWorkerHandler(handlers.Router(), fwd)
		}
		identity.HostTrust(cfg.NodeHostname, inner).ServeHTTP(w, r)
	})

	listeners, err := bindListeners(ctx, cfg, overlayHandler, hostHandler)
	if err != nil {
		return err
	}

	srv := &api.Server{
		Listeners:     listeners,
		ShutdownGrace: cfg.DrainTimeout + shutdownGraceExtra,
	}
	slog.Info("listening",
		"tcp", cfg.ListenAddr,
		"unix", cfg.UnixSocketPath,
		"initial_role", role.Role().String(),
	)
	if runErr := srv.Run(ctx); runErr != nil {
		return fmt.Errorf("server: %w", runErr)
	}
	return nil
}

// bindListeners opens both production listeners (overlay TCP + host
// Unix socket) and pairs each with its handler. On any failure, any
// already-bound listener is closed before returning so we never leak a
// half-bound state up to the caller.
//
// The Unix listener gets a ConnContext that reads SO_PEERCRED at accept
// time and stamps the peer's UID/GID/PID into the request context. The
// HostTrust middleware downstream uses that to reject non-root callers.
// The overlay TCP listener doesn't set ConnContext — TCP has no
// equivalent kernel-side peer-credential mechanism, and the overlay
// path uses IP-derived identity instead.
func bindListeners(
	ctx context.Context, cfg *config.Config, overlay, host http.Handler,
) ([]api.ListenerSpec, error) {
	var lc net.ListenConfig
	tcpLn, err := lc.Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("tcp listen %s: %w", cfg.ListenAddr, err)
	}
	unixLn, err := openUnixListener(ctx, cfg.UnixSocketPath, cfg.UnixSocketMode, cfg.UnixSocketGID)
	if err != nil {
		_ = tcpLn.Close()
		return nil, fmt.Errorf("unix listen %s: %w", cfg.UnixSocketPath, err)
	}
	return []api.ListenerSpec{
		//nolint:exhaustruct // overlay TCP doesn't use ConnContext (no peer creds on TCP)
		{Name: "overlay-tcp", Listener: tcpLn, Handler: overlay},
		{Name: "host-unix", Listener: unixLn, Handler: host, ConnContext: unixPeerCredConnContext},
	}, nil
}

// unixPeerCredConnContext is the http.Server ConnContext we install on
// the Unix-socket listener. It reads SO_PEERCRED at accept time and
// stamps the peer credentials into the request context where HostTrust
// will read them.
//
// On a non-Unix conn (defense-in-depth) or on a platform without
// SO_PEERCRED support (e.g. macOS dev environments), the function
// returns the context unchanged AND logs at WARN. Strict HostTrust then
// rejects every request that lands without peer creds — fail-closed.
// Production must always see Linux + a real Unix listener, so any WARN
// from this function in a real deployment is a regression worth paging.
func unixPeerCredConnContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		slog.Warn("unix listener accepted a non-UnixConn",
			"conn_type", fmt.Sprintf("%T", c),
			"note", "peer-cred extraction skipped; strict HostTrust will 403")
		return ctx
	}
	pc, err := identity.ReadPeerCred(uc)
	if err != nil {
		slog.Warn("read peer cred failed; downstream HostTrust will 403",
			"err", err.Error())
		return ctx
	}
	return identity.WithPeerCred(ctx, pc)
}

// openUnixListener binds a Unix socket at path, self-bootstrapping the
// parent directory's permissions so the operator does not need a separate
// `install -d` step on every host. The bind mount in the stack file
// surfaces this directory on the host; whatever we do to it inside the
// container is what the host sees.
func openUnixListener(ctx context.Context, path string, mode os.FileMode, gid int) (net.Listener, error) {
	if path == "" {
		return nil, errUnixPathEmpty
	}
	parent := filepath.Dir(path)
	if mkErr := os.MkdirAll(parent, socketParentDirMode); mkErr != nil {
		return nil, fmt.Errorf("create parent dir: %w", mkErr)
	}
	// Take ownership of the parent dir even if Docker auto-created it as
	// root:root mode 0755 from the bind mount. The chown enforces our
	// group boundary; the chmod tightens the mode regardless of Docker's
	// default. Uid=-1 leaves it as root (we run as root in the container).
	if chErr := os.Chown(parent, -1, gid); chErr != nil {
		return nil, fmt.Errorf("chown parent %s to gid %d: %w", parent, gid, chErr)
	}
	// Directories need x to be enterable; G302's "file mode ≤ 0600" rule
	// doesn't apply. We use socketParentDirMode (0750) here intentionally.
	if chmErr := os.Chmod(parent, socketParentDirMode); chmErr != nil {
		return nil, fmt.Errorf("chmod parent %s: %w", parent, chmErr)
	}
	if cleanupErr := cleanupStaleSocket(path); cleanupErr != nil {
		return nil, cleanupErr
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	// Chown + chmod *after* Listen, not via SocketOption, because the
	// file's creation mode is masked by the process umask — a tight umask
	// could leave the socket unreachable. Explicit chown/chmod is
	// deterministic.
	if chErr := os.Chown(path, -1, gid); chErr != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chown socket %s to gid %d: %w", path, gid, chErr)
	}
	if chmErr := os.Chmod(path, mode); chmErr != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod %s to %o: %w", path, mode, chmErr)
	}
	return ln, nil
}

// Sentinel errors so err113 / errors.Is patterns can match without
// substring comparisons.
var (
	errUnixPathEmpty   = errors.New("UNIX_SOCKET_PATH is empty")
	errNonSocketAtPath = errors.New("refusing to remove non-socket at path")
)

// cleanupStaleSocket removes a Unix-socket file left behind by a crashed
// prior instance, refusing if the path is occupied by a non-socket
// (operator misconfiguration). Distinguishes stat errors: ErrNotExist
// is the happy path; anything else (EACCES on the parent, broken bind
// mount) surfaces with context so the operator sees the real cause
// instead of a downstream listen error with no breadcrumb.
func cleanupStaleSocket(path string) error {
	info, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%w: %s", errNonSocketAtPath, path)
		}
		if rmErr := os.Remove(path); rmErr != nil {
			return fmt.Errorf("remove stale socket: %w", rmErr)
		}
		return nil
	case errors.Is(statErr, os.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("stat existing socket path %s: %w", path, statErr)
	}
}

// buildWorkerHandler routes /healthz and /role to the local handler tree (so
// the host script can probe a worker locally) and forwards everything else
// to a manager replica.
func buildWorkerHandler(local, fwd http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", local)
	mux.Handle("/role", local)
	mux.Handle("/", fwd)
	return mux
}

// dockerAdapter bridges internal/docker.NodeSummary to internal/drain.NodeSummary
// (same shape, separate package to avoid the drain package importing internal/docker
// and creating a cycle).
type dockerAdapter struct{ c dockerpkg.Client }

func (a dockerAdapter) NodeList(ctx context.Context, filters map[string][]string) ([]drain.NodeSummary, error) {
	src, err := a.c.NodeList(ctx, filters)
	if err != nil {
		return nil, err
	}
	out := make([]drain.NodeSummary, len(src))
	for i, node := range src {
		out[i] = drain.NodeSummary{
			Hostname:     node.Hostname,
			Role:         node.Role,
			Reachability: node.Reachability,
			Availability: node.Availability,
			Leader:       node.Leader,
		}
	}
	return out, nil
}

func (a dockerAdapter) NodeUpdate(ctx context.Context, node, availability string) error {
	return a.c.NodeUpdate(ctx, node, availability)
}

func (a dockerAdapter) NodeTaskCount(ctx context.Context, node string) (int, error) {
	return a.c.NodeTaskCount(ctx, node)
}
