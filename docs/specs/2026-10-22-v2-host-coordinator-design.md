# swarm-auto-drain v2 — host coordinator

**Status:** Approved 2026-10-22. Supersedes the deployment model, trust model
and HTTP contract of `2026-05-25-coordinator-design.md` and
`2026-05-26-daemon-side-policy.md`.
**Consumer design:** matchory/platform `docs/superpowers/specs/2026-10-16-fleet-auto-reboot-design.md` §6.5.

## Why v2

v1 ran as a global swarm service. A drained node runs no tasks, global ones
included, so draining a node stopped that node's own coordinator replica in
the middle of its `/drain` request, and after the reboot nothing answered
`/active`. v1 also allowed exactly one node down per cluster, which does not
scale to a 20–50 node swarm in a fixed nightly window.

## Shape

- **One binary, one subcommand that matters (`run`)**, run by systemd on every
  manager, outside the swarm. Draining a node never stops it.
- **Every manager can answer.** All state is in the swarm's raft store (Docker
  Configs, node availability), so callers try the managers in turn.
- **Callers reach it over the tailnet.** It listens on one address
  (`LISTEN_ADDR`, the manager's tailnet IP). Workers cannot reach any manager
  API themselves, which is why the coordinator exists.
- **The reboot policy is not here.** Windows, gates and the reboot itself
  belong to the caller (matchory/platform `safe_reboot`). The coordinator only
  hands out slots, drains, and reactivates.

## Identity

Every request except `/healthz` is identified by its source address:

1. Ask the local tailscaled (`LocalAPI /localapi/v0/whois?addr=<remote>`) who
   the peer is. Unknown address → 403.
2. The peer must carry at least one tag from `ALLOWED_TAGS` (e.g.
   `tag:swarm-pipeline`). Otherwise → 403.
3. Its MagicDNS name (trailing dot removed), or failing that its computed
   short name, must equal the hostname of a node in this swarm. Otherwise →
   403. Two swarm nodes with that hostname → 403: hostnames are not unique in
   a swarm, and acting on one of them would be a guess.

The matched node is the caller. A caller acts on its own node only; no
request names a node. `tailscale serve` is not used: it passes no identity for
tagged callers.

## Slots

- Workers share `WORKER_SLOTS` slots, Docker Configs named
  `<SLOT_PREFIX>-1` … `<SLOT_PREFIX>-N`. Managers share one,
  `<SLOT_PREFIX>-manager`. `SLOT_PREFIX` defaults to `reboot-slot`.
- Taking a slot creates the first free name. `docker config create` fails if
  the name exists, so two managers never hand out the same slot.
- The Config's data is `{"holder":"<node>","acquired_at":<unix>}`; its labels
  repeat the holder for `docker config ls`.
- **No expiry.** A slot is freed by `/release` from its holder, or by an
  operator (`docker config rm`). A node that never comes back keeps its slot,
  so after `WORKER_SLOTS` such failures no further worker drains.
- One process serialises its own acquires, so a caller retrying against the
  same manager cannot take two slots. Two managers handling the same caller at
  the same instant could; callers try managers one after another.

## HTTP API

All responses are JSON. `503` with `{"error":"not a manager"}` means this
node is not (or no longer) a swarm manager; the caller tries the next one.

### `POST /acquire`

Take a slot for the caller, then drain its node and wait until no task
remains.

| Status | Meaning |
|---|---|
| 200 | Drained; the caller holds a slot. Body `{"slot","tasks_remaining":0,"elapsed_seconds"}`. Calling again while holding a slot drains again and returns 200. |
| 409 | No free slot of the caller's kind, body `{"error","holders":[…]}`. Or this node already has an `/acquire` or `/release` running on this manager. |
| 503 | A manager caller would break quorum (`floor(M/2)+1` other managers must be reachable). Body `{"error","report"}`. Or: not a manager. |
| 408 | Tasks still running after `DRAIN_TIMEOUT_SECONDS`. The coordinator has set the node active again and freed the slot. |
| 500 | Docker error. The coordinator has tried to set the node active and free the slot. |

The rollback behind 408 and 500 runs even when the caller has already hung
up.
| 403 | Caller not identified (see Identity). |

The quorum check runs only when the caller does not already hold a slot.

### `POST /release`

If the caller holds a slot: set its node active, then free the slot. A
request overlapping another one from the same node answers 409. If it
holds none: change nothing, so a node an operator drained by hand stays
drained. Both return 200 with `{"released":true|false}`. If reactivation
fails the slot is kept and the response is 500.

### `GET /status`

`{"node","availability","slot"}`, `slot` being `null` when none is held.

### `GET /healthz`

200 `ok`. No identity.

## Configuration (environment)

| Variable | Default | |
|---|---|---|
| `LISTEN_ADDR` | — (required) | e.g. `100.104.205.4:7777` |
| `ALLOWED_TAGS` | — (required) | comma-separated Tailscale tags |
| `WORKER_SLOTS` | `1` | |
| `SLOT_PREFIX` | `reboot-slot` | |
| `DRAIN_TIMEOUT_SECONDS` | `300` | |
| `DOCKER_SOCKET` | `/var/run/docker.sock` | |
| `TAILSCALE_SOCKET` | `/var/run/tailscale/tailscaled.sock` | |
| `LOG_LEVEL` | `info` | |
| `METRICS_LISTEN_ADDR`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `METRICS_PUSH_INTERVAL_SECONDS` | off | unchanged from v1 |

## Removed from v1

The global-service mode, worker→manager forwarding, overlay encryption check,
role detector, Unix socket and SO_PEERCRED trust, the TTL lock and stale
steal, the `trigger`, `resume`, `install`, `uninstall` and `stack`
subcommands, and the daemon-side policy (windows, blackout files). The
caller's host scripts and systemd units live with the caller.
