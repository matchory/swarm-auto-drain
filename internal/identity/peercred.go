package identity

import (
	"context"
	"errors"
	"net"
)

// PeerCred is the kernel-reported credentials of a Unix-socket peer at
// connect time. Surfaced to the daemon via Linux SO_PEERCRED. The values
// are authoritative — set by the kernel, not the client — and survive
// PID/UID namespace boundaries for UID and GID (PID is meaningful only
// in the peer's namespace).
type PeerCred struct {
	PID uint32
	UID uint32
	GID uint32
}

// ErrInvalidConn is returned when ReadPeerCred is passed a nil connection.
var ErrInvalidConn = errors.New("nil connection")

// ErrUnsupported is returned by ReadPeerCred on non-Linux platforms. The
// production daemon runs on Linux (distroless container); this exists so
// tests on dev machines (macOS) can skip cleanly rather than fail.
var ErrUnsupported = errors.New("SO_PEERCRED not supported on this platform")

type peerCredKey struct{}

// WithPeerCred returns a context that carries the peer credentials so
// downstream handlers and middleware can authorize on them. The
// production wiring sets this via http.Server.ConnContext at accept time.
func WithPeerCred(ctx context.Context, pc PeerCred) context.Context {
	return context.WithValue(ctx, peerCredKey{}, pc)
}

// PeerCredFromContext returns the peer credentials previously injected
// via WithPeerCred, or (zero, false) if none. The bool prevents callers
// from confusing "no creds injected" with "creds where PID/UID/GID are
// all 0" — both are zero values, only the second is legitimate.
func PeerCredFromContext(ctx context.Context) (PeerCred, bool) {
	pc, ok := ctx.Value(peerCredKey{}).(PeerCred)
	return pc, ok
}

// readPeerCredImpl is overridden in peercred_linux.go on Linux and stubbed
// in peercred_other.go elsewhere. Kept as a var so the build-tag-specific
// files can substitute without exporting OS-specific types.
//
//nolint:gochecknoglobals // build-tag-specific dispatch; not user-facing
var readPeerCredImpl = func(*net.UnixConn) (PeerCred, error) {
	return PeerCred{}, ErrUnsupported
}

// ReadPeerCred returns the kernel's view of the connecting peer's
// credentials. Returns ErrInvalidConn for a nil conn, ErrUnsupported on
// non-Linux platforms, or a wrapped syscall error if the getsockopt
// fails.
func ReadPeerCred(conn *net.UnixConn) (PeerCred, error) {
	if conn == nil {
		return PeerCred{}, ErrInvalidConn
	}
	return readPeerCredImpl(conn)
}
