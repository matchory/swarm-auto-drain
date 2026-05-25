#!/bin/bash
# Reference host-side resume script for swarm-auto-drain.
# Called from a systemd oneshot at boot, GATED by
# ConditionPathExists=/var/lib/swarm-auto-drain/draining so non-coordinated
# boots (kernel panic, power loss, manual reboot) skip it entirely.

set -euo pipefail
exec > >(logger -t swarm-auto-drain-resume) 2>&1

COORDINATOR=http://127.0.0.1:7777

# 1. Wait for the local coordinator replica to come up (5 min budget).
for i in $(seq 1 60); do
  if curl -sf --max-time 5 "$COORDINATOR/healthz" >/dev/null; then
    break
  fi
  sleep 5
done
if ! curl -sf --max-time 5 "$COORDINATOR/healthz" >/dev/null; then
  echo "coordinator never came up — leaving sentinel + lock for next boot"
  exit 1
fi

# 2. Set self active. Idempotent — succeeds even if already active.
code=$(curl -s -X POST --max-time 60 -o /dev/null -w '%{http_code}' "$COORDINATOR/active")
if [ "$code" != "200" ]; then
  echo "unexpected code $code on /active"
  exit 1
fi

# 3. Release the lock. The coordinator verifies we are the holder; a 403
#    means a peer stole the lock as stale during our outage, which is the
#    correct end state — do not retry.
code=$(curl -s -X DELETE --max-time 30 -o /dev/null -w '%{http_code}' "$COORDINATOR/lock")
case "$code" in
  204) echo "lock released" ;;
  403|404) echo "lock not held by us (code=$code) — peer took over, leaving be" ;;
  *)   echo "unexpected release code $code"; exit 1 ;;
esac

# 4. Clear sentinel so next boot is a no-op.
rm -f /var/lib/swarm-auto-drain/draining
