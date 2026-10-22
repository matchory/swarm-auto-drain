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

The coordinator itself: see `tests/e2e/`.

## CI

Not run in CI; run it locally before changes that touch Docker behaviour.
