# Operator guide

How to run the coordinator and call it. At Matchory, Puppet does all of this
(matchory/platform `swarm::reboot_coordinator` and `swarm::reboot_hooks`).

## On every manager

Install the binary from a release, checking it against `SHA256SUMS`, as
`/usr/local/sbin/swarm-auto-drain`, and run it with systemd:

```ini
# /etc/systemd/system/swarm-auto-drain.service
[Unit]
Description=swarm-auto-drain reboot coordinator
After=docker.service tailscaled.service
Wants=docker.service tailscaled.service

[Service]
EnvironmentFile=/etc/default/swarm-auto-drain
ExecStart=/usr/local/sbin/swarm-auto-drain run
# The tailnet address may not exist yet right after boot.
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
```

```sh
# /etc/default/swarm-auto-drain
LISTEN_ADDR=100.104.205.4:7777
ALLOWED_TAGS=tag:swarm-pipeline
WORKER_SLOTS=1
```

It runs as root: it needs the Docker socket and tailscaled's LocalAPI.

## Tailnet policy

Callers reach the managers directly on `LISTEN_ADDR`, so the tailnet policy
must let the swarm's tag reach the managers on that port, e.g.

```json
{ "src": ["tag:swarm-pipeline"], "dst": ["tag:swarm-pipeline"], "ip": ["tcp:7777"] }
```

The coordinator rejects every caller that does not carry one of
`ALLOWED_TAGS` or is not a node of the swarm, whatever the policy allows.

## Calling it

A node calls each manager in turn until one answers with anything but `503`
or a connection error:

```sh
# Before the reboot. 200: drained, reboot now. 409/503: try again later.
curl -sS -X POST "http://$manager:7777/acquire"
# After the reboot, once Docker is up.
curl -sS -X POST "http://$manager:7777/release"
```

Keep a marker between the two so `release` runs after a planned reboot only.
`release` from a node that holds no slot changes nothing.

## A node that did not come back

Its slot stays taken. Once the node is fixed and back in the swarm, either it
calls `release`, or an operator runs `docker node update --availability active
<node>` and `docker config rm <slot>`.
