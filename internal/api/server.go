package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// errNoListeners signals an API misuse: Server.Run with no ListenerSpec.
var errNoListeners = errors.New("api.Server: no listeners configured")

// Server runs one http.Server per configured listener (the API, and the
// metrics listener when enabled), all sharing the same ShutdownGrace.
// Field order puts pointer/slice fields up front to shrink the GC pointer-scan window.
type Server struct {
	Listeners     []ListenerSpec
	ShutdownGrace time.Duration
}

// ListenerSpec pairs a pre-bound net.Listener with the handler that
// should serve traffic accepted on it. The Name is used in log lines and
// error messages only.
//
// Field order puts pointer/interface fields first.
type ListenerSpec struct {
	Listener net.Listener
	Handler  http.Handler
	Name     string
}

const (
	defaultShutdownGrace = 30 * time.Second
	readHeaderTimeout    = 10 * time.Second
)

// Run starts every configured listener concurrently and blocks until ctx
// is canceled or one of the underlying http.Server instances fails
// fatally. On cancellation, all listeners get up to ShutdownGrace to
// drain in-flight handlers. Each listener's http.Server.Shutdown runs in
// parallel and shares the same deadline budget.
func (s *Server) Run(ctx context.Context) error {
	if len(s.Listeners) == 0 {
		return errNoListeners
	}
	grace := s.ShutdownGrace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}

	servers := make([]*http.Server, len(s.Listeners))
	errCh := make(chan error, len(s.Listeners))
	var wg sync.WaitGroup
	for i, spec := range s.Listeners {
		srv := &http.Server{
			Handler:           spec.Handler,
			ReadHeaderTimeout: readHeaderTimeout,
		}
		servers[i] = srv
		name, l := spec.Name, spec.Listener
		wg.Go(func() {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("listener %q: %w", name, err)
			}
		})
	}

	// Wait for either ctx cancellation or any listener failing.
	var firstErr error
	select {
	case <-ctx.Done():
	case err := <-errCh:
		firstErr = err
	}

	shutErr := shutdownAll(ctx, servers, grace)
	wg.Wait()
	// Collect errors other listeners reported while we were shutting down.
	// Best-effort: a server that had not reached Serve before the shutdown
	// returns ErrServerClosed and reports nothing.
	close(errCh)
	listenerErrs := []error{firstErr}
	for err := range errCh {
		listenerErrs = append(listenerErrs, err)
	}
	return errors.Join(append(listenerErrs, shutErr)...)
}

// shutdownAll calls Shutdown on every server in parallel with a shared
// grace budget. The parent ctx is accepted only to make the call site
// pass contextcheck; we deliberately do NOT derive from it because
// Shutdown must outlive whatever caused the shutdown — reusing the
// canceled parent ctx would immediately truncate in-flight handlers,
// defeating the point of ShutdownGrace.
//
//nolint:contextcheck // fresh ctx is intentional; see doc comment above
func shutdownAll(_ context.Context, servers []*http.Server, grace time.Duration) error {
	shutCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, len(servers))
	for _, srv := range servers {
		s := srv
		wg.Go(func() {
			if err := s.Shutdown(shutCtx); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	// Aggregate via errors.Join — if both servers fail (e.g. handlers
	// blocked past grace deadline on both), the operator needs to see
	// both errors, not just whichever came back first.
	var collected []error
	for err := range errs {
		collected = append(collected, fmt.Errorf("http shutdown: %w", err))
	}
	return errors.Join(collected...)
}
