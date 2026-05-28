// Package api wires the HTTP API onto the lock store, drainer, and
// caller-identity middleware.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	"github.com/matchory/swarm-auto-drain/internal/drain"
	"github.com/matchory/swarm-auto-drain/internal/identity"
	"github.com/matchory/swarm-auto-drain/internal/lock"
	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

// Handlers wires the HTTP handlers onto their collaborators.
// Field order puts pointer/interface fields first to shrink the GC scan window.
type Handlers struct {
	Store   lock.Store
	Drainer drain.Drainer
	Role    func() string
	// Version is the daemon binary's version string, surfaced in /role so
	// host-side clients can detect daemon-vs-host skew. Empty when unset.
	Version      string
	HTTPMetrics  metrics.HTTPMetrics
	DrainMetrics metrics.DrainMetrics
	LockTTL      time.Duration
	DrainTimeout time.Duration
	PollInterval time.Duration
}

// Router returns an http.Handler mux that dispatches the coordinator's HTTP API.
func (h Handlers) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.healthz)
	mux.HandleFunc("/role", h.role)
	mux.HandleFunc("/managers/health", h.managersHealth)
	mux.HandleFunc("/lock", h.lock)
	mux.HandleFunc("/drain", h.drain)
	mux.HandleFunc("/active", h.active)
	return Metrics(mux, h.HTTPMetrics)
}

func (Handlers) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h Handlers) role(w http.ResponseWriter, r *http.Request) {
	// Per spec §3.2: {"role":"manager"|"worker","leader":"<hostname>"}.
	// Leader is looked up via the Drainer; on transient failure (e.g. lost
	// docker socket) we still return role with leader empty rather than 500.
	leader := ""
	if m, err := h.Drainer.Managers(r.Context()); err == nil {
		leader = m.Leader
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"role":    h.Role(),
		"leader":  leader,
		"version": h.Version,
	})
}

func (h Handlers) managersHealth(w http.ResponseWriter, r *http.Request) {
	m, err := h.Drainer.Managers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h Handlers) lock(w http.ResponseWriter, r *http.Request) {
	id := identity.FromContext(r.Context())
	switch r.Method {
	case http.MethodPost:
		h.lockAcquire(w, r, id)
	case http.MethodDelete:
		h.lockRelease(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h Handlers) lockAcquire(w http.ResponseWriter, r *http.Request, id string) {
	res, err := h.Store.Acquire(r.Context(), id, h.LockTTL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	switch res.Status {
	case lock.Acquired:
		writeJSON(w, http.StatusCreated, lockBody(res.Current))
	case lock.AcquiredViaSteal:
		writeJSON(w, http.StatusGone, lockBody(res.Current))
	case lock.HeldByOther:
		writeJSON(w, http.StatusConflict, lockBody(res.Current))
	default:
		http.Error(w, "unknown acquire status", http.StatusInternalServerError)
	}
}

func (h Handlers) lockRelease(w http.ResponseWriter, r *http.Request, id string) {
	err := h.Store.Release(r.Context(), id)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, lock.ErrNotHolder):
		http.Error(w, "not holder", http.StatusForbidden)
	case errors.Is(err, lock.ErrNoLock):
		http.Error(w, "no lock", http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// lockBody normalises a nil *lock.Lock to an empty JSON object. Current may be
// nil for HeldByOther in a race where the winning peer's lock vanished before
// we could re-inspect; this keeps the response well-formed.
func lockBody(current *lock.Lock) any {
	if current == nil {
		return map[string]any{}
	}
	return current
}

func (h Handlers) drain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := identity.FromContext(r.Context())

	ok, rep, err := h.Drainer.QuorumOK(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		h.DrainMetrics.Total.Add(r.Context(), 1, otelmetric.WithAttributes(attribute.String("outcome", "quorum_fail")))
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":  "manager quorum would be lost",
			"report": rep,
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.DrainTimeout+30*time.Second)
	defer cancel()
	res, err := h.Drainer.Drain(ctx, id, h.DrainTimeout, h.PollInterval)
	if err != nil {
		slog.Error("drain failed", "node", id, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body := map[string]any{
		"tasks_remaining": res.TasksRemaining,
		"elapsed_seconds": int(res.Elapsed.Seconds()),
	}
	if res.TimedOut {
		writeJSON(w, http.StatusRequestTimeout, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (h Handlers) active(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := identity.FromContext(r.Context())
	if err := h.Drainer.Activate(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errchkjson // body shapes used here are always encodable; response is best-effort
	_ = json.NewEncoder(w).Encode(v)
}
