// Package identity resolves the calling node's hostname from the inbound
// request. Loopback connections (the host shell script) are identified by
// the replica's own NODE_HOSTNAME; overlay-network connections (peer
// replicas forwarding) are identified by mapping the source IP to a swarm
// node via the Docker socket. The X-Origin-Node header is advisory: if
// present, it must agree with the IP-derived value, otherwise the request
// is rejected.
//
// Transient identity failures (unknown IP, header/IP mismatch, resolver
// errors) surface as HTTP 503 so forward.Forwarder's 5xx-retry can cover
// raft-propagation lag and IP-recycle races. Configuration errors (no
// resolver, malformed RemoteAddr) surface as 400; those are not
// retryable.
package identity

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

type ctxKey struct{}

const sourceOverlay = "overlay"

// NodeResolver maps an overlay-network source IP to the swarm node
// hostname running the peer replica. Invalidate forces the next lookup
// to bypass cache — used on header/IP mismatch since libnetwork can
// recycle overlay IPs within a stale-positive TTL window.
type NodeResolver interface {
	NodeForOverlayIP(ctx context.Context, ip string) (string, error)
	Invalidate()
}

// Middleware injects the resolved caller identity into the request
// context. Handlers retrieve it via FromContext.
//
// Resolver is consulted only for non-loopback callers and may be nil on
// worker replicas, which only ever see loopback callers (overlay traffic
// is forwarded outbound, never inbound). On a manager replica Resolver is
// required; non-loopback requests are rejected if it is nil.
// Field order puts interface/slice fields first to shrink the GC scan window.
type Middleware struct {
	Resolver     NodeResolver
	PublicPaths  []string
	Metrics      metrics.IdentityMetrics
	NodeHostname string
}

// Wrap returns an http.Handler that resolves the caller identity and
// injects it into the request context, or rejects the request if
// resolution fails. Paths listed in PublicPaths bypass resolution.
func (m Middleware) Wrap(next http.Handler) http.Handler {
	publics := map[string]bool{}
	for _, p := range m.PublicPaths {
		publics[p] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publics[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		id, source, status, err := m.resolve(r)
		m.Metrics.Resolutions.Add(r.Context(), 1, otelmetric.WithAttributes(
			attribute.String("source", source),
			attribute.String("outcome", outcomeFor(err)),
		))
		if err != nil {
			// G706 false positive: slog emits attrs as structured JSON, not
			// printf-style — the untrusted header value cannot inject log lines.
			slog.Warn("identity rejected", //nolint:gosec // structured slog attrs, not format string
				"remote", r.RemoteAddr,
				"claimed", r.Header.Get("X-Origin-Node"),
				"status", status,
				"err", err.Error())
			http.Error(w, "could not resolve caller identity: "+err.Error(), status)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// resolve returns (identity, source, 0, nil) on success. On failure,
// status is the HTTP code the caller should send back — 503 for
// transient causes the forwarder can retry, 400 for permanent
// caller/config errors. source is "loopback" or "overlay" and reflects
// the RemoteAddr classification, not the resolved identity. A
// malformed RemoteAddr that fails before classification falls into
// "overlay" — loopback parses cleanly, so anything broken came from
// the overlay (or untrusted) path.
func (m Middleware) resolve(r *http.Request) (id, source string, status int, err error) {
	host, _, perr := net.SplitHostPort(r.RemoteAddr)
	if perr != nil {
		return "", sourceOverlay, http.StatusBadRequest, errBadRemote
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", sourceOverlay, http.StatusBadRequest, errBadRemote
	}
	if ip.IsLoopback() {
		return m.NodeHostname, "loopback", 0, nil
	}
	source = sourceOverlay
	if m.Resolver == nil {
		return "", source, http.StatusBadRequest, errNoResolver
	}

	derived, rerr := m.Resolver.NodeForOverlayIP(r.Context(), ip.String())
	if rerr != nil {
		return "", source, http.StatusServiceUnavailable, rerr
	}
	if derived == "" {
		// An empty identity must never reach the lock/drain sinks (it would
		// write an unreleasable empty-holder lock). Treat as transient so the
		// forwarder retries once the node's hostname propagates.
		return "", source, http.StatusServiceUnavailable, errEmptyIdentity
	}

	claimed := r.Header.Get("X-Origin-Node")
	if claimed == "" || claimed == derived {
		return derived, source, 0, nil
	}

	// Mismatch. Could be a libnetwork IP recycle within our TTL — the
	// previous owner of this overlay IP died and a different task picked
	// it up. Force-refresh and re-check once before rejecting.
	m.Resolver.Invalidate()
	derived, rerr = m.Resolver.NodeForOverlayIP(r.Context(), ip.String())
	if rerr != nil {
		return "", source, http.StatusServiceUnavailable, rerr
	}
	if claimed != derived {
		return "", source, http.StatusServiceUnavailable, &mismatchError{claimed: claimed, derived: derived}
	}
	return derived, source, 0, nil
}

// outcomeFor classifies a resolve error for the resolutions counter.
// "mismatch" is a header/IP disagreement that survived an Invalidate;
// everything else that errored is bucketed as "unknown_ip" — including
// malformed remote addrs and resolver socket errors. The counter is
// load-bearing for alerting, not forensics; the slog line above carries
// the precise reason for an operator.
func outcomeFor(err error) string {
	if err == nil {
		return "ok"
	}
	var me *mismatchError
	if errors.As(err, &me) {
		return "mismatch"
	}
	return "unknown_ip"
}

type stringError string

func (e stringError) Error() string { return string(e) }

const (
	errBadRemote     = stringError("invalid remote address")
	errNoResolver    = stringError("non-loopback caller but no resolver configured")
	errEmptyIdentity = stringError("resolved identity is empty")
)

type mismatchError struct{ claimed, derived string }

func (e *mismatchError) Error() string {
	return "X-Origin-Node header (" + e.claimed + ") does not match source IP node (" + e.derived + ")"
}

// FromContext returns the identity injected by Wrap, or "" if none.
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// HostTrustOption configures a HostTrust wrapper. The default
// configuration is strict — peer creds must be present in the context
// AND identify the peer as root, otherwise the request is rejected with
// 403.
type HostTrustOption func(*hostTrustConfig)

type hostTrustConfig struct {
	allowMissingPeerCreds bool
}

// AllowMissingPeerCreds opts out of strict peer-credential enforcement.
// Use ONLY in tests, or on platforms that lack SO_PEERCRED (e.g. macOS
// in local dev). Production wiring (the Linux container) must NOT pass
// this option — it disables the Tier 1 kernel-enforced UID-0 gate and
// reduces the boundary back to the filesystem ACL alone.
func AllowMissingPeerCreds() HostTrustOption {
	return func(c *hostTrustConfig) { c.allowMissingPeerCreds = true }
}

// HostTrust returns a handler that injects `hostname` as the caller
// identity. Intended for listeners whose access control is enforced at
// the listener level (e.g. a Unix socket with filesystem permissions)
// rather than by inspecting the request.
//
// Strict mode (the default) requires peer credentials to be present in
// the request context AND identify the peer as UID 0. Both conditions
// are kernel-enforced via SO_PEERCRED, stamped into the context by the
// listener's ConnContext at accept time. Anything else — including
// missing peer creds — is rejected with 403, AND audit-logged so the
// audit pipeline sees denials.
//
// Use this in place of Middleware on a trusted listener — never alongside
// it on a listener that also receives untrusted traffic, because HostTrust
// will override any identity Middleware tried to set.
func HostTrust(hostname string, next http.Handler, opts ...HostTrustOption) http.Handler {
	cfg := hostTrustConfig{allowMissingPeerCreds: false}
	for _, opt := range opts {
		opt(&cfg)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pc, ok := PeerCredFromContext(r.Context())
		switch {
		case !ok && cfg.allowMissingPeerCreds:
			// Test / non-Linux dev: enforcement opted out. Fall through.
		case !ok:
			// Strict mode, peer creds absent. This is a real production
			// concern if it ever fires — Linux + a Unix listener should
			// always populate creds. Log loud, deny.
			logRejection(r, 0, 0, 0, "peer-creds-absent")
			http.Error(w, "peer credentials required", http.StatusForbidden)
			return
		case pc.UID != 0:
			logRejection(r, pc.PID, pc.UID, pc.GID, "non-root-peer")
			http.Error(w, "non-root peer rejected", http.StatusForbidden)
			return
		default:
			// Happy path: peer creds present, UID == 0. Fall through.
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, hostname)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// logRejection emits both a WARN line (for operator inspection) and an
// audit-shaped INFO line (so the audit pipeline filtering on msg=audit
// sees denials, not just grants). The audit fields mirror what
// api.AuditLog emits on the happy path so dashboards don't need
// per-msg-shape conditionals.
func logRejection(r *http.Request, pid, uid, gid uint32, reason string) {
	// G706 false positive: slog emits attrs as structured JSON, not
	// printf-style — none of the values is a format string.
	slog.Warn("host-trust rejected", //nolint:gosec // structured slog attrs, not format string
		"peer_pid", pid,
		"peer_uid", uid,
		"peer_gid", gid,
		"path", r.URL.Path,
		"reason", reason)
	slog.Info("audit", //nolint:gosec // structured slog attrs, not format string
		"method", r.Method,
		"path", r.URL.Path,
		"status", http.StatusForbidden,
		"identity", "",
		"peer_pid", pid,
		"rejected", reason)
}
