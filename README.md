# swarm-auto-drain

**Kured-style safe automated reboots for Docker Swarm.**

> **v2 in progress.** The coordinator moves from a global swarm service to a
> host service on the managers, reached over the tailnet. Releases ship static
> binaries with a SHA-256 file, an SBOM and build-provenance attestations; no
> container image or `install.sh` is published. The Quickstart below describes
> v1 and goes away with v2.

A small Go daemon that coordinates rolling node reboots in a Docker Swarm cluster. Deployed as one global service, it ensures at most one node drains and reboots at a time and refuses to drain a manager that would break raft quorum.

This works similar to [kured](https://kured.dev) in Kubernetes.

## Quickstart

Two commands. Both idempotent.

**1. On every host** — installs the binary, the systemd timer + units, and the `swarm-coord` group:

```bash
curl -sSL https://github.com/matchory/swarm-auto-drain/releases/download/v1.0.0/install.sh \
  | sudo VERSION=v1.0.0 UNIX_SOCKET_GID=9999 bash
```

The bootstrap verifies a SHA-256 against the matching `.sha256` file
published with the release before placing anything under `/usr/local/sbin/`.

**2. On any manager** — renders the stack from the binary's embedded template and deploys it:

```bash
swarm-auto-drain stack | docker stack deploy -c - swarm-auto-drain
```

That's it. `docker service ps swarm-auto-drain_swarm-auto-drain` should show one task running per node, and `journalctl -u docker | grep swarm-auto-drain` should show `overlay encryption verified` and `listening` lines per replica.

Per-host policy lives in `/etc/default/swarm-auto-drain` (reboot window,
blackout file, kill switch). The defaults are documented in
[`docs/consumers/README.md`](docs/consumers/README.md). The trigger timer
fires every 15 min with a 5 min randomized delay to keep clusters from
synchronizing.

## How it works

- **Global Swarm service**, one replica per node, same image on managers and workers. At startup each replica reads `docker info` and decides whether to handle requests in-process (manager) or forward to a peer manager over the overlay network (worker). `docker node promote`/`demote` takes effect on the next request — no restart needed.
- **Two listeners per replica.** A Unix socket on the host for the safe-reboot script; a TCP listener on the encrypted overlay for worker → manager forwarding. Each has its own trust model — see [Security](#security).
- **Cluster-wide mutex** lives in a Docker Config (`swarm-auto-drain-lock`). `docker config create` is atomic on the name — create-fails-on-exists is the CAS primitive. Stale locks (older than `LOCK_TTL_SECONDS`) are reclaimed race-safely.
- **Manager quorum guard.** A manager refuses to drain itself unless `floor(N/2) + 1` other managers remain reachable.
- **Caller identity** is the node hostname. Loopback / Unix socket → the replica's own `NODE_HOSTNAME`. Overlay → derived server-side from the source IP by looking up the swarm task list. The `X-Origin-Node` header is advisory and rejected on mismatch.

## Security

Trust boundaries, top to bottom:

| Hop | Mechanism | What it stops |
|---|---|---|
| Safe-reboot script → coordinator | Unix socket, mode `0660`, group `swarm-coord` | Any local user who isn't in the group can't reach the API. |
| Worker replica → manager replica | Encrypted Swarm overlay network (`driver_opts.encrypted: "true"`) | Sniffing inter-host coordinator traffic. |
| Identity within the cluster | IP-derived from swarm task list, server-side | A compromised peer can't impersonate another node via headers. |
| Replica → Docker daemon | Standard Unix-socket permissions on `/var/run/docker.sock` | — |

Fail-closed defaults — startup aborts if any attached overlay network isn't encrypted, or if the swarm service-name self-discovery doesn't return a value. Override with `ALLOW_UNENCRYPTED_OVERLAY=true` for dev/test only (loud warning in the log).

## Configuration

All knobs are env vars. The reference stack file sets the ones operators normally touch; the rest have defaults that should not need changing.

| Variable | Default | What it does |
|---|---|---|
| `NODE_HOSTNAME` | — (required) | The current node's hostname. Set via Swarm template `{{.Node.Hostname}}`. |
| `UNIX_SOCKET_GID` | `0` (root) | Numeric GID that owns the Unix socket. Set this to the `swarm-coord` group's GID. |
| `UNIX_SOCKET_PATH` | `/var/run/swarm-auto-drain/coord.sock` | Where the API socket appears on the host. |
| `UNIX_SOCKET_MODE` | `0660` | Permission mask for the socket (`0660` = rw for owner + group). |
| `LISTEN_ADDR` | `0.0.0.0:7777` | TCP listener for overlay traffic between replicas. |
| `LOCK_TTL_SECONDS` | `1800` | How long the cluster reboot lock can be held before a peer reclaims it as stale. |
| `DRAIN_TIMEOUT_SECONDS` | `300` | Max time to wait for tasks to drain off a node. |
| `ROLE_RECHECK_SECONDS` | `30` | How often to re-check manager/worker role from the local Docker daemon. |
| `FORWARD_TIMEOUT_SECONDS` | `60` | Per-attempt timeout for worker → manager forwarding. |
| `MANAGER_DISCOVERY_TTL_SECONDS` | `60` | How long a worker caches the manager-IP it forwards to. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |
| `LOCK_CONFIG_NAME` | `swarm-auto-drain-lock` | The Docker Config name used as the cluster mutex. |
| `ALLOW_UNENCRYPTED_OVERLAY` | `false` | Escape hatch — only for dev. Surfaces a loud warning instead of aborting. |

## HTTP API

All endpoints are served on both listeners; the host script talks to the Unix socket, peer replicas talk to the overlay TCP listener.

| Method | Path | What it does |
|---|---|---|
| `POST` | `/lock` | Acquire the cluster reboot lock. `201` on acquire, `410` on stale-steal, `409` if held by someone else. |
| `DELETE` | `/lock` | Release the lock. `204` if you held it, `403` if a peer took over, `404` if no lock. |
| `POST` | `/drain` | Drain the caller's node. `200` on success, `408` on timeout, `503` if quorum guard would be violated. |
| `POST` | `/active` | Set the caller's node back to availability=active. |
| `GET` | `/healthz` | Liveness — always `200`. |
| `GET` | `/role` | `{"role":"manager"|"worker","leader":"<host>"}`. |
| `GET` | `/managers/health` | `{"reachable":N,"total":M,"leader":"<host>"}` — diagnostics. |

The full HTTP contract (status codes, JSON shapes, identity rules) lives in [`docs/specs/2026-05-25-coordinator-design.md`](docs/specs/2026-05-25-coordinator-design.md).

## Metrics

The coordinator exposes OpenTelemetry metrics when configured. Two transports, both opt-in (default off):

- **Prometheus pull**: set `METRICS_LISTEN_ADDR=:9100` and scrape `:9100/metrics`.
- **OTLP push**: set `OTEL_EXPORTER_OTLP_ENDPOINT=host:port` (host:port, no scheme). The standard `OTEL_EXPORTER_OTLP_*` env vars are honoured.

The stack file ships both knobs commented out. See [`docs/specs/2026-05-27-metrics-design.md`](docs/specs/2026-05-27-metrics-design.md) for the full metric catalogue.

## Status

Stable. The HTTP contract, env var names, on-disk install layout, and subcommand surface are covered by semver as of v1.0.0 — see [`CHANGELOG.md`](CHANGELOG.md) for the compatibility statement. Specs under `docs/specs/`, design history in `docs/plans/`. Originally extracted from internal Matchory Puppet infrastructure for the `tier: docker_swarm` case in `updates::reboot`.

## Hacking on the project

```bash
go build ./cmd/swarm-auto-drain   # binary
go test -race ./...               # unit tests
docker build -t swarm-auto-drain:dev .   # container image

# Integration fixture: spins up a 3-manager dind swarm to validate
# Docker SDK assumptions (NOT the binary end-to-end).
docker compose -f tests/integration/docker-compose.yml up -d
bash tests/integration/tests.sh
docker compose -f tests/integration/docker-compose.yml down -v
```

`CLAUDE.md` documents the architecture for contributors — read it before non-trivial changes, especially around the lock store, identity middleware, and role refresh.

## License

MIT.
