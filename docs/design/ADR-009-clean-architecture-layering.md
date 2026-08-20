# ADR-009: Clean Architecture Layering

**Status:** Accepted · **Date:** 2026-08-09 · **Author:** platform team

## Context

The original layout used a flat `internal/<pkg>` structure (`api`, `auth`,
`ca`, `cache`, `config`, `fleet`, `session`, `telemetry`, `version`). As the
codebase grew, this produced ambiguous dependency directions: `api` imported
`fleet`, `cache`, `auth`, `ca`, `session`, `telemetry`, and `version` directly,
and `fleet` imported `session`, with no enforced layering rule. Business logic
(`fleet.Manager`) and infrastructure (`fleet.K8sProvider`) shared a package,
making it impossible to substitute implementations for testing without
reaching into unexported helpers.

## Decision

Reorganize into layered clean architecture:

```
cmd/api/main.go                 — control plane API server (binary `supervisor`)
cmd/ci/main.go                  — CI wrapper CLI (binary `dagger-kubernetes-ci`)
internal/domain/                — pure entities + interfaces (stdlib ONLY)
internal/service/               — business logic (imports domain, observ)
internal/repository/            — infrastructure implementations (imports domain + drivers)
internal/handler/               — Hertz HTTP/SSE/L4 handlers (imports service, repository, domain, observ)
internal/observ/                — logrus logger factory + Prometheus metrics (cross-cutting)
config/loader.go                — Viper config loading + defaults (returns *domain.Config)
tests/integration/              — black-box integration tests
```

Dependency rule (enforced by imports): `handler → service → domain ← repository`.
`domain` imports stdlib only. `observ` is a documented cross-cutting exception:
`service` and `handler` may inject `*observ.Metrics` / `*logrus.Logger`.

## Resolved design decisions

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | `MintingCA` implementation lives in `repository/ca.go` (NOT service/) | CA providers construct it and `EmbeddedProvider` signs server certs with it; putting the impl in service/ would force a repository→service import (layering inversion). |
| D2 | Fleet naming helpers (`VersionSlug`/`StsName`/`PodName`/`ServiceName`) are EXPORTED functions in `domain/fleet.go` | `service.Manager` calls `PodName`/`StsName` and repository providers call all four; domain is the only layer both may import. Pure stdlib. |
| D3 | `k8s_integration_test.go` stays co-located: `internal/repository/k8s_provider_integration_test.go` (keeps `//go:build integration`, package `repository`) | It is white-box (uses unexported `engineLabelApp`/`engineLabelValue`/`enginePort`); cannot compile from `tests/` without exporting internals. |
| D4 | `cmd/dagger-kubernetes-ci` → `cmd/ci` | It is a CI wrapper (execs `dagger`, emits annotations), not a background worker. |
| D5 | Single `domain` package → globally unique names: `FleetProvider`, `CAProvider`, `SessionStore`, `VersionResolver`, `TokenValidator`, `CacheBackend` (interface), `MintingCA` (interface) | `fleet.Provider`/`ca.Provider` would collide; `CacheConfig` is taken by the viper sub-struct. |
| D6 | Auth split: `domain.TokenValidator{ValidateToken(string)(string,error)}`; `extractToken` moves to `handler/auth.go`; handler calls new `authenticate(c)` helper | `ValidateRequest(*app.RequestContext)` cannot live behind a stdlib-only domain interface. Behavior preserved. |
| D7 | Telemetry clients injected into `handler.NewServer` as `domain.TraceRepository` / `domain.LogRepository` (constructed once in main), replacing per-request construction | Dependency inversion; handler must not new-up infrastructure per request. |
| D8 | `LiveHub`/`LiveClient` → `repository/live_hub.go`, consumed by handler as concrete `*repository.LiveHub` (constructed inside `NewServer` as today) | Keeps Hertz SSE types out of domain; handler may import repository. |
| D9 | `handler.NewServer` takes domain interfaces for `MintingCA`/`SessionStore`/`CacheBackend`/`VersionResolver`/`TokenValidator`, concrete `*service.Manager` | Article pattern: handler→service concrete; interfaces where a layer consumes an outer-layer implementation. |
| D10 | `cache.Backend` renamed `service.Cache` (not `Service`) | `service.Service` is meaningless; `Cache` is precise. |
| D11 | Config types → `domain/config.go`; Viper loader → root `config/loader.go` returning `*domain.Config`; YAML files → `config/` | Loader in public root package is acceptable (imports internal/domain legally). |

## Consequences

- `domain` is the single source of truth for entities and interfaces; it has
  zero third-party imports (verified by `go list -deps internal/domain`).
- Every implementation file carries a compile-time assertion
  (`var _ domain.X = (*Y)(nil)`) so interface satisfaction cannot silently
  break.
- Production `service` never imports `repository`; only TEST files in `service/`
  may import `repository` (for `StubProvider`/`K8sProvider` concrete test
  targets).
- `MintingCA` gains an `IssueServerCertificate` method (pure crypto) so
  `EmbeddedProvider` can sign server certs without duplicating the crypto core.
- Binary names are unchanged (`supervisor`, `dagger-kubernetes-ci`); only the source
  paths moved (`cmd/api`, `cmd/ci`).
