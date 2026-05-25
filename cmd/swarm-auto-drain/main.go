// swarm-auto-drain is the coordinator daemon. See docs/specs/.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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

var version = "dev"

func main() {
	cfg, err := config.Load()
	if err != nil {
		os.Stderr.WriteString("config: " + err.Error() + "\n")
		os.Exit(2)
	}
	logging.Setup(cfg.LogLevel)
	slog.Info("starting", "version", version, "node", cfg.NodeHostname)

	dcli, err := dockerpkg.New(cfg.DockerSocket)
	if err != nil {
		slog.Error("docker client", "err", err)
		os.Exit(1)
	}
	defer dcli.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	role := dockerpkg.NewRoleDetector(dcli, cfg.RoleRecheck)
	if err := role.Refresh(ctx); err != nil {
		slog.Error("initial docker info", "err", err)
		os.Exit(1)
	}
	go role.Run(ctx)

	store := &lock.ConfigStore{Docker: dcli, ConfigName: cfg.LockConfigName}
	drainer := &drain.DockerDrainer{Docker: dockerAdapter{dcli}}

	handlers := api.Handlers{
		Store:        store,
		Drainer:      drainer,
		LockTTL:      cfg.LockTTL,
		DrainTimeout: cfg.DrainTimeout,
		PollInterval: 2 * time.Second,
		Role:         func() string { return role.Role().String() },
	}
	mw := identity.Middleware{
		NodeHostname: cfg.NodeHostname,
		PublicPaths:  []string{"/healthz", "/role"},
	}

	var handler http.Handler
	if role.Role() == dockerpkg.RoleManager {
		handler = mw.Wrap(handlers.Router())
	} else {
		mf := &forward.ManagerFinder{
			ServiceDNS: "tasks.swarm-auto-drain",
			Port:       7777,
			Client:     &http.Client{Timeout: 3 * time.Second},
			TTL:        cfg.ManagerDiscoveryTTL,
		}
		fwd := &forward.Forwarder{
			OriginNode:     cfg.NodeHostname,
			FindManagerURL: mf.Find,
			Client:         &http.Client{Timeout: cfg.ForwardTimeout},
			Retries:        2,
		}
		// /healthz and /role still answered locally so the host script's
		// wait loop doesn't depend on a manager being reachable.
		mux := http.NewServeMux()
		localRouter := handlers.Router()
		mux.HandleFunc("/healthz", localRouter.ServeHTTP)
		mux.HandleFunc("/role", localRouter.ServeHTTP)
		mux.Handle("/", fwd)
		handler = mux
	}

	srv := &api.Server{Addr: cfg.ListenAddr, Handler: handler}
	slog.Info("listening", "addr", cfg.ListenAddr, "role", role.Role().String())
	if err := srv.Run(ctx); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
	slog.Info("shutdown complete")
}

// dockerAdapter bridges internal/docker.NodeSummary to internal/drain.NodeSummary
// (same shape, separate package to avoid the drain package importing internal/docker
// and creating a cycle).
type dockerAdapter struct{ c dockerpkg.Client }

func (a dockerAdapter) NodeList(ctx context.Context, fs map[string][]string) ([]drain.NodeSummary, error) {
	src, err := a.c.NodeList(ctx, fs)
	if err != nil {
		return nil, err
	}
	out := make([]drain.NodeSummary, len(src))
	for i, n := range src {
		out[i] = drain.NodeSummary{
			Hostname:     n.Hostname,
			Role:         n.Role,
			Reachability: n.Reachability,
			Availability: n.Availability,
		}
	}
	return out, nil
}
func (a dockerAdapter) NodeUpdate(ctx context.Context, n, av string) error {
	return a.c.NodeUpdate(ctx, n, av)
}
func (a dockerAdapter) NodeTaskCount(ctx context.Context, n string) (int, error) {
	return a.c.NodeTaskCount(ctx, n)
}
