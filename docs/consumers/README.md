# Reference consumer scripts

The shell scripts in this directory are reference implementations of the
host-side integration. They are NOT installed by the coordinator — they
live here for downstream adopters to copy / adapt.

## Files

- `drain-docker-swarm.sh` — called by a host-level orchestrator (cron,
  systemd timer) AFTER preconditions are checked. Acquires the cluster
  lock, drains the node, writes the resume sentinel, reboots.
- `resume-docker-swarm.sh` — called from a sentinel-gated systemd oneshot
  at boot. Waits for the local coordinator replica, sets the node active,
  releases the lock, clears the sentinel.

## Sentinel

Both scripts cooperate around `/var/lib/swarm-auto-drain/draining`. The
sentinel exists between drain (just before `systemctl reboot`) and
successful resume. It gates the resume systemd unit via
`ConditionPathExists` so non-coordinated boots (kernel panic, power
loss, manual `reboot`) do not run the resume logic. This prevents the
resume script from running on every boot and prevents clobbering an
operator's deliberate `availability=drain` setting.

## Suggested systemd unit for the resume oneshot

```ini
[Unit]
Description=swarm-auto-drain resume after coordinated reboot
ConditionPathExists=/var/lib/swarm-auto-drain/draining
After=docker.service network-online.target
Wants=docker.service network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/resume-docker-swarm
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
```

## Preconditions left to the host orchestrator

The coordinator does NOT enforce:
- A "kill switch" for opt-in per host.
- Recency of the last configuration management run.
- The presence of `/var/run/reboot-required`.

Those checks belong upstream of the call to this script — they are the
operator's policy decisions, not the coordinator's job.
