package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/api"
	"github.com/matchory/swarm-auto-drain/internal/config"
	dockerpkg "github.com/matchory/swarm-auto-drain/internal/docker"
	"github.com/matchory/swarm-auto-drain/internal/drain"
	"github.com/matchory/swarm-auto-drain/internal/identity"
	"github.com/matchory/swarm-auto-drain/internal/logging"
	"github.com/matchory/swarm-auto-drain/internal/metrics"
	"github.com/matchory/swarm-auto-drain/internal/slots"
)

const (
	exitConfigErr      = 2
	exitRuntimeErr     = 1
	pollIntervalDrain  = 2 * time.Second
	shutdownGraceExtra = 30 * time.Second
	// Bounds the metric provider's final flush so a stuck OTLP exporter
	// can't block process exit indefinitely.
	metricsShutdownGrace = 5 * time.Second
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
	slog.Info("starting", "version", version)

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

	// Not fatal: on a node that is not (or no longer) a manager every request
	// answers 503 and callers move on to the next manager.
	if info, infoErr := dcli.Info(ctx); infoErr != nil {
		return fmt.Errorf("docker info: %w", infoErr)
	} else if !info.Swarm.ControlAvailable {
		slog.Warn("this node is not a swarm manager; every request will answer 503")
	}

	hostname, _ := os.Hostname()
	mtr, metricsHandler, metricsShutdown, err := metrics.New(ctx, metrics.Config{
		ListenAddr:     cfg.MetricsListenAddr,
		OTLPEndpoint:   cfg.OTLPEndpoint,
		PushInterval:   cfg.MetricsPushInterval,
		ServiceVersion: version,
		NodeHostname:   hostname,
	})
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	defer shutdownMetrics(metricsShutdown)

	handlers := api.Handlers{
		Slots: &slots.Store{
			Docker:      dcli,
			Prefix:      cfg.SlotPrefix,
			WorkerSlots: cfg.WorkerSlots,
			Metrics:     mtr.Slots,
		},
		Drainer:      &drain.DockerDrainer{Docker: dockerAdapter{dcli}, Metrics: mtr.Drain},
		HTTPMetrics:  mtr.HTTP,
		DrainMetrics: mtr.Drain,
		DrainTimeout: cfg.DrainTimeout,
		PollInterval: pollIntervalDrain,
	}
	mw := identity.Middleware{
		WhoIs:       &identity.LocalAPI{Socket: cfg.TailscaleSocket},
		Nodes:       swarmNodes(dcli),
		Metrics:     mtr.Identity,
		AllowedTags: cfg.AllowedTags,
		PublicPaths: []string{"/healthz"},
	}
	// identity → AuditLog (sees the caller, observes the status) → router.
	handler := mw.Wrap(api.AuditLog(handlers.Router()))

	listeners, err := bindListeners(ctx, cfg, handler, metricsHandler)
	if err != nil {
		return err
	}
	srv := &api.Server{
		Listeners:     listeners,
		ShutdownGrace: cfg.DrainTimeout + shutdownGraceExtra,
	}
	slog.Info("listening", "addr", cfg.ListenAddr, "metrics", cfg.MetricsListenAddr,
		"worker_slots", cfg.WorkerSlots, "allowed_tags", cfg.AllowedTags)
	if runErr := srv.Run(ctx); runErr != nil {
		return fmt.Errorf("server: %w", runErr)
	}
	return nil
}

// swarmNodes lists the swarm's nodes for the identity middleware.
func swarmNodes(dcli dockerpkg.Client) func(context.Context) ([]identity.Node, error) {
	return func(ctx context.Context) ([]identity.Node, error) {
		src, err := dcli.NodeList(ctx, nil)
		if err != nil {
			return nil, err
		}
		out := make([]identity.Node, len(src))
		for i, n := range src {
			out[i] = identity.Node{Hostname: n.Hostname, Role: n.Role, Availability: n.Availability}
		}
		return out, nil
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

// bindListeners opens the API listener and, when configured, the metrics
// listener. On failure, anything already bound is closed.
func bindListeners(ctx context.Context, cfg *config.Config, apiH, metricsH http.Handler) ([]api.ListenerSpec, error) {
	var lc net.ListenConfig
	apiLn, err := lc.Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
	}
	specs := []api.ListenerSpec{{Name: "api", Listener: apiLn, Handler: apiH}}
	// The OTLP-push-only configuration has no Prometheus handler.
	if cfg.MetricsListenAddr != "" && metricsH != nil {
		metricsLn, mErr := lc.Listen(ctx, "tcp", cfg.MetricsListenAddr)
		if mErr != nil {
			_ = apiLn.Close()
			return nil, fmt.Errorf("metrics listen %s: %w", cfg.MetricsListenAddr, mErr)
		}
		specs = append(specs, api.ListenerSpec{Name: "metrics", Listener: metricsLn, Handler: metricsH})
	}
	return specs, nil
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
