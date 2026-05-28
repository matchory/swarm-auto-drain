// Package forward implements worker→manager request proxying via the
// swarm overlay network.
package forward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"sync"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/metrics"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// hop-by-hop headers (RFC 7230 §6.1) that an intermediary MUST strip and not
// forward. Connection-specific headers from inbound + response are dropped.
//
//nolint:gochecknoglobals // immutable lookup table; const-equivalent for maps
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

// dnsLookupTimeout bounds the DNS resolution for the manager service name.
const dnsLookupTimeout = 2 * time.Second

// probeTimeout bounds the /role probe to a candidate manager replica.
const probeTimeout = 2 * time.Second

// errUpstream5xx is returned internally when a forwarded request gets a 5xx
// response and we still have retries remaining.
var errUpstream5xx = errors.New("upstream 5xx")

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
// Field order groups pointer/func fields up front so the GC pointer-scan
// window stays compact.
type Forwarder struct {
	FindManagerURL func() (string, error)
	// OnAttemptFailure is invoked when a forwarding attempt fails (connection
	// error or 5xx response). Wired to ManagerFinder.Invalidate so the next
	// attempt re-probes for a healthy manager instead of hammering the cached
	// dead one. Nil-safe.
	OnAttemptFailure func()
	Client           *http.Client
	OriginNode       string
	Metrics          metrics.ForwardMetrics
	Retries          int // 1 attempt + Retries extra attempts; default 0 (one attempt).
}

// ServeHTTP proxies the inbound request to a manager replica, retrying on
// connection failure or 5xx responses.
func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		f.Metrics.Duration.Record(r.Context(), time.Since(start).Seconds())
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	attempts := f.Retries + 1
	var lastErr error
	for i := range attempts {
		base, findErr := f.FindManagerURL()
		if findErr != nil {
			lastErr = findErr
			f.recordAttempt(r.Context(), "connect_error")
			if f.OnAttemptFailure != nil {
				f.OnAttemptFailure()
			}
			continue
		}
		//nolint:gosec // SSRF: by design — this *is* a proxy; URL host is from trusted ManagerFinder
		req, reqErr := http.NewRequestWithContext(r.Context(), r.Method, base+r.URL.RequestURI(), bytes.NewReader(body))
		if reqErr != nil {
			lastErr = reqErr
			continue
		}
		copyHeaders(req.Header, r.Header)
		req.Header.Set("X-Origin-Node", f.OriginNode)

		//nolint:gosec // SSRF: see comment above; this proxy intentionally forwards client URLs
		resp, doErr := f.Client.Do(req)
		if doErr != nil {
			lastErr = doErr
			f.recordAttempt(r.Context(), classifyDoError(doErr))
			if f.OnAttemptFailure != nil {
				f.OnAttemptFailure()
			}
			continue
		}
		// 5xx — treat as a failed attempt. We invalidate the cached
		// manager URL on EVERY 5xx, including the last attempt that
		// gets propagated to the caller; otherwise the next request
		// would re-hit the same sick manager. If we have retries left,
		// loop; otherwise fall through and stream the 5xx upstream.
		if resp.StatusCode >= http.StatusInternalServerError {
			f.recordAttempt(r.Context(), "http_5xx")
			if f.OnAttemptFailure != nil {
				f.OnAttemptFailure()
			}
			if i < attempts-1 {
				_ = resp.Body.Close()
				lastErr = fmt.Errorf("%w: %d", errUpstream5xx, resp.StatusCode)
				continue
			}
		} else {
			f.recordAttempt(r.Context(), "ok")
		}
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		_ = resp.Body.Close()
		return
	}
	http.Error(w, fmt.Sprintf("no manager available: %v", lastErr), http.StatusServiceUnavailable)
}

// recordAttempt increments the attempts counter with the given outcome.
func (f *Forwarder) recordAttempt(ctx context.Context, outcome string) {
	f.Metrics.Attempts.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("outcome", outcome)))
}

// classifyDoError maps an http.Client.Do error to an attempts-counter outcome.
func classifyDoError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "connect_error"
}

// ManagerFinder discovers manager replicas via DNS lookup of the
// service's overlay DNS name, then probes /role on each to find ones in
// manager mode. Results are cached.
type ManagerFinder struct {
	cachedAt   time.Time
	Client     *http.Client
	ServiceDNS string
	cached     string
	Metrics    metrics.ForwardMetrics
	mu         sync.Mutex
	TTL        time.Duration
	Port       int
}

// ErrNoManager indicates no manager replica was found via service DNS.
var ErrNoManager = errors.New("no manager replica found")

// Find returns the base URL of a manager replica, using the cache when fresh
// or re-probing service DNS otherwise. Returns ErrNoManager if no probed peer
// reports manager role.
func (m *ManagerFinder) Find() (string, error) {
	m.mu.Lock()
	if m.cached != "" && time.Since(m.cachedAt) < m.TTL {
		c := m.cached
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()

	lookupCtx, lookupCancel := context.WithTimeout(context.Background(), dnsLookupTimeout)
	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, m.ServiceDNS)
	lookupCancel()
	if err != nil {
		return "", fmt.Errorf("lookup %s: %w", m.ServiceDNS, err)
	}
	for _, addr := range addrs {
		// HTTP (not HTTPS) over the swarm overlay network is intentional:
		// the overlay is internal (no external reachability) and TLS would
		// require provisioning per-task certs we don't currently issue.
		base := "http://" + net.JoinHostPort(addr.IP.String(), strconv.Itoa(m.Port))
		if m.probe(base) {
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
	// context.Background() is intentional: Invalidate is called from
	// synchronous places (the forwarder's OnAttemptFailure callback) that
	// don't thread a parent context through. Cache invalidation is
	// noise-level signal and doesn't need request-bound tracing.
	m.Metrics.CacheInvalidations.Add(context.Background(), 1)
}

// probe issues a short-deadline GET /role to base and reports whether the peer
// self-identifies as a manager. Network errors are treated as "not a manager".
func (m *ManagerFinder) probe(base string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/role", http.NoBody)
	if err != nil {
		return false
	}
	resp, err := m.Client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	// Decode the documented /role contract rather than substring-matching the
	// raw body: a substring check would false-positive on any other field
	// whose value contains "manager" (e.g. a leader hostname literally named
	// "manager"), causing a worker to forward control-plane requests to a peer
	// that isn't actually a manager.
	var payload struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	return payload.Role == "manager"
}
