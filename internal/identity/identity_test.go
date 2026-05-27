package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

// noopIdentity is the Metrics field every Middleware literal in this
// file needs. Recording goes nowhere; tests that assert on metrics
// build their own ManualReader-backed Metrics via metrics.NewManualHarness.
var noopIdentity = metrics.NewNoop().Identity //nolint:gochecknoglobals // test-only fixture

// fakeResolver lets tests stub NodeResolver without spinning up the swarm
// lookup machinery. Invalidations counts force-refresh calls; lookupAfterInvalidate
// optionally swaps the byIP map when Invalidate is called, simulating the
// IP-recycle scenario where a fresh refresh yields a different mapping.
type fakeResolver struct {
	mu                    sync.Mutex
	byIP                  map[string]string
	err                   error
	invalidations         int
	lookupAfterInvalidate map[string]string
}

func (f *fakeResolver) NodeForOverlayIP(_ context.Context, ip string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	if h, ok := f.byIP[ip]; ok {
		return h, nil
	}
	return "", ErrUnknownIP
}

func (f *fakeResolver) Invalidate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidations++
	if f.lookupAfterInvalidate != nil {
		f.byIP = f.lookupAfterInvalidate
	}
}

func TestMiddleware_LoopbackUsesNodeHostname(t *testing.T) {
	mw := Middleware{NodeHostname: "test-host", Metrics: noopIdentity}

	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))

	r := httptest.NewRequest(http.MethodPost, "/lock", http.NoBody)
	r.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if got != "test-host" {
		t.Errorf("identity = %q, want test-host", got)
	}
	if w.Code != 200 {
		t.Errorf("status = %d", w.Code)
	}
}

func TestMiddleware_LoopbackIPv6(t *testing.T) {
	mw := Middleware{NodeHostname: "test-host", Metrics: noopIdentity}
	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest(http.MethodGet, "/role", http.NoBody)
	r.RemoteAddr = "[::1]:54321"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "test-host" {
		t.Errorf("identity = %q", got)
	}
}

func TestMiddleware_OverlayResolverDerivesIdentity(t *testing.T) {
	mw := Middleware{
		NodeHostname: "manager-1",
		Resolver:     &fakeResolver{byIP: map[string]string{"10.0.0.42": "worker-3"}},
		Metrics:      noopIdentity,
	}
	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "10.0.0.42:34567"
	r.Header.Set("X-Origin-Node", "worker-3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "worker-3" {
		t.Errorf("identity = %q, want worker-3", got)
	}
	if w.Code != 200 {
		t.Errorf("status = %d", w.Code)
	}
}

func TestMiddleware_OverlayWorksWithoutHeader(t *testing.T) {
	// Even if the header is absent, the IP-derived value is authoritative.
	mw := Middleware{
		NodeHostname: "manager-1",
		Resolver:     &fakeResolver{byIP: map[string]string{"10.0.0.42": "worker-3"}},
		Metrics:      noopIdentity,
	}
	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "10.0.0.42:34567"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "worker-3" {
		t.Errorf("identity = %q, want worker-3", got)
	}
}

func TestMiddleware_OverlayMismatchTriggersRefreshThenRejects(t *testing.T) {
	// Persistent mismatch (no IP recycle): refresh once, still mismatch,
	// reject with 503 so forward.Forwarder retries.
	fr := &fakeResolver{byIP: map[string]string{"10.0.0.42": "worker-3"}}
	mw := Middleware{NodeHostname: "manager-1", Resolver: fr, Metrics: noopIdentity}
	h := mw.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run on persistent mismatch")
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "10.0.0.42:34567"
	r.Header.Set("X-Origin-Node", "worker-7")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (retryable)", w.Code)
	}
	if fr.invalidations != 1 {
		t.Errorf("invalidations = %d, want 1 (force-refresh on mismatch)", fr.invalidations)
	}
}

func TestMiddleware_OverlayMismatchHealsAfterIPRecycle(t *testing.T) {
	// Cache stale-positive: pre-refresh map says worker-3, fresh says worker-7,
	// header says worker-7. Middleware invalidates, re-lookups, finds the
	// fresh mapping agrees with the header, and accepts the request.
	fr := &fakeResolver{
		byIP:                  map[string]string{"10.0.0.42": "worker-3"},
		lookupAfterInvalidate: map[string]string{"10.0.0.42": "worker-7"},
	}
	mw := Middleware{NodeHostname: "manager-1", Resolver: fr, Metrics: noopIdentity}
	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "10.0.0.42:34567"
	r.Header.Set("X-Origin-Node", "worker-7")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200 (mismatch healed after invalidate)", w.Code)
	}
	if got != "worker-7" {
		t.Errorf("identity = %q, want worker-7", got)
	}
}

func TestMiddleware_OverlayUnknownIPIs503(t *testing.T) {
	// Raft propagation lag: source IP not in task list. Transient — return
	// 503 so forward.Forwarder retries.
	mw := Middleware{
		NodeHostname: "manager-1",
		Resolver:     &fakeResolver{byIP: map[string]string{"10.0.0.42": "worker-3"}},
		Metrics:      noopIdentity,
	}
	h := mw.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run for unknown IP")
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "10.0.0.99:34567"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (retryable)", w.Code)
	}
}

func TestMiddleware_OverlayWithoutResolverIs400(t *testing.T) {
	// Manager misconfigured: no resolver. NOT transient — return 400.
	mw := Middleware{NodeHostname: "manager-1", Metrics: noopIdentity}
	h := mw.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run without resolver on overlay path")
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "10.0.0.42:34567"
	r.Header.Set("X-Origin-Node", "worker-3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (config error, not transient)", w.Code)
	}
}

func TestMiddleware_OverlayResolverErrorIs503(t *testing.T) {
	// Resolver itself errored (docker socket transient).
	mw := Middleware{
		NodeHostname: "manager-1",
		Resolver:     &fakeResolver{err: errors.New("docker socket down")},
		Metrics:      noopIdentity,
	}
	h := mw.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run when resolver fails")
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "10.0.0.42:34567"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (retryable)", w.Code)
	}
}

func TestMiddleware_LoopbackIgnoresXOriginNode(t *testing.T) {
	// Loopback is the host-trust boundary: NODE_HOSTNAME is authoritative.
	// Even an explicit X-Origin-Node from the host script is ignored, so a
	// compromised script cannot trick the local replica into acting on
	// another node's behalf. We assert FromContext returns NODE_HOSTNAME,
	// not the (adversarial) header value.
	mw := Middleware{NodeHostname: "self-host", Metrics: noopIdentity}
	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Origin-Node", "victim-node")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "self-host" {
		t.Errorf("identity = %q, want self-host (loopback ignores X-Origin-Node)", got)
	}
}

func TestMiddleware_IgnoresXForwardedFor(t *testing.T) {
	// Defense against a peer that sets X-Forwarded-For: 127.0.0.1 hoping
	// to be classified as loopback. The middleware must read RemoteAddr
	// only — any proxy-style header on the wire is ignored.
	fr := &fakeResolver{byIP: map[string]string{"10.0.0.42": "worker-3"}}
	mw := Middleware{NodeHostname: "manager-1", Resolver: fr, Metrics: noopIdentity}
	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "10.0.0.42:34567"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	r.Header.Set("X-Real-IP", "127.0.0.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "worker-3" {
		t.Errorf("identity = %q, want worker-3 (proxy headers must be ignored)", got)
	}
}

func TestMiddleware_MalformedRemoteAddrIs400(t *testing.T) {
	// SplitHostPort fails on garbage RemoteAddr (no port separator).
	// Config error, not transient — return 400 so the forwarder doesn't
	// pointlessly retry.
	mw := Middleware{NodeHostname: "manager-1", Resolver: &fakeResolver{}, Metrics: noopIdentity}
	h := mw.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run for malformed RemoteAddr")
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "garbage-no-port"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestMiddleware_NonNumericHostIs400(t *testing.T) {
	// SplitHostPort succeeds on "host:port" syntax even if the host is a
	// DNS name; net.ParseIP then returns nil. Reject explicitly — only
	// numeric IPs are valid sources because the resolver indexes by IP.
	mw := Middleware{NodeHostname: "manager-1", Resolver: &fakeResolver{}, Metrics: noopIdentity}
	h := mw.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run for non-numeric host")
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "not-an-ip:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestMiddleware_EmptyHeaderTreatedAsAbsent(t *testing.T) {
	// An empty X-Origin-Node must be treated like "header not present" —
	// not as a mismatch against the derived value. Otherwise a peer
	// could trip an unnecessary cache invalidation on every request.
	fr := &fakeResolver{byIP: map[string]string{"10.0.0.42": "worker-3"}}
	mw := Middleware{NodeHostname: "manager-1", Resolver: fr, Metrics: noopIdentity}
	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "10.0.0.42:34567"
	r.Header.Set("X-Origin-Node", "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "worker-3" {
		t.Errorf("identity = %q, want worker-3", got)
	}
	if fr.invalidations != 0 {
		t.Errorf("invalidations = %d, want 0 (empty header is not a mismatch)", fr.invalidations)
	}
}

func TestFromContext_EmptyOnUnwrappedRequest(t *testing.T) {
	// Defense-in-depth: if a handler is ever reached without going through
	// Wrap (mux misconfiguration), FromContext must return "" so the
	// downstream lock store / drainer fails clearly rather than acting on
	// some arbitrary value.
	id := FromContext(context.Background())
	if id != "" {
		t.Errorf("FromContext(bare ctx) = %q, want empty string", id)
	}
}

func TestMiddleware_PublicPathsBypassMalformedRemoteAddr(t *testing.T) {
	// /healthz and /role must succeed even from a malformed RemoteAddr —
	// the wait loop on the host script must never get stuck on identity
	// resolution failures for endpoints that don't use identity.
	mw := Middleware{NodeHostname: "manager-1", PublicPaths: []string{"/healthz"}, Metrics: noopIdentity}
	h := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	r.RemoteAddr = "garbage"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (public bypass before remote parsing)", w.Code)
	}
}

func TestHostTrust_RejectsNonRootPeerCred(t *testing.T) {
	// The host-side trust model says: "we trust whoever can open this
	// Unix socket." Filesystem ACL says "members of swarm-coord can
	// open it." Layered on top: the kernel-reported peer UID must be 0.
	// A non-root member of the group connecting to the socket is
	// rejected before the handler runs.
	h := HostTrust("self-host", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run for non-root peer")
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r = r.WithContext(WithPeerCred(r.Context(), PeerCred{PID: 12345, UID: 1000, GID: 9999}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestHostTrust_AcceptsRootPeerCred(t *testing.T) {
	// Symmetric: UID == 0 → handler runs, identity stamped as expected.
	var got string
	h := HostTrust("self-host", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r = r.WithContext(WithPeerCred(r.Context(), PeerCred{PID: 12345, UID: 0, GID: 0}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got != "self-host" {
		t.Errorf("identity = %q, want self-host", got)
	}
}

func TestHostTrust_EmitsAuditLogOnRejection(t *testing.T) {
	// Security observability: a rejected request must appear in the
	// audit-style log stream alongside grants, so dashboards filtering on
	// msg=audit see denials too. Otherwise denied attacks are invisible to
	// the SOC.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	h := HostTrust("self-host", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("must not run")
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r = r.WithContext(WithPeerCred(r.Context(), PeerCred{PID: 9876, UID: 1000, GID: 9999}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	// One audit record with msg=audit, status=403, rejected=non-root-peer.
	for line := range bytes.SplitSeq(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec["msg"] != "audit" {
			continue
		}
		status, sOK := rec["status"].(float64)
		if !sOK || status != http.StatusForbidden {
			t.Errorf("audit.status = %v, want 403", rec["status"])
		}
		if rec["rejected"] != "non-root-peer" {
			t.Errorf("audit.rejected = %v, want non-root-peer", rec["rejected"])
		}
		pid, pOK := rec["peer_pid"].(float64)
		if !pOK || pid != 9876 {
			t.Errorf("audit.peer_pid = %v, want 9876", rec["peer_pid"])
		}
		return
	}
	t.Fatalf("no audit record found in log output:\n%s", buf.String())
}

func TestHostTrust_RejectsAbsentPeerCredInStrictMode(t *testing.T) {
	// Default (strict) mode: missing peer creds → 403. A wiring regression
	// that fails to populate peer creds (e.g. a future net.Conn wrapper
	// hiding *net.UnixConn from the ConnContext type assertion) must
	// fail-closed, not silently fall through to "no enforcement".
	h := HostTrust("self-host", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run when peer creds are absent in strict mode")
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (strict mode rejects absent peer creds)", w.Code)
	}
}

func TestHostTrust_AcceptsAbsentPeerCredWithAllowOption(t *testing.T) {
	// The AllowMissingPeerCreds opt-out exists for non-Linux dev / unit
	// tests where SO_PEERCRED isn't available. With the option set,
	// absent peer creds pass through and the request stamps identity as
	// normal. Production wiring (run.go) must NOT pass this option.
	var got string
	h := HostTrust("self-host", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}), AllowMissingPeerCreds())
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (AllowMissingPeerCreds)", w.Code)
	}
	if got != "self-host" {
		t.Errorf("identity = %q, want self-host", got)
	}
}

// rootCredCtx returns a context stamped with the kernel-shaped peer
// creds that production sees (UID 0). Tests that exercise the happy
// path use this rather than the AllowMissingPeerCreds opt-out, so
// they cover the real production code path.
func rootCredCtx(r *http.Request) *http.Request {
	return r.WithContext(WithPeerCred(r.Context(), PeerCred{PID: 12345, UID: 0, GID: 0}))
}

func TestHostTrust_InjectsFixedIdentity(t *testing.T) {
	// The Unix socket listener's access control is the filesystem mode of
	// the socket file. By the time a request lands in HostTrust, the
	// listener has already authenticated the caller; we just stamp it with
	// the node's own hostname.
	var got string
	h := HostTrust("self-host", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	// RemoteAddr is whatever Go puts there for a Unix socket connection —
	// "@" or empty. HostTrust must not parse it.
	r.RemoteAddr = "@"
	r = rootCredCtx(r)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "self-host" {
		t.Errorf("identity = %q, want self-host", got)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d", w.Code)
	}
}

func TestHostTrust_IgnoresXOriginNode(t *testing.T) {
	// Adversarial X-Origin-Node on a HostTrust listener must be ignored.
	// (The threat: a process that can write to the Unix socket and crafts
	// a header trying to act as another node. HostTrust always overrides.)
	var got string
	h := HostTrust("self-host", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.Header.Set("X-Origin-Node", "victim-node")
	r = rootCredCtx(r)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "self-host" {
		t.Errorf("identity = %q, want self-host (header must be ignored)", got)
	}
}

func TestHostTrust_OverridesPreExistingContextIdentity(t *testing.T) {
	// Defense-in-depth: if HostTrust is stacked on top of a context that
	// already has an identity (misconfiguration), HostTrust's value wins.
	// The contract is "this listener's traffic is host-trust" — period.
	var got string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	})
	h := HostTrust("self-host", inner)
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	ctx := WithPeerCred(r.Context(), PeerCred{PID: 12345, UID: 0, GID: 0})
	ctx = context.WithValue(ctx, ctxKey{}, "stale-identity")
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "self-host" {
		t.Errorf("identity = %q, want self-host (must override pre-existing)", got)
	}
}

func TestMiddleware_PublicEndpointsBypass(t *testing.T) {
	mw := Middleware{NodeHostname: "manager-1", PublicPaths: []string{"/healthz", "/role"}, Metrics: noopIdentity}
	h := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	r.RemoteAddr = "10.0.0.42:34567"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	id := FromContext(context.Background())
	if id != "" {
		t.Errorf("FromContext on bare context = %q, want empty", id)
	}
}

func TestMiddleware_RecordsLoopbackOk(t *testing.T) {
	// Loopback happy path: the resolutions counter must record exactly one
	// {source=loopback, outcome=ok} event so dashboards can split host-script
	// traffic from overlay traffic.
	reader, mset := metrics.NewManualHarness(t)
	mw := Middleware{NodeHostname: "self-host", Metrics: mset.Identity}
	h := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodPost, "/lock", http.NoBody)
	r.RemoteAddr = "127.0.0.1:55555"
	h.ServeHTTP(httptest.NewRecorder(), r)

	points := metrics.FindCounter(t, reader, "swarm_auto_drain.identity.resolutions")
	dp := metrics.PointWithAttrs(t, points, "source", "loopback", "outcome", "ok")
	if dp.Value != 1 {
		t.Errorf("count = %d, want 1", dp.Value)
	}
}

func TestMiddleware_RecordsOverlayMismatch(t *testing.T) {
	// Persistent header/IP mismatch: even after the forced invalidate the
	// derived value still disagrees with X-Origin-Node. Counter records
	// once per Wrap call, classifying the source as overlay (set from the
	// RemoteAddr, not from the resolved identity) and the outcome as
	// mismatch.
	reader, mset := metrics.NewManualHarness(t)
	fr := &fakeResolver{byIP: map[string]string{"10.0.0.2": "node-b"}}
	mw := Middleware{NodeHostname: "manager-1", Resolver: fr, Metrics: mset.Identity}
	h := mw.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run on persistent mismatch")
	}))
	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "10.0.0.2:33333"
	r.Header.Set("X-Origin-Node", "node-impersonator")
	h.ServeHTTP(httptest.NewRecorder(), r)

	points := metrics.FindCounter(t, reader, "swarm_auto_drain.identity.resolutions")
	dp := metrics.PointWithAttrs(t, points, "source", "overlay", "outcome", "mismatch")
	if dp.Value != 1 {
		t.Errorf("count = %d, want 1", dp.Value)
	}
}
