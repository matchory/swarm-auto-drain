package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/drain"
	"github.com/matchory/swarm-auto-drain/internal/identity"
	"github.com/matchory/swarm-auto-drain/internal/lock"
	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

type fakeStore struct {
	state *lock.Lock
}

func (f *fakeStore) Acquire(_ context.Context, holder string, ttl time.Duration) (lock.AcquireResult, error) {
	if f.state == nil {
		f.state = &lock.Lock{Holder: holder, AcquiredAt: 1, TTLSeconds: int(ttl.Seconds())}
		return lock.AcquireResult{Status: lock.Acquired, Current: f.state}, nil
	}
	if f.state.Holder == holder {
		return lock.AcquireResult{Status: lock.Acquired, Current: f.state}, nil
	}
	return lock.AcquireResult{Status: lock.HeldByOther, Current: f.state}, nil
}

func (f *fakeStore) Release(_ context.Context, holder string) error {
	if f.state == nil {
		return lock.ErrNoLock
	}
	if f.state.Holder != holder {
		return lock.ErrNotHolder
	}
	f.state = nil
	return nil
}
func (f *fakeStore) Inspect(_ context.Context) (*lock.Lock, error) { return f.state, nil }

type fakeDrainer struct {
	quorumOK    bool
	quorumRep   drain.QuorumReport
	managers    drain.ManagersHealth
	managersErr error
	drainRes    drain.Result
	drainErr    error
	activateErr error
	// Captured identity arguments — let security tests assert which node a
	// handler operates on, not just whether the handler ran.
	gotQuorumID string
	gotDrainID  string
	gotActiveID string
}

func (f *fakeDrainer) QuorumOK(_ context.Context, id string) (bool, drain.QuorumReport, error) {
	f.gotQuorumID = id
	return f.quorumOK, f.quorumRep, nil
}

func (f *fakeDrainer) Managers(_ context.Context) (drain.ManagersHealth, error) {
	return f.managers, f.managersErr
}

func (f *fakeDrainer) Drain(_ context.Context, id string, _, _ time.Duration) (drain.Result, error) {
	f.gotDrainID = id
	return f.drainRes, f.drainErr
}

func (f *fakeDrainer) Activate(_ context.Context, id string) error {
	f.gotActiveID = id
	return f.activateErr
}

func newTestServer(t *testing.T, store lock.Store, dr drain.Drainer) *httptest.Server {
	t.Helper()
	h := Handlers{
		Store:        store,
		Drainer:      dr,
		LockTTL:      30 * time.Minute,
		DrainTimeout: time.Second,
		PollInterval: 10 * time.Millisecond,
		Role:         func() string { return "manager" },
		HTTPMetrics:  metrics.NewNoop().HTTP,
		DrainMetrics: metrics.NewNoop().Drain,
	}
	mw := identity.Middleware{
		NodeHostname: "self-host",
		PublicPaths:  []string{"/healthz", "/role"},
		Metrics:      metrics.NewNoop().Identity,
	}
	return httptest.NewServer(mw.Wrap(h.Router()))
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestRole(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{
		quorumOK: true,
		managers: drain.ManagersHealth{Leader: "leader-host"},
	})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/role")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Role    string `json:"role"`
		Leader  string `json:"leader"`
		Version string `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Role != "manager" {
		t.Errorf("role = %q", body.Role)
	}
	if body.Leader != "leader-host" {
		t.Errorf("leader = %q", body.Leader)
	}
}

func TestRole_VersionEcho(t *testing.T) {
	h := Handlers{
		Store:        &fakeStore{},
		Drainer:      &fakeDrainer{managers: drain.ManagersHealth{Leader: "ld"}},
		LockTTL:      time.Minute,
		DrainTimeout: time.Second,
		PollInterval: 10 * time.Millisecond,
		Role:         func() string { return "manager" },
		Version:      "v0.2.0",
		HTTPMetrics:  metrics.NewNoop().HTTP,
		DrainMetrics: metrics.NewNoop().Drain,
	}
	srv := httptest.NewServer(h.Router())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/role")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["version"] != "v0.2.0" {
		t.Errorf("version = %q, want v0.2.0", body["version"])
	}
}

func TestManagersHealth(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{
		quorumOK: true,
		managers: drain.ManagersHealth{Reachable: 2, Total: 3, Leader: "m1"},
	})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/managers/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Reachable int    `json:"reachable"`
		Total     int    `json:"total"`
		Leader    string `json:"leader"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Reachable != 2 || body.Total != 3 || body.Leader != "m1" {
		t.Errorf("body = %+v", body)
	}
}

func TestLockAcquire_Fresh(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/lock", "application/json", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
}

func TestLockAcquire_HeldByOther(t *testing.T) {
	store := &fakeStore{state: &lock.Lock{Holder: "other-host", AcquiredAt: 1, TTLSeconds: 1800}}
	srv := newTestServer(t, store, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/lock", "application/json", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestLockRelease_Holder(t *testing.T) {
	store := &fakeStore{state: &lock.Lock{Holder: "self-host", AcquiredAt: 1, TTLSeconds: 1800}}
	srv := newTestServer(t, store, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/lock", http.NoBody)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

func TestLockRelease_NotHolder(t *testing.T) {
	store := &fakeStore{state: &lock.Lock{Holder: "other-host", AcquiredAt: 1, TTLSeconds: 1800}}
	srv := newTestServer(t, store, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/lock", http.NoBody)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestLockRelease_NoLock(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/lock", http.NoBody)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDrain_QuorumFail(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: false, quorumRep: drain.QuorumReport{
		TotalManagers: 3, OthersReachable: 1, Required: 2,
	}})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/drain", "application/json", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestDrain_QuorumFail_RecordsMetric(t *testing.T) {
	reader, m := metrics.NewManualHarness(t)
	h := Handlers{
		Store:        &fakeStore{},
		Drainer:      &fakeDrainer{quorumOK: false, quorumRep: drain.QuorumReport{TotalManagers: 3, OthersReachable: 1, Required: 2}},
		LockTTL:      30 * time.Minute,
		DrainTimeout: time.Second,
		PollInterval: 10 * time.Millisecond,
		Role:         func() string { return "manager" },
		HTTPMetrics:  m.HTTP,
		DrainMetrics: m.Drain,
	}
	mw := identity.Middleware{
		NodeHostname: "self-host",
		PublicPaths:  []string{"/healthz", "/role"},
		Metrics:      metrics.NewNoop().Identity,
	}
	srv := httptest.NewServer(mw.Wrap(h.Router()))
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/drain", "application/json", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	points := metrics.FindCounter(t, reader, "swarm_auto_drain.drain.total")
	dp := metrics.PointWithAttrs(t, points, "outcome", "quorum_fail")
	if dp.Value != 1 {
		t.Errorf("drain_total{outcome=quorum_fail} value = %d, want 1", dp.Value)
	}
}

func TestDrain_Happy(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{
		quorumOK: true,
		drainRes: drain.Result{TasksRemaining: 0, Elapsed: 100 * time.Millisecond},
	})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/drain", "application/json", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestDrain_Timeout(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{
		quorumOK: true,
		drainRes: drain.Result{TasksRemaining: 4, Elapsed: time.Second, TimedOut: true},
	})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/drain", "application/json", nil)
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Errorf("status = %d, want 408", resp.StatusCode)
	}
}

func TestDrain_Error(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true, drainErr: errors.New("boom")})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/drain", "application/json", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestActive(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/active", "application/json", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// stubResolver is the bare-minimum identity.NodeResolver fake for handler
// integration tests. It returns canned IP→hostname mappings and records
// Invalidate calls so we can assert the middleware re-checks on mismatch.
type stubResolver struct {
	byIP          map[string]string
	invalidations int
}

func (s *stubResolver) NodeForOverlayIP(_ context.Context, ip string) (string, error) {
	if h, ok := s.byIP[ip]; ok {
		return h, nil
	}
	return "", identity.ErrUnknownIP
}

func (s *stubResolver) Invalidate() { s.invalidations++ }

// newAuthedRouter wires the production middleware around the handler tree
// with a stub resolver, so tests can simulate overlay-network requests
// (RemoteAddr = 10.0.0.x) and assert the full identity → handler flow.
// It returns an http.Handler driven via httptest.NewRecorder rather than a
// live httptest.NewServer, because NewServer always presents 127.0.0.1 as
// the source IP and we specifically need to exercise the non-loopback path.
func newAuthedRouter(store lock.Store, dr drain.Drainer, resolver identity.NodeResolver) http.Handler {
	h := Handlers{
		Store:        store,
		Drainer:      dr,
		LockTTL:      30 * time.Minute,
		DrainTimeout: time.Second,
		PollInterval: 10 * time.Millisecond,
		Role:         func() string { return "manager" },
		HTTPMetrics:  metrics.NewNoop().HTTP,
		DrainMetrics: metrics.NewNoop().Drain,
	}
	mw := identity.Middleware{
		NodeHostname: "self-host",
		PublicPaths:  []string{"/healthz", "/role"},
		Resolver:     resolver,
		Metrics:      metrics.NewNoop().Identity,
	}
	return mw.Wrap(h.Router())
}

// TestOverlayCaller_LockHolderIsIPDerived asserts the central security
// claim: a forwarded peer's lock holder is the IP-derived hostname, not
// anything the caller put in X-Origin-Node. The fakeStore captures the
// holder it was passed; we check that against the IP→node mapping.
func TestOverlayCaller_LockHolderIsIPDerived(t *testing.T) {
	store := &fakeStore{}
	resolver := &stubResolver{byIP: map[string]string{"10.0.0.42": "worker-3"}}
	router := newAuthedRouter(store, &fakeDrainer{quorumOK: true}, resolver)

	r := httptest.NewRequest(http.MethodPost, "/lock", http.NoBody)
	r.RemoteAddr = "10.0.0.42:34567"
	// Caller does NOT send X-Origin-Node; IP-derived value must be used.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if store.state == nil || store.state.Holder != "worker-3" {
		t.Errorf("lock holder = %+v, want holder=worker-3", store.state)
	}
}

// TestOverlayCaller_SpoofedHeaderRejected asserts that a peer who controls
// 10.0.0.42 (worker-3) cannot acquire a lock under "worker-7" by
// fabricating X-Origin-Node. The middleware must reject before the handler
// runs — no lock state must be touched.
func TestOverlayCaller_SpoofedHeaderRejected(t *testing.T) {
	store := &fakeStore{}
	resolver := &stubResolver{byIP: map[string]string{"10.0.0.42": "worker-3"}}
	router := newAuthedRouter(store, &fakeDrainer{quorumOK: true}, resolver)

	r := httptest.NewRequest(http.MethodPost, "/lock", http.NoBody)
	r.RemoteAddr = "10.0.0.42:34567"
	r.Header.Set("X-Origin-Node", "worker-7") // Impersonation attempt.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (rejected before handler)", w.Code)
	}
	if store.state != nil {
		t.Errorf("lock state = %+v, want nil (handler must not run)", store.state)
	}
	if resolver.invalidations != 1 {
		t.Errorf("resolver invalidations = %d, want 1 (one IP-recycle retry)", resolver.invalidations)
	}
}

// TestOverlayCaller_DrainsCallerNotHeader: the spec forbids acting on any
// node other than the caller's. A peer claiming to be "victim" via
// X-Origin-Node must not be allowed to drain "victim" — the drainer must
// be invoked with the IP-derived hostname or not at all.
func TestOverlayCaller_DrainsCallerNotHeader(t *testing.T) {
	resolver := &stubResolver{byIP: map[string]string{"10.0.0.42": "worker-3"}}
	dr := &fakeDrainer{
		quorumOK: true,
		drainRes: drain.Result{TasksRemaining: 0, Elapsed: 10 * time.Millisecond},
	}
	router := newAuthedRouter(&fakeStore{}, dr, resolver)

	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "10.0.0.42:34567"
	r.Header.Set("X-Origin-Node", "victim-node")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (header mismatch)", w.Code)
	}
	if dr.gotDrainID != "" {
		t.Errorf("drainer called with id=%q, want never called", dr.gotDrainID)
	}
	if dr.gotQuorumID != "" {
		t.Errorf("quorum-check called with id=%q, want never called", dr.gotQuorumID)
	}
}

// TestOverlayCaller_UnknownIPCannotDrain: source IP not in the task list
// (spoofer or pre-propagation peer) must not reach the handler.
func TestOverlayCaller_UnknownIPCannotDrain(t *testing.T) {
	resolver := &stubResolver{byIP: map[string]string{"10.0.0.42": "worker-3"}}
	dr := &fakeDrainer{quorumOK: true}
	router := newAuthedRouter(&fakeStore{}, dr, resolver)

	r := httptest.NewRequest(http.MethodPost, "/drain", http.NoBody)
	r.RemoteAddr = "10.0.0.99:34567" // Not in resolver.
	r.Header.Set("X-Origin-Node", "worker-3")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if dr.gotDrainID != "" {
		t.Errorf("drainer.Drain called with id=%q, must not run", dr.gotDrainID)
	}
}

// TestOverlayCaller_CannotReleaseOthersLock: peer A holds the lock, peer B
// from a different overlay IP cannot release it — the handler returns
// 403 because identity.FromContext returns peer B's hostname and the
// store rejects the mismatch.
func TestOverlayCaller_CannotReleaseOthersLock(t *testing.T) {
	store := &fakeStore{state: &lock.Lock{Holder: "worker-3", AcquiredAt: 1, TTLSeconds: 1800}}
	resolver := &stubResolver{byIP: map[string]string{
		"10.0.0.42": "worker-3",
		"10.0.0.43": "worker-4",
	}}
	router := newAuthedRouter(store, &fakeDrainer{quorumOK: true}, resolver)

	r := httptest.NewRequest(http.MethodDelete, "/lock", http.NoBody)
	r.RemoteAddr = "10.0.0.43:34567" // Worker-4.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if store.state == nil || store.state.Holder != "worker-3" {
		t.Errorf("lock state = %+v, want unchanged (worker-3 still holds)", store.state)
	}
}

// TestLoopbackCaller_HolderIsOwnHostname: the host script reaches us via
// loopback; identity must come from NODE_HOSTNAME, not from anything in
// the request. Even an explicit X-Origin-Node from the host script (which
// is trusted) is ignored — the loopback path always uses our own env.
func TestLoopbackCaller_HolderIsOwnHostname(t *testing.T) {
	store := &fakeStore{}
	router := newAuthedRouter(store, &fakeDrainer{quorumOK: true}, &stubResolver{})

	r := httptest.NewRequest(http.MethodPost, "/lock", http.NoBody)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Origin-Node", "someone-else") // Intentionally adversarial.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if store.state == nil || store.state.Holder != "self-host" {
		t.Errorf("lock holder = %+v, want self-host (loopback ignores header)", store.state)
	}
}

// TestUnixSocket_HostTrustEndToEnd: spin up the Server with a Unix-socket
// listener carrying the HostTrust handler — the production wiring for
// host-script callers. A request over the socket must reach the handlers
// with identity = NodeHostname, exactly as the host-trust contract says.
// This proves the listener / wrapper / handler integration end-to-end on
// a real socket, not just in isolation.
func TestUnixSocket_HostTrustEndToEnd(t *testing.T) {
	sockPath := shortSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	store := &fakeStore{}
	h := Handlers{
		Store:        store,
		Drainer:      &fakeDrainer{quorumOK: true},
		LockTTL:      30 * time.Minute,
		DrainTimeout: time.Second,
		PollInterval: 10 * time.Millisecond,
		Role:         func() string { return "manager" },
		HTTPMetrics:  metrics.NewNoop().HTTP,
		DrainMetrics: metrics.NewNoop().Drain,
	}
	// AllowMissingPeerCreds: these tests connect via DialContext without
	// running through unixPeerCredConnContext, so no peer creds reach the
	// handler. The peer-cred enforcement path is exercised in
	// internal/identity/peercred_test.go directly; these tests focus on
	// HostTrust's other properties (header override, identity stamping).
	handler := identity.HostTrust("self-host", h.Router(), identity.AllowMissingPeerCreds())

	srv := &Server{
		Listeners:     []ListenerSpec{{Name: "unix", Listener: ln, Handler: handler}},
		ShutdownGrace: time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-runDone
	})

	time.Sleep(20 * time.Millisecond) // Let Serve start.

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: time.Second,
	}
	resp, err := client.Post("http://coord/lock", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if store.state == nil || store.state.Holder != "self-host" {
		t.Errorf("lock holder = %+v, want holder=self-host (from HostTrust)", store.state)
	}
}

// TestUnixSocket_HostTrustOverridesAdversarialHeader: a process that can
// open the socket cannot escalate by sending X-Origin-Node for a
// different node. HostTrust ignores the header entirely.
func TestUnixSocket_HostTrustOverridesAdversarialHeader(t *testing.T) {
	sockPath := shortSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	store := &fakeStore{}
	h := Handlers{
		Store:        store,
		Drainer:      &fakeDrainer{quorumOK: true},
		LockTTL:      30 * time.Minute,
		DrainTimeout: time.Second,
		PollInterval: 10 * time.Millisecond,
		Role:         func() string { return "manager" },
		HTTPMetrics:  metrics.NewNoop().HTTP,
		DrainMetrics: metrics.NewNoop().Drain,
	}
	// AllowMissingPeerCreds: these tests connect via DialContext without
	// running through unixPeerCredConnContext, so no peer creds reach the
	// handler. The peer-cred enforcement path is exercised in
	// internal/identity/peercred_test.go directly; these tests focus on
	// HostTrust's other properties (header override, identity stamping).
	handler := identity.HostTrust("self-host", h.Router(), identity.AllowMissingPeerCreds())
	srv := &Server{
		Listeners:     []ListenerSpec{{Name: "unix", Listener: ln, Handler: handler}},
		ShutdownGrace: time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-runDone
	})
	time.Sleep(20 * time.Millisecond)

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: time.Second,
	}
	req, _ := http.NewRequest(http.MethodPost, "http://coord/lock", http.NoBody)
	req.Header.Set("X-Origin-Node", "victim-node")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if store.state == nil || store.state.Holder != "self-host" {
		t.Errorf("lock holder = %+v, want self-host (header must be ignored)", store.state)
	}
}

// TestPublicEndpoints_NoIdentityRequired: /healthz and /role must be
// reachable without identity resolution, so the host script can probe
// itself on a worker locally without round-tripping to a manager.
func TestPublicEndpoints_NoIdentityRequired(t *testing.T) {
	dr := &fakeDrainer{managers: drain.ManagersHealth{Leader: "leader-host"}}
	// Resolver intentionally empty: a public endpoint must bypass resolution
	// entirely, so an empty/erroring resolver must not affect /healthz or /role.
	router := newAuthedRouter(&fakeStore{}, dr, &stubResolver{})

	for _, path := range []string{"/healthz", "/role"} {
		r := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		r.RemoteAddr = "10.0.0.99:34567" // Would otherwise be ErrUnknownIP.
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200 (public)", path, w.Code)
		}
	}
}
