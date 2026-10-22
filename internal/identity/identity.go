// Package identity resolves which swarm node sent a request: tailscaled names
// the peer behind the source address, the peer must carry an allowed tag, and
// its name must be a node of this swarm.
package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	dockerpkg "github.com/matchory/swarm-auto-drain/internal/docker"
	"github.com/matchory/swarm-auto-drain/internal/metrics"
)

// ErrUnknownPeer means tailscaled knows no peer at the address.
var ErrUnknownPeer = errors.New("no tailnet peer at this address")

// Peer is the part of tailscaled's WhoIs answer identity needs.
type Peer struct {
	Name         string   // MagicDNS name with trailing dot, e.g. "whale-4.example.ts.net.".
	ComputedName string   // Short machine name, e.g. "whale-4".
	Tags         []string // e.g. ["tag:swarm-pipeline"]; empty for user-owned devices.
}

// WhoIs names the tailnet peer behind a remote address ("ip:port").
type WhoIs interface {
	WhoIs(ctx context.Context, addr string) (Peer, error)
}

// Node is the part of a swarm node identity needs.
type Node struct {
	Hostname     string
	Role         string // "manager" | "worker".
	Availability string // "active" | "pause" | "drain".
}

// Caller is the identified node, stored in the request context.
type Caller struct {
	Hostname     string
	Availability string
	Manager      bool
}

type ctxKey struct{}

// FromContext returns the caller the middleware identified, or the zero
// Caller on a public path.
func FromContext(ctx context.Context) Caller {
	if c, ok := ctx.Value(ctxKey{}).(Caller); ok {
		return c
	}
	return Caller{}
}

// WithCaller returns ctx carrying c. For tests of handlers behind the
// middleware.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// Middleware identifies every request outside PublicPaths, answering 403 when
// the caller is not a tagged node of this swarm.
type Middleware struct {
	WhoIs       WhoIs
	Nodes       func(ctx context.Context) ([]Node, error)
	Metrics     metrics.IdentityMetrics
	AllowedTags []string
	PublicPaths []string
}

// Wrap returns next behind the identity check.
func (m Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(m.PublicPaths, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		caller, status, err := m.resolve(r)
		m.record(r.Context(), status)
		if err != nil {
			//nolint:gosec // G706: structured slog attrs, not a format string
			slog.Warn("caller rejected", "remote", r.RemoteAddr, "path", r.URL.Path, "status", status, "err", err.Error())
			writeError(w, status, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithCaller(r.Context(), caller)))
	})
}

// errNotManager is the body of every 503 that tells the caller to try the
// next manager.
var errNotManager = errors.New("not a manager")

func (m Middleware) resolve(r *http.Request) (Caller, int, error) {
	peer, err := m.WhoIs.WhoIs(r.Context(), r.RemoteAddr)
	if errors.Is(err, ErrUnknownPeer) {
		return Caller{}, http.StatusForbidden, err
	}
	if err != nil {
		return Caller{}, http.StatusInternalServerError, fmt.Errorf("whois: %w", err)
	}
	if !slices.ContainsFunc(peer.Tags, func(t string) bool { return slices.Contains(m.AllowedTags, t) }) {
		return Caller{}, http.StatusForbidden, fmt.Errorf("peer %s carries none of %v", peer.Name, m.AllowedTags)
	}
	nodes, err := m.Nodes(r.Context())
	if dockerpkg.IsNotManager(err) {
		return Caller{}, http.StatusServiceUnavailable, errNotManager
	}
	if err != nil {
		return Caller{}, http.StatusInternalServerError, fmt.Errorf("node list: %w", err)
	}
	// Exact matches only: the MagicDNS name first, the short name for a
	// swarm whose nodes registered short hostnames.
	for _, name := range []string{strings.TrimSuffix(peer.Name, "."), peer.ComputedName} {
		if name == "" {
			continue
		}
		matches := slices.DeleteFunc(slices.Clone(nodes), func(n Node) bool { return n.Hostname != name })
		switch len(matches) {
		case 0:
			continue
		case 1:
			n := matches[0]
			return Caller{Hostname: n.Hostname, Availability: n.Availability, Manager: n.Role == "manager"}, http.StatusOK, nil
		default:
			// Hostnames are not unique in a swarm; acting on one of several
			// is acting on a guess.
			return Caller{}, http.StatusForbidden, fmt.Errorf("%d swarm nodes are named %s", len(matches), name)
		}
	}
	return Caller{}, http.StatusForbidden, fmt.Errorf("peer %s is not a node of this swarm", peer.Name)
}

func (m Middleware) record(ctx context.Context, status int) {
	outcome := "ok"
	switch status {
	case http.StatusOK:
	case http.StatusForbidden:
		outcome = "forbidden"
	default:
		outcome = "error"
	}
	m.Metrics.Resolutions.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("outcome", outcome)))
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errchkjson // a map of strings always encodes
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// localAPIHost is the Host tailscaled's LocalAPI requires.
const localAPIHost = "local-tailscaled.sock"

// whoisTimeout bounds one LocalAPI call.
const whoisTimeout = 5 * time.Second

// whoisResponse is the part of tailscaled's apitype.WhoIsResponse we read.
// The field names are tailscaled's.
//
//nolint:tagliatelle // tailscaled's wire format
type whoisResponse struct {
	Node *whoisNode `json:"Node"`
}

//nolint:tagliatelle // tailscaled's wire format
type whoisNode struct {
	Name         string   `json:"Name"`
	ComputedName string   `json:"ComputedName"`
	Tags         []string `json:"Tags"`
}

// LocalAPI asks the local tailscaled over its Unix socket.
type LocalAPI struct {
	Socket string
}

// WhoIs implements WhoIs against GET /localapi/v0/whois.
func (l *LocalAPI) WhoIs(ctx context.Context, addr string) (Peer, error) {
	client := &http.Client{
		Timeout: whoisTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", l.Socket)
			},
		},
	}
	u := "http://" + localAPIHost + "/localapi/v0/whois?addr=" + url.QueryEscape(addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return Peer{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Peer{}, fmt.Errorf("localapi: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Peer{}, ErrUnknownPeer
	case resp.StatusCode != http.StatusOK:
		return Peer{}, fmt.Errorf("localapi whois: HTTP %d", resp.StatusCode)
	}
	var body whoisResponse
	if decodeErr := json.NewDecoder(resp.Body).Decode(&body); decodeErr != nil {
		return Peer{}, fmt.Errorf("decode whois: %w", decodeErr)
	}
	if body.Node == nil {
		return Peer{}, ErrUnknownPeer
	}
	return Peer{Name: body.Node.Name, ComputedName: body.Node.ComputedName, Tags: body.Node.Tags}, nil
}
