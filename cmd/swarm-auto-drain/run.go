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
	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

const (
	exitConfigErr      = 2
	exitRuntimeErr     = 1
	pollIntervalDrain  = 2 * time.Second
	managerProbePort   = 7777
	managerProbeClient = 3 * time.Second
	forwardRetries     = 2
	shutdownGraceExtra = 30 * time.Second
	// Bounds the metric provider's final flush so a stuck OTLP exporter
	// can't block process exit indefinitely.
	metricsShutdownGrace = 5 * time.Second
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
	// The initial Refresh records into role.Metrics, but the real provider
	// isn't built until after it (metrics.New needs the resulting role as a
	// resource attr). Seed noop instruments first so the initial Refresh
	// can't nil-panic; mtr.Role replaces them below before role.Run starts.
	role.Metrics = metrics.NewNoop().Role
	if refreshErr := role.Refresh(ctx); refreshErr != nil {
		return fmt.Errorf("initial docker info: %w", refreshErr)
	}

	// Build the metric provider now that the initial role is known
	// (InitialRole is a resource attr). Both readers default off, so this
	// falls back to a noop-backed Metrics unless METRICS_LISTEN_ADDR or
	// OTEL_EXPORTER_OTLP_ENDPOINT is set — domain code records safely
	// either way.
	mtr, metricsHandler, metricsShutdown, err := metrics.New(ctx, metricsConfig(cfg, role.Role().String()))
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	defer shutdownMetrics(metricsShutdown)

	// Set role.Metrics before go role.Run so the detector's early ticks
	// don't record into a nil instrument.
	role.Metrics = mtr.Role
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

	handlers, mw, fwd := buildCollaborators(cfg, dcli, role, mtr, serviceName)
	overlayHandler, hostHandler := buildRoleSwitchedHandlers(cfg, role, &handlers, mw, fwd)

	listeners, err := bindListeners(ctx, cfg, overlayHandler, hostHandler, metricsHandler)
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
		"metrics", cfg.MetricsListenAddr,
		"initial_role", role.Role().String(),
	)
	if runErr := srv.Run(ctx); runErr != nil {
		return fmt.Errorf("server: %w", runErr)
	}
	return nil
}

// buildCollaborators constructs the lock store, drainer, HTTP handlers,
// identity middleware, and forwarder. dcli must already be wrapped with the
// role-refresh decorator before being passed in.
func buildCollaborators(
	cfg *config.Config,
	dcli dockerpkg.Client,
	role *dockerpkg.RoleDetector,
	mtr metrics.Metrics,
	serviceName string,
) (api.Handlers, identity.Middleware, *forward.Forwarder) {
	// Now field intentionally left as the package's default (time.Now);
	// tests inject a fake via the package directly.
	//nolint:exhaustruct // Now defaults to time.Now
	store := &lock.ConfigStore{Docker: dcli, ConfigName: cfg.LockConfigName, Metrics: mtr.Lock}
	drainer := &drain.DockerDrainer{Docker: dockerAdapter{dcli}, Metrics: mtr.Drain}

	handlers := api.Handlers{
		Store:        store,
		Drainer:      drainer,
		LockTTL:      cfg.LockTTL,
		DrainTimeout: cfg.DrainTimeout,
		PollInterval: pollIntervalDrain,
		Role:         func() string { return role.Role().String() },
		Version:      version,
		HTTPMetrics:  mtr.HTTP,
		DrainMetrics: mtr.Drain,
	}
	// TTL defaults to defaultResolverTTL (5s) inside the resolver.
	//nolint:exhaustruct // TTL has a sensible default
	resolver := &identity.SwarmResolver{Docker: dcli, ServiceName: serviceName}
	mw := identity.Middleware{
		NodeHostname: cfg.NodeHostname,
		PublicPaths:  []string{"/healthz", "/role", "/metrics"},
		Resolver:     resolver,
		Metrics:      mtr.Identity,
	}

	finder := &forward.ManagerFinder{
		ServiceDNS: "tasks." + serviceName,
		Port:       managerProbePort,
		Client:     &http.Client{Timeout: managerProbeClient},
		TTL:        cfg.ManagerDiscoveryTTL,
		Metrics:    mtr.Forward,
	}
	fwd := &forward.Forwarder{
		OriginNode:       cfg.NodeHostname,
		FindManagerURL:   finder.Find,
		OnAttemptFailure: finder.Invalidate,
		Client:           &http.Client{Timeout: cfg.ForwardTimeout},
		Retries:          forwardRetries,
		Metrics:          mtr.Forward,
	}
	return handlers, mw, fwd
}

// buildRoleSwitchedHandlers builds the two listener-specific HTTP handlers
// (overlay TCP and host Unix-socket), each of which picks between a manager
// or worker handler tree based on the live role check.
//
// Middleware order (outer to inner) in each manager-mode path:
//
//	identity (HostTrust or Middleware)
//	  → AuditLog (sees resolved identity, observes handler status)
//	    → Router
//
// The handler trees are assembled ONCE here, not per request: the router,
// middleware, and worker mux are all role-invariant and stateless, so the
// per-request closures only need to read role.Role() and pick which
// prebuilt tree to serve.
func buildRoleSwitchedHandlers(
	cfg *config.Config,
	role *dockerpkg.RoleDetector,
	handlers *api.Handlers,
	mw identity.Middleware,
	fwd *forward.Forwarder,
) (overlayHandler, hostHandler http.HandlerFunc) {
	audited := api.AuditLog(handlers.Router())
	workerTree := buildWorkerHandler(handlers.Router(), fwd)

	// Overlay TCP listener: peer replicas only. Manager mode wraps the
	// router in the IP-derived identity middleware; worker mode forwards
	// outbound and never touches identity.
	overlayManager := mw.Wrap(audited)
	overlayHandler = func(w http.ResponseWriter, r *http.Request) {
		if role.Role() == dockerpkg.RoleManager {
			overlayManager.ServeHTTP(w, r)
		} else {
			workerTree.ServeHTTP(w, r)
		}
	}
	// Host Unix-socket listener: filesystem permissions are the access
	// boundary, with the kernel-reported UID == 0 check via SO_PEERCRED
	// layered on top. HostTrust enforces that on BOTH manager and worker
	// replicas — the gate must be uniform across the cluster, otherwise
	// a non-root member of the swarm-coord group could drive /drain from
	// a worker (forwarded to a manager) but not from a manager directly.
	// In worker mode HostTrust still injects identity, but the forwarder
	// overwrites X-Origin-Node from cfg.NodeHostname before relaying.
	hostManager := identity.HostTrust(cfg.NodeHostname, audited)
	hostWorker := identity.HostTrust(cfg.NodeHostname, workerTree)
	hostHandler = func(w http.ResponseWriter, r *http.Request) {
		if role.Role() == dockerpkg.RoleManager {
			hostManager.ServeHTTP(w, r)
		} else {
			hostWorker.ServeHTTP(w, r)
		}
	}
	return overlayHandler, hostHandler
}

// metricsConfig assembles the metrics provider config from the daemon
// config plus the startup role snapshot (a resource attribute).
func metricsConfig(cfg *config.Config, initialRole string) metrics.Config {
	return metrics.Config{
		ListenAddr:     cfg.MetricsListenAddr,
		OTLPEndpoint:   cfg.OTLPEndpoint,
		PushInterval:   cfg.MetricsPushInterval,
		ServiceVersion: version,
		NodeHostname:   cfg.NodeHostname,
		InitialRole:    initialRole,
	}
}

// shutdownMetrics flushes the metric provider on daemon exit with a
// bounded budget. A nil shutdown func (no readers configured) is a no-op.
func shutdownMetrics(shutdown metrics.ShutdownFunc) {
	if shutdown == nil {
		return
	}
	sCtx, cancel := context.WithTimeout(context.Background(), metricsShutdownGrace)
	defer cancel()
	if err := shutdown(sCtx); err != nil {
		slog.Warn("metrics shutdown", "err", err)
	}
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
	ctx context.Context, cfg *config.Config, overlay, host, metricsH http.Handler,
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
	specs := []api.ListenerSpec{
		//nolint:exhaustruct // overlay TCP doesn't use ConnContext (no peer creds on TCP)
		{Name: "overlay-tcp", Listener: tcpLn, Handler: overlay},
		{Name: "host-unix", Listener: unixLn, Handler: host, ConnContext: unixPeerCredConnContext},
	}
	// Metrics listener binds only when METRICS_LISTEN_ADDR is set and a
	// real (non-noop) Prometheus handler exists. The OTLP-push-only path
	// produces a nil handler and skips this listener entirely.
	if cfg.MetricsListenAddr != "" && metricsH != nil {
		metricsLn, mErr := lc.Listen(ctx, "tcp", cfg.MetricsListenAddr)
		if mErr != nil {
			_ = tcpLn.Close()
			_ = unixLn.Close()
			return nil, fmt.Errorf("metrics listen %s: %w", cfg.MetricsListenAddr, mErr)
		}
		//nolint:exhaustruct // metrics listener doesn't use ConnContext (no peer creds on TCP)
		specs = append(specs, api.ListenerSpec{Name: "metrics", Listener: metricsLn, Handler: metricsH})
	}
	return specs, nil
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
