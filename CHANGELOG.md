# Changelog

All notable changes to swarm-auto-drain are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Stability guarantees for the HTTP contract, env var names, and on-disk paths are
spelled out under the [Compatibility](#compatibility) section below.

## [Unreleased]

### Fixed

- **Nodes now come back into service after a coordinated reboot.** Draining a
  node stops every task on it, the coordinator's replica included, so after the
  reboot nothing on the node could undo the drain (and workers have no manager
  API). The replica on the raft leader now does it: `POST /drain` from the lock
  holder records a drain mark (node ID, timestamps) as labels on the lock
  Config; the leader waits until that node has been seen `down`, `disconnected`
  or raft-`unreachable` and is `ready` again, sets it `active` (only from
  `drain`) and releases exactly that lock instance. Progress is kept in the
  labels, so a leader change mid-way continues where it stopped. Manually
  drained nodes and nodes that never return are never reactivated. See
  `docs/specs/2026-10-16-leader-reactivation.md`.
- **A worker's forwarded `/drain` is no longer cut off** by the 60s forward
  timeout: the forwarder now waits at least `DRAIN_TIMEOUT_SECONDS` + 30s.
- **The rendered stack sets `stop_grace_period: 330s`** so a node's own replica,
  stopped by that node's drain, can still return the `/drain` answer.

### Changed

- **`swarm-auto-drain resume` no longer calls `POST /active`.** It recreates the
  socket directory, waits up to `RESUME_TIMEOUT_SECONDS` (default 900) for the
  local replica to answer (which happens only once the node is active), makes a
  best-effort `DELETE /lock`, and clears the sentinel. On timeout it exits 1 and
  keeps the sentinel. Upgrade the daemon (stack) before the host binary: an old
  daemon never reactivates the node, so the new resume times out.
- **`POST /drain`** returns `500` without draining if the caller holds the lock
  and the drain mark cannot be written. Status codes and bodies are otherwise
  unchanged; a caller that does not hold the lock is drained unmarked, as before.

### Added

- `REACTIVATE_INTERVAL_SECONDS` (daemon, default 5) and `RESUME_TIMEOUT_SECONDS`
  (host, default 900).
- Metric `swarm_auto_drain_reactivate_steps_total{step,outcome}`;
  `lock_release_total` gains `outcome="reactivated"`.

## [1.0.0] — 2026-05-28

First stable release. The HTTP contract, env var names, install layout, and the
`stack` / `install` / `trigger` subcommands are now covered by semver.

### Added

- **Cluster coordinator.** Single Go binary deployed as a Swarm global service.
  One replica per node; the same image runs on managers and workers and decides
  per-request which mode to operate in, so `docker node promote` / `demote`
  takes effect without restarting the service.
- **Cluster reboot mutex** backed by a Docker Config (`swarm-auto-drain-lock`).
  `docker config create` is atomic on the name and acts as the CAS primitive.
  Stale holders are reclaimed by peers after `LOCK_TTL_SECONDS`, with the
  steal+recreate race shrunk by re-inspecting the document before deletion and
  verifying ownership after the recreate.
- **Quorum-aware drain.** A manager refuses to drain unless
  `floor(N/2) + 1` *other* managers remain reachable. Drain polls until task
  count hits zero or `DRAIN_TIMEOUT_SECONDS` elapses; SIGTERM grace is sized
  to let an in-flight drain run to completion.
- **Worker → manager forwarding** over the encrypted Swarm overlay. Hop-by-hop
  headers are stripped, the manager cache is invalidated on every failed
  attempt, and retries cover the propagation window when a peer task has just
  joined and isn't yet in the manager's task list.
- **IP-derived identity.** Caller hostname is resolved server-side from the
  TCP source IP via the swarm task list; the `X-Origin-Node` header is
  advisory only and a mismatch fails the request. Loopback callers use the
  replica's own `NODE_HOSTNAME`.
- **Host-side surface.** `swarm-auto-drain install` provisions the
  `swarm-coord` group, the Unix socket directory, `/etc/default/swarm-auto-drain`,
  and the three systemd units (`trigger.timer`, `trigger.service`,
  `resume.service`). `swarm-auto-drain trigger` is the timer's entrypoint and
  speaks to the local replica over the Unix socket.
- **Stack rendering.** `swarm-auto-drain stack` prints a deploy-ready Compose
  file pinned to the binary's own version, with the Unix-socket bind, the
  encrypted overlay, and the `{{.Node.Hostname}}` template injection wired up.
- **OpenTelemetry metrics**, opt-in. Either Prometheus pull
  (`METRICS_LISTEN_ADDR`) or OTLP push (`OTEL_EXPORTER_OTLP_ENDPOINT`). Covers
  lock acquire/release/steal/hold, drain duration and `tasks_remaining`, role
  refreshes and transitions, forwarder attempts and cache invalidations,
  identity resolution outcomes, and HTTP route latency on a bounded allowlist.
- **Fail-closed startup.** The replica refuses to start if any attached
  overlay isn't encrypted, or if swarm service-name self-discovery returns
  empty. `ALLOW_UNENCRYPTED_OVERLAY=true` is the documented dev/test escape
  hatch and emits a loud warning.
- **Curl-bashable install script** with checksum verification against the
  release's `.sha256` companion file.

### Documentation

- HTTP contract spec at `docs/specs/2026-05-25-coordinator-design.md` (status
  codes, JSON shapes, identity rules — asserted by the handlers test suite).
- Metrics catalogue at `docs/specs/2026-05-27-metrics-design.md`.
- Host-side policy reference at `docs/specs/2026-05-26-daemon-side-policy.md`.
- Operator install guide at `docs/guide.md`.
- Architecture notes for contributors at `CLAUDE.md`.

## Compatibility

Starting with v1.0.0, the following surfaces follow semver:

- **HTTP contract** — paths, methods, status codes, and JSON shapes documented
  in `docs/specs/2026-05-25-coordinator-design.md`. Breaking changes bump the
  major version.
- **Env var names and semantics** documented in the README's Configuration
  table. New vars may be added in minor releases with safe defaults; removals
  or semantic changes bump the major version.
- **Subcommand surface** (`stack`, `install`, `trigger`) and their flags.
- **On-disk layout** written by `install`: `/usr/local/sbin/swarm-auto-drain`,
  `/etc/default/swarm-auto-drain`, the three systemd units, the
  `swarm-coord` group, and the Unix-socket directory.

Not covered by semver: internal Go package layout under `internal/`, log
message wording, metric attribute values that aren't part of the metric's
documented identity, and the contents of `docs/plans/` (historical artifacts).

[Unreleased]: https://github.com/matchory/swarm-auto-drain/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/matchory/swarm-auto-drain/releases/tag/v1.0.0
