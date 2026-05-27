# swarm-auto-drain — Metrics Design Spec

**Date:** 2026-05-27
**Status:** Approved for implementation
**Origin:** Stable-readiness review on branch `implement-coordinator` (2026-05-27). Observability gap identified as a blocker for promoting the project beyond "Pre-alpha".

---

## 1. Problem

The coordinator runs on every node of a Docker Swarm and performs destructive operations on shared cluster state (the reboot mutex, node availability changes). Today its only observability surface is `slog` output. An operator cannot answer questions like:

- How often is the cluster-wide lock contended?
- Are stale locks being stolen (a leading indicator of nodes crashing mid-drain)?
- How long do drains actually take? Are any timing out?
- Is the worker→manager forwarder retrying excessively?
- How often is the role detector seeing role flips?

Without metrics, regressions and slow-burn failures are invisible until something breaks loudly. This spec defines the metrics surface required to confidently promote the coordinator to "Beta", and ultimately "Stable", status.

## 2. Goals and non-goals

**Goals**

- Expose domain metrics (lock, drain, role, forward, identity) sufficient to diagnose the coordinator's behaviour without reading logs.
- Expose generic HTTP RED metrics (requests, errors, durations) for SLO alerting.
- Expose Go runtime metrics for "is the process healthy" baselines.
- Support both Prometheus pull and OTLP push without code duplication.
- Default to no metrics emission — both readers are off unless explicitly configured. Adds zero operational surface for existing deployments.

**Non-goals**

- Distributed tracing. The coordinator's request flow is shallow (host → local replica → optional forward → manager); a trace tells you little that a log line plus duration histograms don't.
- Per-lock-id or per-source-IP labels. Unbounded cardinality.
- Custom dashboards or alert rules. Those belong in the consumer's monitoring repo, not this project.
- Authenticating or rate-limiting `/metrics`. The endpoint is bound to a dedicated address; access control is the operator's choice (typically a private network or a sidecar scraper).

## 3. Architecture

### 3.1 Single MeterProvider, two readers

One `sdkmetric.MeterProvider` is constructed at startup with up to two `Reader`s attached:

- **Prometheus exporter** (`go.opentelemetry.io/otel/exporters/prometheus`) — implements `Reader` and exposes an `http.Handler` that serves text-format metrics. Attached only when `METRICS_LISTEN_ADDR` is set.
- **OTLP HTTP push exporter** (`go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp`) — wrapped in a `PeriodicReader`. Attached only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set.

Both readers, one, or neither are all valid configurations. `metrics.New` constructs exactly what is configured. With neither configured, the provider still exists (so the instrument handles are non-nil and recording calls are cheap no-ops), but no exporter is attached and no listener is started.

### 3.2 Package layout

A new package `internal/metrics` owns:

- The `Metrics` struct — a hierarchy of typed instrument handles grouped by subsystem.
- `metrics.New(ctx, cfg) (*Metrics, http.Handler, ShutdownFunc, error)` — constructs the provider, registers the runtime instrumentation, and returns the Prom handler (nil when pull is disabled) and a shutdown function that flushes both exporters with a configurable budget.
- `metrics.NewForTesting(provider) *Metrics` — test helper that builds the instrument struct from a caller-supplied provider, skipping exporter and listener setup.

The package does not import `internal/api`, `internal/lock`, or other domain packages. Domain packages import `internal/metrics` and receive only the sub-struct they need (e.g. `lock.NewStore` takes `metrics.LockMetrics`). This mirrors the existing import-cycle discipline described in CLAUDE.md.

### 3.3 Recording model

`Metrics` is a struct of typed OTel instruments:

```go
type Metrics struct {
    Lock     LockMetrics
    Drain    DrainMetrics
    Role     RoleMetrics
    Forward  ForwardMetrics
    Identity IdentityMetrics
    HTTP     HTTPMetrics
}

type LockMetrics struct {
    Acquire metric.Int64Counter
    Release metric.Int64Counter
    Steals  metric.Int64Counter
    Hold    metric.Float64Histogram
    Held    metric.Int64Gauge
}
```

When metrics are not configured (both readers disabled), `metrics.New` falls back to `go.opentelemetry.io/otel/metric/noop` so the returned `Metrics` is fully populated with no-op instrument handles — domain code records as usual and the values go nowhere. Tests use `metrics.NewNoop()` for the same effect.

Constructors in each instrumented package gain one extra parameter, the relevant sub-struct. `cmd/swarm-auto-drain/main.go` builds `*Metrics` first and threads the sub-views through to each collaborator. No global registry, no package-level singleton.

### 3.4 Listener wiring

When `METRICS_LISTEN_ADDR` is non-empty, `main.go` appends a third `ListenerSpec` to the existing `api.Server` config:

```go
ListenerSpec{Name: "metrics", Listener: <bound to METRICS_LISTEN_ADDR>, Handler: metricsHandler}
```

This reuses the existing multi-listener infrastructure and shutdown discipline. The metrics listener has no identity middleware — the access boundary is the bind address. Default suggestion is `:9100` (the Prometheus node-exporter port convention); operators are free to choose another.

The metrics listener is **not** added when `METRICS_LISTEN_ADDR` is empty, so existing deployments are unaffected.

### 3.5 OTLP push

When `OTEL_EXPORTER_OTLP_ENDPOINT` (or any of the standard `OTEL_EXPORTER_OTLP_METRICS_*` env vars) is set, the OTLP metric HTTP exporter is constructed via its default `NewFromEnv`-style constructor. We rely on the OTel SDK's standard environment-variable handling for headers, TLS, timeouts, and protocol selection — no custom auth or transport code lives in this project.

The exporter is wrapped in a `sdkmetric.PeriodicReader` with interval `METRICS_PUSH_INTERVAL_SECONDS` (default 15s). On `Shutdown` the reader is flushed with a 5-second budget.

### 3.6 Resource attributes

The `MeterProvider` is constructed with a `resource.Resource` populated from existing config:

| Attribute | Value |
|---|---|
| `service.name` | `swarm-auto-drain` |
| `service.version` | build-time `version` string from `cmd/swarm-auto-drain` |
| `host.name` | `NODE_HOSTNAME` |
| `swarm.role` | initial role at startup (`manager` or `worker`) — best-effort context; the role can flip later and the resource is immutable, so consumers should treat this as the role at process start, not authoritative |

## 4. Metric catalogue

All names are prefixed `swarm_auto_drain_`. Histograms use the `_seconds` suffix as per Prometheus conventions; the Prom exporter handles the unit rewrite from the OTel-side units automatically.

### 4.1 Lock

| Name | Type | Labels | Notes |
|---|---|---|---|
| `lock_acquire_total` | counter | `outcome={acquired,eexist,stolen,error}` | One increment per `Acquire` call |
| `lock_release_total` | counter | `outcome={released,not_holder,not_found,error}` | One increment per `Release` call |
| `lock_steals_total` | counter | `reason={ttl_expired}` | Leading indicator of crashed holders |
| `lock_hold_seconds` | histogram | — | Recorded on the release path; SLO input |
| `lock_held` | gauge | — | 1 if this replica currently holds a lock, 0 otherwise |

### 4.2 Drain

| Name | Type | Labels | Notes |
|---|---|---|---|
| `drain_total` | counter | `outcome={ok,quorum_fail,timeout,error}` | One per `Drain` call |
| `drain_seconds` | histogram | — | Total duration from start to completion or timeout |
| `drain_tasks_remaining` | gauge | — | Updated each poll iteration; reset to 0 when idle |
| `quorum_check_total` | counter | `result={ok,fail}` | Pre-drain quorum check |

### 4.3 Role

| Name | Type | Labels | Notes |
|---|---|---|---|
| `role` | gauge | — | `0=worker, 1=manager` |
| `role_refresh_total` | counter | `trigger={periodic,error_recovery,manual}, outcome={ok,error}` | Detector health |
| `role_transitions_total` | counter | `from,to ∈ {worker,manager}` | Promotion/demotion events |

### 4.4 Forward

| Name | Type | Labels | Notes |
|---|---|---|---|
| `forward_attempts_total` | counter | `outcome={ok,connect_error,http_5xx,timeout}` | Per-attempt, not per-request |
| `forward_seconds` | histogram | — | Total proxied duration including retries |
| `forward_manager_cache_invalidations_total` | counter | — | Leading indicator of manager trouble |

### 4.5 Identity

| Name | Type | Labels | Notes |
|---|---|---|---|
| `identity_resolutions_total` | counter | `source={loopback,overlay}, outcome={ok,mismatch,unknown_ip}` | Spikes in `mismatch`/`unknown_ip` are suspicious |

### 4.6 HTTP

| Name | Type | Labels | Notes |
|---|---|---|---|
| `http_requests_total` | counter | `method, route, status` | RED metrics |
| `http_request_seconds` | histogram | `method, route` | RED metrics |

`route` is the **template** path drawn from a fixed allowlist (`/healthz`, `/role`, `/managers/health`, `/lock`, `/drain`, `/active`, `/metrics`). Unknown paths bucket to `route="other"` so the label is bounded at 8 values regardless of probe traffic.

### 4.7 Runtime

Registered via `go.opentelemetry.io/contrib/instrumentation/runtime` with default options. Provides:

- `process.runtime.go.goroutines`
- `process.runtime.go.mem.heap_*`
- `process.runtime.go.gc.*`

File descriptors are not directly covered by the contrib package and are skipped.

### 4.8 Cardinality budget

| Subsystem | Approx series per replica |
|---|---|
| Lock | ~25 |
| Drain | ~15 |
| Role | ~10 |
| Forward | ~15 |
| Identity | ~10 |
| HTTP (worst case, dense traffic) | ~200 |
| Runtime | ~40 |
| **Total upper bound** | **~315 / replica** |

For a 30-node cluster: ~10k active series. Comfortable for a single Prometheus instance.

## 5. Configuration

Three new environment variables, parsed by `internal/config`. None are required; all default to "off" or to a conventional value.

| Var | Default | Effect |
|---|---|---|
| `METRICS_LISTEN_ADDR` | `""` (disabled) | When set (e.g. `:9100`), starts a third listener serving the Prom exporter's `/metrics` handler. Empty = no listener, no Prom reader registered. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `""` (disabled) | Standard OTel env var. When set, attaches the OTLP HTTP metric exporter as a `PeriodicReader`. Empty = no push, no OTLP reader registered. |
| `METRICS_PUSH_INTERVAL_SECONDS` | `15` | Push cadence for the periodic reader. Ignored if OTLP push is disabled. |

Standard `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_INSECURE`, etc. are honoured via the SDK's default-from-env constructor. No custom auth code lives in this project.

The deployed stack file (`deploy/`) does not set any of these by default. Commented-out example lines are added documenting the conventional setup.

## 6. Instrumentation surface

Each instrumented package gains one constructor parameter (the relevant sub-struct of `Metrics`). Recording happens at the obvious return paths:

- `internal/lock/config_store.go` — record outcome on each return branch of `Acquire` and `Release`. Record `lock_hold_seconds` on the release path. Update `lock_held` gauge on both acquire and release.
- `internal/drain/drainer.go` — record outcome on `Drain` return; update `drain_tasks_remaining` each poll iteration; record `drain_seconds` on return. Quorum check records `quorum_check_total` before returning.
- `internal/docker/role_detector.go` — record refresh outcome on each tick and on synchronous refresh. Record transition counter when the cached role changes from its previous value.
- `internal/forward/forwarder.go` — increment `forward_attempts_total` in the retry loop with the appropriate outcome. Record `forward_seconds` on final return. Increment `forward_manager_cache_invalidations_total` from the existing `OnAttemptFailure` hook.
- `internal/identity/*.go` — record `identity_resolutions_total` in the middleware after IP→hostname resolution, with `source` distinguishing loopback from overlay callers.

HTTP middleware is implemented in a new `internal/api/metrics.go` exporting `Metrics(next http.Handler, m HTTPMetrics) http.Handler`. It wraps `http.ResponseWriter` to capture status, maps the path to a template via a fixed allowlist, and records both instruments. Plugged into `Handlers.Router()` outside `AuditLog` so middleware overhead is itself measured.

## 7. Testing

### 7.1 Unit tests

For each instrumented package, table-driven tests drive the function through each outcome branch and assert the corresponding instrument was incremented with the right attributes. The test scaffolding uses `sdkmetric.NewManualReader`:

```go
reader := sdkmetric.NewManualReader()
provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
m := metrics.NewForTesting(provider)
// ... exercise code under test ...
var rm metricdata.ResourceMetrics
_ = reader.Collect(ctx, &rm)
// assert specific data points and attributes
```

A small helper in `internal/metrics/testing.go` reduces the boilerplate of finding a named metric and asserting its data points.

### 7.2 HTTP middleware test

Table-driven, verifying:
- Known routes get the correct template label.
- Unknown paths bucket to `route="other"`.
- Status code is captured correctly across 2xx/4xx/5xx.
- Duration histogram receives a non-zero observation.
- Middleware is transparent: response body, headers, and panics propagate unchanged.

### 7.3 Integration smoke test

`tests/integration/tests.sh` gains one line: after the existing lock-acquire test, `curl http://<replica>:9100/metrics` is checked for a 200 status and the presence of `swarm_auto_drain_lock_acquire_total`. Validates the wiring (listener spec, exporter, handler) actually serves what the unit tests assume.

### 7.4 Out of scope for tests

- Exact histogram bucket boundaries.
- Runtime metric values (covered by the contrib package's own tests).
- Byte-exact Prometheus text-format output.
- OTLP push round-trip in CI. The push reader is exercised by the OTel SDK's own tests. Local verification against `otel-collector` is documented in `tests/integration/README.md`.

## 8. Dependencies

Three new direct module dependencies:

- `go.opentelemetry.io/otel/exporters/prometheus`
- `go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp`
- `go.opentelemetry.io/contrib/instrumentation/runtime`

The core OTel modules (`otel`, `otel/metric`, `otel/sdk`, `otel/sdk/metric`) are already transitive deps via the Docker SDK and become direct on first import.

## 9. Migration and rollout

- The feature is dormant by default. Existing deployments see no behavioural change after upgrade.
- Enabling metrics is a stack-file edit (add `METRICS_LISTEN_ADDR=:9100` to the service env) followed by a service update. No state migration.
- The Prom listener address can be changed without restart by redeploying the stack.
- Disabling metrics is symmetric: remove the env var, redeploy.

## 10. Open questions deferred to implementation

- Whether the `lock_held` gauge needs an observable-callback variant for accuracy across replica restarts, or if event-driven updates from acquire/release are sufficient. Default to event-driven; revisit if drift is observed.
- Whether to expose the Prom exporter's process collector (which adds `process_*` metrics like CPU and RSS). Default to off — the runtime instrumentation covers the Go-specific surface, and the rest is the host monitoring's job.

---

## Appendix A — Naming reference

OTel canonical names (used internally) follow `subsystem.name` dot-separated form. The Prom exporter rewrites these to underscore-separated form with the `swarm_auto_drain_` prefix. Both representations refer to the same instrument. Where the catalogue above lists Prometheus-style names, they are the canonical reference.
