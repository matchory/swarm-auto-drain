# swarm-auto-drain

**Kured-style safe automated reboots for Docker Swarm.**

A small Go daemon that coordinates rolling node reboots in a Docker Swarm cluster. Each node runs an instance (global service); a cluster-wide mutex ensures at most one node drains at a time; manager-quorum is preserved across reboots.

Solves a real gap: [kured](https://kured.dev) is the standard tool for this in Kubernetes, but no equivalent exists for Docker Swarm — Swarm workers can't call manager-only APIs directly, so the obvious "per-node daemon with a cluster-wide lock" pattern doesn't trivially translate.

## How it works

- Deployed as a **single global Docker Swarm service**, one replica per node.
- Each replica mounts the local Docker socket. At startup it detects whether it's running on a manager or a worker via `docker info`.
- **Manager replicas** handle requests in-process (they have manager API access).
- **Worker replicas** forward requests to a manager replica via Swarm's internal overlay DNS (`tasks.swarm-auto-drain`), injecting `X-Origin-Node` from the swarm template variable `{{.Node.Hostname}}`.
- Each node's reboot script calls `http://127.0.0.1:7777/lock` → `/drain` → `systemctl reboot`. After boot, a systemd oneshot calls `/active` → `/lock` (DELETE).
- **Lock state** lives in a `docker config` named `swarm-auto-drain-lock`. `docker config create` is atomic — create-fails-on-exists gives the CAS primitive. Stale locks (older than the configured TTL) are stolen.
- **Trust boundaries:** host shell script ↔ local replica = `127.0.0.1` bind (root-on-host trust); worker replica ↔ manager replica = the swarm overlay network (which requires the swarm join token to attach to). No bearer tokens, no mTLS, no per-host identity material required.

## Status

Pre-alpha. Spec under `docs/specs/`, implementation plan under `docs/plans/`.

Originally extracted from internal Matchory Puppet infrastructure for the `tier: docker_swarm` case in `updates::reboot`.

## Development

```bash
# Build the binary
go build ./cmd/swarm-auto-drain

# Run the test suite
go test ./...

# Build the container image
docker build -t swarm-auto-drain:dev .

# Spin up the integration test fixture
docker compose -f tests/integration/docker-compose.yml up -d
bash tests/integration/tests.sh
docker compose -f tests/integration/docker-compose.yml down -v
```

## Deploying

```bash
# Pin a version (or use :latest)
VERSION=v0.1.0 docker stack deploy -c deploy/swarm-auto-drain.stack.yml swarm-auto-drain

# Verify
docker service ls
docker service ps swarm-auto-drain_swarm-auto-drain
```

Pre-requisite on every swarm node: `/var/lib/swarm-auto-drain/` must
exist with mode 0755, owned by root. This is mounted into the
coordinator container as the sentinel-state directory shared with the
host's reboot orchestrator scripts.

See [`docs/consumers/`](docs/consumers/) for reference host-side scripts.

## Contributing

This project is pre-alpha. PRs welcome once the first tagged release
ships.

## License

MIT.
