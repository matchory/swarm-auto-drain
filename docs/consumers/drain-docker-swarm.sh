#!/bin/bash
# Reference host-side drain script for swarm-auto-drain.
# Called by a host-level safe-reboot orchestrator (cron, systemd timer, etc.)
# AFTER all preconditions are checked (kill switch, puppet recency, reboot
# required). On success: writes /var/lib/swarm-auto-drain/draining and
# `systemctl reboot`s the host.

set -euo pipefail
exec > >(logger -t swarm-auto-drain-drain) 2>&1

COORDINATOR=http://127.0.0.1:7777

# Capture the HTTP code separately from the body so curl exit status only
# reflects transport errors, not application 4xx/5xx. (Avoid `curl -f`, which
# under `set -e` would kill the script before we can map 409/408/503 to a
# graceful "defer this run" branch.)
http_post() {
  curl -s -X POST --max-time "$2" -o /dev/null -w '%{http_code}' "$1"
}

# 1. Acquire the cluster-wide lock.
code=$(http_post "$COORDINATOR/lock" 60)
case "$code" in
  201|410) echo "lock acquired (code=$code)" ;;
  409)     echo "lock held by another node — deferring this run"; exit 0 ;;
  000)     echo "coordinator unreachable"; exit 1 ;;
  *)       echo "coordinator returned unexpected code $code on /lock"; exit 1 ;;
esac

# 2. Best-effort release if anything below fails before we touch the sentinel.
release() {
  curl -s -X DELETE --max-time 30 -o /dev/null "$COORDINATOR/lock" || true
}
trap 'release; exit 1' ERR

# 3. Drain — coordinator handles manager-quorum guard internally.
code=$(http_post "$COORDINATOR/drain" 600)
case "$code" in
  200) echo "drain complete" ;;
  408) echo "drain timed out — aborting, leaving allocation drained for operator triage"; trap - ERR; release; exit 1 ;;
  503) echo "drain refused (manager quorum) — deferring this run"; trap - ERR; release; exit 0 ;;
  000) echo "coordinator unreachable mid-drain"; exit 1 ;;
  *)   echo "unexpected code $code on /drain"; exit 1 ;;
esac

# 4. Sentinel + reboot.
trap - ERR
mkdir -p /var/lib/swarm-auto-drain
touch /var/lib/swarm-auto-drain/draining
echo "rebooting"
systemctl reboot
