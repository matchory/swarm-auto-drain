package identity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// defaultResolverTTL is the cache lifetime used when SwarmResolver.TTL is zero.
const defaultResolverTTL = 5 * time.Second

// TaskLookup is the minimal Docker surface SwarmResolver needs. Declared
// here (rather than imported from internal/docker) to keep the identity
// package free of Docker SDK dependencies — same pattern as internal/lock
// and internal/drain.
type TaskLookup interface {
	TasksByServiceWithIPs(ctx context.Context, serviceName string) (map[string]string, error)
}

// ErrUnknownIP is returned when the source IP doesn't appear in any of
// our service's task network attachments. The middleware treats this as
// transient (the peer might have just joined and not yet propagated
// through raft) and returns a 5xx so forward.Forwarder retries.
var ErrUnknownIP = errors.New("source IP not found in any peer task")

// SwarmResolver maps an overlay-network IP to a node hostname by listing
// running tasks of our own service via the Docker socket and matching
// against their network attachments. Results are TTL-cached, and
// concurrent refreshes are single-flighted so a burst of cache misses
// fires exactly one TaskList+NodeList round-trip rather than N.
//
// The TTL trades freshness for socket load. Task IPs are stable for the
// lifetime of a container, so the only churn is task creation/destruction
// (replica replaced after node reboot, service scaled). A 5-second TTL
// means a worker that just respawned may be rejected for up to one TTL,
// covered by the middleware returning 5xx (retryable by the forwarder).
// InvalGen note: invalGen is incremented on each Invalidate. An
// in-flight refresh that observed an older value discards its RPC
// result rather than writing it back into the cache — see refresh().
type SwarmResolver struct {
	Docker      TaskLookup
	cachedAt    time.Time
	cache       map[string]string
	ServiceName string
	mu          sync.Mutex    // Protects cache, cachedAt, gen, invalGen, missGen.
	refreshMu   sync.Mutex    // Serializes refresh RPCs (single-flight).
	gen         uint64        // Incremented on each successful refresh.
	invalGen    uint64        // See note on the SwarmResolver type doc.
	missGen     uint64        // gen of the last miss-refresh that still didn't find the IP.
	TTL         time.Duration // Default 5s.
}

// Invalidate marks the cache stale so the next NodeForOverlayIP call
// forces a fresh refresh. Used by the middleware on header/IP mismatch:
// libnetwork can recycle overlay IPs within the TTL window, so a stale
// positive (wrong node attribution) is possible even when the cache
// looks fresh.
//
// Bumps invalGen too, so any concurrent refresh whose RPC started
// before this Invalidate will discard its result rather than overwrite
// our zeroed cachedAt — without that guard, the in-flight refresh's
// fresh-cache write would race-clobber the invalidation and allow a
// spoofed-identity request to slip through on the second pass.
func (r *SwarmResolver) Invalidate() {
	r.mu.Lock()
	r.cachedAt = time.Time{}
	r.invalGen++
	r.mu.Unlock()
}

// NodeForOverlayIP returns the swarm node hostname for the given overlay IP,
// consulting the cache then refreshing on miss.
func (r *SwarmResolver) NodeForOverlayIP(ctx context.Context, ip string) (string, error) {
	cache, snapGen, missDone, fresh := r.snapshotCache()
	if fresh {
		if h, ok := cache[ip]; ok {
			return h, nil
		}
		// Fresh cache but IP missing. Refresh once to pick up a peer that
		// joined since the last refresh — but only if we haven't ALREADY
		// miss-refreshed against this cache generation. Without that guard a
		// burst of unknown-IP requests (a spoofed/recycled source, or a flood
		// from one bad peer) would each force a full TaskList+NodeList,
		// defeating the TTL cache. A genuinely-new peer is still picked up at
		// the next TTL refresh, within the contract documented on the type.
		if missDone {
			return "", ErrUnknownIP
		}
	}
	refreshed, err := r.refresh(ctx, snapGen)
	if err != nil {
		return "", fmt.Errorf("tasks by service: %w", err)
	}
	if h, ok := refreshed[ip]; ok {
		return h, nil
	}
	r.recordMiss()
	return "", ErrUnknownIP
}

// recordMiss marks the current cache generation as having been refreshed in
// response to a cache miss that still didn't find the IP, so subsequent misses
// against the same generation skip the redundant RPC.
func (r *SwarmResolver) recordMiss() {
	r.mu.Lock()
	r.missGen = r.gen
	r.mu.Unlock()
}

func (r *SwarmResolver) ttl() time.Duration {
	if r.TTL == 0 {
		return defaultResolverTTL
	}
	return r.TTL
}

// snapshotCache returns (cache, generation, missDone, fresh). The generation
// lets refresh detect that a concurrent caller has already advanced the cache
// past the observed state, avoiding a redundant RPC. missDone reports whether
// the current generation has already been miss-refreshed, so a repeat miss can
// skip the RPC.
func (r *SwarmResolver) snapshotCache() (cache map[string]string, gen uint64, missDone, fresh bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fresh = r.cache != nil && time.Since(r.cachedAt) < r.ttl()
	missDone = r.gen != 0 && r.missGen == r.gen
	return r.cache, r.gen, missDone, fresh
}

// refresh fetches a fresh task map. Concurrent callers single-flight on
// refreshMu: the first caller does the RPC; subsequent callers wait then
// skip the RPC if the cache generation has advanced past their snapshot.
// This bounds docker-socket load under a burst of cache misses
// (drain-time fan-in) to one in-flight refresh at a time.
//
// Race-with-Invalidate: if Invalidate fires while our RPC is in flight,
// our result is potentially stale wrt whatever event prompted the
// invalidation (e.g. overlay-IP recycle). Returning the result to our
// own caller is fine — they can verify the mapping against fresh data
// downstream — but writing it back into the cache would let the NEXT
// caller see our stale data despite the Invalidate. Detect this via
// invalGen: capture it before the RPC and skip the cache write if it
// has advanced.
func (r *SwarmResolver) refresh(ctx context.Context, snapGen uint64) (map[string]string, error) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	// Re-check generation: a peer may have already refreshed while we waited.
	r.mu.Lock()
	startInvalGen := r.invalGen
	if r.gen != snapGen && r.cache != nil && time.Since(r.cachedAt) < r.ttl() {
		c := r.cache
		r.mu.Unlock()
		return c, nil
	}
	r.mu.Unlock()

	fresh, err := r.Docker.TasksByServiceWithIPs(ctx, r.ServiceName)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.invalGen == startInvalGen {
		r.cache, r.cachedAt, r.gen = fresh, time.Now(), r.gen+1
	}
	r.mu.Unlock()
	return fresh, nil
}
