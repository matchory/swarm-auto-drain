# End-to-end tests

The released binary's code path, against a real swarm: three managers and two
workers in Docker-in-Docker, the coordinator running on each manager as it
does under systemd, and tailscaled replaced by `fakelocalapi`, which answers
WhoIs from `peers.json` (source IP → tailnet name and tags). Callers send
their requests from their own container, so the source address is what
identifies them, as on the tailnet. The binary has no test hooks.

```bash
tests/e2e/run.sh                      # every scenario, ~6 min
tests/e2e/run.sh test_quorum          # a subset
KEEP=1 tests/e2e/run.sh test_identity # leave the fixture up
docker compose -f tests/e2e/docker-compose.yml down -v
```

No ports are published, so it runs beside other dind fixtures. Needs Docker
and Go. CI runs it on every pull request.

## Scenarios

| Scenario | Expected |
|---|---|
| `test_identity` | Untagged, other cluster's tag, tagged non-node → 403. |
| `test_drain_waits_for_containers` | `/acquire` returns only after the node's containers stopped. |
| `test_drain_timeout_rolls_back` | Past `DRAIN_TIMEOUT_SECONDS`: 408, node active, slot free. |
| `test_client_disconnect_rolls_back` | Caller gives up mid-drain: node active, slot free. |
| `test_last_slot_race` | Two workers, two managers, one slot: one 200, one 409, only the winner drained. |
| `test_operator_drain_kept` | Node drained or paused by hand: `/acquire` 423, `/release` changes nothing. |
| `test_leaked_slot` | Slot Config left behind: 409 naming its holder; removing it frees it. |
| `test_unplaceable_task` | Task pinned to the node: the drain completes and the service is down until `/release`. |
| `test_update_out_of_sequence` | Node updates racing with drain/active: every call still succeeds. |
| `test_demoted_manager` | Demoted manager answers 503 "not a manager", takes a worker slot; promoted again, serves. |
| `test_quorum` | A manager down: a manager's `/acquire` is 503 (quorum), a worker's is 200. |
| `test_coordinator_restart_mid_drain` | SIGTERM mid-drain: the in-flight drain finishes (200) before exit. |
| `test_coordinator_crash_mid_drain` | SIGKILL mid-drain: node stays drained with its slot; a retry on another manager finishes it. |
| `test_manager_dies_mid_drain` | The answering manager's host dies: a retry on another manager finishes the drain. |
| `test_leader_election_mid_drain` | The raft leader dies mid-drain served by another manager: the drain completes. |
