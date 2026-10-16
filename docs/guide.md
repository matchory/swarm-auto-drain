# Operator install guide

Two commands to deploy a cluster, both idempotent.

## On every node

```bash
curl -sSL https://github.com/matchory/swarm-auto-drain/releases/download/v1.0.0/install.sh \
  | sudo VERSION=v1.0.0 UNIX_SOCKET_GID=9999 bash
```

The bootstrap script downloads the matching `swarm-auto-drain-linux-<arch>`
binary, verifies its SHA-256 against the published `.sha256` file, installs
it under `/usr/local/sbin/`, and then runs `swarm-auto-drain install --gid
$UNIX_SOCKET_GID`. The install subcommand:

- Creates the `swarm-coord` group with the GID you pinned (idempotent —
  errors only if the group already exists with a different GID).
- Drops `/etc/default/swarm-auto-drain` with the policy defaults
  documented at the bottom of this page. Preserved across upgrades
  unless you re-run with `--force`.
- Installs three systemd units under `/etc/systemd/system/`:
  - `swarm-auto-drain-trigger.timer` — fires every 15 min (with 5 min
    randomized delay so cluster nodes don't synchronize).
  - `swarm-auto-drain-trigger.service` — the timer's `ExecStart`.
  - `swarm-auto-drain-resume.service` — gated by
    `ConditionPathExists=/var/lib/swarm-auto-drain/draining`, runs at boot
    after a coordinated reboot.
- `systemctl daemon-reload`, enables + starts the timer, enables the
  resume service.

Re-running the curl-bash on an already-installed host is safe — same
binary, same units, no-op on the env file.

## On any manager

```bash
swarm-auto-drain stack | docker stack deploy -c - swarm-auto-drain
```

`stack` renders the embedded template to stdout, pinned to the binary's
own version. Re-run after upgrading the binary to refresh the image tag.

Flags (rarely needed): `--version`, `--gid`, `--image-registry`,
`--service-name`, `--allow-unencrypted` (dev only).

## Per-host policy — `/etc/default/swarm-auto-drain`

The trigger timer loads this file before each invocation. The defaults
match the embedded template the install subcommand writes:

| Variable | Default | What it does |
|---|---|---|
| `COORDINATOR_SOCKET` | `/var/run/swarm-auto-drain/coord.sock` | Must match the bind mount in the stack. |
| `SENTINEL_FILE` | `/var/run/reboot-required` | Ubuntu/Debian default. Trigger no-ops if absent. |
| `DISABLED_FILE` | `/etc/swarm-auto-drain/disabled` | Kill switch — `touch` to disable a host. |
| `RESUME_SENTINEL` | `/var/lib/swarm-auto-drain/draining` | Written before reboot; gates the resume oneshot. |
| `REBOOT_WINDOW` | `Mon-Fri 02:00-06:00 UTC` | Recurring window. Empty = always in-window. |
| `BLACKOUT_FILE` | `/etc/swarm-auto-drain/blackout` | Optional date-range blocklist. |
| `LOCK_TIMEOUT_SECONDS` | `60` | Per-call HTTP timeout for `/lock`. |
| `DRAIN_TIMEOUT_SECONDS` | `600` | Per-call HTTP timeout for `/drain`. |
| `RESUME_TIMEOUT_SECONDS` | `900` | How long `resume` waits after boot for the node to be active again. |

### Reboot window

```
REBOOT_WINDOW="Mon-Fri 02:00-06:00 UTC"
```

- Days: case-insensitive English short names; either `Mon-Fri` (range)
  or `Sat,Sun` (comma-list).
- Times: 24-hour `HH:MM-HH:MM`, UTC only (parse error otherwise).
- No cross-midnight ranges. Split into two same-day windows on either
  side of midnight if you need overnight coverage.
- Empty value = always in-window.

### Blackout file

```
# /etc/swarm-auto-drain/blackout
2026-12-20:2027-01-05
2027-07-04
```

Each line is a single date or an inclusive `FROM:TO` range, UTC. `#`
starts a comment line. Missing file = no blackouts active.

### Kill switch

Create `/etc/swarm-auto-drain/disabled` (or whatever `DISABLED_FILE`
points at) to declare a host "operator-managed" without disabling the
systemd unit. The trigger exits 0 with `reason=disabled` and the timer
keeps firing harmlessly.

## Uninstall

```
sudo swarm-auto-drain uninstall
```

Stops + disables the units, removes the unit files, daemon-reloads. The
env file, binary, `swarm-coord` group, and `/var/lib/swarm-auto-drain/`
state are deliberately not removed — the summary prints what's left so
the operator can decide. After `uninstall`, re-running `install`
produces a clean working state.

## Trust boundaries

- **Host → coordinator:** Unix-domain socket at
  `/var/run/swarm-auto-drain/coord.sock`, mode `0660`, group
  `swarm-coord`. Only members of `swarm-coord` AND running as UID 0 can
  call the API (the daemon checks `SO_PEERCRED` at accept time). No
  bearer tokens, no TCP loopback.
- **Worker replica → manager replica:** Swarm overlay network created
  with `driver_opts.encrypted: "true"`. The coordinator refuses to start
  if any attached overlay isn't encrypted (override:
  `ALLOW_UNENCRYPTED_OVERLAY=true` — dev-only).
- **Caller identity:** within the swarm boundary, the manager-side
  middleware derives the caller's node hostname from its overlay source
  IP (looked up in the swarm task list). The `X-Origin-Node` header is
  advisory — a mismatch with the IP-derived value fails the request.
  Host-side (over the Unix socket) identity is the replica's own
  `NODE_HOSTNAME`, set at deploy time via the swarm template
  `{{.Node.Hostname}}`.

## Resume sentinel

`/var/lib/swarm-auto-drain/draining` exists between drain (just before
`systemctl reboot`) and successful resume. It gates the resume systemd
unit via `ConditionPathExists` so non-coordinated boots (kernel panic,
power loss, manual `reboot`) do not run the resume logic. This prevents
the resume service from firing on every boot and prevents clobbering an
operator's deliberate `availability=drain` setting.

Resume does not reactivate the node itself: the drained node has no
coordinator replica (Swarm stops every task on a drained node) and a worker
has no manager API. The leader's replica sets the node active and releases
the lock once it has seen the node go down and come back. Resume waits until
the local socket answers again (the replica is rescheduled only on an active
node), then removes the sentinel. If that does not happen within
`RESUME_TIMEOUT_SECONDS` it exits 1, keeps the sentinel, and the node stays
drained for the operator. See
[`specs/2026-10-16-leader-reactivation.md`](specs/2026-10-16-leader-reactivation.md).
