#!/bin/bash
# Integration tests against the dind fixture in docker-compose.yml.
# Assumes the fixture is already running.

set -euo pipefail

MGR1=tcp://localhost:12375
MGR2=tcp://localhost:12376
MGR3=tcp://localhost:12377

wait_dockerd() {
  local h=$1 i
  for i in $(seq 1 60); do
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

# --- Test 1: lock acquire/release via Docker Configs ---
echo "=== Test 1: lock acquire/release ==="
HOLDER_A_JSON='{"holder":"node-a","acquired_at":'$(date -u +%s)',"ttl_seconds":1800}'
echo "$HOLDER_A_JSON" | DOCKER_HOST=$MGR1 docker config create swarm-auto-drain-lock - >/dev/null

# Concurrent create from a different "node" must fail.
HOLDER_B_JSON='{"holder":"node-b","acquired_at":'$(date -u +%s)',"ttl_seconds":1800}'
if echo "$HOLDER_B_JSON" | DOCKER_HOST=$MGR2 docker config create swarm-auto-drain-lock - >/dev/null 2>&1; then
  echo "FAIL: concurrent create succeeded"; exit 1
fi
DOCKER_HOST=$MGR1 docker config rm swarm-auto-drain-lock >/dev/null

# --- Test 2: drain and active via API ---
echo "=== Test 2: node drain/active ==="
DOCKER_HOST=$MGR1 docker node update --availability drain dind-mgr2 >/dev/null
sleep 2
AVAIL=$(DOCKER_HOST=$MGR1 docker node inspect dind-mgr2 --format '{{ .Spec.Availability }}')
[ "$AVAIL" = "drain" ] || { echo "FAIL: drain not set (got $AVAIL)"; exit 1; }
DOCKER_HOST=$MGR1 docker node update --availability active dind-mgr2 >/dev/null

echo "ALL INTEGRATION TESTS PASSED"
