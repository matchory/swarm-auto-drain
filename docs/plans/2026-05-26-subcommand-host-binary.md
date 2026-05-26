# Subcommand-dispatched single binary for host + cluster

**Date:** 2026-05-26
**Status:** Approved for implementation
**Prerequisite:** Coordinator daemon as built per [`2026-05-25-coordinator-design.md`](../specs/2026-05-25-coordinator-design.md), including the Unix-socket listener and encrypted-overlay gate added in the May 26 follow-up work.

---

## 1. Problem

The coordinator daemon is solid as a cluster-side API, but the host-side install is currently three shell scripts + a stack file + a one-liner `groupadd`. Compared to kured's "kubectl apply, done" experience that's noisy, and the shell scripts are the hardest piece to maintain — `set -euo pipefail` + `trap ... ERR` is not a great place for policy logic (maintenance windows, blackout dates, sentinel detection, version handshakes).

Swarm structurally forbids the kured approach. Swarm services can't set `privileged: true` or `pid: host` in stack files, so the nsenter-into-PID-1 trick that lets kured trigger `systemctl reboot` from a pod doesn't port. The host *must* execute the reboot. So there will always be some host-side install — the question is how small and how maintainable we can make it.

**Goal:** make `swarm-auto-drain` a single binary with subcommands that handle every operator-facing concern. Same binary in the container, on the host, and as the generator of its own stack file. Two commands to deploy a cluster, both idempotent. Policy logic in Go (testable), not bash.

## 2. Target operator UX

```bash
# every host (idempotent — re-run after upgrades)
curl -sSL https://github.com/matchory/swarm-auto-drain/releases/download/v0.2.0/install.sh \
  | sudo VERSION=v0.2.0 UNIX_SOCKET_GID=9999 bash

# any manager (renders stack from the binary's embedded template)
swarm-auto-drain stack | docker stack deploy -c - swarm-auto-drain
```

That's the entire deploy. Done.

## 3. Subcommand surface

```
swarm-auto-drain run          # daemon (container entrypoint, also default when no args)
swarm-auto-drain trigger      # what the host timer fires
swarm-auto-drain resume       # what the boot oneshot fires
swarm-auto-drain install      # per-host bootstrap (idempotent)
swarm-auto-drain uninstall    # symmetric to install (stops units, removes them)
swarm-auto-drain stack        # render cluster stack file to stdout
swarm-auto-drain version      # prints version + commit, for diagnostics
```

`os.Args[1]` dispatches. No args → `run` (preserves the existing container entrypoint behavior; we update the Dockerfile to be explicit too).

## 4. Locked design decisions

| Question | Decision | Reasoning |
|---|---|---|
| `uninstall` subcommand? | **Include.** | Symmetric to install. Cleanup story for interrupted upgrades. Shares paths constant with install — ~30 LOC. |
| Cross-midnight reboot windows (`22:00-06:00`)? | **Parse error**, with helpful message. | Less rope. Operators wanting overnight reboots set `Mon-Fri 02:00-06:00` and accept early morning. |
| Version handshake on mismatch? | **Warn-only**, not strict. | Strict-by-default would block rolling upgrades (can't update every host binary and the daemon image atomically). |
| Operator-overridable binary path? | **Defer.** Units hardcode `/usr/local/sbin/swarm-auto-drain`. | Adding `--bin-path` is one templating round-trip; ship without and add when there's a real reason. |
| `install` auto-deploys the stack on managers? | **No.** Strict per-host vs per-cluster separation. | Mixing them creates ambiguity about what happened when. `swarm-auto-drain stack \| docker stack deploy` is already a one-liner. |
| Timezone scope for `REBOOT_WINDOW`? | **UTC only** for v1. | Per-replica `TZ` adds confusion for cross-region clusters where different windows would be wanted anyway. Add later if asked. |
| `/drain` HTTP API behavior during a blackout? | **Let through.** | Blackouts gate the autonomous loop only. Manual operator drain via the Unix socket always works. |
| Replicate `docker stack deploy` programmatically? | **No.** Just render to stdout, pipe to docker. | Hand-rolling stack-deploy semantics (update strategies, rollback, config diffing) is reinventing Docker CLI badly. Pipe pattern preserves auditability. |
| Local non-root user reaches `/lock` `/drain`? | **Rejected via `SO_PEERCRED` UID check.** | Kernel-enforced, namespace-safe. Layered on top of the filesystem ACL. Trigger systemd unit already runs as root. |
| Daemon-side enforcement of cluster reboot policy (window, blackouts, rate limit)? | **Deferred** to follow-up — see [`docs/specs/2026-05-26-daemon-side-policy.md`](../specs/2026-05-26-daemon-side-policy.md). | Tier 1 (peer UID gate) ships now; defense-in-depth window enforcement is a separate spec because it touches the per-host vs cluster-wide policy model. |

## 5. Code organization

```
cmd/swarm-auto-drain/
  main.go                                   # subcommand dispatch
  run.go                                    # daemon (current main.go logic moves here)
  trigger.go
  resume.go
  install.go
  uninstall.go
  stack.go
  version.go

internal/policy/
  window.go         window_test.go          # ParseWindow, InWindow(now, w)
  blackout.go       blackout_test.go        # ParseBlackoutFile, InBlackout(now, ranges)
  sentinel.go       sentinel_test.go        # NeedsReboot(path) bool
  decision.go       decision_test.go        # ShouldRebootNow(cfg, now) (bool, reason)

internal/hostclient/
  client.go         client_test.go          # Unix-socket HTTP client wrapping the API

internal/hostinstall/
  install.go        install_test.go         # writes embedded files, runs systemctl
  paths.go                                  # all install paths in one place
  files/
    swarm-auto-drain-trigger.service
    swarm-auto-drain-trigger.timer
    swarm-auto-drain-resume.service
    swarm-auto-drain.env

internal/stackgen/
  render.go         render_test.go          # template + validation
  files/
    stack.yml.tmpl
```

Existing packages (`internal/config`, `internal/lock`, `internal/identity`, `internal/api`, `internal/drain`, etc.) are unchanged — only the `run` subcommand consumes them. The new internal packages are consumed by the *host* subcommands and have no cross-dependencies with the daemon plumbing.

## 6. Policy semantics

These are the operator-facing semantics. Lock them in before implementation.

### 6.1 Reboot window

`REBOOT_WINDOW=Mon-Fri 02:00-06:00 UTC` — recurring weekly window during which automatic reboots are allowed.

- **Days**: English names, case-insensitive (`Mon|Tue|Wed|Thu|Fri|Sat|Sun`). Comma-list (`Sat,Sun`) or range (`Mon-Fri`).
- **Times**: `HH:MM-HH:MM`, 24h, UTC.
- **Timezone token**: required, must be `UTC` in v1 (parse error otherwise to make the constraint explicit).
- **Cross-midnight ranges** (`22:00-06:00`): parse error with message `"window may not cross midnight; use 'Sun 22:00-23:59,Mon 00:00-06:00' or shift to a same-day range"`.
- **Empty / unset**: always in-window (no restriction).

### 6.2 Blackout file

`BLACKOUT_FILE=/etc/swarm-auto-drain/blackout` (default; missing file = no blackouts active).

Format — newline-separated entries, comments OK:
```
# Holidays
2026-12-20:2027-01-05
2027-07-04
2027-12-24:2027-12-26
```

Each non-comment line is either `YYYY-MM-DD` (single day, UTC) or `YYYY-MM-DD:YYYY-MM-DD` (inclusive range, UTC).

Re-read on every `trigger` invocation. No cache — trigger fires every 15 min, the file IO is irrelevant.

### 6.3 Sentinel file

`SENTINEL_FILE=/var/run/reboot-required` (default).

Just `os.Stat`. Exists → continue. Missing → exit 0 with reason `"no reboot required"`.

Default is Ubuntu/Debian's `update-notifier-common` convention. RHEL/Fedora operators wire `dnf needs-restarting -r` into a small shell wrapper that touches a file, point `SENTINEL_FILE` at that file. Flatcar's `update_engine_client` flips a similar flag.

### 6.4 Kill switch

`DISABLED_FILE=/etc/swarm-auto-drain/disabled` (default; missing file = normal operation).

Checked first. Exists → exit 0 immediately with reason `"disabled"`, before any other check. Operator's "leave this host alone" knob — useful for declaring a host operator-managed without disabling the unit.

### 6.5 Decision order in `trigger`

1. Kill switch (`disabled` file)
2. Sentinel (`reboot-required` file)
3. Window (current time in window?)
4. Blackout (current date in any blackout range?)
5. Lock acquire → drain → reboot

Each step that exits early logs a structured `reason=` field. Telemetry-friendly without us having to design telemetry.

## 7. Trigger flow

```
load /etc/default/swarm-auto-drain
if kill_switch_exists           → exit 0 reason=disabled
if !sentinel_exists             → exit 0 reason=no-reboot-required
if !in_window(now, window)      → exit 0 reason=outside-window
if in_blackout(now, ranges)     → exit 0 reason=in-blackout

dial UNIX_SOCKET → fail = exit 1
GET /role → if version major-skew vs own version, log warn

POST /lock
  201|410 → continue
  409     → exit 0 reason=lock-held-by-peer
  others  → exit 1 reason=lock-failed

defer best_effort_release_lock_on_error()

POST /drain
  200 → continue
  408 → release, exit 1 reason=drain-timeout
  503 → release, exit 0 reason=quorum-guard
  others → release, exit 1 reason=drain-failed

write RESUME_SENTINEL
exec systemctl reboot
```

Identical logic to the current `drain-docker-swarm.sh`, just in Go with proper error handling and unit tests.

## 8. Resume flow

```
load env file
wait up to 5min for /healthz on UNIX_SOCKET
POST /active → 200 required
DELETE /lock
  204 → log released
  403|404 → log peer-took-over (don't fail; this is the correct end state)
  others → exit 1
rm RESUME_SENTINEL
```

Replaces `resume-docker-swarm.sh` line-for-line.

## 9. Install flow

```
require euid == 0 unless --root specified (tests)
load embed.FS

ensure group swarm-coord exists with gid == --gid
  exists with same gid     → skip
  exists with different gid → error (refuse silent uid surprises)
  not present              → groupadd -r -g $gid swarm-coord

write $root/etc/default/swarm-auto-drain
  exists and !--force → skip (preserve operator customizations)
  else                → render template, write

write systemd units (always overwrite — version-bound to binary):
  $root/etc/systemd/system/swarm-auto-drain-trigger.service
  $root/etc/systemd/system/swarm-auto-drain-trigger.timer
  $root/etc/systemd/system/swarm-auto-drain-resume.service

systemctl daemon-reload
systemctl enable --now swarm-auto-drain-trigger.timer
systemctl enable swarm-auto-drain-resume.service   # no --now; ConditionPathExists gates it

print summary:
  - socket path, group + gid
  - "next: deploy the stack with: swarm-auto-drain stack | docker stack deploy -c - swarm-auto-drain"
```

**Flags:**
- `--gid <n>` (required) — numeric GID for the `swarm-coord` group
- `--root <path>` — prefix all install paths (for tests; defaults to `/`)
- `--force` — overwrite the env file even if it already exists
- `--user <name>` — if set, also `usermod -aG swarm-coord $name`

**Idempotency contract:** running `install` twice with the same flags produces byte-identical filesystem state on the second run.

## 10. Uninstall flow

```
require euid == 0 unless --root specified

systemctl stop swarm-auto-drain-trigger.timer  || true
systemctl stop swarm-auto-drain-resume.service || true
systemctl disable swarm-auto-drain-trigger.timer  || true
systemctl disable swarm-auto-drain-resume.service || true

rm -f systemd unit files
systemctl daemon-reload

print summary listing operator state NOT removed:
  - /etc/default/swarm-auto-drain    (env file)
  - /usr/local/sbin/swarm-auto-drain (binary)
  - group swarm-coord                (may be in use)
  - /var/lib/swarm-auto-drain/       (resume sentinel state)
  → "remove manually if a clean slate is desired"
```

Tolerant of partial state — every `systemctl` call has `|| true` semantics (errors logged but not fatal). After uninstall, `install` should produce a clean working state again.

## 11. Stack render flow

```
parse flags: --version, --gid, --image-registry, --service-name, --allow-unencrypted
load embedded stack.yml.tmpl
render with the values
validate:
  - if generated stack has unencrypted overlay and !--allow-unencrypted → error
  - if --service-name not DNS-safe → error
print to stdout
```

**Flags:**
- `--version <vX.Y.Z>` — image tag (default: the binary's own version)
- `--gid <n>` — UNIX_SOCKET_GID env var in the rendered service
- `--image-registry <prefix>` — default `ghcr.io/matchory`
- `--service-name <name>` — stack/service name (default `swarm-auto-drain`)
- `--allow-unencrypted` — generate a stack without `encrypted: "true"` (dev only)

Default invocation with zero flags produces a working stack file pinned to the binary's own version. Tests assert that the generated stack passes the runtime's own startup checks (encrypted overlay, valid service name).

## 12. Env file template

`/etc/default/swarm-auto-drain`, written by `install` from an embedded template:

```bash
# Coordinator Unix socket — must match the bind mount in the stack file.
COORDINATOR_SOCKET=/var/run/swarm-auto-drain/coord.sock

# Path checked to determine whether the host needs a reboot. Ubuntu/Debian
# default. RHEL/Fedora: wrap `dnf needs-restarting -r` in a script that
# touches a file and point this at that file. Flatcar: use the update_engine
# notification path.
SENTINEL_FILE=/var/run/reboot-required

# Kill switch — if this file exists, never reboot this host.
DISABLED_FILE=/etc/swarm-auto-drain/disabled

# Resume sentinel — written before reboot, gates the resume oneshot via
# ConditionPathExists.
RESUME_SENTINEL=/var/lib/swarm-auto-drain/draining

# Reboot window. Days are Mon|Tue|...|Sun (English, case-insensitive).
# Times are 24h HH:MM in UTC. Empty = always in window.
REBOOT_WINDOW="Mon-Fri 02:00-06:00 UTC"

# Optional blackout file. Format documented in
# docs/specs/2026-05-26-subcommand-host-binary.md §6.2.
BLACKOUT_FILE=/etc/swarm-auto-drain/blackout

# Per-call HTTP timeouts (seconds).
LOCK_TIMEOUT_SECONDS=60
DRAIN_TIMEOUT_SECONDS=600
```

`install --force` overwrites; default `install` preserves the operator's customizations across upgrades.

## 13. Embedded systemd units

`swarm-auto-drain-trigger.service`:
```
[Unit]
Description=swarm-auto-drain reboot trigger
After=docker.service network-online.target
Wants=docker.service network-online.target

[Service]
Type=oneshot
EnvironmentFile=/etc/default/swarm-auto-drain
ExecStart=/usr/local/sbin/swarm-auto-drain trigger
```

`swarm-auto-drain-trigger.timer`:
```
[Unit]
Description=Periodically check whether this swarm node needs a coordinated reboot

[Timer]
OnBootSec=10min
OnUnitActiveSec=15min
RandomizedDelaySec=5min

[Install]
WantedBy=timers.target
```

`RandomizedDelaySec=5min` spreads the cluster's check times — 50+ nodes firing at exactly `:00 :15 :30 :45` would create useless coordinator load spikes.

`swarm-auto-drain-resume.service`:
```
[Unit]
Description=swarm-auto-drain resume after coordinated reboot
ConditionPathExists=/var/lib/swarm-auto-drain/draining
After=docker.service network-online.target
Wants=docker.service network-online.target

[Service]
Type=oneshot
EnvironmentFile=/etc/default/swarm-auto-drain
ExecStart=/usr/local/sbin/swarm-auto-drain resume
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
```

## 14. Version handshake

Extend the `/role` response (defined in `internal/api/handlers.go`) from:
```json
{"role":"manager", "leader":"host1"}
```
to:
```json
{"role":"manager", "leader":"host1", "version":"v0.2.0"}
```

The `version` variable already exists at `cmd/swarm-auto-drain/main.go:24`; just plumb it through. Additive change — existing clients ignore unknown fields, so this doesn't break the existing handler tests.

`trigger` and `resume` call `GET /role` early, parse the returned version as semver, and on **major-version mismatch** log a structured warning:
```
level=WARN msg="daemon version skew" daemon=v0.3.0 host=v0.2.0 hint="upgrade host binary"
```

Continue running. Refusing would prevent rolling upgrades (you can't update every host binary and the daemon image at the exact same moment). A `--strict-version` flag for paranoid operators can be added later if asked.

## 15. Peer-credential gate on the Unix socket

The Unix-socket listener accepts any process that can open the socket file. Filesystem ACL (mode 0660, group `swarm-coord`) is the access boundary today. We tighten it further using Linux `SO_PEERCRED`:

- On each accepted connection, the daemon calls `unix.GetsockoptUcred(fd, SOL_SOCKET, SO_PEERCRED)` to read the connecting peer's kernel-reported `{Pid, Uid, Gid}`.
- Connections with `Uid != 0` are rejected with a 403 before any handler runs.
- The peer `Pid` is stamped into the request context and emitted on every structured log line for the request (`peer_pid=12345`). Operators with auditd or systemd-journald correlation can chain back to the originating service. `Uid` and `Gid` are not logged because the UID-0 requirement makes them tautological.

What this closes: a process that's landed in the `swarm-coord` group but isn't root cannot drain. The kernel surfaces the real UID at accept time; no header, no client signal, no spoof.

What it doesn't close: anyone already root on the host. Irreducible — root owns everything by definition.

The trigger systemd unit's `[Service]` block has no `User=` directive, so `ExecStart` runs as root. No change required there. The check lives in the existing `identity.HostTrust` path on the Unix listener; the overlay TCP listener is unaffected (peer creds don't apply to TCP).

**Namespace caveat:** `SO_PEERCRED` returns the UID in the kernel's view, which survives namespace boundaries between the container daemon and host clients. PID semantics are namespace-dependent — the value is meaningful in the *peer's* namespace, not necessarily in the daemon's. We log it without resolving it; operators correlate manually with their host-side audit trail.

**Tests:** `internal/identity/peercred_test.go` against a real `net.Listen("unix", ...)` listener in `t.TempDir()`. Verifies the UID check rejects non-zero, and that the PID is propagated into the request context.

## 16. Distribution

**GitHub Releases**: a workflow under `.github/workflows/release.yml` triggered on `v*` tags. Builds static binaries with `CGO_ENABLED=0`:
- `swarm-auto-drain-linux-amd64`
- `swarm-auto-drain-linux-arm64`
- `swarm-auto-drain-linux-amd64.sha256`
- `swarm-auto-drain-linux-arm64.sha256`
- `install.sh`

**`install.sh`** (curl-bashable):
```bash
#!/usr/bin/env bash
set -euo pipefail
VERSION="${VERSION:-latest}"
GID="${UNIX_SOCKET_GID:?UNIX_SOCKET_GID is required}"
ARCH="$(dpkg --print-architecture 2>/dev/null || uname -m)"
case "$ARCH" in
  amd64|x86_64)   ARCH=amd64 ;;
  arm64|aarch64)  ARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac
URL="https://github.com/matchory/swarm-auto-drain/releases/download/${VERSION}/swarm-auto-drain-linux-${ARCH}"
SHA="https://github.com/matchory/swarm-auto-drain/releases/download/${VERSION}/swarm-auto-drain-linux-${ARCH}.sha256"
curl -fsSL -o /tmp/sad.tmp "$URL"
expected=$(curl -fsSL "$SHA" | awk '{print $1}')
actual=$(sha256sum /tmp/sad.tmp | awk '{print $1}')
[ "$expected" = "$actual" ] || { echo "checksum mismatch" >&2; exit 1; }
install -m 0755 -o root -g root /tmp/sad.tmp /usr/local/sbin/swarm-auto-drain
rm -f /tmp/sad.tmp
/usr/local/sbin/swarm-auto-drain install --gid "$GID"
```

Checksum verification is non-negotiable for a curl-bash. The Dockerfile reuses the same binary (built once per release, then both released to GH and copied into the image).

## 17. Migration from shell scripts

The branch is pre-alpha. No real users to break. Plan:

- Delete `docs/consumers/drain-docker-swarm.sh` and `docs/consumers/resume-docker-swarm.sh`.
- Delete `docs/consumers/stack.yml` — the embedded template in `internal/stackgen/files/stack.yml.tmpl` becomes the single source of truth. `swarm-auto-drain stack` prints it.
- Rewrite `docs/consumers/README.md` to point at the install.sh + `swarm-auto-drain stack` flow.
- Rewrite the root README Quickstart to match.

The systemd unit examples in the old README move into the binary as embedded files. No lossy migration — operators who copied the shell scripts to their own infra still have them, and the API the scripts call against is unchanged.

## 18. Testing strategy

Each layer gets its own kind of test:

| Layer | Test style |
|---|---|
| Policy (`window`, `blackout`, `sentinel`, `decision`) | Pure unit tests with table-driven cases. No filesystem touching except where explicitly testing file checks (then `t.TempDir()`). |
| Host client | Unit tests against `net.Listen("unix", ...)` in `t.TempDir()` + canned `httptest.Server` handlers. Uses the same short-path helper from `internal/api/server_test.go`. |
| `trigger` and `resume` flow | Integration tests: spin up the in-process daemon `http.Handler` (no Docker dependency — use the fake `Drainer` and fake `Store` from the existing `internal/api` tests) bound to a Unix socket, invoke the trigger/resume entry function directly, assert the right HTTP requests went out, and assert that the systemctl-exec function (stubbed via a package-level var) was called. |
| `install` and `uninstall` | Unit tests with `--root $(t.TempDir())`. Assert exact file checksums after run-twice (idempotency). `systemctl` calls are stubbed via the same exec-var pattern. |
| `stack` render | Unit tests: render with default flags, parse the resulting YAML, assert key invariants (encrypted overlay present, service name DNS-safe, image tag matches expected). Cross-validate by feeding the rendered stack through the runtime's own `CheckOverlayEncryption` + `ValidateServiceName` to catch drift. |
| Version handshake | Unit test: fake `/role` returning a major-skew version, assert the structured warning is logged. |

The `systemctl` and `exec.Command` invocations all route through a `runner` package var so tests can substitute a recording fake. Standard pattern.

No new integration-test infrastructure needed; the existing `tests/integration/` dind setup is for daemon-side validation and stays as-is.

## 19. Sequencing — six PRs

Each PR is reviewable in under ~300 LOC excluding tests/embedded files. PRs are mergeable independently (each compiles and passes tests on its own) but build on each other; later PRs assume earlier ones are landed.

### PR 1 — subcommand dispatch + `run.go` refactor + Unix-socket peer-cred gate

**Scope:**
- Add `cmd/swarm-auto-drain/main.go` that dispatches on `os.Args[1]`.
- Move existing `run()` function and helpers into `cmd/swarm-auto-drain/run.go`. Pure refactor, no behavior change for the daemon.
- Add `cmd/swarm-auto-drain/version.go` (trivial).
- Update Dockerfile `CMD` from `[/swarm-auto-drain]` to `[/swarm-auto-drain, run]`.
- Add `internal/identity/peercred.go` — `SO_PEERCRED` reader that returns `{Pid, Uid, Gid}` for a `*net.UnixConn`.
- Wrap the Unix listener so each accepted conn carries peer creds in the request context; `identity.HostTrust` middleware rejects `Uid != 0` with 403 before stamping identity.
- Update structured logging in `internal/api/handlers.go` to emit `peer_pid` on every request that came in over the Unix listener.
- Tests: `peercred_test.go` against a real `net.Listen("unix", ...)` in `t.TempDir()`; integration test that verifies a non-root caller is rejected (run the test with `setuid` syscall to drop privileges in a subprocess, or use a UID-spoofing fake — TBD during implementation).

**Estimated size:** ~200 LOC moved + 80 new + ~150 LOC tests.

### PR 2 — policy package + host client

**Scope:**
- `internal/policy/{window,blackout,sentinel,decision}.go` with full unit tests.
- `internal/hostclient/client.go` with unit tests against an in-process Unix-socket fake.
- Nothing wired up yet — these are libraries waiting for callers.

**Estimated size:** ~400 LOC + ~500 LOC of tests.

### PR 3 — `trigger` subcommand

**Scope:**
- `cmd/swarm-auto-drain/trigger.go` wires the policy package + host client + a stubbable `systemctlReboot` function var.
- Integration test in `cmd/swarm-auto-drain/trigger_test.go` runs the full flow against an in-process fake daemon.
- Extend `/role` handler to include `version` in the response (additive; update one existing test).

**Estimated size:** ~80 LOC + ~250 LOC of tests.

### PR 4 — `resume` subcommand

**Scope:**
- `cmd/swarm-auto-drain/resume.go` mirroring trigger structure.
- Integration test analogous to trigger's.

**Estimated size:** ~50 LOC + ~150 LOC of tests.

### PR 5 — `install` + `uninstall` + embedded systemd units

**Scope:**
- `internal/hostinstall/install.go`, `paths.go`, `files/*.{service,timer,env}`.
- `cmd/swarm-auto-drain/install.go` and `uninstall.go` thin wrappers.
- Idempotency tests with `t.TempDir()` and `--root` flag.

**Estimated size:** ~200 LOC + ~250 LOC of tests + ~50 LOC embedded files.

### PR 6 — `stack` subcommand + release workflow + docs cleanup

**Scope:**
- `internal/stackgen/render.go` + embedded `stack.yml.tmpl`.
- `cmd/swarm-auto-drain/stack.go`.
- `.github/workflows/release.yml` — build + checksum + upload binaries on tag.
- `install.sh` published with the release.
- Delete `docs/consumers/{drain-docker-swarm.sh,resume-docker-swarm.sh,stack.yml}`.
- Rewrite `docs/consumers/README.md` and root `README.md` Quickstart.

**Estimated size:** ~150 LOC + ~200 LOC of tests + ~100 LOC docs/CI/embed.

## 20. Risks

| Risk | Mitigation |
|---|---|
| Binary on host vs in container drift after partial upgrade | Version handshake on every `trigger`/`resume`. Log a clear `daemon=vX host=vY upgrade host binary` warning. Document in release notes: upgrade container first, hosts second. |
| `systemctl` invocations in tests | All shell-outs route through a `runner` package var (default `exec.Command`) so tests substitute a recording fake. Standard pattern. |
| Embedded unit files reference a hardcoded `/usr/local/sbin/swarm-auto-drain` path | Single constant in `internal/hostinstall/paths.go`. Unit files are `text/template`-rendered at install time so this can be substituted later if `--bin-path` is added. ~5 lines of plumbing. |
| `groupadd` missing on minimal images | Documented prerequisite. Could later replace with direct `/etc/group` editing for Flatcar (read-only `/`); Flatcar-specific concern, defer. |
| Cross-platform path differences (`/var/run` vs `/run`) | Document `/var/run` as canonical; symlink on modern distros makes both work. No special-case code needed. |
| Operator overrides the env file, then `install` runs and silently clobbers it | `install` skips writing the env file if it exists; `--force` is required to overwrite. Print a warning if the env file exists but its values look unset/default. |
| `swarm-auto-drain stack` drift between rendered output and runtime startup checks | Stack-render tests pipe the output through the runtime's own `CheckOverlayEncryption` + `ValidateServiceName`. If the runtime tightens a check, the stack test fails and forces the template to match. |
| `RandomizedDelaySec=5min` causes drain-time bursts to cluster anyway when many nodes need reboots simultaneously | The coordinator already single-flights at the lock layer — only one node drains at a time. The 5-min jitter just spreads the lock-attempt traffic; serialization is enforced by the lock store regardless. |
| Subcommand boundaries get blurred over time (host code accidentally importing daemon-only stuff) | Convention: subcommand files in `cmd/` stay thin (wire-up only). All logic lives in `internal/` packages that are explicitly host-side (`policy`, `hostclient`, `hostinstall`, `stackgen`) or explicitly daemon-side (everything else). A linter rule can be added later if needed. |

## 21. Out of scope (for this plan)

- `.deb` packaging — install.sh covers the use case; add later if volume justifies.
- Per-replica timezone support — UTC-only for v1.
- Auto-deploy of the stack from `install` — explicit operator step.
- `swarm-auto-drain doctor` diagnostic subcommand — could be useful for support, defer.
- Metrics / Prometheus endpoint — separate concern.
- Slack / webhook notifications on reboot — operators wire via systemd `OnFailure=` for now.
- Maintenance-window override (e.g., "drain immediately, ignore window") — operators can drain manually via `curl --unix-socket .../drain`; the window only gates the autonomous loop.
