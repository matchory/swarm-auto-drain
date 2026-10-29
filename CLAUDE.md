# CLAUDE.md

- Lint with `golangci-lint run`. CI pins v2.11.3; newer local versions add linters (e.g. `exhaustruct_v5`) that CI does not run, so only findings from v2.11.3 count.
- There is no auth code, on purpose. Trust comes from the Unix socket's mode and group, the encrypted internal overlay, and caller identity derived server-side from the swarm task list (`internal/identity`). `X-Origin-Node` is advisory only. Do not add auth or start trusting the header.
- The role (manager/worker) is checked on every request, not once at startup, so `docker node promote`/`demote` works without a restart. Keep it that way.
- `docs/specs/2026-05-25-coordinator-design.md` defines the HTTP contract (status codes, JSON shapes, identity rules), and `internal/api/handlers_test.go` asserts it. Change spec, tests and the README API table together.
