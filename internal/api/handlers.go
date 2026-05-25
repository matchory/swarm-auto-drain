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

	"github.com/matchory/swarm-auto-drain/internal/drain"
	"github.com/matchory/swarm-auto-drain/internal/identity"
	"github.com/matchory/swarm-auto-drain/internal/lock"
)

type Handlers struct {
	Store        lock.Store
	Drainer      drain.Drainer
	LockTTL      time.Duration
	DrainTimeout time.Duration
	PollInterval time.Duration
	Role         func() string
}

func (h Handlers) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.healthz)
	mux.HandleFunc("/role", h.role)
	mux.HandleFunc("/managers/health", h.managersHealth)
	mux.HandleFunc("/lock", h.lock)
	mux.HandleFunc("/drain", h.drain)
	mux.HandleFunc("/active", h.active)
	return mux
}

func (h Handlers) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h Handlers) role(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"role": h.Role()})
}

func (h Handlers) managersHealth(w http.ResponseWriter, r *http.Request) {
	_, rep, err := h.Drainer.QuorumOK(r.Context(), "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (h Handlers) lock(w http.ResponseWriter, r *http.Request) {
	id := identity.FromContext(r.Context())
	switch r.Method {
	case http.MethodPost:
		res, err := h.Store.Acquire(r.Context(), id, h.LockTTL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		switch res.Status {
		case lock.Acquired:
			writeJSON(w, http.StatusCreated, res.Current)
		case lock.AcquiredViaSteal:
			writeJSON(w, http.StatusGone, res.Current)
		case lock.HeldByOther:
			writeJSON(w, http.StatusConflict, res.Current)
		default:
			http.Error(w, "unknown acquire status", http.StatusInternalServerError)
		}
	case http.MethodDelete:
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
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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
	_ = json.NewEncoder(w).Encode(v)
}
