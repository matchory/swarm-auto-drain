package identity

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"time"
)

type fakeLookup struct {
	mu    sync.Mutex
	calls int
	out   map[string]string
	err   error
}

func (f *fakeLookup) TasksByServiceWithIPs(_ context.Context, _ string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	// Return a copy so the resolver can't mutate our fixture.
	cp := make(map[string]string, len(f.out))
	maps.Copy(cp, f.out)
	return cp, nil
}

func TestSwarmResolver_CachesByTTL(t *testing.T) {
	fl := &fakeLookup{out: map[string]string{"10.0.0.42": "worker-3"}}
	r := &SwarmResolver{Docker: fl, ServiceName: "svc", TTL: time.Hour}

	for i := range 5 {
		got, err := r.NodeForOverlayIP(context.Background(), "10.0.0.42")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got != "worker-3" {
			t.Errorf("got %q", got)
		}
	}
	if fl.calls != 1 {
		t.Errorf("docker calls = %d, want 1 (cached)", fl.calls)
	}
}

func TestSwarmResolver_UnknownIPTriggersRefreshOnce(t *testing.T) {
	// First lookup populates the cache. A subsequent miss on an unknown IP
	// forces exactly one refresh, then returns ErrUnknownIP without further
	// calls until the cache expires.
	fl := &fakeLookup{out: map[string]string{"10.0.0.42": "worker-3"}}
	r := &SwarmResolver{Docker: fl, ServiceName: "svc", TTL: time.Hour}

	_, _ = r.NodeForOverlayIP(context.Background(), "10.0.0.42") // Call 1.
	_, err := r.NodeForOverlayIP(context.Background(), "10.0.0.99")
	if !errors.Is(err, ErrUnknownIP) {
		t.Fatalf("err = %v, want ErrUnknownIP", err)
	}
	if fl.calls != 2 {
		t.Errorf("docker calls = %d, want 2 (initial + refresh-on-miss)", fl.calls)
	}
}

func TestSwarmResolver_RepeatedUnknownIPRefreshesOnce(t *testing.T) {
	// A flood of lookups for the SAME unknown IP must trigger at most one
	// refresh per cache generation — otherwise each miss would fire a full
	// TaskList+NodeList, defeating the TTL cache under a bad/spoofed peer.
	fl := &fakeLookup{out: map[string]string{"10.0.0.42": "worker-3"}}
	r := &SwarmResolver{Docker: fl, ServiceName: "svc", TTL: time.Hour}

	_, _ = r.NodeForOverlayIP(context.Background(), "10.0.0.42") // Call 1: populate (1 RPC).
	for i := range 5 {
		_, err := r.NodeForOverlayIP(context.Background(), "10.0.0.99")
		if !errors.Is(err, ErrUnknownIP) {
			t.Fatalf("miss %d: err = %v, want ErrUnknownIP", i, err)
		}
	}
	if fl.calls != 2 {
		t.Errorf("docker calls = %d, want 2 (initial + one miss-refresh, not one per miss)", fl.calls)
	}
}

func TestSwarmResolver_NewIPVisibleAfterRefresh(t *testing.T) {
	fl := &fakeLookup{out: map[string]string{"10.0.0.42": "worker-3"}}
	r := &SwarmResolver{Docker: fl, ServiceName: "svc", TTL: time.Hour}
	_, _ = r.NodeForOverlayIP(context.Background(), "10.0.0.42")

	// Simulate a new task joining; the resolver refreshes on cache miss.
	fl.mu.Lock()
	fl.out["10.0.0.43"] = "worker-4"
	fl.mu.Unlock()

	got, err := r.NodeForOverlayIP(context.Background(), "10.0.0.43")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "worker-4" {
		t.Errorf("got %q, want worker-4", got)
	}
}

func TestSwarmResolver_PropagatesLookupError(t *testing.T) {
	fl := &fakeLookup{err: errors.New("boom")}
	r := &SwarmResolver{Docker: fl, ServiceName: "svc", TTL: time.Hour}
	_, err := r.NodeForOverlayIP(context.Background(), "10.0.0.42")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSwarmResolver_TTLExpiryRefreshes(t *testing.T) {
	fl := &fakeLookup{out: map[string]string{"10.0.0.42": "worker-3"}}
	r := &SwarmResolver{Docker: fl, ServiceName: "svc", TTL: 10 * time.Millisecond}

	_, _ = r.NodeForOverlayIP(context.Background(), "10.0.0.42")
	time.Sleep(25 * time.Millisecond)
	_, _ = r.NodeForOverlayIP(context.Background(), "10.0.0.42")
	if fl.calls != 2 {
		t.Errorf("docker calls = %d, want 2 (refreshed after TTL)", fl.calls)
	}
}

func TestSwarmResolver_InvalidateForcesRefresh(t *testing.T) {
	fl := &fakeLookup{out: map[string]string{"10.0.0.42": "worker-3"}}
	r := &SwarmResolver{Docker: fl, ServiceName: "svc", TTL: time.Hour}

	_, _ = r.NodeForOverlayIP(context.Background(), "10.0.0.42") // Populates cache.
	r.Invalidate()
	_, _ = r.NodeForOverlayIP(context.Background(), "10.0.0.42") // Must re-fetch.
	if fl.calls != 2 {
		t.Errorf("docker calls = %d, want 2 (Invalidate forces refresh)", fl.calls)
	}
}

func TestSwarmResolver_RefreshErrorOnColdCache(t *testing.T) {
	// Cold-start failure (docker socket down) must surface as a wrapped
	// error, not ErrUnknownIP. Both paths produce 5xx via the middleware,
	// but pinning the error shape gives the operator a useful log line.
	fl := &fakeLookup{err: errors.New("docker socket down")}
	r := &SwarmResolver{Docker: fl, ServiceName: "svc", TTL: time.Hour}
	_, err := r.NodeForOverlayIP(context.Background(), "10.0.0.42")
	if err == nil {
		t.Fatal("expected error on cold-cache refresh failure")
	}
	if errors.Is(err, ErrUnknownIP) {
		t.Errorf("err = %v, want wrapped socket error not ErrUnknownIP", err)
	}
}

func TestSwarmResolver_InvalidateThenRefreshSeesNewMapping(t *testing.T) {
	// IP-recycle scenario: libnetwork reassigns 10.0.0.42 from worker-3
	// to worker-7 inside the TTL window. After Invalidate, the next
	// lookup must return the new value — this is the property the
	// middleware's mismatch-heal path depends on.
	fl := &fakeLookup{out: map[string]string{"10.0.0.42": "worker-3"}}
	r := &SwarmResolver{Docker: fl, ServiceName: "svc", TTL: time.Hour}

	if got, _ := r.NodeForOverlayIP(context.Background(), "10.0.0.42"); got != "worker-3" {
		t.Fatalf("initial got %q, want worker-3", got)
	}

	fl.mu.Lock()
	fl.out = map[string]string{"10.0.0.42": "worker-7"}
	fl.mu.Unlock()

	r.Invalidate()
	got, err := r.NodeForOverlayIP(context.Background(), "10.0.0.42")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "worker-7" {
		t.Errorf("got %q, want worker-7 (post-invalidate refresh)", got)
	}
}

func TestSwarmResolver_ContextCancellationPropagates(t *testing.T) {
	// If the request context is canceled mid-resolve (client disconnect
	// during a drain-time storm), the Docker call must observe the
	// cancellation rather than hanging. The resolver forwards the
	// caller's ctx without wrapping it in its own deadline.
	fl := &ctxObservingLookup{}
	r := &SwarmResolver{Docker: fl, ServiceName: "svc", TTL: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = r.NodeForOverlayIP(ctx, "10.0.0.42")
	if !fl.sawDone {
		t.Error("Docker call did not observe ctx.Done(); resolver swallowed cancellation")
	}
}

type ctxObservingLookup struct {
	sawDone bool
}

func (c *ctxObservingLookup) TasksByServiceWithIPs(ctx context.Context, _ string) (map[string]string, error) {
	select {
	case <-ctx.Done():
		c.sawDone = true
		return nil, ctx.Err()
	default:
		return map[string]string{}, nil
	}
}

func TestSwarmResolver_CacheImmutableAfterFetch(t *testing.T) {
	// Security property: once a refresh has populated the cache, mutating
	// the upstream source map must not retroactively change the
	// resolver's answers. The resolver copies the map (defense against a
	// hostile Docker SDK implementation, or against the underlying SDK
	// reusing a backing slice between calls).
	fl := &fakeLookup{out: map[string]string{"10.0.0.42": "worker-3"}}
	r := &SwarmResolver{Docker: fl, ServiceName: "svc", TTL: time.Hour}

	if got, _ := r.NodeForOverlayIP(context.Background(), "10.0.0.42"); got != "worker-3" {
		t.Fatalf("initial got %q, want worker-3", got)
	}

	// Mutate the upstream fixture in place after the refresh has cached
	// The resolver's own cache survives even if a real SDK reuses backing
	// storage. The lookup fixture already returns a copy on each call;
	// mutating fl.out post-fetch must not propagate into the resolver.
	fl.mu.Lock()
	fl.out["10.0.0.42"] = "attacker-controlled"
	fl.mu.Unlock()

	got, _ := r.NodeForOverlayIP(context.Background(), "10.0.0.42")
	if got != "worker-3" {
		t.Errorf("post-mutation got = %q, want worker-3 (cache must not alias fixture)", got)
	}
}

func TestSwarmResolver_InvalidateDuringRefreshDiscardsResult(t *testing.T) {
	// Race: thread A sees a mismatch and calls Invalidate while thread B's
	// refresh RPC is already in flight. Without the invalGen guard, B's
	// fresh result overwrites A's invalidation: the cache is now "fresh"
	// again with B's (possibly stale-by-recycle) data, and A's retry
	// pulls the wrong mapping and incorrectly accepts the spoofed
	// identity. The guard discards B's write, so A's retry forces a new
	// RPC.
	sl := &slowLookup{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
		out:     map[string]string{"10.0.0.42": "stale-worker"},
	}
	r := &SwarmResolver{Docker: sl, ServiceName: "svc", TTL: time.Hour}

	// Thread B: kicks off a refresh and blocks in the RPC.
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		_, _ = r.NodeForOverlayIP(context.Background(), "10.0.0.42")
	}()
	<-sl.started // B is in-flight.

	// Thread A: invalidates while B's RPC is blocked.
	r.Invalidate()

	// Release B; its RPC returns the old (stale) mapping.
	close(sl.release)
	<-bDone

	// Assert B did NOT write its stale result into the cache. The cache
	// should still be stale (cachedAt == zero from Invalidate). A check
	// via the public method would force a fresh refresh, which is fine —
	// what we're verifying is that the in-flight result didn't clobber.
	r.mu.Lock()
	gotCachedAt := r.cachedAt
	r.mu.Unlock()
	if !gotCachedAt.IsZero() {
		t.Errorf("cachedAt = %v, want zero (B's in-flight result must not have written cache after Invalidate)",
			gotCachedAt)
	}
}

// slowLookup blocks until told to proceed, so we can race many concurrent
// callers into the resolver's refresh path simultaneously and observe that
// they single-flight.
type slowLookup struct {
	started chan struct{}
	release chan struct{}
	calls   int
	out     map[string]string
	mu      sync.Mutex
}

func (s *slowLookup) TasksByServiceWithIPs(_ context.Context, _ string) (map[string]string, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-s.release
	cp := make(map[string]string, len(s.out))
	maps.Copy(cp, s.out)
	return cp, nil
}

func TestSwarmResolver_SingleFlightUnderBurst(t *testing.T) {
	// 50 concurrent callers hit a cold resolver. They must collapse to a
	// single in-flight TasksByServiceWithIPs RPC — otherwise a drain-time
	// burst stampedes the Docker socket.
	sl := &slowLookup{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
		out:     map[string]string{"10.0.0.42": "worker-3"},
	}
	r := &SwarmResolver{Docker: sl, ServiceName: "svc", TTL: time.Hour}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			_, _ = r.NodeForOverlayIP(context.Background(), "10.0.0.42")
		}()
	}

	// Wait for the first call to actually be in-flight, then release it.
	// Any callers that arrived after will block on refreshMu and re-check
	// the (now-warm) cache rather than firing their own RPC.
	<-sl.started
	// Give the rest a moment to queue on refreshMu.
	time.Sleep(20 * time.Millisecond)
	close(sl.release)
	wg.Wait()

	sl.mu.Lock()
	calls := sl.calls
	sl.mu.Unlock()
	if calls != 1 {
		t.Errorf("docker calls = %d, want 1 (single-flighted)", calls)
	}
}
