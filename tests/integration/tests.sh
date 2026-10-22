#!/bin/bash
# Integration tests against the dind fixture in docker-compose.yml.
# Assumes the fixture is already running.

set -euo pipefail

MGR1=tcp://localhost:12375
MGR2=tcp://localhost:12376
MGR3=tcp://localhost:12377

wait_dockerd() {
  local h=$1
  for _ in $(seq 1 60); do
    if DOCKER_HOST=$h docker info >/dev/null 2>&1; then return; fi
    sleep 1
  done
  echo "$h: dockerd did not come up"; exit 1
}

for h in $MGR1 $MGR2 $MGR3; do wait_dockerd "$h"; done

# Initialize swarm on mgr1, join others as managers.
DOCKER_HOST=$MGR1 docker swarm init --advertise-addr 10.99.0.11 >/dev/null
TOKEN=$(DOCKER_HOST=$MGR1 docker swarm join-token -q manager)
DOCKER_HOST=$MGR2 docker swarm join --token "$TOKEN" 10.99.0.11:2377 >/dev/null
DOCKER_HOST=$MGR3 docker swarm join --token "$TOKEN" 10.99.0.11:2377 >/dev/null

echo "swarm formed:"
DOCKER_HOST=$MGR1 docker node ls

# --- Test 1: a slot Config can be created once only ---
echo "=== Test 1: slot create is exclusive ==="
echo '{"holder":"node-a","acquired_at":'"$(date -u +%s)"'}' \
  | DOCKER_HOST=$MGR1 docker config create reboot-slot-1 - >/dev/null
# The same name from another manager must fail: that is the slot's CAS.
if echo '{"holder":"node-b","acquired_at":'"$(date -u +%s)"'}' \
  | DOCKER_HOST=$MGR2 docker config create reboot-slot-1 - >/dev/null 2>&1; then
  echo "FAIL: second create of the same slot succeeded"; exit 1
fi
DOCKER_HOST=$MGR1 docker config rm reboot-slot-1 >/dev/null

# --- Test 2: drain and active via API ---
# Swarm node hostnames are the dind container IDs, not the compose service
# names, so resolve a non-leader node ID dynamically rather than hardcoding.
echo "=== Test 2: node drain/active ==="
NODE=$(DOCKER_HOST=$MGR1 docker node ls --format '{{.ID}} {{.ManagerStatus}}' \
  | awk '$2 != "Leader" {print $1; exit}')
[ -n "$NODE" ] || { echo "FAIL: could not resolve a non-leader node"; exit 1; }
DOCKER_HOST=$MGR1 docker node update --availability drain "$NODE" >/dev/null
sleep 2
AVAIL=$(DOCKER_HOST=$MGR1 docker node inspect "$NODE" --format '{{ .Spec.Availability }}')
[ "$AVAIL" = "drain" ] || { echo "FAIL: drain not set (got $AVAIL)"; exit 1; }
DOCKER_HOST=$MGR1 docker node update --availability active "$NODE" >/dev/null

echo "ALL INTEGRATION TESTS PASSED"
