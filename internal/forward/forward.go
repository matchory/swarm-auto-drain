// Package forward implements worker→manager request proxying via the
// swarm overlay network.
package forward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"sync"
	"time"
)

// hop-by-hop headers (RFC 7230 §6.1) that an intermediary MUST strip and not
// forward. Connection-specific headers from inbound + response are dropped.
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopHeaders[textproto.CanonicalMIMEHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// Forwarder is an http.Handler that proxies inbound requests to a manager
// replica. Used in worker mode only.
type Forwarder struct {
	OriginNode     string
	FindManagerURL func() (string, error)
	// OnAttemptFailure is invoked when a forwarding attempt fails (connection
	// error or 5xx response). Wired to ManagerFinder.Invalidate so the next
	// attempt re-probes for a healthy manager instead of hammering the cached
	// dead one. Nil-safe.
	OnAttemptFailure func()
	Client           *http.Client
	Retries          int // 1 attempt + Retries extra attempts; default 0 (one attempt)
}

func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	attempts := f.Retries + 1
	var lastErr error
	for i := 0; i < attempts; i++ {
		base, err := f.FindManagerURL()
		if err != nil {
			lastErr = err
			if f.OnAttemptFailure != nil {
				f.OnAttemptFailure()
			}
			continue
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, base+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		copyHeaders(req.Header, r.Header)
		req.Header.Set("X-Origin-Node", f.OriginNode)

		resp, err := f.Client.Do(req)
		if err != nil {
			lastErr = err
			if f.OnAttemptFailure != nil {
				f.OnAttemptFailure()
			}
			continue
		}
		// 5xx — treat as a failed attempt so we re-probe; we still propagate
		// the last 5xx response if we run out of retries.
		if resp.StatusCode >= 500 && i < attempts-1 {
			resp.Body.Close()
			lastErr = fmt.Errorf("upstream %d", resp.StatusCode)
			if f.OnAttemptFailure != nil {
				f.OnAttemptFailure()
			}
			continue
		}
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		resp.Body.Close()
		return
	}
	http.Error(w, fmt.Sprintf("no manager available: %v", lastErr), http.StatusServiceUnavailable)
}

// ManagerFinder discovers manager replicas via DNS lookup of the
// service's overlay DNS name, then probes /role on each to find ones in
// manager mode. Results are cached.
type ManagerFinder struct {
	ServiceDNS string
	Port       int
	Client     *http.Client
	TTL        time.Duration

	mu       sync.Mutex
	cached   string
	cachedAt time.Time
}

var ErrNoManager = errors.New("no manager replica found")

func (m *ManagerFinder) Find() (string, error) {
	m.mu.Lock()
	if m.cached != "" && time.Since(m.cachedAt) < m.TTL {
		c := m.cached
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()

	ips, err := net.LookupIP(m.ServiceDNS)
	if err != nil {
		return "", fmt.Errorf("lookup %s: %w", m.ServiceDNS, err)
	}
	for _, ip := range ips {
		base := fmt.Sprintf("http://%s:%d", ip.String(), m.Port)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/role", nil)
		resp, err := m.Client.Do(req)
		cancel()
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && bytes.Contains(body, []byte(`"manager"`)) {
			m.mu.Lock()
			m.cached, m.cachedAt = base, time.Now()
			m.mu.Unlock()
			return base, nil
		}
	}
	return "", ErrNoManager
}

// Invalidate clears the cache so the next Find call re-probes.
func (m *ManagerFinder) Invalidate() {
	m.mu.Lock()
	m.cached = ""
	m.mu.Unlock()
}
