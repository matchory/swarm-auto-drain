// Package identity resolves the calling node's hostname from the inbound
// request. Loopback connections (the host shell script) are identified by
// the replica's own NODE_HOSTNAME; overlay-network connections (peer
// replicas forwarding) are identified by the X-Origin-Node header.
package identity

import (
	"context"
	"net"
	"net/http"
)

type ctxKey struct{}

// Middleware injects the resolved caller identity into the request
// context. Handlers retrieve it via FromContext.
type Middleware struct {
	NodeHostname string
	PublicPaths  []string
}

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
		id := resolve(r, m.NodeHostname)
		if id == "" {
			http.Error(w, "could not resolve caller identity (loopback or X-Origin-Node required)", http.StatusBadRequest)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func resolve(r *http.Request, nodeHostname string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		ip := net.ParseIP(host)
		if ip != nil && ip.IsLoopback() {
			return nodeHostname
		}
	}
	return r.Header.Get("X-Origin-Node")
}

// FromContext returns the identity injected by Wrap, or "" if none.
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}
