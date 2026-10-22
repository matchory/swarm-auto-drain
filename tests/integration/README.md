# Integration tests

These tests check the Docker behaviour the coordinator relies on, against a
real 3-manager Docker Swarm formed inside Docker-in-Docker (dind) containers.

## Run locally

```bash
docker compose -f tests/integration/docker-compose.yml up -d
bash tests/integration/tests.sh
docker compose -f tests/integration/docker-compose.yml down -v
```

## What they cover

- Slot exclusivity: a second `docker config create` of the same name, from
  another manager, fails.
- Node availability state transitions (drain ↔ active).

## What they do not cover

The coordinator itself. Identity needs a real tailscaled, which the fixture
does not have; the HTTP contract is covered by the Go unit tests, and the
binary is tested end to end on a real swarm before it is armed (matchory/platform
fleet-auto-reboot plan, Task 6 Step 0).

## CI

Not run in CI; run it locally before changes that touch Docker behaviour.
