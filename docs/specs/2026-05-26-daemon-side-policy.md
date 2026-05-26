# Daemon-side enforcement of cluster reboot policy

**Date:** 2026-05-26
**Status:** Draft — to be expanded
**Origin:** Discussion in [`docs/plans/2026-05-26-subcommand-host-binary.md`](../plans/2026-05-26-subcommand-host-binary.md), §4 locked decisions ("Daemon-side enforcement…" row).

## Idea

The host-side `trigger` binary checks the maintenance window and blackout dates before calling `/drain`. This is correct as a UX optimization — don't bother the daemon if policy says no — but it leaves the daemon trusting the caller. A buggy or rogue client that bypasses the trigger can drain at any time.

Move the maintenance window and blackout-date logic into the daemon as a second layer. The daemon refuses `/drain` outside the configured window or during a blackout, regardless of who called and how. Defense in depth: policy is enforced on both sides, daemon is the authoritative source.

## Scope

- **Maintenance window: cluster-wide only.** Set via env vars on the swarm service (one source of truth). No per-host overrides — a node that drains outside the global window is a policy violation, full stop.
- **Blackout dates: cluster-wide list**, same model — set at deploy time, no per-host overrides.
- **Rate limit on `/drain`**: refuse more than one drain per host per N minutes (default 5). Catches runaway-loop abuse without blocking legitimate retries that survive the timeout.
- **Manual escape hatch**: operators who need to drain outside the window in an emergency redeploy the stack with an empty window. No special "force" header — emergencies are rare and a redeploy makes the override auditable in the swarm service history.
- **Host-side checks become redundant**, and we drop them: with daemon-side enforcement, the trigger binary stops caring about windows/blackouts entirely and just calls `/drain` whenever the sentinel says so. Single source of truth, less code, no client-server policy drift.

## Open questions

- Window/blackout config: env vars on the service (simple, requires service update to change) or runtime-mutable via API? Env vars are simpler and align with the rest of the daemon's config model; defer the runtime-mutable case until there's a clear need.
- Rate-limit storage: in-memory per-replica (cheap, loses state on restart), or persisted in the Docker Config alongside the lock (durable, but a swarm-wide write on every drain)? Lean toward in-memory — restart resets the cooldown, which is acceptable.
- Should the `GET /role` response advertise the configured window so a client could short-circuit if it wanted to? Probably yes — strictly informational, doesn't change the daemon's authoritative role.
