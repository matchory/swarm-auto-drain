# Integration tests

These tests verify the lock primitive and drain operations against a real
3-manager Docker Swarm formed inside Docker-in-Docker (dind) containers.

## Run locally

```bash
docker compose -f tests/integration/docker-compose.yml up -d
bash tests/integration/tests.sh
docker compose -f tests/integration/docker-compose.yml down -v
```

## What they cover

- Docker Config CAS: concurrent `docker config create` of the same name
  succeeds for exactly one caller; the other gets `AlreadyExists`.
- Node availability state transitions (drain ↔ active).

## What they do NOT cover (yet)

- The coordinator binary itself end-to-end. That requires deploying
  the swarm-auto-drain stack onto the dind swarm and is left as
  follow-up. The Go unit tests cover the binary's logic; this fixture
  primarily validates the assumptions about how the Docker SDK behaves.

## CI

`.github/workflows/ci.yml` runs this same script in a GitHub-hosted
runner that supports nested Docker.
