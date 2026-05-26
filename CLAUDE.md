# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build the binary (output: ./swarm-auto-drain, gitignored)
go build ./cmd/swarm-auto-drain

# Full test suite (CI runs with -race)
go test -race ./...

# Single package / single test
go test ./internal/lock
go test ./internal/lock -run TestConfigStore_Acquire_StaleSteal -v

# Vet (CI runs this before tests)
go vet ./...

# Container image (multi-stage, distroless static, nonroot)
docker build -t swarm-auto-drain:dev .

# Integration fixture (3-manager dind swarm — validates Docker SDK assumptions,
# not the binary end-to-end; see tests/integration/README.md).
docker compose -f tests/integration/docker-compose.yml up -d
bash tests/integration/tests.sh
docker compose -f tests/integration/docker-compose.yml down -v
```

Go version: **1.25** (per `go.mod` and CI). Sole non-stdlib runtime dep is `github.com/docker/docker` (the moby SDK).

## Architecture

The coordinator is **one Go binary**, deployed as **one Docker Swarm service in global mode** (one replica per node). The same image runs on managers and workers; behavior is decided at runtime.

### Request flow

```
host script ──127.0.0.1:7777──> local replica
                                  │
                          role == manager? ──yes──> handle in-process
                                  │
                                  no
                                  │
                                  ▼
                         forward over overlay net to tasks.<service-name>:7777
                         (service name self-discovered at startup;
                          injects X-Origin-Node — advisory, verified
                          against source IP on the manager side)
                                  │
                                  ▼
                         manager replica handles
```

`cmd/swarm-auto-drain/main.go` wires this: a single `http.HandlerFunc` checks `role.Role()` on every request and dispatches to either `managerHandler` (identity middleware + handler tree) or `workerHandler` (local-only paths + forwarder for the rest). Re-dispatching per-request — rather than per-process — is deliberate: `docker node promote`/`demote` takes effect on the next inbound request, no restart required.

Role detection is two-tiered. `internal/docker.RoleDetector` refreshes the cached role on a ticker (default 30s) — that's the steady-state mechanism. On top of that, `dockerpkg.WithRoleRefresh` wraps the Docker client passed to handlers so that any "not a swarm manager" error from a manager-only API call triggers a **synchronous** role refresh before the error bubbles back. A manager that's just been demoted thus self-corrects within the same failed request: the caller's retry (e.g. `forward.Forwarder.Retries`) sees the updated role on the next dispatch and gets routed to the worker handler, which forwards to a peer manager. The detector itself is constructed with the raw, unwrapped client so its periodic `Info` call can't recurse through the wrapper.

### Trust model

Read this before changing anything auth-related: **there is no auth code, by design.**

- Host → local replica: `127.0.0.1` bind (root-on-host trust). The stack file publishes port 7777 with `host_ip: 127.0.0.1`.
- Worker → manager replica: the swarm internal overlay network (`internal: true`). Attaching to it requires the swarm join token.
- Replica → docker socket: standard UNIX permissions.

Caller identity (the node hostname) is resolved by `internal/identity`:

- Loopback → the replica's own `NODE_HOSTNAME` env var.
- Non-loopback → derived **server-side** from the source IP by `identity.SwarmResolver`, which calls `TasksByServiceWithIPs(serviceName)` on the docker socket and maps source-IP → task → node hostname. The forwarder still injects `X-Origin-Node` but it's **advisory only**: a mismatch with the IP-derived value fails the request with 400. The header cannot be used to impersonate another node.

The resolver is TTL-cached (5s default) to keep socket load bounded under drain-time bursts; on a cache miss it refreshes exactly once before returning `ErrUnknownIP`. The worker forwarder's `Retries` covers the propagation window for a peer task that just joined and isn't yet in the manager's task list.

`/healthz` and `/role` remain public (no identity required) so workers can probe themselves locally. The resolver is only attached to the manager-side middleware — worker mode never sees non-loopback callers (the overlay traffic is forwarded *out*, not in).

The service name passed to the resolver and to `forward.ManagerFinder` is **self-discovered** at startup via `dcli.SelfServiceName`: the binary reads its own container ID from `/proc/self/cgroup`, calls `ContainerInspect`, and reads `com.docker.swarm.service.name` (a label the swarm orchestrator stamps on every task container). Consumers may name the service whatever they like in their stack file; the coordinator adapts. Discovery failing at startup (e.g. not running inside a swarm task) is fatal — we refuse to run rather than silently accept spoofed headers.

### The lock (`internal/lock`)

The cluster-wide reboot mutex lives in a single Docker Config named `swarm-auto-drain-lock`. `docker config create` is atomic on the name — that's the CAS primitive (Acquired / EEXIST / steal stale path). Two non-obvious bits:

1. **Stale-steal race shrinking** (`config_store.go` Acquire step 5): before deleting an expired holder's config, re-inspect to confirm the lock document is still the *same* document we read. If a peer already stole and recreated in the gap, deleting would wipe the peer's fresh lock and both nodes would believe they own the cluster mutex. Don't simplify this.
2. **Error classification** uses `errdefs.IsNotFound` / `errdefs.IsConflict` first, with narrow substring fallbacks for non-canonical wrappers and test fakes. Don't broaden the substrings.

### Drain (`internal/drain`)

`/drain` first runs `QuorumOK`: a manager refuses to drain unless `floor(N/2)+1` *other* managers remain reachable. Only then sets availability to `drain` and polls until task count hits zero or `DrainTimeout` elapses. The HTTP handler wraps the drain call in `DrainTimeout + 30s` so the operation can run to completion rather than being cut off by an upstream client deadline; the server's shutdown grace is sized the same way (`DrainTimeout + 30s`) so SIGTERM doesn't truncate an in-flight drain.

### Forwarding (`internal/forward`)

`Forwarder` proxies worker requests to a manager. It strips RFC 7230 hop-by-hop headers, injects `X-Origin-Node` (advisory — the manager verifies it against the source IP), retries on connection failure or 5xx, and invalidates the cached manager URL on every failed attempt (via `OnAttemptFailure → ManagerFinder.Invalidate`) so retries don't pile onto a dead replica. `ManagerFinder` does DNS lookup on `tasks.<service-name>` (service name self-discovered at startup), probes `/role` on each returned IP, and caches the first one reporting `"manager"`.

### Package boundaries

Note the intentional avoidance of import cycles:

- `internal/lock` defines a local `docker` interface (the subset of `internal/docker.Client` it needs) rather than importing `internal/docker`.
- `internal/drain` defines `drain.NodeSummary` and a local docker interface; `cmd/swarm-auto-drain` defines a tiny `dockerAdapter` that converts `internal/docker.NodeSummary` → `drain.NodeSummary`.

When adding a new package that needs Docker client access, follow the same pattern: declare the minimal interface in the consumer package.

## Configuration

All config is env vars, loaded by `internal/config`. `NODE_HOSTNAME` is **required** and is injected via the swarm template `{{.Node.Hostname}}` in the stack file — there's no fallback to `os.Hostname()` because inside a container that would be the container ID, not the node name. Defaults for everything else are in `config.go`; the deployed stack also sets explicit values for `LOCK_TTL_SECONDS`, `DRAIN_TIMEOUT_SECONDS`, `ROLE_RECHECK_SECONDS`, `LOG_LEVEL`.

## Specs

The design spec at `docs/specs/2026-05-25-coordinator-design.md` is the source of truth for HTTP contract semantics (status codes, JSON shapes, identity rules). When changing handler behavior, cross-check against the spec — the handlers test suite (`internal/api/handlers_test.go`) asserts spec-level contracts, not just implementation details.
