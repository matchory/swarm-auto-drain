package identity

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

type fakeWhoIs struct {
	peers map[string]Peer
	err   error
}

func (f fakeWhoIs) WhoIs(_ context.Context, addr string) (Peer, error) {
	if f.err != nil {
		return Peer{}, f.err
	}
	p, ok := f.peers[addr]
	if !ok {
		return Peer{}, ErrUnknownPeer
	}
	return p, nil
}

// notManager is dockerd's message, verbatim.
const notManager = "Error response from daemon: This node is not a swarm manager."

var swarmNodes = []Node{
	{Hostname: "whale-1.example.ts.net", Role: "manager", Availability: "active"},
	{Hostname: "whale-4.example.ts.net", Role: "worker", Availability: "drain"},
}

func middleware(who WhoIs, nodes func(context.Context) ([]Node, error)) Middleware {
	if nodes == nil {
		nodes = func(context.Context) ([]Node, error) { return swarmNodes, nil }
	}
	return Middleware{WhoIs: who, Nodes: nodes, AllowedTags: []string{"tag:swarm-pipeline"}, Metrics: metrics.NewNoop().Identity}
}

// serve runs one request from remoteAddr through the middleware and returns
// the status and the caller the inner handler saw.
func serve(t *testing.T, m Middleware, remoteAddr string) (int, Caller) {
	t.Helper()
	var seen Caller
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/acquire", http.NoBody)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, seen
}

func TestMiddleware_TaggedSwarmNodeIsTheCaller(t *testing.T) {
	who := fakeWhoIs{peers: map[string]Peer{
		"100.1.1.4:5000": {Name: "whale-4.example.ts.net.", ComputedName: "whale-4", Tags: []string{"tag:server", "tag:swarm-pipeline"}},
	}}
	code, c := serve(t, middleware(who, nil), "100.1.1.4:5000")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if c.Hostname != "whale-4.example.ts.net" || c.Manager || c.Availability != "drain" {
		t.Fatalf("caller = %+v", c)
	}
}

func TestMiddleware_ShortSwarmHostnameMatchesComputedName(t *testing.T) {
	who := fakeWhoIs{peers: map[string]Peer{
		"100.1.1.1:5000": {Name: "whale-1.example.ts.net.", ComputedName: "whale-1", Tags: []string{"tag:swarm-pipeline"}},
	}}
	nodes := func(context.Context) ([]Node, error) {
		return []Node{{Hostname: "whale-1", Role: "manager", Availability: "active"}}, nil
	}
	code, c := serve(t, middleware(who, nodes), "100.1.1.1:5000")
	if code != http.StatusOK || c.Hostname != "whale-1" || !c.Manager {
		t.Fatalf("status = %d, caller = %+v", code, c)
	}
}

func TestMiddleware_Rejects(t *testing.T) {
	cases := map[string]Peer{
		"untagged":       {Name: "whale-4.example.ts.net.", ComputedName: "whale-4"},
		"wrong tag":      {Name: "whale-4.example.ts.net.", ComputedName: "whale-4", Tags: []string{"tag:swarm-app"}},
		"not in swarm":   {Name: "laptop.example.ts.net.", ComputedName: "laptop", Tags: []string{"tag:swarm-pipeline"}},
		"user device":    {Name: "whale-4.example.ts.net.", ComputedName: "whale-4", Tags: nil},
		"prefix of node": {Name: "whale.example.ts.net.", ComputedName: "whale", Tags: []string{"tag:swarm-pipeline"}},
	}
	for name, peer := range cases {
		t.Run(name, func(t *testing.T) {
			who := fakeWhoIs{peers: map[string]Peer{"100.1.1.9:1": peer}}
			if code, _ := serve(t, middleware(who, nil), "100.1.1.9:1"); code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", code)
			}
		})
	}
}

func TestMiddleware_DuplicateHostnameIsForbidden(t *testing.T) {
	who := fakeWhoIs{peers: map[string]Peer{
		"100.1.1.4:5000": {Name: "whale-4.example.ts.net.", Tags: []string{"tag:swarm-pipeline"}},
	}}
	nodes := func(context.Context) ([]Node, error) {
		return []Node{
			{Hostname: "whale-4.example.ts.net", Role: "worker"},
			{Hostname: "whale-4.example.ts.net", Role: "manager"},
		}, nil
	}
	if code, _ := serve(t, middleware(who, nodes), "100.1.1.4:5000"); code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
}

func TestMiddleware_UnknownAddressIsForbidden(t *testing.T) {
	if code, _ := serve(t, middleware(fakeWhoIs{}, nil), "203.0.113.5:1"); code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
}

func TestMiddleware_TailscaledDownIsAnError(t *testing.T) {
	who := fakeWhoIs{err: errors.New("dial unix: no such file")}
	if code, _ := serve(t, middleware(who, nil), "100.1.1.4:5000"); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
}

func TestMiddleware_NotAManagerIs503(t *testing.T) {
	who := fakeWhoIs{peers: map[string]Peer{
		"100.1.1.4:5000": {Name: "whale-4.example.ts.net.", Tags: []string{"tag:swarm-pipeline"}},
	}}
	nodes := func(context.Context) ([]Node, error) {
		return nil, errors.New(notManager)
	}
	if code, _ := serve(t, middleware(who, nodes), "100.1.1.4:5000"); code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
}

func TestMiddleware_PublicPathSkipsIdentity(t *testing.T) {
	m := middleware(fakeWhoIs{}, nil)
	m.PublicPaths = []string{"/healthz"}
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	req.RemoteAddr = "203.0.113.5:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// localAPI serves a fake tailscaled LocalAPI on a Unix socket.
func localAPI(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	// A short directory: t.TempDir() can exceed macOS's 104-byte socket path limit.
	dir, err := os.MkdirTemp("", "ts") //nolint:usetesting // see above
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func TestLocalAPI_WhoIs(t *testing.T) {
	var gotHost, gotAddr string
	sock := localAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotAddr = r.Host, r.URL.Query().Get("addr")
		if r.URL.Path != "/localapi/v0/whois" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"Node":{"Name":"whale-4.example.ts.net.","ComputedName":"whale-4",` +
			`"Tags":["tag:swarm-pipeline"]},"UserProfile":{"LoginName":"tagged-devices"}}`))
	})
	p, err := (&LocalAPI{Socket: sock}).WhoIs(context.Background(), "100.1.1.4:5000")
	if err != nil {
		t.Fatalf("WhoIs: %v", err)
	}
	if gotHost != "local-tailscaled.sock" || gotAddr != "100.1.1.4:5000" {
		t.Fatalf("request host=%q addr=%q", gotHost, gotAddr)
	}
	if p.Name != "whale-4.example.ts.net." || p.ComputedName != "whale-4" || len(p.Tags) != 1 {
		t.Fatalf("peer = %+v", p)
	}
}

func TestLocalAPI_UnknownPeer(t *testing.T) {
	sock := localAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no match for IP:port", http.StatusNotFound)
	})
	_, err := (&LocalAPI{Socket: sock}).WhoIs(context.Background(), "203.0.113.5:1")
	if !errors.Is(err, ErrUnknownPeer) {
		t.Fatalf("err = %v, want ErrUnknownPeer", err)
	}
}

func TestLocalAPI_ServerErrorIsNotUnknownPeer(t *testing.T) {
	sock := localAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err := (&LocalAPI{Socket: sock}).WhoIs(context.Background(), "100.1.1.4:5000")
	if err == nil || errors.Is(err, ErrUnknownPeer) {
		t.Fatalf("err = %v, want a non-ErrUnknownPeer error", err)
	}
}
