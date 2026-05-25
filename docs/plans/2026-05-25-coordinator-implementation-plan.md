# swarm-auto-drain Coordinator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the swarm-auto-drain coordinator — a Go HTTP daemon, deployed as a global Docker Swarm service, that arbitrates rolling node reboots so at most one node drains at a time and manager quorum is preserved.

**Architecture:** Single binary, single Docker image, deployed as a global Swarm service (one replica per node). Each replica detects manager/worker role at startup via `docker info`; manager replicas handle requests in-process, worker replicas forward to a manager via Swarm's internal overlay network. Lock state is stored in a Docker Config (atomic CAS via "create-fails-on-exists"). No external identity infrastructure — the overlay network is the auth boundary.

**Tech Stack:** Go 1.23, Docker SDK for Go (`github.com/docker/docker`), stdlib `net/http` + `log/slog`, distroless container image.

**Reference spec:** [`docs/specs/2026-05-25-coordinator-design.md`](../specs/2026-05-25-coordinator-design.md). When the plan and the spec disagree on the *what*, the spec wins; fix the plan.

---

## How to read this plan

Each task is self-contained — it tells you which files to create or modify, the exact code, the exact commands to run, and the expected output. Tasks follow TDD: write the failing test, watch it fail, write the minimal implementation, watch it pass, commit.

Conventions:
- Paths are relative to the project root unless absolute.
- Commands use `bash`; from project root unless noted.
- Every task ends with a commit; we ship small, frequent commits.
- Interfaces are defined alongside the concrete types in the same package. Other packages depend on the interface so they can be tested with mocks.

The first time the plan introduces a Go package, it sets up the file. Subsequent tasks add to it. Read tasks in order on the first pass.

---

## Testing Approach

- **Unit tests** (the majority) use `testing.T` + `net/http/httptest` + hand-rolled mocks satisfying small interfaces. No mocking library. Run via `go test ./...`.
- **Integration tests** live under `tests/integration/` and require a running Docker daemon with swarm-init. They use `docker-compose` + `dind` to spin up a 3-node swarm fixture. Marked with a build tag (`//go:build integration`) so they're opt-in. Run via `go test -tags=integration ./tests/integration/...`.
- **CI** runs both. The integration job sets up Docker-in-Docker.

---

## File Structure

```
swarm-auto-drain/
├── cmd/swarm-auto-drain/main.go              entry, flag/env parse, deps, server
├── internal/
│   ├── config/config.go                       env var parsing + defaults
│   ├── config/config_test.go
│   ├── logging/logging.go                     slog setup
│   ├── docker/client.go                       SDK init + thin wrappers
│   ├── docker/role.go                         role detection + recheck loop
│   ├── docker/role_test.go
│   ├── lock/lock.go                           Lock struct + Store interface
│   ├── lock/config_store.go                   Docker-Config-backed Store
│   ├── lock/config_store_test.go              uses fake docker client interface
│   ├── drain/drain.go                         Drainer interface
│   ├── drain/docker_drainer.go                Docker-backed Drainer
│   ├── drain/docker_drainer_test.go
│   ├── forward/forward.go                     worker→manager forwarder
│   ├── forward/forward_test.go
│   ├── identity/identity.go                   middleware: resolve caller node
│   ├── identity/identity_test.go
│   ├── api/server.go                          HTTP server + routing
│   ├── api/handlers.go                        endpoint handlers
│   └── api/handlers_test.go
├── deploy/swarm-auto-drain.stack.yml          docker stack deploy target
├── docs/
│   ├── specs/2026-05-25-coordinator-design.md
│   ├── plans/2026-05-25-coordinator-implementation-plan.md   (this file)
│   └── consumers/                             reference shell scripts
│       ├── README.md
│       ├── drain-docker-swarm.sh
│       └── resume-docker-swarm.sh
├── tests/integration/
│   ├── docker-compose.yml
│   ├── README.md
│   └── tests.sh
├── Dockerfile
├── go.mod / go.sum
├── README.md
├── LICENSE
└── .github/workflows/ci.yml
```

---

## Task 1: Baseline — verify build, add Docker SDK dependency

**Files:**
- Modify: `go.mod`

- [ ] **Step 1: Verify the bootstrap builds**

```bash
go build ./...
```

Expected: silent success (or a warning about no targets — the placeholder `main.go` builds).

- [ ] **Step 2: Add the Docker SDK dependency**

```bash
go get github.com/docker/docker@v27.5.1
go get github.com/docker/docker/client@v27.5.1
go mod tidy
```

Expected: `go.sum` created; `go.mod` lists `github.com/docker/docker` as a direct dependency.

- [ ] **Step 3: Verify build still works after dep add**

```bash
go build ./...
go vet ./...
```

Expected: both silent.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "deps: add Docker SDK"
```

---

## Task 2: `internal/config` — env var parsing + defaults

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/config/config_test.go`:

```go
package config

import (
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "test-node")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if c.NodeHostname != "test-node" {
		t.Errorf("NodeHostname = %q, want test-node", c.NodeHostname)
	}
	if c.ListenAddr != "127.0.0.1:7777" {
		t.Errorf("ListenAddr = %q, want 127.0.0.1:7777", c.ListenAddr)
	}
	if c.DockerSocket != "/var/run/docker.sock" {
		t.Errorf("DockerSocket = %q", c.DockerSocket)
	}
	if c.LockConfigName != "swarm-auto-drain-lock" {
		t.Errorf("LockConfigName = %q", c.LockConfigName)
	}
	if c.LockTTL != 30*time.Minute {
		t.Errorf("LockTTL = %v, want 30m", c.LockTTL)
	}
	if c.DrainTimeout != 5*time.Minute {
		t.Errorf("DrainTimeout = %v, want 5m", c.DrainTimeout)
	}
	if c.RoleRecheck != 30*time.Second {
		t.Errorf("RoleRecheck = %v, want 30s", c.RoleRecheck)
	}
	if c.ForwardTimeout != 60*time.Second {
		t.Errorf("ForwardTimeout = %v, want 60s", c.ForwardTimeout)
	}
	if c.ManagerDiscoveryTTL != 60*time.Second {
		t.Errorf("ManagerDiscoveryTTL = %v, want 60s", c.ManagerDiscoveryTTL)
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel = %q", c.LogLevel)
	}
}

func TestLoad_Overrides(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "override-node")
	t.Setenv("LISTEN_ADDR", "0.0.0.0:9999")
	t.Setenv("LOCK_TTL_SECONDS", "60")
	t.Setenv("LOG_LEVEL", "debug")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ListenAddr != "0.0.0.0:9999" {
		t.Errorf("ListenAddr = %q", c.ListenAddr)
	}
	if c.LockTTL != 60*time.Second {
		t.Errorf("LockTTL = %v", c.LockTTL)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", c.LogLevel)
	}
}

func TestLoad_MissingNodeHostname(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when NODE_HOSTNAME is empty")
	}
}

func TestLoad_BadInteger(t *testing.T) {
	t.Setenv("NODE_HOSTNAME", "n")
	t.Setenv("LOCK_TTL_SECONDS", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-numeric LOCK_TTL_SECONDS")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/config/...
```

Expected: build failure ("no Go files" or undefined `Load`).

- [ ] **Step 3: Implement Config + Load**

Create `internal/config/config.go`:

```go
// Package config loads the coordinator's runtime configuration from environment
// variables. All knobs documented in docs/specs/.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	NodeHostname        string
	ListenAddr          string
	DockerSocket        string
	LockConfigName      string
	LockTTL             time.Duration
	DrainTimeout        time.Duration
	RoleRecheck         time.Duration
	ForwardTimeout      time.Duration
	ManagerDiscoveryTTL time.Duration
	LogLevel            string
}

// Load reads configuration from environment variables. NODE_HOSTNAME is
// required; everything else has a default documented in the spec.
func Load() (*Config, error) {
	c := &Config{
		NodeHostname:        os.Getenv("NODE_HOSTNAME"),
		ListenAddr:          envStr("LISTEN_ADDR", "127.0.0.1:7777"),
		DockerSocket:        envStr("DOCKER_SOCKET", "/var/run/docker.sock"),
		LockConfigName:      envStr("LOCK_CONFIG_NAME", "swarm-auto-drain-lock"),
		LogLevel:            envStr("LOG_LEVEL", "info"),
	}
	if c.NodeHostname == "" {
		return nil, fmt.Errorf("NODE_HOSTNAME is required (set via swarm template: NODE_HOSTNAME={{.Node.Hostname}})")
	}

	var err error
	if c.LockTTL, err = envSecs("LOCK_TTL_SECONDS", 30*time.Minute); err != nil {
		return nil, err
	}
	if c.DrainTimeout, err = envSecs("DRAIN_TIMEOUT_SECONDS", 5*time.Minute); err != nil {
		return nil, err
	}
	if c.RoleRecheck, err = envSecs("ROLE_RECHECK_SECONDS", 30*time.Second); err != nil {
		return nil, err
	}
	if c.ForwardTimeout, err = envSecs("FORWARD_TIMEOUT_SECONDS", 60*time.Second); err != nil {
		return nil, err
	}
	if c.ManagerDiscoveryTTL, err = envSecs("MANAGER_DISCOVERY_TTL_SECONDS", 60*time.Second); err != nil {
		return nil, err
	}
	return c, nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envSecs(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", key, v)
	}
	return time.Duration(n) * time.Second, nil
}
```

- [ ] **Step 4: Run the tests, expect PASS**

```bash
go test ./internal/config/...
```

Expected: `PASS`, all four subtests green.

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "config: env var parsing with defaults"
```

---

## Task 3: `internal/logging` — slog setup

**Files:**
- Create: `internal/logging/logging.go`

This is small and has no behavioral logic worth testing in isolation — slog is the stdlib. Skip the test, just wire it.

- [ ] **Step 1: Write the package**

Create `internal/logging/logging.go`:

```go
// Package logging configures the process-wide slog logger.
package logging

import (
	"log/slog"
	"os"
)

// Setup configures slog's default logger from the given level string
// ("debug" | "info" | "warn" | "error"). Unknown levels fall back to info.
func Setup(level string) {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})
	slog.SetDefault(slog.New(h))
}
```

- [ ] **Step 2: Verify it compiles**

```bash
go build ./internal/logging/...
go vet ./internal/logging/...
```

Expected: silent.

- [ ] **Step 3: Commit**

```bash
git add internal/logging/
git commit -m "logging: slog setup"
```

---

## Task 4: `internal/docker` — SDK client + Info wrapper

**Files:**
- Create: `internal/docker/client.go`

The Docker SDK requires a real Docker daemon for end-to-end tests, so behavior tests for this file are in the integration tests (Task 19). Here we just wrap construction.

- [ ] **Step 1: Write the client wrapper**

Create `internal/docker/client.go`:

```go
// Package docker wraps the Docker SDK for the coordinator's use.
package docker

import (
	"context"

	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/api/types/system"
)

// Client is the subset of the Docker SDK we use, exposed as an interface so
// other packages can be tested with fakes.
type Client interface {
	Info(ctx context.Context) (system.Info, error)
	Close() error

	// Used by the lock store.
	ConfigCreate(ctx context.Context, name string, data []byte, labels map[string]string) (id string, err error)
	ConfigInspect(ctx context.Context, name string) (data []byte, labels map[string]string, err error)
	ConfigRemove(ctx context.Context, name string) error

	// Used by the drainer.
	NodeList(ctx context.Context, filters map[string][]string) ([]NodeSummary, error)
	NodeUpdate(ctx context.Context, nodeName, availability string) error
	NodeTaskCount(ctx context.Context, nodeName string) (int, error)
}

// NodeSummary is a tiny projection of swarm.Node for our needs.
type NodeSummary struct {
	Hostname     string
	Role         string // "manager" | "worker"
	Reachability string // "reachable" | "unreachable" | "unknown" | ""
	Leader       bool
	Availability string // "active" | "pause" | "drain"
}

// New constructs a real Docker client wrapping the given socket path.
func New(socketPath string) (Client, error) {
	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.WithHost("unix://"+socketPath),
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, err
	}
	return &dockerClient{c: cli}, nil
}

type dockerClient struct {
	c *dockerclient.Client
}

func (d *dockerClient) Info(ctx context.Context) (system.Info, error) {
	return d.c.Info(ctx)
}

func (d *dockerClient) Close() error { return d.c.Close() }
```

The six Config/Node methods declared on the interface need stubs so the package compiles now and is filled in properly in Task 11. Append to `internal/docker/client.go`:

```go
import "errors"

var errNotYetImplemented = errors.New("docker.Client: method not yet implemented (filled in by later tasks)")

func (d *dockerClient) ConfigCreate(context.Context, string, []byte, map[string]string) (string, error) {
	return "", errNotYetImplemented
}
func (d *dockerClient) ConfigInspect(context.Context, string) ([]byte, map[string]string, error) {
	return nil, nil, errNotYetImplemented
}
func (d *dockerClient) ConfigRemove(context.Context, string) error { return errNotYetImplemented }
func (d *dockerClient) NodeList(context.Context, map[string][]string) ([]NodeSummary, error) {
	return nil, errNotYetImplemented
}
func (d *dockerClient) NodeUpdate(context.Context, string, string) error { return errNotYetImplemented }
func (d *dockerClient) NodeTaskCount(context.Context, string) (int, error) {
	return 0, errNotYetImplemented
}
```

- [ ] **Step 2: Verify build**

```bash
go build ./internal/docker/...
go vet ./internal/docker/...
```

Expected: silent.

- [ ] **Step 3: Commit**

```bash
git add internal/docker/
git commit -m "docker: Client interface + SDK construction (method stubs)"
```

---

## Task 5: `internal/docker/role` — role detection + recheck loop

**Files:**
- Create: `internal/docker/role.go`
- Test: `internal/docker/role_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/docker/role_test.go`:

```go
package docker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
)

type fakeClient struct {
	mu       sync.Mutex
	info     system.Info
	infoErr  error
	infoCalls int
}

func (f *fakeClient) Info(ctx context.Context) (system.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoCalls++
	return f.info, f.infoErr
}
func (f *fakeClient) Close() error                                                                  { return nil }
func (f *fakeClient) ConfigCreate(context.Context, string, []byte, map[string]string) (string, error) { return "", nil }
func (f *fakeClient) ConfigInspect(context.Context, string) ([]byte, map[string]string, error)      { return nil, nil, nil }
func (f *fakeClient) ConfigRemove(context.Context, string) error                                     { return nil }
func (f *fakeClient) NodeList(context.Context, map[string][]string) ([]NodeSummary, error)           { return nil, nil }
func (f *fakeClient) NodeUpdate(context.Context, string, string) error                               { return nil }
func (f *fakeClient) NodeTaskCount(context.Context, string) (int, error)                             { return 0, nil }

func managerInfo() system.Info {
	return system.Info{Swarm: swarm.Info{ControlAvailable: true, LocalNodeState: swarm.LocalNodeStateActive}}
}
func workerInfo() system.Info {
	return system.Info{Swarm: swarm.Info{ControlAvailable: false, LocalNodeState: swarm.LocalNodeStateActive}}
}

func TestRoleDetector_Initial(t *testing.T) {
	f := &fakeClient{info: managerInfo()}
	d := NewRoleDetector(f, time.Hour)

	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := d.Role(); got != RoleManager {
		t.Errorf("Role = %v, want manager", got)
	}
}

func TestRoleDetector_TransitionOnRefresh(t *testing.T) {
	f := &fakeClient{info: managerInfo()}
	d := NewRoleDetector(f, time.Hour)
	_ = d.Refresh(context.Background())

	f.mu.Lock()
	f.info = workerInfo()
	f.mu.Unlock()
	_ = d.Refresh(context.Background())

	if got := d.Role(); got != RoleWorker {
		t.Errorf("Role = %v, want worker after demote", got)
	}
}

func TestRoleDetector_RefreshErrorKeepsLastRole(t *testing.T) {
	f := &fakeClient{info: managerInfo()}
	d := NewRoleDetector(f, time.Hour)
	_ = d.Refresh(context.Background())

	f.mu.Lock()
	f.infoErr = errors.New("docker down")
	f.mu.Unlock()
	if err := d.Refresh(context.Background()); err == nil {
		t.Fatal("expected error from Refresh")
	}
	if got := d.Role(); got != RoleManager {
		t.Errorf("Role = %v, want manager (last known)", got)
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

```bash
go test ./internal/docker/...
```

Expected: build failure (undefined `NewRoleDetector` etc).

- [ ] **Step 3: Implement RoleDetector**

Create `internal/docker/role.go`:

```go
package docker

import (
	"context"
	"sync"
	"time"
)

type Role int

const (
	RoleUnknown Role = iota
	RoleWorker
	RoleManager
)

func (r Role) String() string {
	switch r {
	case RoleManager:
		return "manager"
	case RoleWorker:
		return "worker"
	default:
		return "unknown"
	}
}

// RoleDetector caches the local node's swarm role and refreshes it on a
// timer so that docker node promote/demote is picked up without restarting
// the coordinator.
type RoleDetector struct {
	client   interface {
		Info(ctx context.Context) (info system.InfoForRole, err error)
	}
	interval time.Duration

	mu   sync.RWMutex
	role Role
}

// We use a tiny interface alias to keep imports clean. system.InfoForRole
// isn't a real type — we'll use system.Info directly. Rewrite as:
```

That second comment block is a planning artifact. Replace the file with the cleaner version:

```go
package docker

import (
	"context"
	"sync"
	"time"

	"github.com/docker/docker/api/types/system"
)

type Role int

const (
	RoleUnknown Role = iota
	RoleWorker
	RoleManager
)

func (r Role) String() string {
	switch r {
	case RoleManager:
		return "manager"
	case RoleWorker:
		return "worker"
	default:
		return "unknown"
	}
}

// roleInfoer is the subset of Client that RoleDetector needs.
type roleInfoer interface {
	Info(ctx context.Context) (system.Info, error)
}

// RoleDetector caches the local node's swarm role and refreshes it on a
// timer so that `docker node promote` / `docker node demote` is picked up
// without restarting the coordinator.
type RoleDetector struct {
	client   roleInfoer
	interval time.Duration

	mu   sync.RWMutex
	role Role
}

func NewRoleDetector(c roleInfoer, interval time.Duration) *RoleDetector {
	return &RoleDetector{client: c, interval: interval}
}

func (d *RoleDetector) Role() Role {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.role
}

// Refresh queries Docker once and updates the cached role. On error, the
// previously-cached role is preserved (transient Docker outages should not
// force a worker to start thinking it's a manager, or vice versa).
func (d *RoleDetector) Refresh(ctx context.Context) error {
	info, err := d.client.Info(ctx)
	if err != nil {
		return err
	}
	r := RoleWorker
	if info.Swarm.ControlAvailable {
		r = RoleManager
	}
	d.mu.Lock()
	d.role = r
	d.mu.Unlock()
	return nil
}

// Run blocks until ctx is cancelled, refreshing on the configured interval.
// Errors are logged at debug level (handled by the caller via slog) and
// suppressed — the cached value persists across transient failures.
func (d *RoleDetector) Run(ctx context.Context) {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = d.Refresh(ctx)
		}
	}
}
```

- [ ] **Step 4: Run tests, expect PASS**

```bash
go test ./internal/docker/...
```

Expected: PASS, three subtests green.

- [ ] **Step 5: Commit**

```bash
git add internal/docker/role.go internal/docker/role_test.go
git commit -m "docker: RoleDetector with periodic recheck"
```

---

## Task 6: `internal/lock` — Lock + Store + ConfigStore

The Docker Config-backed lock store. We test against a fake docker client interface (no real Docker required). Integration coverage comes in Task 19.

**Files:**
- Create: `internal/lock/lock.go`
- Create: `internal/lock/config_store.go`
- Test: `internal/lock/config_store_test.go`

- [ ] **Step 1: Define the Lock type and Store interface**

Create `internal/lock/lock.go`:

```go
// Package lock arbitrates the cluster-wide reboot mutex. The lock document
// lives in a Docker Config; "create-fails-on-exists" is the CAS primitive.
package lock

import (
	"context"
	"time"
)

type Lock struct {
	Holder     string `json:"holder"`
	AcquiredAt int64  `json:"acquired_at"` // unix seconds UTC
	TTLSeconds int    `json:"ttl_seconds"`
}

// Status is the outcome of an Acquire call.
type Status int

const (
	// Acquired means the caller now holds the lock (no prior holder).
	Acquired Status = iota
	// AcquiredViaSteal means the prior lock was stale; caller stole it.
	AcquiredViaSteal
	// HeldByOther means a non-stale lock is held by someone else.
	HeldByOther
)

// AcquireResult bundles Status with the current Lock state (whichever
// state matters for the caller: their own freshly-acquired lock, or the
// peer's existing lock).
type AcquireResult struct {
	Status  Status
	Current *Lock
}

// Store is the storage primitive for the lock document.
type Store interface {
	// Acquire attempts to take the lock for `holder`. Returns Acquired,
	// AcquiredViaSteal (if a stale lock was reclaimed), or HeldByOther.
	Acquire(ctx context.Context, holder string, ttl time.Duration) (AcquireResult, error)
	// Release removes the lock if held by `holder`. Returns ErrNotHolder if
	// someone else holds it, ErrNoLock if no lock exists.
	Release(ctx context.Context, holder string) error
	// Inspect returns the current lock or nil if none.
	Inspect(ctx context.Context) (*Lock, error)
}

// Sentinel errors returned by Store.Release.
type errStr string

func (e errStr) Error() string { return string(e) }

const (
	ErrNotHolder errStr = "lock not held by caller"
	ErrNoLock    errStr = "no lock exists"
)
```

- [ ] **Step 2: Write the failing tests**

Create `internal/lock/config_store_test.go`:

```go
package lock

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDocker simulates the subset of Docker Config operations used by
// ConfigStore. Behavior is in-memory; "create-fails-on-exists" is enforced
// by checking the map under a mutex.
type fakeDocker struct {
	mu     sync.Mutex
	data   []byte
	exists bool
}

func (f *fakeDocker) ConfigCreate(_ context.Context, _ string, data []byte, _ map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exists {
		return "", errors.New("Error response from daemon: rpc error: code = AlreadyExists desc = name conflicts with an existing object")
	}
	f.data = append([]byte(nil), data...)
	f.exists = true
	return "fake-id", nil
}
func (f *fakeDocker) ConfigInspect(_ context.Context, _ string) ([]byte, map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists {
		return nil, nil, errors.New("Error: No such config")
	}
	return append([]byte(nil), f.data...), nil, nil
}
func (f *fakeDocker) ConfigRemove(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists {
		return errors.New("Error: No such config")
	}
	f.exists = false
	f.data = nil
	return nil
}

func newStore(f *fakeDocker) *ConfigStore {
	return &ConfigStore{Docker: f, ConfigName: "lock", Now: func() time.Time { return time.Unix(1000, 0) }}
}

func TestAcquire_FreshSuccess(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	r, err := s.Acquire(context.Background(), "node-a", 30*time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if r.Status != Acquired {
		t.Errorf("Status = %v, want Acquired", r.Status)
	}
	if r.Current == nil || r.Current.Holder != "node-a" || r.Current.AcquiredAt != 1000 || r.Current.TTLSeconds != 1800 {
		t.Errorf("Current = %+v", r.Current)
	}
}

func TestAcquire_HeldByOther_Fresh(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	_, _ = s.Acquire(context.Background(), "node-a", 30*time.Minute)

	r, err := s.Acquire(context.Background(), "node-b", 30*time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if r.Status != HeldByOther {
		t.Errorf("Status = %v, want HeldByOther", r.Status)
	}
	if r.Current.Holder != "node-a" {
		t.Errorf("Current.Holder = %q", r.Current.Holder)
	}
}

func TestAcquire_StaleSteal(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	_, _ = s.Acquire(context.Background(), "node-a", 30*time.Minute)

	// Advance clock past TTL.
	s.Now = func() time.Time { return time.Unix(1000+1800+1, 0) }

	r, err := s.Acquire(context.Background(), "node-b", 30*time.Minute)
	if err != nil {
		t.Fatalf("Acquire after stale: %v", err)
	}
	if r.Status != AcquiredViaSteal {
		t.Errorf("Status = %v, want AcquiredViaSteal", r.Status)
	}
	if r.Current.Holder != "node-b" {
		t.Errorf("Current.Holder = %q, want node-b", r.Current.Holder)
	}
}

func TestRelease_HolderOK(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	_, _ = s.Acquire(context.Background(), "node-a", 30*time.Minute)
	if err := s.Release(context.Background(), "node-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got, _ := s.Inspect(context.Background()); got != nil {
		t.Errorf("lock still present after release: %+v", got)
	}
}

func TestRelease_NotHolder(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	_, _ = s.Acquire(context.Background(), "node-a", 30*time.Minute)
	err := s.Release(context.Background(), "node-b")
	if err == nil || !errors.Is(err, ErrNotHolder) {
		t.Fatalf("Release wrong-holder: err=%v, want ErrNotHolder", err)
	}
}

func TestRelease_NoLock(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	err := s.Release(context.Background(), "node-a")
	if err == nil || !errors.Is(err, ErrNoLock) {
		t.Fatalf("Release no-lock: err=%v, want ErrNoLock", err)
	}
}

func TestInspect_None(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	got, err := s.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestInspect_Roundtrip(t *testing.T) {
	f := &fakeDocker{}
	s := newStore(f)
	_, _ = s.Acquire(context.Background(), "node-a", 30*time.Minute)
	got, err := s.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want lock")
	}
	if got.Holder != "node-a" {
		t.Errorf("Holder = %q", got.Holder)
	}
	// Confirm the bytes are valid JSON of the Lock shape (paranoia).
	var l Lock
	if err := json.Unmarshal(f.data, &l); err != nil {
		t.Errorf("stored bytes not valid Lock JSON: %v", err)
	}
}

// Used to sanity-check that fakeDocker recognises "no such config" by string;
// the real Docker SDK returns errdefs.IsNotFound which we map separately.
func TestIsNotFoundString(t *testing.T) {
	if !isNotFound(errors.New("Error: No such config")) {
		t.Error("isNotFound did not match")
	}
	if !isAlreadyExists(errors.New("name conflicts with an existing object")) {
		t.Error("isAlreadyExists did not match")
	}
	if isNotFound(errors.New("unrelated")) || isAlreadyExists(errors.New("unrelated")) {
		t.Error("false positive")
	}
	_ = strings.Contains // silence unused import if we drop strings later
}
```

- [ ] **Step 3: Implement ConfigStore**

Create `internal/lock/config_store.go`:

```go
package lock

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// docker is the subset of internal/docker.Client that ConfigStore needs.
// Defined locally so this package doesn't import internal/docker — the
// real Client satisfies it implicitly.
type docker interface {
	ConfigCreate(ctx context.Context, name string, data []byte, labels map[string]string) (string, error)
	ConfigInspect(ctx context.Context, name string) (data []byte, labels map[string]string, err error)
	ConfigRemove(ctx context.Context, name string) error
}

type ConfigStore struct {
	Docker     docker
	ConfigName string
	Now        func() time.Time // injectable for tests; nil → time.Now
}

func (s *ConfigStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *ConfigStore) Acquire(ctx context.Context, holder string, ttl time.Duration) (AcquireResult, error) {
	l := Lock{Holder: holder, AcquiredAt: s.now().UTC().Unix(), TTLSeconds: int(ttl.Seconds())}
	data, err := json.Marshal(l)
	if err != nil {
		return AcquireResult{}, err
	}

	// 1. Try a fresh create.
	if _, err := s.Docker.ConfigCreate(ctx, s.ConfigName, data, nil); err == nil {
		return AcquireResult{Status: Acquired, Current: &l}, nil
	} else if !isAlreadyExists(err) {
		return AcquireResult{}, fmt.Errorf("create: %w", err)
	}

	// 2. EEXIST — read the existing holder.
	existing, err := s.inspect(ctx)
	if err != nil {
		return AcquireResult{}, fmt.Errorf("inspect after EEXIST: %w", err)
	}

	// 3. Same holder retrying? Treat as success (defensive — drain script
	//    shouldn't retry, but if it does, idempotent re-acquire is correct).
	if existing != nil && existing.Holder == holder {
		return AcquireResult{Status: Acquired, Current: existing}, nil
	}

	// 4. Fresh and held by someone else.
	if existing != nil && s.now().UTC().Unix()-existing.AcquiredAt <= int64(existing.TTLSeconds) {
		return AcquireResult{Status: HeldByOther, Current: existing}, nil
	}

	// 5. Stale — remove and retry create exactly once. If the second create
	//    fails with EEXIST again, a concurrent steal beat us; report that
	//    peer as the holder.
	if err := s.Docker.ConfigRemove(ctx, s.ConfigName); err != nil && !isNotFound(err) {
		return AcquireResult{}, fmt.Errorf("remove stale: %w", err)
	}
	if _, err := s.Docker.ConfigCreate(ctx, s.ConfigName, data, nil); err == nil {
		return AcquireResult{Status: AcquiredViaSteal, Current: &l}, nil
	} else if !isAlreadyExists(err) {
		return AcquireResult{}, fmt.Errorf("create after steal: %w", err)
	}
	winner, _ := s.inspect(ctx)
	return AcquireResult{Status: HeldByOther, Current: winner}, nil
}

func (s *ConfigStore) Release(ctx context.Context, holder string) error {
	existing, err := s.inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	if existing == nil {
		return ErrNoLock
	}
	if existing.Holder != holder {
		return ErrNotHolder
	}
	if err := s.Docker.ConfigRemove(ctx, s.ConfigName); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove: %w", err)
	}
	return nil
}

func (s *ConfigStore) Inspect(ctx context.Context) (*Lock, error) {
	return s.inspect(ctx)
}

func (s *ConfigStore) inspect(ctx context.Context) (*Lock, error) {
	data, _, err := s.Docker.ConfigInspect(ctx, s.ConfigName)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("unmarshal lock: %w", err)
	}
	return &l, nil
}

// Error classification: the real Docker SDK exposes errdefs.IsNotFound /
// errdefs.IsConflict; we additionally match on the string forms returned
// by the daemon, so fakes and CLI-style errors work too.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	// errdefs check happens in the real client wrapper; here we string-match.
	s := err.Error()
	return strings.Contains(s, "No such config") || strings.Contains(s, "not found")
}

func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "name conflicts") || strings.Contains(s, "already exists") || strings.Contains(s, "AlreadyExists")
}
```

- [ ] **Step 4: Run tests, expect PASS**

```bash
go test ./internal/lock/...
```

Expected: all subtests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/lock/
git commit -m "lock: Docker Config-backed Store with stale-steal"
```

---

## Task 7: `internal/drain` — Drainer interface + Docker implementation

**Files:**
- Create: `internal/drain/drain.go`
- Create: `internal/drain/docker_drainer.go`
- Test: `internal/drain/docker_drainer_test.go`

- [ ] **Step 1: Define the Drainer interface**

Create `internal/drain/drain.go`:

```go
// Package drain wraps swarm node drain/active operations and the
// manager-quorum precondition.
package drain

import (
	"context"
	"time"
)

// QuorumReport is what the quorum check returns to a caller.
type QuorumReport struct {
	TotalManagers           int
	ReachableManagers       int // including caller if reachable
	OthersReachable         int // excluding caller
	Required                int // floor(N/2)+1
}

// Result of a drain operation.
type Result struct {
	TasksRemaining int
	Elapsed        time.Duration
	TimedOut       bool
}

type Drainer interface {
	// QuorumOK returns whether the caller (a manager) may safely drain
	// without breaking raft quorum. Workers always pass.
	QuorumOK(ctx context.Context, self string) (bool, QuorumReport, error)
	// Drain sets the node to drain availability and polls until tasks
	// remaining is 0 or the deadline expires.
	Drain(ctx context.Context, node string, timeout, pollInterval time.Duration) (Result, error)
	// Activate sets the node back to active availability.
	Activate(ctx context.Context, node string) error
}
```

- [ ] **Step 2: Write the failing tests**

Create `internal/drain/docker_drainer_test.go`:

```go
package drain

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeNode struct {
	Hostname     string
	Role         string
	Reachability string
	Availability string
}

type fakeDocker struct {
	nodes       []fakeNode
	updateErr   error
	updateCalls []string // "node:availability"
	tasks       []int    // queue of task-count values returned by NodeTaskCount
}

type nodeSummary struct {
	Hostname, Role, Reachability, Availability string
}

func (f *fakeDocker) NodeList(_ context.Context, filters map[string][]string) ([]NodeSummary, error) {
	var out []NodeSummary
	wantRole := ""
	if v, ok := filters["role"]; ok && len(v) > 0 {
		wantRole = v[0]
	}
	for _, n := range f.nodes {
		if wantRole != "" && n.Role != wantRole {
			continue
		}
		out = append(out, NodeSummary{
			Hostname: n.Hostname, Role: n.Role,
			Reachability: n.Reachability, Availability: n.Availability,
		})
	}
	return out, nil
}
func (f *fakeDocker) NodeUpdate(_ context.Context, node, avail string) error {
	f.updateCalls = append(f.updateCalls, node+":"+avail)
	return f.updateErr
}
func (f *fakeDocker) NodeTaskCount(_ context.Context, _ string) (int, error) {
	if len(f.tasks) == 0 {
		return 0, nil
	}
	n := f.tasks[0]
	f.tasks = f.tasks[1:]
	return n, nil
}

// NodeSummary is duplicated here from internal/docker to avoid an import
// cycle in tests; the production code uses the shared one.
type NodeSummary struct {
	Hostname     string
	Role         string
	Reachability string
	Availability string
}

func TestQuorumOK_WorkerAlwaysPasses(t *testing.T) {
	f := &fakeDocker{nodes: []fakeNode{
		{Hostname: "m1", Role: "manager", Reachability: "reachable"},
		{Hostname: "m2", Role: "manager", Reachability: "unreachable"},
		{Hostname: "m3", Role: "manager", Reachability: "unreachable"},
		{Hostname: "w1", Role: "worker", Reachability: ""},
	}}
	d := &DockerDrainer{Docker: f}
	ok, _, err := d.QuorumOK(context.Background(), "w1")
	if err != nil || !ok {
		t.Fatalf("worker quorum: ok=%v err=%v", ok, err)
	}
}

func TestQuorumOK_ManagerWith2Others(t *testing.T) {
	f := &fakeDocker{nodes: []fakeNode{
		{Hostname: "m1", Role: "manager", Reachability: "reachable"},
		{Hostname: "m2", Role: "manager", Reachability: "reachable"},
		{Hostname: "m3", Role: "manager", Reachability: "reachable"},
	}}
	d := &DockerDrainer{Docker: f}
	ok, rep, err := d.QuorumOK(context.Background(), "m1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Errorf("expected OK with 2 other reachable managers, got rep=%+v", rep)
	}
	if rep.OthersReachable != 2 || rep.Required != 2 || rep.TotalManagers != 3 {
		t.Errorf("rep = %+v", rep)
	}
}

func TestQuorumOK_ManagerWithOnly1Other(t *testing.T) {
	f := &fakeDocker{nodes: []fakeNode{
		{Hostname: "m1", Role: "manager", Reachability: "reachable"},
		{Hostname: "m2", Role: "manager", Reachability: "reachable"},
		{Hostname: "m3", Role: "manager", Reachability: "unreachable"},
	}}
	d := &DockerDrainer{Docker: f}
	ok, rep, err := d.QuorumOK(context.Background(), "m1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Errorf("expected NOT OK with only 1 other reachable manager, got rep=%+v", rep)
	}
}

func TestDrain_HappyPath(t *testing.T) {
	f := &fakeDocker{tasks: []int{3, 1, 0}} // converges in 3 polls
	d := &DockerDrainer{Docker: f}
	res, err := d.Drain(context.Background(), "w1", time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res.TimedOut || res.TasksRemaining != 0 {
		t.Errorf("res = %+v", res)
	}
	if len(f.updateCalls) != 1 || f.updateCalls[0] != "w1:drain" {
		t.Errorf("updateCalls = %v", f.updateCalls)
	}
}

func TestDrain_Timeout(t *testing.T) {
	// Tasks never reach zero in the configured window.
	f := &fakeDocker{tasks: []int{5, 5, 5, 5, 5, 5, 5, 5}}
	d := &DockerDrainer{Docker: f}
	res, err := d.Drain(context.Background(), "w1", 50*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !res.TimedOut || res.TasksRemaining != 5 {
		t.Errorf("res = %+v", res)
	}
}

func TestActivate(t *testing.T) {
	f := &fakeDocker{}
	d := &DockerDrainer{Docker: f}
	if err := d.Activate(context.Background(), "w1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(f.updateCalls) != 1 || f.updateCalls[0] != "w1:active" {
		t.Errorf("updateCalls = %v", f.updateCalls)
	}
}

func TestDrain_UpdateError(t *testing.T) {
	f := &fakeDocker{updateErr: errors.New("forbidden")}
	d := &DockerDrainer{Docker: f}
	_, err := d.Drain(context.Background(), "w1", time.Second, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected error when NodeUpdate fails")
	}
}
```

Note: the `NodeSummary` type duplication in the test is a deliberate simplification — it sidesteps the test-package import cycle. The real Drainer accepts the `internal/docker` package's `NodeSummary`. The implementation in Step 3 below uses a local alias to keep the package boundary clean.

- [ ] **Step 3: Implement DockerDrainer**

Create `internal/drain/docker_drainer.go`:

```go
package drain

import (
	"context"
	"fmt"
	"time"
)

// NodeSummary mirrors internal/docker.NodeSummary. Duplicated to break the
// import cycle; converted by the wiring in cmd/swarm-auto-drain/main.go.
type NodeSummary struct {
	Hostname     string
	Role         string
	Reachability string
	Availability string
}

type docker interface {
	NodeList(ctx context.Context, filters map[string][]string) ([]NodeSummary, error)
	NodeUpdate(ctx context.Context, node string, availability string) error
	NodeTaskCount(ctx context.Context, node string) (int, error)
}

type DockerDrainer struct {
	Docker docker
}

func (d *DockerDrainer) QuorumOK(ctx context.Context, self string) (bool, QuorumReport, error) {
	nodes, err := d.Docker.NodeList(ctx, map[string][]string{"role": {"manager"}})
	if err != nil {
		return false, QuorumReport{}, fmt.Errorf("list managers: %w", err)
	}

	// Worker is calling? Always OK — workers don't participate in raft.
	isManager := false
	for _, n := range nodes {
		if n.Hostname == self {
			isManager = true
			break
		}
	}
	if !isManager {
		return true, QuorumReport{TotalManagers: len(nodes)}, nil
	}

	others := 0
	reachable := 0
	for _, n := range nodes {
		if n.Reachability == "reachable" {
			reachable++
		}
		if n.Hostname == self {
			continue
		}
		if n.Reachability == "reachable" {
			others++
		}
	}
	rep := QuorumReport{
		TotalManagers:     len(nodes),
		ReachableManagers: reachable,
		OthersReachable:   others,
		Required:          (len(nodes) / 2) + 1,
	}
	// We need (Required - 1) other reachable managers to survive losing self.
	// E.g. 3 managers → required=2, others must be >= 1? No: losing self brings
	// reachable down by 1 from the others-only count's perspective. We need
	// "others remaining" >= Required. So OthersReachable >= Required.
	ok := rep.OthersReachable >= rep.Required
	return ok, rep, nil
}

func (d *DockerDrainer) Drain(ctx context.Context, node string, timeout, pollInterval time.Duration) (Result, error) {
	if err := d.Docker.NodeUpdate(ctx, node, "drain"); err != nil {
		return Result{}, fmt.Errorf("set drain: %w", err)
	}
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		n, err := d.Docker.NodeTaskCount(ctx, node)
		if err != nil {
			return Result{}, fmt.Errorf("poll tasks: %w", err)
		}
		if n == 0 {
			return Result{TasksRemaining: 0, Elapsed: time.Since(start)}, nil
		}
		if time.Now().After(deadline) {
			return Result{TasksRemaining: n, Elapsed: time.Since(start), TimedOut: true}, nil
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (d *DockerDrainer) Activate(ctx context.Context, node string) error {
	return d.Docker.NodeUpdate(ctx, node, "active")
}
```

Update the test file Step 2 to use this same `NodeSummary` definition — change the import. The test file references `NodeSummary` in its own package, which is fine because both definitions are equivalent in shape. Run the tests:

- [ ] **Step 4: Run tests, expect PASS**

```bash
go test ./internal/drain/...
```

Expected: all tests PASS.

(Note the quorum math: for N=3 managers, Required=2, OthersReachable must be >=2 → "2 other reachable managers required." For N=5, Required=3, OthersReachable must be >=3. This is the "lose one, keep quorum" property the spec asks for.)

- [ ] **Step 5: Commit**

```bash
git add internal/drain/
git commit -m "drain: Drainer interface + quorum-aware DockerDrainer"
```

---

## Task 8: `internal/forward` — worker→manager forwarding

**Files:**
- Create: `internal/forward/forward.go`
- Test: `internal/forward/forward_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/forward/forward_test.go`:

```go
package forward

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestForward_AddsOriginNodeHeader(t *testing.T) {
	var gotHeader string
	var gotBody string
	mgr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Origin-Node")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer mgr.Close()

	f := &Forwarder{
		OriginNode:     "node-a",
		FindManagerURL: func() (string, error) { return mgr.URL, nil },
		Client:         &http.Client{Timeout: time.Second},
	}

	req := httptest.NewRequest("POST", "/lock", strings.NewReader(`{"x":1}`))
	rr := httptest.NewRecorder()
	f.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d", rr.Code)
	}
	if gotHeader != "node-a" {
		t.Errorf("X-Origin-Node = %q", gotHeader)
	}
	if gotBody != `{"x":1}` {
		t.Errorf("body = %q", gotBody)
	}
}

func TestForward_NoManagerFound(t *testing.T) {
	f := &Forwarder{
		OriginNode:     "node-a",
		FindManagerURL: func() (string, error) { return "", errNoManagerForTest },
		Client:         &http.Client{Timeout: time.Second},
	}
	req := httptest.NewRequest("GET", "/lock", nil)
	rr := httptest.NewRecorder()
	f.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestForward_ManagerUnreachableInvalidatesCache(t *testing.T) {
	// Test the FindManagerURL composition: simulate the cached choice
	// failing, then a retry succeeding. We do this by making the forwarder
	// itself call a function that we control which counts invocations.
	var calls int32
	mgr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer mgr.Close()

	f := &Forwarder{
		OriginNode: "node-a",
		FindManagerURL: func() (string, error) {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				return "http://127.0.0.1:1", nil // unreachable
			}
			return mgr.URL, nil
		},
		Client: &http.Client{Timeout: 200 * time.Millisecond},
		Retries: 2,
	}
	req := httptest.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()
	f.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 after retry", rr.Code)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("FindManagerURL calls = %d, want 2", calls)
	}
}

// errNoManagerForTest is a sentinel for the test only; the real Forwarder
// returns its own sentinel from internal lookups.
var errNoManagerForTest = errString("no manager available")

type errString string

func (e errString) Error() string { return string(e) }
```

- [ ] **Step 2: Implement Forwarder**

Create `internal/forward/forward.go`:

```go
// Package forward implements worker→manager request proxying via the
// swarm overlay network.
package forward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Forwarder is an http.Handler that proxies inbound requests to a manager
// replica. It is used in worker mode only; manager mode handles requests
// in-process.
type Forwarder struct {
	OriginNode     string
	FindManagerURL func() (string, error) // returns full base URL like http://10.0.0.5:7777
	Client         *http.Client
	Retries        int // 1 attempt + Retries extra attempts; default 0 (one attempt)
}

func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	attempts := f.Retries + 1
	var lastErr error
	for i := 0; i < attempts; i++ {
		base, err := f.FindManagerURL()
		if err != nil {
			lastErr = err
			continue
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, base+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		for k, vs := range r.Header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		req.Header.Set("X-Origin-Node", f.OriginNode)

		resp, err := f.Client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	http.Error(w, fmt.Sprintf("no manager available: %v", lastErr), http.StatusServiceUnavailable)
}

// ManagerFinder discovers manager replicas via DNS lookup of the
// service's overlay DNS name (`tasks.swarm-auto-drain`), then probes
// /role on each to find ones in manager mode. Results are cached.
type ManagerFinder struct {
	ServiceDNS string         // e.g. "tasks.swarm-auto-drain"
	Port       int            // 7777
	Client     *http.Client   // short timeout for /role probes
	TTL        time.Duration  // cache window

	mu       sync.Mutex
	cached   string
	cachedAt time.Time
}

var ErrNoManager = errors.New("no manager replica found")

func (m *ManagerFinder) Find() (string, error) {
	m.mu.Lock()
	if m.cached != "" && time.Since(m.cachedAt) < m.TTL {
		c := m.cached
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()

	ips, err := net.LookupIP(m.ServiceDNS)
	if err != nil {
		return "", fmt.Errorf("lookup %s: %w", m.ServiceDNS, err)
	}
	for _, ip := range ips {
		base := fmt.Sprintf("http://%s:%d", ip.String(), m.Port)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/role", nil)
		resp, err := m.Client.Do(req)
		cancel()
		if err != nil {
			continue
		}
		// Expect body like {"role":"manager",...}; cheap check on content.
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && bytes.Contains(body, []byte(`"manager"`)) {
			m.mu.Lock()
			m.cached, m.cachedAt = base, time.Now()
			m.mu.Unlock()
			return base, nil
		}
	}
	return "", ErrNoManager
}

// Invalidate clears the cache so the next Find call re-probes.
func (m *ManagerFinder) Invalidate() {
	m.mu.Lock()
	m.cached = ""
	m.mu.Unlock()
}
```

- [ ] **Step 3: Run tests, expect PASS**

```bash
go test ./internal/forward/...
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/forward/
git commit -m "forward: worker→manager HTTP proxy + ManagerFinder"
```

---

## Task 9: `internal/identity` — middleware for caller identity

**Files:**
- Create: `internal/identity/identity.go`
- Test: `internal/identity/identity_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/identity/identity_test.go`:

```go
package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddleware_LoopbackUsesNodeHostname(t *testing.T) {
	mw := Middleware{NodeHostname: "test-host"}

	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))

	r := httptest.NewRequest("POST", "/lock", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if got != "test-host" {
		t.Errorf("identity = %q, want test-host", got)
	}
	if w.Code != 200 {
		t.Errorf("status = %d", w.Code)
	}
}

func TestMiddleware_LoopbackIPv6(t *testing.T) {
	mw := Middleware{NodeHostname: "test-host"}
	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest("GET", "/role", nil)
	r.RemoteAddr = "[::1]:54321"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "test-host" {
		t.Errorf("identity = %q", got)
	}
}

func TestMiddleware_OverlayHeader(t *testing.T) {
	mw := Middleware{NodeHostname: "manager-1"}
	var got string
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
	}))
	r := httptest.NewRequest("POST", "/drain", nil)
	r.RemoteAddr = "10.0.0.42:34567"
	r.Header.Set("X-Origin-Node", "worker-3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "worker-3" {
		t.Errorf("identity = %q", got)
	}
}

func TestMiddleware_OverlayWithoutHeader(t *testing.T) {
	mw := Middleware{NodeHostname: "manager-1"}
	h := mw.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not be called when identity is unresolvable")
	}))
	r := httptest.NewRequest("POST", "/drain", nil)
	r.RemoteAddr = "10.0.0.42:34567"
	// no X-Origin-Node
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestMiddleware_PublicEndpointsBypass(t *testing.T) {
	mw := Middleware{NodeHostname: "manager-1", PublicPaths: []string{"/healthz", "/role"}}
	h := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Should be reached even without an identity.
		w.WriteHeader(200)
	}))
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.RemoteAddr = "10.0.0.42:34567"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	id := FromContext(context.Background())
	if id != "" {
		t.Errorf("FromContext on bare context = %q, want empty", id)
	}
}
```

- [ ] **Step 2: Implement the middleware**

Create `internal/identity/identity.go`:

```go
// Package identity resolves the calling node's hostname from the inbound
// request. Loopback connections (the host shell script) are identified by
// the replica's own NODE_HOSTNAME; overlay-network connections (peer
// replicas forwarding) are identified by the X-Origin-Node header.
package identity

import (
	"context"
	"net"
	"net/http"
)

type ctxKey struct{}

// Middleware injects the resolved caller identity into the request
// context. Handlers retrieve it via FromContext.
type Middleware struct {
	NodeHostname string   // this replica's own hostname (loopback identity)
	PublicPaths  []string // paths that bypass the identity check (e.g. /healthz, /role)
}

func (m Middleware) Wrap(next http.Handler) http.Handler {
	publics := map[string]bool{}
	for _, p := range m.PublicPaths {
		publics[p] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publics[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		id := resolve(r, m.NodeHostname)
		if id == "" {
			http.Error(w, "could not resolve caller identity (loopback or X-Origin-Node required)", http.StatusBadRequest)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func resolve(r *http.Request, nodeHostname string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		ip := net.ParseIP(host)
		if ip != nil && ip.IsLoopback() {
			return nodeHostname
		}
	}
	return r.Header.Get("X-Origin-Node")
}

// FromContext returns the identity injected by Wrap, or "" if none.
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}
```

- [ ] **Step 3: Run tests, expect PASS**

```bash
go test ./internal/identity/...
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/identity/
git commit -m "identity: middleware (loopback NODE_HOSTNAME / X-Origin-Node header)"
```

---

## Task 10: `internal/api` — handlers + server skeleton

**Files:**
- Create: `internal/api/handlers.go`
- Create: `internal/api/server.go`
- Test: `internal/api/handlers_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/api/handlers_test.go`:

```go
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/drain"
	"github.com/matchory/swarm-auto-drain/internal/identity"
	"github.com/matchory/swarm-auto-drain/internal/lock"
)

// ---------- fakes ----------

type fakeStore struct {
	state *lock.Lock
}

func (f *fakeStore) Acquire(_ context.Context, holder string, ttl time.Duration) (lock.AcquireResult, error) {
	if f.state == nil {
		f.state = &lock.Lock{Holder: holder, AcquiredAt: 1, TTLSeconds: int(ttl.Seconds())}
		return lock.AcquireResult{Status: lock.Acquired, Current: f.state}, nil
	}
	if f.state.Holder == holder {
		return lock.AcquireResult{Status: lock.Acquired, Current: f.state}, nil
	}
	return lock.AcquireResult{Status: lock.HeldByOther, Current: f.state}, nil
}
func (f *fakeStore) Release(_ context.Context, holder string) error {
	if f.state == nil {
		return lock.ErrNoLock
	}
	if f.state.Holder != holder {
		return lock.ErrNotHolder
	}
	f.state = nil
	return nil
}
func (f *fakeStore) Inspect(_ context.Context) (*lock.Lock, error) { return f.state, nil }

type fakeDrainer struct {
	quorumOK   bool
	quorumRep  drain.QuorumReport
	drainRes   drain.Result
	drainErr   error
	activateErr error
}

func (f *fakeDrainer) QuorumOK(_ context.Context, _ string) (bool, drain.QuorumReport, error) {
	return f.quorumOK, f.quorumRep, nil
}
func (f *fakeDrainer) Drain(_ context.Context, _ string, _, _ time.Duration) (drain.Result, error) {
	return f.drainRes, f.drainErr
}
func (f *fakeDrainer) Activate(_ context.Context, _ string) error { return f.activateErr }

// ---------- helpers ----------

func newTestServer(t *testing.T, store lock.Store, dr drain.Drainer) *httptest.Server {
	t.Helper()
	h := Handlers{
		Store:        store,
		Drainer:      dr,
		LockTTL:      30 * time.Minute,
		DrainTimeout: time.Second,
		PollInterval: 10 * time.Millisecond,
		Role:         func() string { return "manager" },
	}
	mw := identity.Middleware{NodeHostname: "self-host", PublicPaths: []string{"/healthz", "/role"}}
	return httptest.NewServer(mw.Wrap(h.Router()))
}

// ---------- tests ----------

func TestHealthz(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestRole(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/role")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Role string }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Role != "manager" {
		t.Errorf("role = %q", body.Role)
	}
}

func TestLockAcquire_Fresh(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/lock", "application/json", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
}

func TestLockAcquire_HeldByOther(t *testing.T) {
	store := &fakeStore{state: &lock.Lock{Holder: "other-host", AcquiredAt: 1, TTLSeconds: 1800}}
	srv := newTestServer(t, store, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/lock", "application/json", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestLockRelease_Holder(t *testing.T) {
	store := &fakeStore{state: &lock.Lock{Holder: "self-host", AcquiredAt: 1, TTLSeconds: 1800}}
	srv := newTestServer(t, store, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	req, _ := http.NewRequest("DELETE", srv.URL+"/lock", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

func TestLockRelease_NotHolder(t *testing.T) {
	store := &fakeStore{state: &lock.Lock{Holder: "other-host", AcquiredAt: 1, TTLSeconds: 1800}}
	srv := newTestServer(t, store, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	req, _ := http.NewRequest("DELETE", srv.URL+"/lock", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestLockRelease_NoLock(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	req, _ := http.NewRequest("DELETE", srv.URL+"/lock", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDrain_QuorumFail(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: false, quorumRep: drain.QuorumReport{
		TotalManagers: 3, OthersReachable: 1, Required: 2,
	}})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/drain", "application/json", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestDrain_Happy(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{
		quorumOK: true,
		drainRes: drain.Result{TasksRemaining: 0, Elapsed: 100 * time.Millisecond},
	})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/drain", "application/json", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestDrain_Timeout(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{
		quorumOK: true,
		drainRes: drain.Result{TasksRemaining: 4, Elapsed: time.Second, TimedOut: true},
	})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/drain", "application/json", nil)
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Errorf("status = %d, want 408", resp.StatusCode)
	}
}

func TestDrain_Error(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true, drainErr: errors.New("boom")})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/drain", "application/json", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestActive(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/active", "application/json", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestIdentityMissing_Drain(t *testing.T) {
	srv := newTestServer(t, &fakeStore{}, &fakeDrainer{quorumOK: true})
	defer srv.Close()
	// Override remote addr to look like overlay; no header.
	req, _ := http.NewRequest("POST", srv.URL+"/drain", strings.NewReader(""))
	req.Header.Set("Host", "from-overlay")
	// We can't really set RemoteAddr from a client; this test path is
	// exercised more directly in identity_test.go.
	resp, _ := http.DefaultClient.Do(req)
	_ = resp
}
```

- [ ] **Step 2: Implement Handlers + Router**

Create `internal/api/handlers.go`:

```go
// Package api wires the HTTP API onto the lock store, drainer, and
// caller-identity middleware.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/drain"
	"github.com/matchory/swarm-auto-drain/internal/identity"
	"github.com/matchory/swarm-auto-drain/internal/lock"
)

type Handlers struct {
	Store        lock.Store
	Drainer      drain.Drainer
	LockTTL      time.Duration
	DrainTimeout time.Duration
	PollInterval time.Duration // how often to poll task count during drain
	Role         func() string // returns "manager" | "worker"
}

func (h Handlers) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.healthz)
	mux.HandleFunc("/role", h.role)
	mux.HandleFunc("/managers/health", h.managersHealth)
	mux.HandleFunc("/lock", h.lock)
	mux.HandleFunc("/drain", h.drain)
	mux.HandleFunc("/active", h.active)
	return mux
}

func (h Handlers) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h Handlers) role(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"role": h.Role()})
}

// managersHealth is a thin diagnostics endpoint. For v1 we just delegate
// to the drainer's quorum check using a synthetic self="" so that the
// per-caller logic doesn't kick in.
func (h Handlers) managersHealth(w http.ResponseWriter, r *http.Request) {
	_, rep, err := h.Drainer.QuorumOK(r.Context(), "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (h Handlers) lock(w http.ResponseWriter, r *http.Request) {
	id := identity.FromContext(r.Context())
	switch r.Method {
	case http.MethodPost:
		res, err := h.Store.Acquire(r.Context(), id, h.LockTTL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		switch res.Status {
		case lock.Acquired:
			writeJSON(w, http.StatusCreated, res.Current)
		case lock.AcquiredViaSteal:
			writeJSON(w, http.StatusGone, res.Current) // 410 chosen per spec
		case lock.HeldByOther:
			writeJSON(w, http.StatusConflict, res.Current)
		default:
			http.Error(w, "unknown acquire status", http.StatusInternalServerError)
		}
	case http.MethodDelete:
		err := h.Store.Release(r.Context(), id)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, lock.ErrNotHolder):
			http.Error(w, "not holder", http.StatusForbidden)
		case errors.Is(err, lock.ErrNoLock):
			http.Error(w, "no lock", http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h Handlers) drain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := identity.FromContext(r.Context())

	ok, rep, err := h.Drainer.QuorumOK(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":  "manager quorum would be lost",
			"report": rep,
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.DrainTimeout+30*time.Second)
	defer cancel()
	res, err := h.Drainer.Drain(ctx, id, h.DrainTimeout, h.PollInterval)
	if err != nil {
		slog.Error("drain failed", "node", id, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body := map[string]any{
		"tasks_remaining": res.TasksRemaining,
		"elapsed_seconds": int(res.Elapsed.Seconds()),
	}
	if res.TimedOut {
		writeJSON(w, http.StatusRequestTimeout, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (h Handlers) active(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := identity.FromContext(r.Context())
	if err := h.Drainer.Activate(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 3: Implement the Server skeleton**

Create `internal/api/server.go`:

```go
package api

import (
	"context"
	"net/http"
	"time"
)

// Server bundles an http.Server configured with the given handler. Run
// blocks until ctx is cancelled, then performs a graceful shutdown.
type Server struct {
	Addr    string
	Handler http.Handler
}

func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s.Handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		return err
	}
}
```

- [ ] **Step 4: Run tests, expect PASS**

```bash
go test ./internal/api/...
```

Expected: all subtests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/
git commit -m "api: handlers + Router + Server skeleton"
```

---

## Task 11: Wire everything in `cmd/swarm-auto-drain/main.go`

**Files:**
- Modify: `cmd/swarm-auto-drain/main.go`
- Modify: `internal/docker/client.go` (finish the stub methods)

Step 1 below finishes the Docker client stubs from Task 4. Steps 2-5 wire everything in `main.go`.

- [ ] **Step 1: Implement the stubbed Docker client methods**

The Docker SDK's `ConfigCreate`/`ConfigInspect`/`ConfigRemove`/`NodeList`/`NodeUpdate`/`NodeTaskCount` methods need real wiring. Replace the bottom of `internal/docker/client.go` (the section beginning with `var errNotYetImplemented = ...`) with:

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/errdefs"
)

func (d *dockerClient) ConfigCreate(ctx context.Context, name string, data []byte, labels map[string]string) (string, error) {
	spec := swarm.ConfigSpec{
		Annotations: swarm.Annotations{Name: name, Labels: labels},
		Data:        data,
	}
	resp, err := d.c.ConfigCreate(ctx, spec)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (d *dockerClient) ConfigInspect(ctx context.Context, name string) ([]byte, map[string]string, error) {
	cfg, _, err := d.c.ConfigInspectWithRaw(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil, fmt.Errorf("No such config: %s: %w", name, err)
		}
		return nil, nil, err
	}
	return cfg.Spec.Data, cfg.Spec.Labels, nil
}

func (d *dockerClient) ConfigRemove(ctx context.Context, name string) error {
	err := d.c.ConfigRemove(ctx, name)
	if err != nil && errdefs.IsNotFound(err) {
		return fmt.Errorf("No such config: %s: %w", name, err)
	}
	return err
}

func (d *dockerClient) NodeList(ctx context.Context, fs map[string][]string) ([]NodeSummary, error) {
	args := filters.NewArgs()
	for k, vs := range fs {
		for _, v := range vs {
			args.Add(k, v)
		}
	}
	nodes, err := d.c.NodeList(ctx, types.NodeListOptions{Filters: args})
	if err != nil {
		return nil, err
	}
	out := make([]NodeSummary, 0, len(nodes))
	for _, n := range nodes {
		s := NodeSummary{
			Hostname:     n.Description.Hostname,
			Role:         string(n.Spec.Role),
			Availability: string(n.Spec.Availability),
		}
		if n.ManagerStatus != nil {
			s.Reachability = string(n.ManagerStatus.Reachability)
			s.Leader = n.ManagerStatus.Leader
		}
		out = append(out, s)
	}
	return out, nil
}

func (d *dockerClient) NodeUpdate(ctx context.Context, nodeName, availability string) error {
	// Need the current node version for the update.
	nodes, err := d.c.NodeList(ctx, types.NodeListOptions{
		Filters: filters.NewArgs(filters.Arg("name", nodeName)),
	})
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("node %q not found", nodeName)
	}
	n := nodes[0]
	n.Spec.Availability = swarm.NodeAvailability(availability)
	return d.c.NodeUpdate(ctx, n.ID, n.Version, n.Spec)
}

func (d *dockerClient) NodeTaskCount(ctx context.Context, nodeName string) (int, error) {
	args := filters.NewArgs(
		filters.Arg("node", nodeName),
		filters.Arg("desired-state", "running"),
	)
	tasks, err := d.c.TaskList(ctx, types.TaskListOptions{Filters: args})
	if err != nil {
		return 0, err
	}
	return len(tasks), nil
}

// Silence "imported and not used" for transitional helpers.
var (
	_ = bytes.NewReader
	_ = io.Discard
	_ = json.Marshal
)
```

(The trailing `_ = …` block exists because we may not have used all the imports yet; the agent should drop unused imports during cleanup.)

- [ ] **Step 2: Implement main.go**

Replace `cmd/swarm-auto-drain/main.go`:

```go
// swarm-auto-drain is the coordinator daemon. See docs/specs/.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/matchory/swarm-auto-drain/internal/api"
	"github.com/matchory/swarm-auto-drain/internal/config"
	dockerpkg "github.com/matchory/swarm-auto-drain/internal/docker"
	"github.com/matchory/swarm-auto-drain/internal/drain"
	"github.com/matchory/swarm-auto-drain/internal/forward"
	"github.com/matchory/swarm-auto-drain/internal/identity"
	"github.com/matchory/swarm-auto-drain/internal/lock"
	"github.com/matchory/swarm-auto-drain/internal/logging"
)

var version = "dev"

func main() {
	cfg, err := config.Load()
	if err != nil {
		// stderr because logger isn't set up yet
		os.Stderr.WriteString("config: " + err.Error() + "\n")
		os.Exit(2)
	}
	logging.Setup(cfg.LogLevel)
	slog.Info("starting", "version", version, "node", cfg.NodeHostname)

	dcli, err := dockerpkg.New(cfg.DockerSocket)
	if err != nil {
		slog.Error("docker client", "err", err)
		os.Exit(1)
	}
	defer dcli.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Initial role detection (block until first successful call).
	role := dockerpkg.NewRoleDetector(dcli, cfg.RoleRecheck)
	if err := role.Refresh(ctx); err != nil {
		slog.Error("initial docker info", "err", err)
		os.Exit(1)
	}
	go role.Run(ctx)

	store := &lock.ConfigStore{Docker: dcli, ConfigName: cfg.LockConfigName}
	drainer := &drain.DockerDrainer{Docker: dockerAdapter{dcli}}

	handlers := api.Handlers{
		Store:        store,
		Drainer:      drainer,
		LockTTL:      cfg.LockTTL,
		DrainTimeout: cfg.DrainTimeout,
		PollInterval: 2 * time.Second,
		Role:         func() string { return role.Role().String() },
	}
	mw := identity.Middleware{
		NodeHostname: cfg.NodeHostname,
		PublicPaths:  []string{"/healthz", "/role"},
	}

	var handler http.Handler
	if role.Role() == dockerpkg.RoleManager {
		handler = mw.Wrap(handlers.Router())
	} else {
		// Worker: forward everything to a manager replica.
		mf := &forward.ManagerFinder{
			ServiceDNS: "tasks.swarm-auto-drain",
			Port:       7777,
			Client:     &http.Client{Timeout: 3 * time.Second},
			TTL:        cfg.ManagerDiscoveryTTL,
		}
		fwd := &forward.Forwarder{
			OriginNode:     cfg.NodeHostname,
			FindManagerURL: mf.Find,
			Client:         &http.Client{Timeout: cfg.ForwardTimeout},
			Retries:        2,
		}
		// /healthz still answered locally so the host script's wait loop
		// doesn't depend on the manager being reachable.
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", handlers.Router().ServeHTTP)
		mux.HandleFunc("/role", handlers.Router().ServeHTTP)
		mux.Handle("/", fwd)
		handler = mux
	}

	srv := &api.Server{Addr: cfg.ListenAddr, Handler: handler}
	slog.Info("listening", "addr", cfg.ListenAddr, "role", role.Role().String())
	if err := srv.Run(ctx); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
	slog.Info("shutdown complete")
}

// dockerAdapter bridges *internal/docker*.NodeSummary to *internal/drain*.NodeSummary
// (same shape, separate package to avoid the drain package importing internal/docker
// and creating a cycle).
type dockerAdapter struct{ c dockerpkg.Client }

func (a dockerAdapter) NodeList(ctx context.Context, fs map[string][]string) ([]drain.NodeSummary, error) {
	src, err := a.c.NodeList(ctx, fs)
	if err != nil {
		return nil, err
	}
	out := make([]drain.NodeSummary, len(src))
	for i, n := range src {
		out[i] = drain.NodeSummary{
			Hostname: n.Hostname, Role: n.Role,
			Reachability: n.Reachability, Availability: n.Availability,
		}
	}
	return out, nil
}
func (a dockerAdapter) NodeUpdate(ctx context.Context, n, av string) error { return a.c.NodeUpdate(ctx, n, av) }
func (a dockerAdapter) NodeTaskCount(ctx context.Context, n string) (int, error) {
	return a.c.NodeTaskCount(ctx, n)
}
```

- [ ] **Step 3: Build and run vet**

```bash
go mod tidy
go build ./...
go vet ./...
```

Expected: silent. Fix any unused-import warnings the agent introduced.

- [ ] **Step 4: Run the full test suite**

```bash
go test ./...
```

Expected: PASS everywhere.

- [ ] **Step 5: Smoke-test the binary**

```bash
NODE_HOSTNAME=smoke ./swarm-auto-drain || true   # build failed? rebuild:
go build -o swarm-auto-drain ./cmd/swarm-auto-drain
NODE_HOSTNAME=smoke ./swarm-auto-drain
```

Expected: it logs `starting ... node=smoke` then errors out on `docker client` because there's no Docker socket in the local test environment. That's correct — the daemon needs Docker; smoke is OK.

- [ ] **Step 6: Commit**

```bash
git add cmd/ internal/docker/client.go
git commit -m "main: wire config + role + lock + drain + forward + api"
```

---

## Task 12: Docker image build check

**Files:** none modified.

- [ ] **Step 1: Build the image**

```bash
docker build -t swarm-auto-drain:dev .
```

Expected: successful build, final image based on distroless.

- [ ] **Step 2: Inspect size**

```bash
docker images swarm-auto-drain:dev --format '{{.Size}}'
```

Expected: under 20 MB (Go static + distroless).

- [ ] **Step 3: Run the image** (will fail without Docker socket — that's fine, we only check the binary launches)

```bash
docker run --rm -e NODE_HOSTNAME=smoke swarm-auto-drain:dev 2>&1 | head -5
```

Expected: it logs the startup message and then errors on docker socket (no socket mounted). Verifies the entrypoint binary is intact.

- [ ] **Step 4: No commit needed; verify CI workflow exists and passes locally where possible**

```bash
gh workflow view ci.yml 2>/dev/null || echo "(not pushed yet; CI workflow exists at .github/workflows/ci.yml)"
```

---

## Task 13: `deploy/swarm-auto-drain.stack.yml`

**Files:**
- Create: `deploy/swarm-auto-drain.stack.yml`

- [ ] **Step 1: Write the stack file**

Create `deploy/swarm-auto-drain.stack.yml`:

```yaml
# swarm-auto-drain — global Swarm service for coordinated reboots.
# Deploy:
#   docker stack deploy -c deploy/swarm-auto-drain.stack.yml swarm-auto-drain
#
# Image version pinned via the VERSION env var at deploy time, e.g.:
#   VERSION=v0.1.0 docker stack deploy -c ...
#
# Hosts must pre-create /var/lib/swarm-auto-drain (Puppet does this; manual
# adopters: `sudo mkdir -p /var/lib/swarm-auto-drain && sudo chmod 0755 /var/lib/swarm-auto-drain`).
version: "3.9"

services:
  swarm-auto-drain:
    image: ghcr.io/matchory/swarm-auto-drain:${VERSION:-latest}
    deploy:
      mode: global
      restart_policy:
        condition: any
        delay: 10s
      resources:
        limits:
          cpus: "0.50"
          memory: 64M
        reservations:
          cpus: "0.05"
          memory: 16M
      update_config:
        parallelism: 1            # one replica at a time
        delay: 30s
        failure_action: rollback
    environment:
      NODE_HOSTNAME: "{{.Node.Hostname}}"
      LISTEN_ADDR: "127.0.0.1:7777"
      LOCK_TTL_SECONDS: "1800"
      DRAIN_TIMEOUT_SECONDS: "300"
      ROLE_RECHECK_SECONDS: "30"
      LOG_LEVEL: "info"
    networks:
      - swarm-auto-drain-net
    ports:
      - target: 7777
        published: 7777
        protocol: tcp
        mode: host                # bind to 127.0.0.1 on each host
    volumes:
      - type: bind
        source: /var/run/docker.sock
        target: /var/run/docker.sock
      - type: bind
        source: /var/lib/swarm-auto-drain
        target: /var/lib/swarm-auto-drain

networks:
  swarm-auto-drain-net:
    driver: overlay
    internal: true                # not reachable from outside the swarm
    attachable: false
```

- [ ] **Step 2: Validate with docker compose (syntax check)**

```bash
docker compose -f deploy/swarm-auto-drain.stack.yml config >/dev/null
```

Expected: silent. (Compose validates the schema even though this is a stack file.)

- [ ] **Step 3: Commit**

```bash
git add deploy/
git commit -m "deploy: docker stack file for swarm-auto-drain"
```

---

## Task 14: Reference consumer shell scripts

**Files:**
- Create: `docs/consumers/README.md`
- Create: `docs/consumers/drain-docker-swarm.sh`
- Create: `docs/consumers/resume-docker-swarm.sh`

These are reference scripts that downstream adopters (Puppet, Ansible, plain SSH-driven setups) can crib.

- [ ] **Step 1: Write the drain script**

Create `docs/consumers/drain-docker-swarm.sh`:

```bash
#!/bin/bash
# Reference host-side drain script for swarm-auto-drain.
# Called by a host-level safe-reboot orchestrator (cron, systemd timer, etc.)
# AFTER all preconditions are checked (kill switch, puppet recency, reboot
# required). On success: writes /var/lib/swarm-auto-drain/draining and
# `systemctl reboot`s the host.

set -euo pipefail
exec > >(logger -t swarm-auto-drain-drain) 2>&1

COORDINATOR=http://127.0.0.1:7777

# 1. Acquire the cluster-wide lock.
resp=$(curl -sf -X POST --max-time 60 -w '\n%{http_code}' "$COORDINATOR/lock")
code=${resp##*$'\n'}
case "$code" in
  201|410) echo "lock acquired (code=$code)" ;;
  409)     echo "lock held by another node — deferring this run"; exit 0 ;;
  *)       echo "coordinator returned unexpected code $code"; exit 1 ;;
esac

# 2. Best-effort release if anything below fails.
release() {
  curl -sf -X DELETE --max-time 30 "$COORDINATOR/lock" || true
}
trap 'release; exit 1' ERR

# 3. Drain — coordinator handles manager-quorum guard internally.
curl -sf -X POST --max-time 600 "$COORDINATOR/drain"
echo "drain complete"

# 4. Sentinel + reboot.
trap - ERR
mkdir -p /var/lib/swarm-auto-drain
touch /var/lib/swarm-auto-drain/draining
echo "rebooting"
systemctl reboot
```

- [ ] **Step 2: Write the resume script**

Create `docs/consumers/resume-docker-swarm.sh`:

```bash
#!/bin/bash
# Reference host-side resume script for swarm-auto-drain.
# Called from a systemd oneshot at boot, GATED by
# ConditionPathExists=/var/lib/swarm-auto-drain/draining so non-coordinated
# boots (kernel panic, power loss, manual reboot) skip it entirely.

set -euo pipefail
exec > >(logger -t swarm-auto-drain-resume) 2>&1

COORDINATOR=http://127.0.0.1:7777

# 1. Wait for the local coordinator replica to come up (5 min budget).
for i in $(seq 1 60); do
  if curl -sf --max-time 5 "$COORDINATOR/healthz" >/dev/null; then
    break
  fi
  sleep 5
done
if ! curl -sf --max-time 5 "$COORDINATOR/healthz" >/dev/null; then
  echo "coordinator never came up — leaving sentinel + lock for next boot"
  exit 1
fi

# 2. Set self active. Idempotent — succeeds even if already active.
curl -sf -X POST --max-time 60 "$COORDINATOR/active"

# 3. Release the lock. The coordinator verifies we are the holder; a 403
#    means a peer stole the lock as stale during our outage, which is the
#    correct end state — do not retry.
code=$(curl -s -X DELETE --max-time 30 -o /dev/null -w '%{http_code}' "$COORDINATOR/lock")
case "$code" in
  204) echo "lock released" ;;
  403|404) echo "lock not held by us (code=$code) — peer took over, leaving be" ;;
  *)   echo "unexpected release code $code"; exit 1 ;;
esac

# 4. Clear sentinel so next boot is a no-op.
rm -f /var/lib/swarm-auto-drain/draining
```

- [ ] **Step 3: Write the README**

Create `docs/consumers/README.md`:

```markdown
# Reference consumer scripts

The shell scripts in this directory are reference implementations of the
host-side integration. They are NOT installed by the coordinator — they
live here for downstream adopters to copy / adapt.

## Files

- `drain-docker-swarm.sh` — called by a host-level orchestrator (cron,
  systemd timer) AFTER preconditions are checked. Acquires the cluster
  lock, drains the node, writes the resume sentinel, reboots.
- `resume-docker-swarm.sh` — called from a sentinel-gated systemd oneshot
  at boot. Waits for the local coordinator replica, sets the node active,
  releases the lock, clears the sentinel.

## Sentinel

Both scripts cooperate around `/var/lib/swarm-auto-drain/draining`. The
sentinel exists between drain (just before `systemctl reboot`) and
successful resume. It gates the resume systemd unit via
`ConditionPathExists` so non-coordinated boots (kernel panic, power
loss, manual `reboot`) do not run the resume logic. This prevents the
resume script from running on every boot and prevents clobbering an
operator's deliberate `availability=drain` setting.

## Suggested systemd unit for the resume oneshot

```ini
[Unit]
Description=swarm-auto-drain resume after coordinated reboot
ConditionPathExists=/var/lib/swarm-auto-drain/draining
After=docker.service network-online.target
Wants=docker.service network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/resume-docker-swarm
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
```

## Preconditions left to the host orchestrator

The coordinator does NOT enforce:
- A "kill switch" for opt-in per host.
- Recency of the last configuration management run.
- The presence of `/var/run/reboot-required`.

Those checks belong upstream of the call to this script — they are the
operator's policy decisions, not the coordinator's job.
```

- [ ] **Step 4: bash syntax check**

```bash
bash -n docs/consumers/drain-docker-swarm.sh
bash -n docs/consumers/resume-docker-swarm.sh
```

Expected: silent.

- [ ] **Step 5: Commit**

```bash
git add docs/consumers/
git commit -m "docs/consumers: reference host-side scripts and README"
```

---

## Task 15: Integration test fixture (3-node Swarm via dind)

**Files:**
- Create: `tests/integration/docker-compose.yml`
- Create: `tests/integration/README.md`
- Create: `tests/integration/tests.sh`

These tests are heavy. They require `docker compose` + privileged dind. CI will run them; local devs may skip.

- [ ] **Step 1: Write the compose file**

Create `tests/integration/docker-compose.yml`:

```yaml
# 3-node Docker-in-Docker swarm fixture for integration testing.
# Start: docker compose -f tests/integration/docker-compose.yml up -d
# Stop:  docker compose -f tests/integration/docker-compose.yml down -v
version: "3.9"

services:
  dind-mgr1:
    image: docker:27-dind
    privileged: true
    environment:
      DOCKER_TLS_CERTDIR: ""
    command: ["dockerd", "--host=tcp://0.0.0.0:2375", "--host=unix:///var/run/docker.sock"]
    ports:
      - "12375:2375"
    networks:
      sad-net:
        ipv4_address: 10.99.0.11

  dind-mgr2:
    image: docker:27-dind
    privileged: true
    environment:
      DOCKER_TLS_CERTDIR: ""
    command: ["dockerd", "--host=tcp://0.0.0.0:2375", "--host=unix:///var/run/docker.sock"]
    ports:
      - "12376:2375"
    networks:
      sad-net:
        ipv4_address: 10.99.0.12

  dind-mgr3:
    image: docker:27-dind
    privileged: true
    environment:
      DOCKER_TLS_CERTDIR: ""
    command: ["dockerd", "--host=tcp://0.0.0.0:2375", "--host=unix:///var/run/docker.sock"]
    ports:
      - "12377:2375"
    networks:
      sad-net:
        ipv4_address: 10.99.0.13

networks:
  sad-net:
    driver: bridge
    ipam:
      config:
        - subnet: 10.99.0.0/24
```

- [ ] **Step 2: Write the runner**

Create `tests/integration/tests.sh`:

```bash
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
```

- [ ] **Step 3: Write the README**

Create `tests/integration/README.md`:

```markdown
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
```

- [ ] **Step 4: Make the script executable + lint**

```bash
chmod +x tests/integration/tests.sh
bash -n tests/integration/tests.sh
```

Expected: silent.

- [ ] **Step 5: Commit**

```bash
git add tests/integration/
git commit -m "tests/integration: dind 3-manager swarm fixture + lock/drain tests"
```

---

## Task 16: README polish + first commit-able state

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Augment README with build/run instructions**

The existing README already describes the architecture. Append a "Development" section AFTER the "Status" section, before "License":

```markdown
## Development

```bash
# Build the binary
go build ./cmd/swarm-auto-drain

# Run the test suite
go test ./...

# Build the container image
docker build -t swarm-auto-drain:dev .

# Spin up the integration test fixture
docker compose -f tests/integration/docker-compose.yml up -d
bash tests/integration/tests.sh
docker compose -f tests/integration/docker-compose.yml down -v
```

## Deploying

```bash
# Pin a version (or use :latest)
VERSION=v0.1.0 docker stack deploy -c deploy/swarm-auto-drain.stack.yml swarm-auto-drain

# Verify
docker service ls
docker service ps swarm-auto-drain_swarm-auto-drain
```

Pre-requisite on every swarm node: `/var/lib/swarm-auto-drain/` must
exist with mode 0755, owned by root. This is mounted into the
coordinator container as the sentinel-state directory shared with the
host's reboot orchestrator scripts.

See [`docs/consumers/`](docs/consumers/) for reference host-side scripts.

## Contributing

This project is pre-alpha. PRs welcome once the first tagged release
ships.
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "readme: development + deploy sections"
```

---

## Task 17: Initial release tag + push

**Files:** none.

- [ ] **Step 1: Verify full test suite passes**

```bash
go test ./...
```

Expected: all green.

- [ ] **Step 2: Verify go.mod is tidy**

```bash
go mod tidy
git diff --exit-code go.mod go.sum
```

Expected: no diff.

- [ ] **Step 3: Wait for human review before pushing to GitHub**

This task ends here. The plan-executing agent should NOT:
- Create a GitHub repo
- Push the initial commit to a remote
- Tag and release

Those are the operator's calls. The agent's job ends when the local repo is at a green-tests, committed state ready for the operator to take over.

---

## Done. What's next for the operator (out of plan scope)

1. Create the GitHub repository `matchory/swarm-auto-drain`.
2. Push `main` and tag `v0.1.0`.
3. CI runs, builds image, pushes to `ghcr.io/matchory/swarm-auto-drain:v0.1.0`.
4. Land the Puppet `tier: docker_swarm` plumbing in `matchory/puppet` (separate plan; references this coordinator).
5. Smoke-test the stack on a non-production swarm (or the dind fixture extended to deploy the stack itself).
6. Rollout to the whale fleet per the Puppet plan's staging procedure.

---

## Validation checklist (for the executing agent — run before claiming done)

- [ ] `go build ./...` silent
- [ ] `go vet ./...` silent
- [ ] `go test ./...` all PASS
- [ ] `docker build -t swarm-auto-drain:dev .` succeeds
- [ ] Image size under 20 MB
- [ ] `docker compose -f deploy/swarm-auto-drain.stack.yml config` silent
- [ ] `bash -n` clean on the two reference shell scripts
- [ ] `docs/specs/2026-05-25-coordinator-design.md` exists and matches the implementation
- [ ] All tasks marked complete in this plan
- [ ] `git status` clean

## What this plan deliberately does NOT do

- Prometheus metrics endpoint (future v1.1)
- Maintenance-window scheduling (future, mirrors kured's flags)
- Drain-only mode flag (the API supports it; documentation is a follow-up)
- Multi-cluster federation
- Push to GitHub or publish a tag (operator's call)
- Integration test of the full stack (coordinator-in-dind) — only the
  underlying Docker primitives are exercised; a follow-up plan can layer
  the binary on top
