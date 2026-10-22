# swarm-auto-drain — Coordinator Design Spec

> **Superseded by** `2026-10-22-v2-host-coordinator-design.md` (v2): deployment, trust model and HTTP contract.

**Date:** 2026-05-25
**Status:** Approved for implementation
**Origin:** Extracted from the Matchory Puppet `updates::reboot` Docker Swarm tier discussion.

---

## 1. Problem

Docker Swarm has no equivalent of [kured](https://kured.dev). Hosts running unattended-upgrades or otherwise tracking `/var/run/reboot-required` need a way to reboot under coordination so that:

- At most one swarm node reboots at a time (cluster-wide mutex).
- Tasks drain off the node before reboot (`docker node update --availability drain`), then resume after.
- Manager nodes do not break raft quorum (a manager refuses to drain if fewer than `floor(N/2) + 1` other managers would remain reachable).
- A node that crashed mid-drain doesn't pin the cluster forever — stale locks are reclaimed after a TTL.

The naive "every node runs a daemon with a cluster-wide lock" pattern (kured's model) doesn't translate directly because **Docker Swarm worker nodes cannot call manager-only APIs**. `docker node update`, `docker config create`, `docker node ls` are all manager-only — a worker daemon attempting them gets `This node is not a swarm manager`.

This spec defines a coordinator service that solves that gap.

## 2. Architecture overview

One Docker image, one Swarm service, **global mode** — one replica per node.

```
┌─────────────────────────────── swarm-auto-drain (global service) ──────────────────────────────┐
│                                                                                                │
│  ┌── manager-1 ──────────┐  ┌── manager-2 ──────────┐  ┌── manager-3 ──────────┐               │
│  │ Replica               │  │ Replica               │  │ Replica               │               │
│  │  - role: manager      │  │  - role: manager      │  │  - role: manager      │               │
│  │  - handles in-process │  │  - handles in-process │  │  - handles in-process │               │
│  │  - docker.sock has    │  │  - docker.sock has    │  │  - docker.sock has    │               │
│  │    full mgr API       │  │    full mgr API       │  │    full mgr API       │               │
│  └───────────────────────┘  └───────────────────────┘  └───────────────────────┘               │
│                                       ▲                                                        │
│                                       │ overlay DNS: tasks.swarm-auto-drain                    │
│                                       │ (worker replicas round-robin to managers)              │
│                                       │                                                        │
│  ┌── worker-1 ──────────┐  ┌── worker-2 ──────────┐  ...  ┌── worker-9 ──────────┐             │
│  │ Replica              │  │ Replica              │       │ Replica              │             │
│  │  - role: worker      │  │  - role: worker      │       │  - role: worker      │             │
│  │  - forwards to a     │  │  - forwards to a     │       │  - forwards to a     │             │
│  │    manager replica   │  │    manager replica   │       │    manager replica   │             │
│  │  - injects header    │  │  - injects header    │       │  - injects header    │             │
│  │    X-Origin-Node     │  │    X-Origin-Node     │       │    X-Origin-Node     │             │
│  └──────────────────────┘  └──────────────────────┘       └──────────────────────┘             │
└────────────────────────────────────────────────────────────────────────────────────────────────┘

      ▲                                          ▲
      │ 127.0.0.1:7777                           │ 127.0.0.1:7777
      │                                          │
   ┌──┴──────────────────┐                    ┌──┴──────────────────┐
   │ /usr/local/sbin/    │                    │ /usr/local/sbin/    │
   │   safe-reboot.sh    │                    │   safe-reboot.sh    │
   │   on manager host   │                    │   on worker host    │
   └─────────────────────┘                    └─────────────────────┘
```

### Key design properties

- **Single image, single service.** Promote/demote operations are picked up at runtime via periodic `docker info` re-check.
- **No identity infrastructure.** Trust boundaries are:
  - Host → local replica: `127.0.0.1` bind (root-on-host trust).
  - Worker replica → manager replica: the swarm internal overlay network (attaching to it requires the swarm join token). Caller identity within that boundary is derived from the source IP via the Docker socket — not from a client-controlled header — so a malicious peer cannot impersonate another node by setting `X-Origin-Node`. See §3.3.
  - Replica → Docker socket: standard Docker UNIX socket permissions.
- **Lock primitive:** `docker config create swarm-auto-drain-lock` — atomic CAS (create-fails-on-exists) backed by Swarm raft.
- **Sentinel pattern** mirrors `updates::reboot` ES tier: `/var/lib/swarm-auto-drain/draining` written before reboot, gates the resume systemd unit via `ConditionPathExists` so non-coordinated boots (kernel panic, power loss, manual `reboot`) do not run the resume logic.

## 3. The coordinator service

### 3.1 Runtime role detection

At startup, every replica calls `docker info` via the local socket. The boolean `Swarm.ControlAvailable` (true iff this daemon is a manager AND the control plane is reachable) determines role:

- `true` → manager mode: handle requests in-process.
- `false` → worker mode: forward requests to `tasks.swarm-auto-drain:7777` via the overlay network.

Re-checked every 30s in a background goroutine. In addition, the Docker client used by the handler tree is wrapped with `dockerpkg.WithRoleRefresh`: any manager-only API call (`docker config create/inspect/rm`, `docker node ls/update`, etc.) that returns the daemon's `"This node is not a swarm manager"` error triggers a synchronous role refresh before the error returns. This means a demoted manager flips its cached role within the same failed request — the caller's retry (e.g. `forward.Forwarder.Retries`) then dispatches to the worker handler and forwards to a peer manager rather than failing again locally. Together, the periodic poll bounds the worst-case lag for the *symmetric* case (worker→manager promotion, where the local socket can't observe the change), while the lazy-on-error path effectively eliminates lag for the demote case that would otherwise leak `500`s for up to one poll interval.

This handles `docker node promote` / `docker node demote` without a stack restart.

### 3.2 HTTP API

All endpoints listen on `127.0.0.1:7777` (host-network publishing). JSON request/response bodies. No authentication header required — the trust boundary is the bind address + the overlay network membership of the forwarded hop.

| Method | Path                | Behavior                                                                 |
|--------|---------------------|--------------------------------------------------------------------------|
| GET    | `/healthz`          | 200 always (used by the host-side resume script's wait loop)             |
| GET    | `/role`             | `{"role":"manager"\|"worker","leader":"<hostname>"}` (informational)     |
| POST   | `/lock`             | Acquire the cluster-wide reboot lock for the caller's node               |
| DELETE | `/lock`             | Release the lock (only the current holder may release)                   |
| POST   | `/drain`            | Drain the caller's node (waits until task count is 0 or timeout)         |
| POST   | `/active`           | Set the caller's node back to availability=active                        |
| GET    | `/managers/health`  | `{"reachable":N,"total":M,"leader":"<host>"}` — for diagnostics          |

### 3.3 Caller identity

Identity is the **node hostname** of the original caller. Resolution rules:

- If the request arrived on `127.0.0.1` (loopback from the host script), identity = the replica's own `NODE_HOSTNAME` env var (injected at deploy time via `{{.Node.Hostname}}`).
- If the request arrived from a peer replica over the overlay network, identity is **derived server-side** from the source IP: the manager replica looks up the IP in its own service's task list (`TaskList(service=<self>, desired-state=running)` joined with `NodeList`) and reads the hostname of the node whose task owns that overlay address. The forwarding replica still sets an `X-Origin-Node` header; the manager treats it as advisory and **rejects the request if the header disagrees with the IP-derived value**.

The IP-derived path is fail-closed: if the source IP doesn't match any running task of our service (spoofer, or a peer task that just joined and hasn't propagated through raft yet), the request returns `400`. The worker-side forwarder will retry via `forward.Retries`, which covers the brief propagation window.

The service name used in the lookup is **self-discovered** at startup, not configured. The coordinator reads its own container ID from `/proc/self/cgroup`, calls `ContainerInspect`, and reads the `com.docker.swarm.service.name` label that the swarm orchestrator stamps on every task container. This means consumers may name the service whatever they like in their stack file — the coordinator adapts. The same discovered name is also used to build the overlay DNS name (`tasks.<service-name>`) for worker→manager forwarding.

The coordinator never trusts a body-supplied identity. There is no `/nodes/{name}` URL segment — every endpoint acts on the caller's own node only.

### 3.4 Lock acquisition

```
POST /lock
→ build JSON: {"holder":"<caller>","acquired_at":<unix_ts>,"ttl_seconds":1800}
→ docker config create swarm-auto-drain-lock --label data=<json> -
   • on success (201): return 201 to caller, lock is theirs
   • on EEXIST (409):
       - read existing config: docker config inspect swarm-auto-drain-lock --format '{{ .Spec.Data }}' (base64-decode)
       - parse acquired_at + ttl_seconds
       - if (now - acquired_at) > ttl_seconds → STALE
           → docker config rm swarm-auto-drain-lock
           → retry create from step 1 (race-safe because create is atomic)
           → on success: return 410 (stale-steal performed; caller treats as acquired)
       - else FRESH and not us → return 409 with body {"holder":"<other>","acquired_at":N}
```

Concurrent stale-steal attempts are race-safe: `docker config rm` may succeed for either party, but only one subsequent `create` succeeds (atomic). The other gets EEXIST and bails cleanly.

### 3.5 Lock release

```
DELETE /lock
→ read swarm-auto-drain-lock
   • not found → 404
   • holder != caller → 403 (don't release someone else's lock)
   • holder == caller → docker config rm swarm-auto-drain-lock → 204
```

The "holder != caller" path is critical — if our reboot took weirdly long and a peer stole the lock as stale, we must NOT release on resume. Mirror of the ES tier's `resume-elasticsearch` rule.

### 3.6 Drain

```
POST /drain
→ identity = caller (loopback NODE_HOSTNAME or X-Origin-Node)
→ if caller is a manager:
    docker node ls --filter role=manager --format '{{.Hostname}}\t{{.Status.Reachability}}'
    count REACHABLE managers excluding the caller
    if count < floor(N/2) + 1 (where N = total managers) → return 503 with body explaining
→ docker node update --availability drain <caller>
→ poll docker node ps <caller> --filter desired-state=running -q | wc -l until 0 or timeout (default 300s)
   • on drain success: return 200 {"tasks_remaining":0,"elapsed_seconds":N}
   • on timeout: return 408 {"tasks_remaining":N,"elapsed_seconds":N}
       (caller's reboot script will abort, leaving the node drained but unrebooted;
        operator decides next step. Allocation/availability NOT restored — operator
        must do that manually after diagnosing why tasks didn't drain.)
```

The quorum guard is intentionally placed in the coordinator (single source of truth), not duplicated in the drain shell script.

### 3.7 Active

```
POST /active
→ identity = caller
→ docker node update --availability active <caller>
→ return 200
```

### 3.8 Worker forwarding logic

A worker-mode replica handling an inbound request:

1. Take the caller's identity (always `NODE_HOSTNAME` for loopback; this replica IS the worker).
2. Resolve `tasks.swarm-auto-drain` via DNS → list of all replica overlay IPs.
3. Filter to manager-mode replicas: try `GET /role` on each, pick first that returns `role=manager`. Cache result for the configurable cache window.
4. Forward the original request to the chosen manager, adding header `X-Origin-Node: <NODE_HOSTNAME>`.
5. On connection error or 5xx, drop the cached choice and retry with the next manager.
6. Pass through the response body and status code to the caller.

## 4. Deployment

### 4.1 Docker stack

```yaml
version: "3.9"

services:
  swarm-auto-drain:
    image: ghcr.io/matchory/swarm-auto-drain:${VERSION:-latest}
    deploy:
      mode: global
      restart_policy:
        condition: any
        delay: 10s
      resources:
        limits:
          cpus: "0.5"
          memory: 64M
    environment:
      NODE_HOSTNAME: "{{.Node.Hostname}}"
      LOCK_TTL_SECONDS: "1800"
      DRAIN_TIMEOUT_SECONDS: "300"
      ROLE_RECHECK_SECONDS: "30"
      LOG_LEVEL: "info"
    networks:
      - swarm-auto-drain-net
    ports:
      - target: 7777
        published: 7777
        protocol: tcp
        mode: host             # bind to 127.0.0.1 on each host
    volumes:
      - type: bind
        source: /var/run/docker.sock
        target: /var/run/docker.sock
      - type: bind
        source: /var/lib/swarm-auto-drain
        target: /host/var/lib/swarm-auto-drain

networks:
  swarm-auto-drain-net:
    driver: overlay
    internal: true             # not reachable from outside the swarm
    attachable: false
```

The `published: 7777, mode: host` plus the container listening on `127.0.0.1:7777` (not `0.0.0.0`) means only root on the host can reach the API. The internal overlay net handles replica-to-replica forwarding.

### 4.2 Bootstrap

`docker stack deploy -c swarm-auto-drain.yml swarm-auto-drain` — no secrets to pre-create, no certs to issue, no Configs to seed. Lock Config is created on demand.

### 4.3 Upgrades

Standard `docker stack deploy` redeploy. Global service uses rolling update (one replica at a time) by default. A replica restart on a node currently mid-drain is a 30s blip; the in-flight HTTP request to the host script will see a connection reset, and the host's drain script returns non-zero and exits cleanly (allocation rolled back via the coordinator's release hook if implemented, or the next attempt steals as stale).

## 5. Host-side integration (consumer-side)

This service exposes the API; consumers (host shell scripts, Puppet modules, etc.) call it.

The reference consumer is the Matchory `updates::reboot` Puppet module's `tier: docker_swarm` case, which deploys:

- `/usr/local/sbin/safe-reboot.sh` — common preamble (kill-switch + recency + reboot-required checks), then calls `drain-docker-swarm`.
- `/usr/local/sbin/drain-docker-swarm` — POST /lock → POST /drain → touch sentinel → systemctl reboot.
- `/usr/local/sbin/resume-docker-swarm` — wait for /healthz → POST /active → DELETE /lock → rm sentinel.
- `/etc/systemd/system/resume-docker-swarm.service` — oneshot, `ConditionPathExists=/var/lib/swarm-auto-drain/draining`.

These scripts are NOT part of this project — they live in the consumer (Matchory Puppet). This project ships only the coordinator and a `docs/consumers/` directory with reference shell scripts other adopters can crib.

## 6. Configuration

Env vars consumed by the binary:

| Var                       | Default     | Meaning                                                   |
|---------------------------|-------------|-----------------------------------------------------------|
| `NODE_HOSTNAME`           | (required)  | This node's hostname; set via swarm template at deploy.   |
| `LISTEN_ADDR`             | `127.0.0.1:7777` | HTTP bind address.                                  |
| `DOCKER_SOCKET`           | `/var/run/docker.sock` | Path to local Docker daemon socket.            |
| `LOCK_CONFIG_NAME`        | `swarm-auto-drain-lock` | Name of the Docker Config used as the lock doc.|
| `LOCK_TTL_SECONDS`        | `1800`      | Lock TTL; staler than this is reclaimable.                |
| `DRAIN_TIMEOUT_SECONDS`   | `300`       | Max time to wait for tasks to leave during drain.         |
| `ROLE_RECHECK_SECONDS`    | `30`        | How often to re-evaluate manager vs worker role.          |
| `FORWARD_TIMEOUT_SECONDS` | `60`        | Per-request timeout when worker forwards to manager.      |
| `MANAGER_DISCOVERY_TTL_SECONDS` | `60`  | How long to cache "which manager replica works."          |
| `LOG_LEVEL`               | `info`      | `debug` / `info` / `warn` / `error`.                      |

## 7. Failure modes & invariants

- **Manager rebooting mid-quorum-guard:** the guard is checked once at drain time, then the node leaves. If a second manager dies during the drain window, the swarm loses quorum. Mitigation: the guard requires `floor(N/2) + 1` *other* managers reachable — so for N=3, the guard requires 2 others, leaving zero slack. The operator's mental model: "swarms with 3 managers can lose at most one to maintenance." For more headroom, run 5 managers.

- **Coordinator replica dies during a host's drain script:** the host's `curl --max-time 600 POST /drain` returns a connection error; drain script's ERR trap calls `DELETE /lock` (best-effort) and exits non-zero. The host doesn't reboot. Next week's timer retries. Sentinel was not yet written, so resume doesn't fire on subsequent unrelated boots.

- **Coordinator replica dies between sentinel-write and `systemctl reboot`:** the sentinel is now stale. On next boot, resume runs unconditionally because the sentinel is present — but the lock may not be ours (peer might have stolen it as stale during the unplanned outage window). The DELETE /lock handler returns 403 (not holder); resume script logs that and clears the sentinel.

- **Stale lock from a node that genuinely crashed:** after `LOCK_TTL_SECONDS`, the next acquisition attempt by any node performs the steal. Tunable via env var.

- **Concurrent stale-steal:** `docker config create` is atomic; only one steal wins. Loser gets 409 and bails.

- **Manager promoted while it was a worker replica:** role-recheck loop notices within `ROLE_RECHECK_SECONDS`; replica transitions to handling requests locally. No data loss — in-flight forwarded request continues to old manager.

- **Manager demoted while it was a manager replica:** same recheck loop catches it. New role: forward. Any in-flight request being handled locally completes (the local Docker socket will start returning 403 on manager-only calls; coordinator returns 503 to the caller; caller's drain script retries next week).

- **Two nodes simultaneously fire safe-reboot at exactly the same instant:** lock create is atomic; one wins (201), other gets 409 (held) and exits cleanly.

## 8. Observability

- All HTTP requests logged at info level with: timestamp, method, path, caller identity, response code, latency.
- Docker API calls logged at debug level.
- `/role` and `/managers/health` are read-only and safe to scrape from monitoring.
- Future (not v1): Prometheus `/metrics` endpoint with `swarm_auto_drain_lock_held{holder=}`, `swarm_auto_drain_drain_seconds`, etc.

## 9. Out of scope for v1

- Prometheus metrics endpoint.
- Webhook on lock acquire/release.
- Maintenance windows (`--start-time` / `--end-time` like kured).
- Multi-cluster federation.
- Drain-only mode (no reboot trigger) — the coordinator never reboots hosts; the host script does. So this is naturally already in scope; just need to document the "use the API without the reference shell scripts" path.

## 10. OSS positioning

- **Repo:** `github.com/matchory/swarm-auto-drain`
- **Image:** `ghcr.io/matchory/swarm-auto-drain`
- **License:** MIT
- **Differentiator:** the first kured-equivalent for Docker Swarm; zero identity infrastructure required (no certs, no tokens, no external identity provider); deploy with one `docker stack deploy`.

## 11. Reference

- [kured](https://kured.dev) — the Kubernetes inspiration.
- [morph027 / Ansible rolling Swarm updates](https://morph027.gitlab.io/blog/ansible-rolling-swarm-updates/) — the established alternative (centralized orchestration from a control machine; this project moves it into the swarm itself).
- [Docker Swarm node management docs](https://docs.docker.com/engine/swarm/manage-nodes/).
- [Tecnativa/docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy) — considered as the API-surfacing layer; rejected because host shell scripts can't easily attach to overlay networks.
