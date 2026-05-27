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

# --- Test 3: coordinator /metrics endpoint (end-to-end) ---
# Unlike tests 1 & 2 (which exercise the Docker SDK directly), this test
# deploys the real coordinator binary onto the dind swarm and scrapes its
# Prometheus listener. The production stack ships metrics OFF; here we set
# METRICS_LISTEN_ADDR=:9100 explicitly to turn the listener on.
#
# We deploy via `docker service create` rather than `docker stack deploy`
# because a stack prefixes the service name (`<stack>_<service>`), and the
# coordinator refuses to start under a name containing an underscore (it
# isn't a valid DNS label for overlay forwarding). A plain service create
# lets us pick a DNS-safe single-label name.
echo "=== Test 3: coordinator /metrics endpoint ==="
HERE=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$HERE/../.." && pwd)
IMAGE=local/swarm-auto-drain:dev
SERVICE=swarmautodrain

# Build the coordinator image once on the host daemon.
docker build -t "$IMAGE" "$REPO_ROOT" >/dev/null

# dind daemons can't see the host's image store; load the image into each
# and pre-create the host socket dir the bind mount expects (normally made
# by the installer — swarm refuses a task whose bind source is missing).
IMG_TAR=$(mktemp)
docker save "$IMAGE" -o "$IMG_TAR"
for h in $MGR1 $MGR2 $MGR3; do
  DOCKER_HOST=$h docker load -i "$IMG_TAR" >/dev/null
  DOCKER_HOST=$h docker run --rm -v /var/run:/hostrun busybox \
    mkdir -p /hostrun/swarm-auto-drain >/dev/null
done
rm -f "$IMG_TAR"

# Attachable=false matches production; the coordinator self-discovers its
# service name from the swarm task label, so no env wiring is needed.
DOCKER_HOST=$MGR1 docker network create -d overlay --attachable=false sadnet >/dev/null 2>&1 || true
DOCKER_HOST=$MGR1 docker service create \
  --name "$SERVICE" \
  --mode global \
  --network sadnet \
  --env NODE_HOSTNAME='{{.Node.Hostname}}' \
  --env METRICS_LISTEN_ADDR=:9100 \
  --env ALLOW_UNENCRYPTED_OVERLAY=true \
  --env UNIX_SOCKET_GID=9999 \
  --mount type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock \
  --mount type=bind,source=/var/run/swarm-auto-drain,target=/var/run/swarm-auto-drain \
  "$IMAGE" run >/dev/null

echo "[metrics] waiting for coordinator task to start"
for i in $(seq 1 60); do
  RUNNING=$(DOCKER_HOST=$MGR1 docker service ps "$SERVICE" \
    --filter desired-state=running --format '{{.CurrentState}}' 2>/dev/null \
    | grep -c '^Running' || true)
  [ "${RUNNING:-0}" -ge 1 ] && break
  sleep 2
done
[ "${RUNNING:-0}" -ge 1 ] || {
  echo "FAIL: coordinator task never reached Running"
  DOCKER_HOST=$MGR1 docker service ps "$SERVICE" --no-trunc || true
  DOCKER_HOST=$MGR1 docker service logs "$SERVICE" 2>&1 | tail -20 || true
  exit 1
}

# Drive POST /lock so swarm_auto_drain_lock_acquire_total is created (OTel
# emits a counter series only after its first observation). The acquire
# must originate from loopback so the coordinator treats it as its own node
# identity (a non-loopback source IP that isn't a peer task is rejected).
# Joining the coordinator container's network namespace gives us its
# loopback; from there we hit both the API (:7777) and metrics (:9100).
sleep 2
CID=$(DOCKER_HOST=$MGR1 docker ps --filter "name=$SERVICE" --format '{{.ID}}' | head -1)
[ -n "$CID" ] || { echo "FAIL: no coordinator container on mgr1"; exit 1; }
DOCKER_HOST=$MGR1 docker run --rm --network "container:$CID" busybox \
  wget -q -O- --post-data='' http://127.0.0.1:7777/lock >/dev/null 2>&1 || true

echo "[metrics] scraping :9100/metrics from the coordinator on mgr1"
METRICS_BODY=$(DOCKER_HOST=$MGR1 docker run --rm --network "container:$CID" busybox \
  wget -q -O- http://127.0.0.1:9100/metrics 2>/dev/null || true)
# Prometheus exposition name: OTel dotted swarm_auto_drain.lock.acquire
# becomes underscored with a _total counter suffix.
if ! echo "$METRICS_BODY" | grep -q 'swarm_auto_drain_lock_acquire_total'; then
  echo "FAIL: /metrics did not contain swarm_auto_drain_lock_acquire_total"
  echo "$METRICS_BODY" | head -30
  exit 1
fi
echo "[metrics] OK"

echo "ALL INTEGRATION TESTS PASSED"
