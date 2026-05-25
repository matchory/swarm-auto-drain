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

## License

MIT.
