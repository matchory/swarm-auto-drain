package api

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/matchory/swarm-auto-drain/internal/identity"
)

// auditedPaths is the set of security-relevant endpoints whose calls
// always get a structured log line. /healthz and /role are deliberately
// excluded — they fire on every probe from the host script and would
// drown the audit trail.
//
//nolint:gochecknoglobals // immutable lookup table; const-equivalent for maps
var auditedPaths = map[string]bool{
	"/lock":   true,
	"/drain":  true,
	"/active": true,
}

// AuditLog wraps a handler so that calls to security-relevant endpoints
// emit a structured log line including the resolved identity, the
// response status, and (when present) the connecting process's PID from
// SO_PEERCRED.
//
// The peer_pid attribute appears only on requests that came in over the
// Unix socket — TCP overlay traffic has no equivalent kernel-side
// credential. Omit rather than emit zero so a reader can't mistake
// "absent" for "PID 0."
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
			"identity", identity.FromContext(r.Context()),
		}
		if pc, ok := identity.PeerCredFromContext(r.Context()); ok {
			attrs = append(attrs, "peer_pid", pc.PID)
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
