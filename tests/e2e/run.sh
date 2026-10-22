#!/bin/bash
# e2e: the real coordinator binary on a dind swarm, with fakelocalapi standing
# in for tailscaled. Brings the fixture up, runs every scenario, tears it down
# (KEEP=1 leaves it up). Run a subset with: ./run.sh test_quorum test_leaked_slot
set -euo pipefail
cd "$(dirname "$0")"

COMPOSE=(docker compose -f docker-compose.yml)
IMAGE=alpine:3.21
DRAIN_TIMEOUT=20

log() { printf '\n=== %s\n' "$*"; }
fail() {
  echo "FAIL: $*"
  exit 1
}

# x NODE CMD...: run CMD in NODE's container; d NODE ARGS...: docker CLI there.
x() { "${COMPOSE[@]}" exec -T "$@"; }
d() {
  local n=$1
  shift
  x "$n" docker "$@"
}
ip() { echo "10.98.0.1${1#whale-}"; }
fqdn() { echo "$1.e2e.ts.net"; }

# call FROM VIA METHOD PATH [MAX]: FROM's request to VIA's coordinator. Sets
# CODE (000 if no answer) and BODY.
call() {
  local out
  out=$(x "$1" curl -s -w '\n%{http_code}' --max-time "${5:-120}" -X "$3" "http://$(ip "$2"):7777$4") || true
  CODE=${out##*$'\n'}
  BODY=${out%$'\n'*}
  [ "${BODY}" != "${CODE}" ] || BODY=
  echo "$1 -> $2 $3 $4: ${CODE} ${BODY}"
}
expect() { [ "${CODE}" = "$1" ] || fail "expected $1, got ${CODE} ${BODY}"; }

# Cluster state is read through QUERY, a manager that is up.
QUERY=whale-1
q() { d "${QUERY}" "$@"; }
availability() { q node inspect "$(fqdn "$1")" -f '{{.Spec.Availability}}'; }
availability_is() { [ "$(availability "$1")" = "$2" ]; }
reachability_is() { [ "$(q node inspect "$(fqdn "$1")" -f '{{.ManagerStatus.Reachability}}')" = "$2" ]; }
state_is() { [ "$(q node inspect "$(fqdn "$1")" -f '{{.Status.State}}')" = "$2" ]; }
replicas_is() { [ "$(q service ls --filter "name=$1" --format '{{.Replicas}}')" = "$2" ]; }
no_slots() { [ -z "$(slots)" ]; }
expect_availability() {
  local got
  got=$(availability "$1")
  [ "${got}" = "$2" ] || fail "$1 is ${got}, want $2"
}
slots() { q config ls --format '{{.Name}}' | { grep '^reboot-slot' || true; } | sort | xargs; }
expect_slots() {
  local got
  got=$(slots)
  [ "${got}" = "$1" ] || fail "slots are '${got}', want '$1'"
}

wait_for() {
  local what=$1 tries=$2
  shift 2
  for _ in $(seq "${tries}"); do
    if "$@" >/dev/null 2>&1; then return; fi
    sleep 1
  done
  fail "timed out waiting for ${what}"
}

wait_dockerd() { wait_for "dockerd on $1" 60 d "$1" info; }

start_coordinator() {
  local n=$1
  x "$n" sh -c 'pkill -f ^/e2e/bin/; true'
  "${COMPOSE[@]}" exec -d "$n" sh -c '/e2e/bin/fakelocalapi >>/var/log/fakelocalapi.log 2>&1'
  "${COMPOSE[@]}" exec -d \
    -e LISTEN_ADDR="$(ip "$n"):7777" -e ALLOWED_TAGS=tag:swarm-e2e \
    -e TAILSCALE_SOCKET=/run/fake-tailscaled.sock -e WORKER_SLOTS=1 \
    -e DRAIN_TIMEOUT_SECONDS="${DRAIN_TIMEOUT}" -e LOG_LEVEL=debug \
    "$n" sh -c '/e2e/bin/swarm-auto-drain run >>/var/log/swarm-auto-drain.log 2>&1'
  wait_for "coordinator on $n" 30 x "$n" curl -sf "http://$(ip "$n"):7777/healthz"
}
not_manager() { ! x "$1" docker node ls >/dev/null 2>&1; }
# stop_coordinator NODE [SIGNAL]: TERM waits for in-flight requests, KILL is a crash.
stop_coordinator() { x "$1" pkill "-${2:-TERM}" -f '^/e2e/bin/swarm-auto-drain' || true; }

# A container's dockerd and swarm state survive stop/start; its processes do not.
stop_node() {
  "${COMPOSE[@]}" stop -t 1 "$1" >/dev/null
  if [ "$1" = "${QUERY}" ]; then
    QUERY=whale-2
    [ "$1" != whale-2 ] || QUERY=whale-3
  fi
}
start_node() {
  "${COMPOSE[@]}" start "$1" >/dev/null
  wait_dockerd "$1"
  wait_for "$1 ready" 60 state_is "$1" ready
  start_coordinator "$1"
}

# slow NAME NODE GRACE: one task pinned to NODE that ignores SIGTERM, so it
# stops only when GRACE seconds run out.
slow() {
  q service create -d -q --name "$1" --no-resolve-image \
    --constraint "node.hostname==$(fqdn "$2")" --stop-grace-period "$3s" \
    "${IMAGE}" sh -c 'trap "" TERM; sleep 3600' >/dev/null
  wait_for "$1 running" 60 replicas_is "$1" 1/1
}
running_on() { d "$1" ps -q | wc -l | tr -d ' '; }
cleanup_services() {
  local svcs
  svcs=$(q service ls -q)
  # shellcheck disable=SC2086 # one ID per word
  [ -z "${svcs}" ] || q service rm ${svcs} >/dev/null
  # Removed services' tasks would otherwise sit out their stop grace period.
  local n ids
  for n in whale-1 whale-2 whale-3 whale-4 whale-5; do
    ids=$(d "$n" ps -aq 2>/dev/null) || continue
    # shellcheck disable=SC2086 # one ID per word
    [ -z "${ids}" ] || d "$n" rm -f ${ids} >/dev/null 2>&1 || true
  done
}

# Every scenario starts and must end here: all nodes active, no slot, no service.
baseline() {
  cleanup_services
  for n in whale-1 whale-2 whale-3 whale-4 whale-5; do
    availability_is "$n" active || q node update --availability active "$(fqdn "$n")" >/dev/null
  done
  local s
  for s in $(slots); do q config rm "$s" >/dev/null; done
}

setup() {
  local arch
  arch=$(docker version -f '{{.Server.Arch}}')
  log "build (linux/${arch})"
  mkdir -p bin
  CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" go build -o bin/swarm-auto-drain ../../cmd/swarm-auto-drain
  CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" go build -o bin/fakelocalapi ./fakelocalapi

  log "fixture"
  "${COMPOSE[@]}" up -d --build --quiet-pull
  for n in whale-1 whale-2 whale-3 whale-4 whale-5; do wait_dockerd "$n"; done
  d whale-1 swarm init --advertise-addr "$(ip whale-1)" >/dev/null
  local mt wt
  mt=$(d whale-1 swarm join-token -q manager)
  wt=$(d whale-1 swarm join-token -q worker)
  for n in whale-2 whale-3; do d "$n" swarm join --token "${mt}" "$(ip whale-1):2377" >/dev/null; done
  for n in whale-4 whale-5; do d "$n" swarm join --token "${wt}" "$(ip whale-1):2377" >/dev/null; done
  d whale-1 node ls

  # Images come from the host, not a registry.
  docker pull -q "${IMAGE}" >/dev/null
  for n in whale-1 whale-2 whale-3 whale-4 whale-5; do
    docker save "${IMAGE}" | "${COMPOSE[@]}" exec -T "$n" docker load -q >/dev/null
  done
  for n in whale-1 whale-2 whale-3; do start_coordinator "$n"; done
}

teardown() {
  local rc=$?
  if [ "${rc}" -ne 0 ]; then
    for n in whale-1 whale-2 whale-3; do
      echo "--- ${n} coordinator log (tail)"
      x "$n" tail -n 30 /var/log/swarm-auto-drain.log 2>/dev/null || true
    done
  fi
  [ -n "${KEEP:-}" ] || "${COMPOSE[@]}" down -v -t 1 >/dev/null 2>&1 || true
  exit "${rc}"
}

# --- scenarios ---------------------------------------------------------------

test_identity() {
  log "identity: only tagged nodes of this swarm"
  call untagged whale-1 GET /status && expect 403
  call other-cluster whale-1 GET /status && expect 403
  call intruder whale-1 POST /acquire && expect 403
  call whale-4 whale-1 GET /status && expect 200
  call whale-4 whale-1 GET /healthz && expect 200
  expect_slots ""
}

test_drain_waits_for_containers() {
  log "drain waits until the containers have stopped (v2.0.0 returned at once)"
  slow slow4 whale-4 8
  call whale-4 whale-1 POST /acquire && expect 200
  [ "$(running_on whale-4)" = 0 ] || fail "containers still running on whale-4 after /acquire"
  local elapsed
  elapsed=$(sed -n 's/.*"elapsed_seconds":\([0-9]*\).*/\1/p' <<<"${BODY}")
  [ "${elapsed}" -ge 7 ] || fail "acquire took ${elapsed}s, the task's grace is 8s"
  expect_availability whale-4 drain
  expect_slots reboot-slot-1
  call whale-4 whale-2 POST /release && expect 200
  expect_availability whale-4 active
  expect_slots ""
}

test_drain_timeout_rolls_back() {
  log "a drain past DRAIN_TIMEOUT_SECONDS rolls back (408)"
  slow slow4 whale-4 120
  call whale-4 whale-1 POST /acquire && expect 408
  expect_availability whale-4 active
  expect_slots ""
}

test_client_disconnect_rolls_back() {
  log "a caller that gives up mid-drain gets the node and slot back"
  slow slow4 whale-4 120
  call whale-4 whale-1 POST /acquire 3 && expect 000
  wait_for "rollback" 15 availability_is whale-4 active
  wait_for "slot freed" 15 no_slots
}

test_last_slot_race() {
  log "two workers race for the last slot through different managers"
  slow slow4 whale-4 5
  slow slow5 whale-5 5
  local a b
  a=$(mktemp)
  b=$(mktemp)
  call whale-4 whale-1 POST /acquire >"${a}" &
  call whale-5 whale-2 POST /acquire >"${b}" &
  wait
  cat "${a}" "${b}"
  local codes
  codes=$(cat "${a}" "${b}" | sed 's/.*: \([0-9]*\).*/\1/' | sort | xargs)
  rm -f "${a}" "${b}"
  [ "${codes}" = "200 409" ] || fail "codes are '${codes}', want one 200 and one 409"
  expect_slots reboot-slot-1
  # Only the winner is drained.
  local drained
  drained=$( (availability whale-4; availability whale-5) | sort | xargs)
  [ "${drained}" = "active drain" ] || fail "availabilities are '${drained}'"
  for n in whale-4 whale-5; do call "$n" whale-3 POST /release && expect 200; done
  expect_slots ""
}

test_operator_drain_kept() {
  log "a node an operator drained stays drained"
  q node update --availability drain "$(fqdn whale-5)" >/dev/null
  call whale-5 whale-1 POST /acquire && expect 423
  expect_slots ""
  call whale-5 whale-1 POST /release && expect 200
  grep -q '"released":false' <<<"${BODY}" || fail "release body ${BODY}"
  expect_availability whale-5 drain
  q node update --availability pause "$(fqdn whale-5)" >/dev/null
  call whale-5 whale-2 POST /acquire && expect 423
  expect_availability whale-5 pause
}

test_leaked_slot() {
  log "a slot an operator left behind blocks, removing it frees it"
  echo '{"holder":"gone.e2e.ts.net","acquired_at":1}' | q config create reboot-slot-1 - >/dev/null
  call whale-4 whale-1 POST /acquire && expect 409
  grep -q 'gone.e2e.ts.net' <<<"${BODY}" || fail "409 does not name the holder: ${BODY}"
  expect_availability whale-4 active
  q config rm reboot-slot-1 >/dev/null
  call whale-4 whale-1 POST /acquire && expect 200
  call whale-4 whale-1 POST /release && expect 200
  expect_slots ""
}

test_quorum() {
  log "a manager may not drain while another manager is down (503); workers may"
  stop_node whale-3
  wait_for "whale-3 unreachable" 60 reachability_is whale-3 unreachable
  call whale-2 whale-1 POST /acquire && expect 503
  grep -q 'quorum' <<<"${BODY}" || fail "503 is not the quorum refusal: ${BODY}"
  expect_availability whale-2 active
  expect_slots ""
  call whale-4 whale-1 POST /acquire && expect 200
  call whale-4 whale-1 POST /release && expect 200
  start_node whale-3
  call whale-2 whale-1 POST /acquire && expect 200
  call whale-2 whale-1 POST /release && expect 200
  expect_slots ""
}

test_demoted_manager() {
  log "a demoted manager answers 'not a manager'; promoted again, it serves"
  q node demote "$(fqdn whale-3)" >/dev/null
  wait_for "whale-3 demoted" 30 not_manager whale-3
  call whale-4 whale-3 GET /status && expect 503
  grep -q '"not a manager"' <<<"${BODY}" || fail "body ${BODY}"
  # As a worker now, whale-3 takes a worker slot.
  call whale-3 whale-1 POST /acquire && expect 200
  grep -q '"reboot-slot-1"' <<<"${BODY}" || fail "demoted manager did not get a worker slot: ${BODY}"
  call whale-3 whale-1 POST /release && expect 200
  q node promote "$(fqdn whale-3)" >/dev/null
  wait_for "whale-3 promoted" 30 x whale-3 docker node ls
  call whale-4 whale-3 GET /status && expect 200
}

test_coordinator_restart_mid_drain() {
  log "the coordinator is restarted mid-drain: it finishes the drain first"
  slow slow4 whale-4 10
  local out
  out=$(mktemp)
  call whale-4 whale-1 POST /acquire >"${out}" &
  sleep 4
  stop_coordinator whale-1 TERM
  wait
  cat "${out}"
  grep -q ': 200 ' "${out}" || fail "in-flight acquire did not finish"
  rm -f "${out}"
  [ "$(running_on whale-4)" = 0 ] || fail "containers still running on whale-4"
  start_coordinator whale-1
  call whale-4 whale-1 POST /release && expect 200
}

test_coordinator_crash_mid_drain() {
  log "the coordinator crashes mid-drain; the client's retry on another manager finishes it"
  slow slow4 whale-4 15
  call whale-4 whale-1 POST /acquire >/dev/null &
  sleep 4
  stop_coordinator whale-1 KILL
  wait
  # Nothing rolled back: the node is drained and keeps its slot.
  expect_availability whale-4 drain
  expect_slots reboot-slot-1
  [ "$(running_on whale-4)" = 1 ] || fail "the task should still be stopping"
  call whale-4 whale-2 POST /acquire && expect 200
  [ "$(running_on whale-4)" = 0 ] || fail "containers still running on whale-4"
  call whale-4 whale-2 POST /release && expect 200
  expect_slots ""
  start_coordinator whale-1
}

test_manager_dies_mid_drain() {
  log "the answering manager's host dies mid-drain; the retry on another manager finishes it"
  slow slow4 whale-4 15
  call whale-4 whale-1 POST /acquire >/dev/null &
  sleep 4
  stop_node whale-1
  wait
  call whale-4 whale-2 POST /acquire && expect 200
  [ "$(running_on whale-4)" = 0 ] || fail "containers still running on whale-4"
  call whale-4 whale-2 POST /release && expect 200
  start_node whale-1
}

test_leader_election_mid_drain() {
  log "the raft leader dies mid-drain served by another manager"
  local leader via
  leader=$(q node ls --format '{{.Hostname}} {{.ManagerStatus}}' | awk '$2 == "Leader" {print $1}')
  leader=${leader%%.*}
  for via in whale-1 whale-2 whale-3; do [ "${via}" != "${leader}" ] && break; done
  echo "leader ${leader}, asking ${via}"
  slow slow4 whale-4 10
  call whale-4 "${via}" POST /acquire >"${TMPDIR:-/tmp}/sad-e2e-leader" &
  sleep 3
  stop_node "${leader}"
  wait
  cat "${TMPDIR:-/tmp}/sad-e2e-leader"
  CODE=$(sed 's/.*: \([0-9]*\).*/\1/' "${TMPDIR:-/tmp}/sad-e2e-leader")
  # During the election, Docker calls can fail; the client then retries.
  if [ "${CODE}" != 200 ]; then call whale-4 "${via}" POST /acquire && expect 200; fi
  [ "$(running_on whale-4)" = 0 ] || fail "containers still running on whale-4"
  call whale-4 "${via}" POST /release && expect 200
  expect_slots ""
  start_node "${leader}"
}

test_update_out_of_sequence() {
  log "node updates racing with drain/active are retried"
  "${COMPOSE[@]}" exec -d whale-2 sh -c \
    "i=0; while [ \$i -lt 300 ]; do docker node update --label-add e2e=\$i $(fqdn whale-4) >/dev/null 2>&1; i=\$((i+1)); done"
  local before after
  before=$(label_e2e)
  for _ in 1 2 3 4 5; do
    call whale-4 whale-1 POST /acquire && expect 200
    call whale-4 whale-1 POST /release && expect 200
  done
  after=$(label_e2e)
  x whale-2 pkill -f 'label-add' || true
  # The racing updates really ran alongside.
  [ "${after:-0}" -gt "${before:-0}" ] || fail "label updates did not run (${before} -> ${after})"
  expect_slots ""
}

label_e2e() { q node inspect "$(fqdn whale-4)" -f '{{index .Spec.Labels "e2e"}}'; }

test_unplaceable_task() {
  log "a task with nowhere else to go: the drain completes, the service is down"
  slow pinned whale-4 1
  call whale-4 whale-1 POST /acquire && expect 200
  replicas_is pinned 0/1 || fail "pinned service still has a running replica"
  call whale-4 whale-1 POST /release && expect 200
  wait_for "pinned back" 60 replicas_is pinned 1/1
}

ALL=(
  test_identity
  test_drain_waits_for_containers
  test_drain_timeout_rolls_back
  test_client_disconnect_rolls_back
  test_last_slot_race
  test_operator_drain_kept
  test_leaked_slot
  test_unplaceable_task
  test_update_out_of_sequence
  test_demoted_manager
  test_quorum
  test_coordinator_restart_mid_drain
  test_coordinator_crash_mid_drain
  test_manager_dies_mid_drain
  test_leader_election_mid_drain
)

TESTS=("${ALL[@]}")
[ $# -eq 0 ] || TESTS=("$@")
trap teardown EXIT
setup
for t in "${TESTS[@]}"; do
  baseline
  "$t"
done
baseline
echo
echo "ALL E2E SCENARIOS PASSED"
