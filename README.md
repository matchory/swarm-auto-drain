# swarm-auto-drain

**Rolling reboots for Docker Swarm: reboot slots, a manager quorum guard, and drain.**

A small Go daemon that runs on every swarm manager. A node about to reboot asks
it, over the tailnet, for a reboot slot; the coordinator drains the node and
waits until its tasks have moved. After the reboot the node asks again to be
set active and give its slot back. At most `WORKER_SLOTS` workers and one
manager are out at a time, and a manager is only drained if quorum survives
without it.

The coordinator decides nothing about *when* to reboot. Windows, gates and
the reboot itself belong to the caller (at Matchory: Puppet's `safe_reboot`).

## How it works

- **Host service on the managers**, outside the swarm, so draining a node never
  stops its own coordinator. Every manager can answer: the state lives in the
  swarm's raft store.
- **Slots are Docker Configs** (`reboot-slot-1` … `reboot-slot-N`,
  `reboot-slot-manager`). `docker config create` fails if the name exists,
  which makes taking a slot atomic across managers. Slots never expire: a node
  that does not come back keeps its slot, which stops the roll once every slot
  is held that way. `docker config ls` shows who holds what.
- **Caller identity comes from the tailnet.** The coordinator listens on the
  manager's tailnet address and asks the local tailscaled who the peer is. The
  peer must carry an allowed tag and be a node of this swarm. A caller can only
  act on its own node.

The full contract is
[`docs/specs/2026-10-22-v2-host-coordinator-design.md`](docs/specs/2026-10-22-v2-host-coordinator-design.md).

## HTTP API

| Method | Path | |
|---|---|---|
| `POST` | `/acquire` | Take a slot, drain, wait for the tasks. `200` drained · `409` no free slot · `503` quorum would break, or not a manager · `408` drain timed out (rolled back) · `403` caller not identified |
| `POST` | `/release` | If holding a slot: set the node active, free the slot. `200 {"released":true|false}` |
| `GET` | `/status` | `{"node","availability","slot"}` |
| `GET` | `/healthz` | `200 ok`, no identity |

## Configuration

| Variable | Default | |
|---|---|---|
| `LISTEN_ADDR` | — (required) | One address, normally the manager's tailnet IP: `100.x.y.z:7777` |
| `ALLOWED_TAGS` | — (required) | Comma-separated Tailscale tags a caller must carry one of |
| `WORKER_SLOTS` | `1` | Workers that may be drained at once |
| `SLOT_PREFIX` | `reboot-slot` | Config name prefix |
| `DRAIN_TIMEOUT_SECONDS` | `300` | |
| `DOCKER_SOCKET` | `/var/run/docker.sock` | |
| `TAILSCALE_SOCKET` | `/var/run/tailscale/tailscaled.sock` | |
| `LOG_LEVEL` | `info` | |
| `METRICS_LISTEN_ADDR` | off | Prometheus pull endpoint, e.g. `127.0.0.1:9100` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | off | OTLP push, `host:port` |

Running it: [`docs/guide.md`](docs/guide.md).

## Releases

Each `v*` tag publishes static `linux/amd64` and `linux/arm64` binaries,
`SHA256SUMS`, an SPDX SBOM, and build-provenance and SBOM attestations:

```bash
gh attestation verify swarm-auto-drain-linux-amd64 --owner matchory
```

## Hacking on the project

```bash
go build ./cmd/swarm-auto-drain
go test -race ./...
# Docker behaviour the coordinator relies on, against a 3-manager dind swarm:
docker compose -f tests/integration/docker-compose.yml up -d
bash tests/integration/tests.sh
docker compose -f tests/integration/docker-compose.yml down -v
```

`CLAUDE.md` describes the architecture for contributors.

## License

MIT.
