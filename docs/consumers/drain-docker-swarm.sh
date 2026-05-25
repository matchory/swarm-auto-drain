#!/bin/bash
# Reference host-side drain script for swarm-auto-drain.
# Called by a host-level safe-reboot orchestrator (cron, systemd timer, etc.)
# AFTER all preconditions are checked (kill switch, puppet recency, reboot
# required). On success: writes /var/lib/swarm-auto-drain/draining and
# `systemctl reboot`s the host.

set -euo pipefail
exec > >(logger -t swarm-auto-drain-drain) 2>&1

COORDINATOR=http://127.0.0.1:7777

# 1. Acquire the cluster-wide lock.
resp=$(curl -sf -X POST --max-time 60 -w '\n%{http_code}' "$COORDINATOR/lock")
code=${resp##*$'\n'}
case "$code" in
  201|410) echo "lock acquired (code=$code)" ;;
  409)     echo "lock held by another node — deferring this run"; exit 0 ;;
  *)       echo "coordinator returned unexpected code $code"; exit 1 ;;
esac

# 2. Best-effort release if anything below fails.
release() {
  curl -sf -X DELETE --max-time 30 "$COORDINATOR/lock" || true
}
trap 'release; exit 1' ERR

# 3. Drain — coordinator handles manager-quorum guard internally.
curl -sf -X POST --max-time 600 "$COORDINATOR/drain"
echo "drain complete"

# 4. Sentinel + reboot.
trap - ERR
mkdir -p /var/lib/swarm-auto-drain
touch /var/lib/swarm-auto-drain/draining
echo "rebooting"
systemctl reboot
