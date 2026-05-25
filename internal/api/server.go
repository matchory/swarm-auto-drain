package api

import (
	"context"
	"net/http"
	"time"
)

// Server bundles an http.Server configured with the given handler. Run
// blocks until ctx is cancelled, then performs a graceful shutdown.
type Server struct {
	Addr    string
	Handler http.Handler
	// ShutdownGrace bounds how long Shutdown waits for in-flight handlers to
	// complete. Should be at least DrainTimeout + a small buffer so an
	// in-flight POST /drain isn't truncated mid-operation on SIGTERM. Zero
	// → 30 seconds.
	ShutdownGrace time.Duration
}

func (s *Server) Run(ctx context.Context) error {
	grace := s.ShutdownGrace
	if grace <= 0 {
		grace = 30 * time.Second
	}
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s.Handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}
