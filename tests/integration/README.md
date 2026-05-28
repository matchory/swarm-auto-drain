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
- Metrics: the coordinator is deployed onto the dind swarm with
  `METRICS_LISTEN_ADDR=:9100`, and `:9100/metrics` is scraped to confirm it
  serves Prometheus output including `swarm_auto_drain_lock_acquire_total`
  after a lock acquire.

## What they do NOT cover (yet)

- The coordinator's full request flow (worker → manager forwarding, drain,
  quorum guard). The metrics test deploys the binary and exercises a single
  loopback `/lock`, but the broader HTTP contract is covered by the Go unit
  tests; this fixture primarily validates assumptions about how the Docker
  SDK behaves.

## CI

Not currently invoked from `.github/workflows/ci.yml` — the fixture is
run locally before changes that touch Docker SDK assumptions. CI runs
`go vet` and the race-enabled unit suite; `release.yml` builds the
multi-arch image on tag.
