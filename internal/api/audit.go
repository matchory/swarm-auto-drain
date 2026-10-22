package api

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/matchory/swarm-auto-drain/internal/identity"
)

// auditedPaths is the set of endpoints that change cluster state; each call
// gets a structured log line. /healthz and /status are left out: probes
// would drown the trail.
//
//nolint:gochecknoglobals // immutable lookup table; const-equivalent for maps
var auditedPaths = map[string]bool{
	"/acquire": true,
	"/release": true,
}

// AuditLog wraps a handler so that calls to state-changing endpoints emit a
// structured log line with the caller's node, source address and the
// response status.
//
// Install AuditLog INSIDE the identity middleware (so it sees the
// resolved identity) and OUTSIDE the actual router (so it can observe
// the handler's status code).
func AuditLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !auditedPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK, wroteHeader: false}
		next.ServeHTTP(sr, r)
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", sr.status,
			"node", identity.FromContext(r.Context()).Hostname,
			"remote", r.RemoteAddr,
		}
		// G706 false positive: slog emits attrs as structured JSON, not
		// printf-style — none of the values is a format string.
		slog.Info("audit", attrs...) //nolint:gosec // structured slog attrs, not format string
	})
}

// statusRecorder wraps an http.ResponseWriter to remember the status
// code sent to the client. Default is 200 because http.ResponseWriter
// implicitly emits 200 on the first Write call if WriteHeader hasn't
// been called explicitly.
type statusRecorder struct {
	http.ResponseWriter

	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(body []byte) (int, error) {
	if !s.wroteHeader {
		// Mirror http.ResponseWriter's implicit-200 behavior so our
		// recorded status matches what the client actually sees.
		s.wroteHeader = true
	}
	n, err := s.ResponseWriter.Write(body)
	if err != nil {
		return n, fmt.Errorf("status recorder write: %w", err)
	}
	return n, nil
}
