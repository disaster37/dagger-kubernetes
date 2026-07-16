# ADR-008: Prometheus Metrics Dependency Injection

**Status:** Accepted · **Date:** 2025-07-15 · **Author:** compliance team

## Context

The original metrics implementation used `promauto` package-level global
variables declared at the top of `internal/observ/metrics.go`. These globals
made it impossible to isolate metrics between test runs (double-registration
panics) and violated the project's convention of dependency injection
(AGENTS.md rule 10).

## Decision

Refactor metrics into an injected `Metrics` struct:

```go
type Metrics struct {
    EngineAcquireTotal    *prometheus.CounterVec
    EngineAcquireDuration *prometheus.HistogramVec
    ActiveLeases          prometheus.Gauge
    ActiveReplicas        *prometheus.GaugeVec
    OTelIngestTotal       *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics
```

- `NewMetrics` registers all collectors on the provided `prometheus.Registerer`
  (pass `prometheus.DefaultRegisterer` in production, `nil` or a fresh
  registry in tests).
- The `*Metrics` value is injected into `Server`, `Manager`, and any other
  component that needs to record metrics.
- No package-level `promauto` globals exist.

## Rationale

- Dependency injection enables test isolation: each test creates its own
  registry and `Metrics` instance, eliminating double-registration panics.
- Following the injection pattern used by all other components in the
  codebase (`logger`, `sessions`, `fleetManager`, etc.).
- Prometheus best practices recommend passing a `Registerer` explicitly.

## Consequences

- All call sites changed from `observ.EngineAcquireTotal...` to
  `s.metrics.EngineAcquireTotal...`.
- The `Metrics` struct is passed to `NewServer` and `NewManager` via the
  constructor.
- When `reg` is nil, collectors are usable (counter increments work) but
  not registered — safe for tests that don't scrape `/metrics`.
