// Package api serves the coordinator's HTTP API: reboot slots, drain and
// reactivation for the calling node. The contract is
// docs/specs/2026-10-22-v2-host-coordinator-design.md.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	dockerpkg "github.com/matchory/swarm-auto-drain/internal/docker"
	"github.com/matchory/swarm-auto-drain/internal/drain"
	"github.com/matchory/swarm-auto-drain/internal/identity"
	"github.com/matchory/swarm-auto-drain/internal/metrics"
	"github.com/matchory/swarm-auto-drain/internal/slots"
)

// SlotStore is what the handlers need from slots.Store.
type SlotStore interface {
	Acquire(ctx context.Context, holder string, kind slots.Kind) (*slots.Slot, []string, error)
	Release(ctx context.Context, holder string) (bool, error)
	HeldBy(ctx context.Context, holder string) (*slots.Slot, error)
}

// Handlers wires the HTTP handlers onto their collaborators.
// Field order puts pointer/interface fields first to shrink the GC scan window.
type Handlers struct {
	Slots        SlotStore
	Drainer      drain.Drainer
	HTTPMetrics  metrics.HTTPMetrics
	DrainMetrics metrics.DrainMetrics
	DrainTimeout time.Duration
	PollInterval time.Duration
	// inFlight holds the nodes with an /acquire or /release running here.
	// Set by Router.
	inFlight *sync.Map
}

const (
	// drainGrace lets a drain run to its own timeout instead of being cut
	// off by the request context.
	drainGrace = 30 * time.Second
	// rollbackTimeout bounds a rollback, which runs even after the caller
	// has gone.
	rollbackTimeout = 30 * time.Second
)

// Router returns the API mux.
func (h Handlers) Router() http.Handler {
	served := h
	if served.inFlight == nil {
		served.inFlight = &sync.Map{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/acquire", served.acquire)
	mux.HandleFunc("/release", served.release)
	mux.HandleFunc("/status", served.status)
	return Metrics(mux, h.HTTPMetrics)
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h Handlers) acquire(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	c := identity.FromContext(ctx)
	done, ok := h.claim(c.Hostname)
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": errInProgress.Error()})
		return
	}
	defer done()

	slot, err := h.Slots.HeldBy(ctx, c.Hostname)
	if err != nil {
		fail(w, err)
		return
	}
	if slot == nil {
		// Not active without a slot of ours: an operator drained or paused
		// the node. Its /release would set it active again.
		if c.Availability != "active" {
			writeJSON(w, http.StatusLocked, map[string]string{"error": "node is " + c.Availability + " by an operator"})
			return
		}
		if slot = h.takeSlot(w, r, c); slot == nil {
			return
		}
	}

	drainCtx, cancel := context.WithTimeout(ctx, h.DrainTimeout+drainGrace)
	defer cancel()
	res, err := h.Drainer.Drain(drainCtx, c.Hostname, h.DrainTimeout, h.PollInterval)
	if err != nil {
		slog.Error("drain failed, rolling back", "node", c.Hostname, "err", err)
		h.rollback(ctx, c.Hostname)
		fail(w, err)
		return
	}
	body := map[string]any{
		"slot":            slot,
		"tasks_remaining": res.TasksRemaining,
		"elapsed_seconds": int(res.Elapsed.Seconds()),
	}
	if res.TimedOut {
		slog.Warn("drain timed out, rolling back", "node", c.Hostname, "tasks_remaining", res.TasksRemaining)
		h.rollback(ctx, c.Hostname)
		delete(body, "slot")
		writeJSON(w, http.StatusRequestTimeout, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// takeSlot checks quorum for a manager, then takes a slot of the caller's
// kind. On nil it has already written the response.
func (h Handlers) takeSlot(w http.ResponseWriter, r *http.Request, c identity.Caller) *slots.Slot {
	ctx := r.Context()
	kind := slots.Worker
	if c.Manager {
		kind = slots.Manager
		ok, rep, err := h.Drainer.QuorumOK(ctx, c.Hostname)
		if err != nil {
			fail(w, err)
			return nil
		}
		if !ok {
			h.DrainMetrics.Total.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("outcome", "quorum_fail")))
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "manager quorum would be lost", "report": rep})
			return nil
		}
	}
	slot, holders, err := h.Slots.Acquire(ctx, c.Hostname, kind)
	if err != nil {
		fail(w, err)
		return nil
	}
	if slot == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "no free slot", "holders": holders})
	}
	return slot
}

// claim marks node as having a request in flight here, so overlapping
// requests from one node cannot both drain it or race a rollback. False
// means one is already running.
func (h Handlers) claim(node string) (release func(), ok bool) {
	if _, loaded := h.inFlight.LoadOrStore(node, struct{}{}); loaded {
		return nil, false
	}
	return func() { h.inFlight.Delete(node) }, true
}

// rollback sets node active and frees its slot after a failed drain.
// Best-effort: failures are logged, the drain failure is what the caller
// sees. It does not use the request's context, which is canceled when the
// caller gives up: that is when a rollback is needed most.
func (h Handlers) rollback(reqCtx context.Context, node string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), rollbackTimeout)
	defer cancel()
	if err := h.Drainer.Activate(ctx, node); err != nil {
		slog.Error("rollback: activate failed; slot kept", "node", node, "err", err)
		return
	}
	if _, err := h.Slots.Release(ctx, node); err != nil {
		slog.Error("rollback: release failed", "node", node, "err", err)
	}
}

func (h Handlers) release(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	c := identity.FromContext(ctx)
	done, ok := h.claim(c.Hostname)
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": errInProgress.Error()})
		return
	}
	defer done()

	slot, err := h.Slots.HeldBy(ctx, c.Hostname)
	if err != nil {
		fail(w, err)
		return
	}
	// No slot: an operator may have drained this node by hand; leave it.
	if slot == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"released": false})
		return
	}
	// Active before freeing the slot, so a failed activation keeps the slot
	// and no further node drains while this one carries nothing.
	if err = h.Drainer.Activate(ctx, c.Hostname); err != nil {
		fail(w, err)
		return
	}
	if _, err = h.Slots.Release(ctx, c.Hostname); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"released": true})
}

func (h Handlers) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c := identity.FromContext(r.Context())
	slot, err := h.Slots.HeldBy(r.Context(), c.Hostname)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": c.Hostname, "availability": c.Availability, "slot": slot})
}

var (
	// errNotManager tells the caller to try the next manager.
	errNotManager = errors.New("not a manager")
	// errInProgress: this node already has a request running here.
	errInProgress = errors.New("a request for this node is in progress")
)

// fail answers 503 when this node is not a swarm manager, 500 otherwise.
func fail(w http.ResponseWriter, err error) {
	if dockerpkg.IsNotManager(err) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": errNotManager.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errchkjson // body shapes used here are always encodable; response is best-effort
	_ = json.NewEncoder(w).Encode(v)
}
