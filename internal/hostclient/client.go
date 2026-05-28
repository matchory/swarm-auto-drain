// Package hostclient is a Unix-socket HTTP client for the swarm-auto-drain
// coordinator. It speaks the contract defined in
// docs/specs/2026-05-25-coordinator-design.md and is intended for use by the
// host-side subcommands (trigger, resume) that the same binary exposes.
package hostclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// defaultTimeout applies to each call when the user hasn't supplied their
// own HTTPClient. Matches the order-of-magnitude expectations of the
// underlying endpoints: lock/active are sub-second, /drain runs for minutes
// but its caller passes its own context deadline.
const defaultTimeout = 30 * time.Second

// Client is a Unix-socket HTTP client for the swarm-auto-drain coordinator.
// A zero Timeout means "use the default" when HTTPClient is nil. If
// HTTPClient is supplied, Timeout is ignored — callers are expected to
// configure their own client end-to-end in that case.
type Client struct {
	HTTPClient *http.Client
	SocketPath string
	Timeout    time.Duration
}

// Role models the GET /role response. The Version field was added by the
// daemon side later; older daemons omit it, in which case the field stays
// empty.
type Role struct {
	Role    string `json:"role"`
	Leader  string `json:"leader"`
	Version string `json:"version,omitempty"`
}

// Lock models the /lock JSON body returned by the daemon. Shape matches
// internal/lock.Lock: holder hostname, AcquiredAt as Unix seconds (UTC),
// and TTLSeconds. The daemon returns this for both 201/410 (the freshly
// acquired/stolen lock, owned by us) and 409 (the conflicting holder).
type Lock struct {
	Holder     string `json:"holder,omitempty"`
	AcquiredAt int64  `json:"acquired_at,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// LockAcquireResult captures POST /lock's per-status semantics:
//
//	201 → Acquired      (lock is now ours; Lock describes our entry)
//	410 → Stole         (we reclaimed a stale lock; treat as acquired)
//	409 → HeldByOther   (Lock describes the current holder)
type LockAcquireResult struct {
	Lock   Lock
	Status int
}

// New returns a Client with sensible defaults. The HTTPClient is constructed
// lazily on first use so callers may still override it after construction.
func New(socketPath string) *Client {
	return &Client{SocketPath: socketPath}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	// A fresh client per call would defeat connection reuse; we cache one
	// on the Client struct via a sync.Once-style pattern using the field
	// directly — assignment is safe because hostclient.Client is not
	// documented as concurrent-safe (callers create one per host script).
	c.HTTPClient = &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", c.SocketPath)
			},
		},
	}
	return c.HTTPClient
}

// do performs the request, reads the full body, and closes it. Returning the
// body bytes (not the response) keeps every caller's resource handling
// uniform; we never need streaming for these endpoints.
func (c *Client) do(ctx context.Context, method, path string) (int, []byte, error) {
	// The hostname in the URL is meaningless — DialContext pins us to the
	// socket — but net/http requires a syntactically valid URL.
	//revive:disable-next-line:unsecure-url-scheme // unix socket — scheme is a syntactic placeholder
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, http.NoBody)
	if err != nil {
		return 0, nil, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s via %s: %w", method, path, c.SocketPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read body %s %s: %w", method, path, err)
	}
	return resp.StatusCode, body, nil
}

// GetHealthz returns nil iff the daemon answers 2xx on /healthz.
func (c *Client) GetHealthz(ctx context.Context) error {
	status, body, err := c.do(ctx, http.MethodGet, "/healthz")
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("GET /healthz: status %d: %s", status, body)
	}
	return nil
}

// GetRole fetches the /role JSON.
func (c *Client) GetRole(ctx context.Context) (Role, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/role")
	if err != nil {
		return Role{}, err
	}
	if status/100 != 2 {
		return Role{}, fmt.Errorf("GET /role: status %d: %s", status, body)
	}
	var r Role
	if err := json.Unmarshal(body, &r); err != nil {
		return Role{}, fmt.Errorf("decode /role body %q: %w", body, err)
	}
	return r, nil
}

// AcquireLock POSTs /lock. The returned Status is one of 201/409/410 per the
// spec; any other status comes back as an error. Lock is decoded from the
// response body for 201/409/410 alike when the body is present.
func (c *Client) AcquireLock(ctx context.Context) (LockAcquireResult, error) {
	status, body, err := c.do(ctx, http.MethodPost, "/lock")
	if err != nil {
		return LockAcquireResult{}, err
	}
	switch status {
	case http.StatusCreated, http.StatusConflict, http.StatusGone:
		var l Lock
		if len(body) > 0 {
			if err := json.Unmarshal(body, &l); err != nil {
				return LockAcquireResult{}, fmt.Errorf("decode /lock body %q: %w", body, err)
			}
		}
		return LockAcquireResult{Status: status, Lock: l}, nil
	default:
		return LockAcquireResult{}, fmt.Errorf("POST /lock: status %d: %s", status, body)
	}
}

// ReleaseLock DELETEs /lock and returns the raw HTTP status. The spec maps
// 204/403/404 — we let the caller switch on the value rather than encoding
// the semantics here.
func (c *Client) ReleaseLock(ctx context.Context) (int, error) {
	status, body, err := c.do(ctx, http.MethodDelete, "/lock")
	if err != nil {
		return 0, err
	}
	switch status {
	case http.StatusNoContent, http.StatusForbidden, http.StatusNotFound:
		return status, nil
	default:
		return status, fmt.Errorf("DELETE /lock: status %d: %s", status, body)
	}
}

// Drain POSTs /drain. The body is returned verbatim because the daemon
// includes diagnostic detail (tasks_remaining, elapsed_seconds, or a
// quorum-error report) the caller logs as-is.
func (c *Client) Drain(ctx context.Context) (int, []byte, error) {
	return c.do(ctx, http.MethodPost, "/drain")
}

// Activate POSTs /active. Any non-2xx is treated as an error — the spec
// only defines the happy path here.
func (c *Client) Activate(ctx context.Context) error {
	status, body, err := c.do(ctx, http.MethodPost, "/active")
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("POST /active: status %d: %s", status, body)
	}
	return nil
}
