# Leader-side reactivation after a coordinated reboot

**Date:** 2026-10-16
**Status:** Implemented (unreleased)
**Amends:** [`2026-05-25-coordinator-design.md`](2026-05-25-coordinator-design.md) §3.6, §3.7, §5, §6, §7

## 1. Problem

Draining a node makes Swarm stop **every** task on it, global-service tasks
included. The coordinator replica on a drained node is therefore gone for the
whole drain → reboot → resume cycle. The v1.0.0 resume flow (`POST /active`,
`DELETE /lock` over the local socket) cannot work after the reboot:

- the local socket has no replica behind it, on workers and managers alike;
- a worker cannot run `docker node update` anyway (manager-only API).

The node stays drained and the lock stays held until `LOCK_TTL_SECONDS`, after
which a peer steals the lock and the node stays drained for good.

## 2. Design

The replica on the raft **leader** reactivates the lock holder and releases the
lock once the holder has rebooted and is back. The host-side resume step only
waits for that.

### 2.1 The drain mark (lock Config labels)

`POST /drain` from the lock holder records, before it drains, that this is a
coordinator drain for a reboot. The mark is stored as labels on the lock's
Docker Config:

| Label | Value |
|---|---|
| `swarm-auto-drain.drained-node-id` | Swarm node ID of the holder |
| `swarm-auto-drain.drained-at` | Unix seconds, when the drain started |
| `swarm-auto-drain.down-seen-at` | Unix seconds, first time the leader saw the node down (absent until then) |
| `swarm-auto-drain.activated-at` | Unix seconds, when the leader set it active (absent until then) |

Why labels on the lock Config:

- **Survives leader failover.** It is raft state, like the lock itself.
  In-memory state on the leader would be lost exactly when the holder is the
  leader (it reboots, a new leader takes over).
- **Lives and dies with the lock instance.** A released or stolen lock takes
  its mark with it, so a stale mark can never point the leader at a node that
  no longer holds the lock. A node label would outlive the lock and need its
  own cleanup.
- **Updatable with a version check.** Config data is immutable and is the
  HTTP-visible lock document; labels are the only part Swarm lets us change,
  and the update carries the object version, so concurrent writers fail
  instead of overwriting each other.
- **Lock JSON unchanged.** `POST /lock` bodies keep their v1.0.0 shape.

The mark names the node by **ID** as well as by the lock's holder hostname.
The leader only acts when the node with that ID still has the holder's
hostname, so a node re-joined under the same hostname (new ID) is never
mistaken for the drained one.

If the caller of `/drain` does not hold the lock, the drain proceeds unmarked
(v1.0.0 behaviour) and the node is never reactivated automatically. If the
mark cannot be written for any other reason, `/drain` fails with `500` and
does not drain: a drained holder without a mark would stay drained after its
reboot.

### 2.2 The reactivation loop

Every replica runs the loop; each tick (`REACTIVATE_INTERVAL_SECONDS`, default
5) it re-checks that it is a manager and that its node is the raft leader, and
does nothing otherwise. On the leader:

```
no lock, or lock without mark            → idle (manual drains are never touched)
mark's node ID unknown to the swarm      → node_gone (lock kept until TTL)
node ID's hostname ≠ lock holder         → holder_mismatch (never act)
activated-at set                         → release the lock instance
down-seen-at unset:
    node down / disconnected / unreachable → record down-seen-at
    otherwise                              → awaiting_down
down-seen-at set:
    node not Ready                         → awaiting_return
    node Ready                             → activate (drain→active only),
                                             record activated-at, release
```

Every step is idempotent and its progress is in the labels, so a leader change
at any point resumes from the last recorded step:

- **Activate** is a compare-and-set: it sets `active` only while the node is
  `drain`, using the node object's version. If an operator changed the
  availability in the meantime (e.g. `pause`), it is left alone and the loop
  still releases the lock.
- **Release** removes the lock Config by **ID**, i.e. exactly the instance the
  loop read. A lock released and re-acquired in between has a new ID and is
  untouched (`lock_gone`).
- Order is activate → record → release. A crash after activating but before
  recording repeats the (now no-op) activation; a crash after recording
  repeats only the release.

Each step logs at info and increments
`swarm_auto_drain_reactivate_steps_total{step=down_seen|activate|release,outcome=ok|noop|error}`.

### 2.3 The "rebooted and back" signal

The leader reactivates only after it has **observed the node not Ready** after
the mark was written, then **Ready again**. Evidence of "went down":

- node state `disconnected` — the agent's session to the leader's dispatcher
  ended. A clean reboot stops dockerd, which ends the session immediately;
- node state `down` — no heartbeat for the dispatcher's grace period
  (5s heartbeat × 3);
- manager reachability `unreachable` — raft lost the manager.

`unknown` is deliberately **not** evidence: after any leader election every
node is `unknown` until it heartbeats, rebooted or not.

The first sighting is persisted (`down-seen-at`), so a leader change between
"down" and "back" does not lose it.

Alternatives rejected:

- *Engine restart / boot ID in node description* — Swarm exposes neither.
- *Node `UpdatedAt` / version moved since the drain* — any label or spec
  change by an operator, or certificate renewal, would look like a reboot.
- *Docker event stream (`state.old`/`state.new`)* — catches sub-poll-interval
  transitions, but events are lost across leader failover and add a stream
  to supervise. A possible follow-up if fast reboots are ever missed.
- *Time since drain* — would reactivate a node whose reboot never happened.

### 2.4 Failure modes

| Situation | Result |
|---|---|
| Node never comes back | Stays drained, lock held (`awaiting_return`) until `LOCK_TTL_SECONDS`; a peer then steals the lock, the new lock has no mark, the dead node stays drained. |
| Reboot shorter than one poll interval and shorter than the heartbeat grace, so no non-Ready state is ever observed | Not reactivated (`awaiting_down`). Same end state as "never comes back"; the host's resume times out and exits 1. A real reboot (firmware, kernel, dockerd start, TLS session) is well above 5s, and `disconnected` is written the moment dockerd stops, so this is not expected in practice. |
| Reboot of the leader itself, back before the new leader's 15s grace for `unknown` nodes expires | Not observed via node state; raft `unreachable` usually is. Otherwise as above. |
| Drain timed out (`408`) or `systemctl reboot` failed after a successful drain | Node stays drained with its mark, as v1.0.0 specifies for a timeout. If it later goes down and comes back (an operator reboots it, or a network partition long enough to mark it `down`), the leader reactivates it and releases the lock. |
| Transient agent disconnect of a drained holder that has not rebooted yet | Counts as "went down". The host reboots right after a successful drain, so the window is seconds; the consequence is an early reactivation of a node that is about to reboot anyway. |
| Leader node is drained (no replica on it) | Nobody reconciles. A reboot that needs reactivation waits until leadership moves to a manager running the coordinator, or the TTL. Keep the leader schedulable, or keep the coordinator running on it. |
| Operator drains a node by hand | No mark, never touched. |
| Operator changes availability while the holder is rebooting | Activation is a no-op (only drain→active), the lock is released. |
| Version conflict on a label write | The tick fails, is logged, and is retried next tick. |

## 3. Host side

`swarm-auto-drain resume` (boot-time oneshot, gated by the resume sentinel):

1. Recreates the socket directory (`/var/run` is a tmpfs; Swarm refuses to
   start a task whose bind-mount source is missing).
2. Waits until the local socket answers `/healthz`, up to
   `RESUME_TIMEOUT_SECONDS` (default 900). A replica answering proves the node
   is Active: Swarm schedules the global coordinator task only on an Active
   node. This works the same on workers and managers.
3. Best-effort `DELETE /lock` (normally `404`: the leader released it). Its
   outcome does not decide the exit code.
4. Removes the resume sentinel, exits 0.

On timeout it exits 1 and keeps the sentinel.

It no longer calls `POST /active`.

## 4. Forwarding and shutdown timeouts

- A worker's `/drain` is forwarded to a manager, which holds it for up to
  `DRAIN_TIMEOUT_SECONDS` + 30s. The forwarder's timeout is now
  `max(FORWARD_TIMEOUT_SECONDS, DRAIN_TIMEOUT_SECONDS + 30s)`, so it no longer
  gives up (and retries a second `/drain`) while the manager is still
  draining. One HTTP client serves all forwarded paths, so a hung manager now
  also holds other forwarded calls that long; that only delays a failure.
- The drained node's own replica is stopped by the drain while it may still
  be relaying or handling that node's `/drain`. The rendered stack sets
  `stop_grace_period: 330s` (the in-process shutdown grace,
  `DRAIN_TIMEOUT_SECONDS` + 30s) so the answer reaches the host instead of
  being cut off by Docker's 10s default. Adjust it with `DRAIN_TIMEOUT_SECONDS`.

## 5. HTTP contract

No status code or body changes.

- `POST /drain`: new side effect (the drain mark) when the caller holds the
  lock; new `500` when the mark cannot be written (nothing drained then).
- `POST /active`, `DELETE /lock`: unchanged. A host-side `POST /active` after a
  reboot is unnecessary; it only works once the node is already active.
- After a successful `/drain`, the caller's local socket goes away (its
  replica is stopped). Consumers must not plan further calls on that node
  until it is active again — including on the `408` path.

## 6. Configuration

| Variable | Default | Where | Meaning |
|---|---|---|---|
| `REACTIVATE_INTERVAL_SECONDS` | `5` | daemon | Leader's reactivation poll interval. Keep well below a reboot's down time. |
| `RESUME_TIMEOUT_SECONDS` | `900` | host (`resume`) | How long resume waits for the node to be active again. |
