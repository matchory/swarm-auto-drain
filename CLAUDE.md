# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build the binary (output: ./swarm-auto-drain, gitignored)
go build ./cmd/swarm-auto-drain

# Full test suite (CI runs with -race)
go test -race ./...

# Vet (CI runs this before tests)
go vet ./...

# Lint: CI pins golangci-lint v2.11.3. It cannot load packages built by a
# newer Go than it was built with, so run it under the go.mod toolchain:
GOTOOLCHAIN=go1.25.9 go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.3 run ./...
# Linux-only files are skipped on macOS; lint with GOOS=linux too.

# Integration fixture (3-manager dind swarm — Docker behaviour only, not the
# binary; see tests/integration/README.md).
docker compose -f tests/integration/docker-compose.yml up -d
bash tests/integration/tests.sh
docker compose -f tests/integration/docker-compose.yml down -v
```

## Architecture

One Go binary, run by systemd on every swarm manager, **outside** the swarm:
draining a node must never stop its own coordinator (v1's global service did,
which is why v2 exists). The contract is
`docs/specs/2026-10-22-v2-host-coordinator-design.md`; the handler tests
assert it.

Request path, outer to inner (`cmd/swarm-auto-drain/run.go`):

```
TCP on LISTEN_ADDR (the manager's tailnet IP)
  → identity.Middleware   who is the caller? (403 / 503 / 500)
    → api.AuditLog        one log line per /acquire, /release
      → api.Handlers      /acquire, /release, /status, /healthz
```

- **Identity** (`internal/identity`): tailscaled's LocalAPI `WhoIs` names the
  peer behind `r.RemoteAddr`; it must carry one of `ALLOWED_TAGS` and its
  MagicDNS name (or short name) must equal a swarm node's hostname, exactly.
  The caller only ever acts on its own node. There is no other auth.
- **Slots** (`internal/slots`): Docker Configs `<prefix>-1..N` for workers and
  `<prefix>-manager`. `ConfigCreate` failing on an existing name is the CAS.
  No expiry, by design: a node that never returns keeps its slot.
- **Drain** (`internal/drain`): quorum guard (`floor(M/2)+1` *other* managers
  reachable), set `drain`, poll until no running task. `/acquire` rolls back
  (active + free slot) on timeout or error.
- **Not a manager**: any Docker call failing with "not a swarm manager"
  becomes `503 {"error":"not a manager"}`, so callers try the next manager.

### Package boundaries

`internal/slots` and `internal/drain` declare the minimal Docker interface
they need instead of importing `internal/docker`; `cmd/swarm-auto-drain`
adapts between them. Follow the same pattern for new packages.

## Configuration

Env vars only, loaded by `internal/config`. `LISTEN_ADDR` (one address, never
a wildcard) and `ALLOWED_TAGS` are required.
