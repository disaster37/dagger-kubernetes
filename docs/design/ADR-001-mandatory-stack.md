# ADR-001: Mandatory Library Stack

**Status:** Accepted · **Date:** 2025-06-01 · **Author:** dagger-cache team

## Context

The project must use a consistent, auditable set of libraries to reduce
cognitive overhead and simplify dependency management. AGENTS.md defines
a mandatory stack that every contributor must follow.

During the initial build, the codebase used `go.uber.org/zap` for logging,
`net/http` for the HTTP server, `gorilla/websocket` for live streaming, and
the stdlib `flag` package for CLI parsing. A compliance audit (2025-07)
found these violated the mandatory stack.

## Decision

Replace every non-compliant library with the mandated alternative:

| Purpose        | Before              | After                            |
|----------------|---------------------|----------------------------------|
| CLI            | `flag`              | `github.com/urfave/cli/v2`       |
| Configuration  | `spf13/viper`       | `spf13/viper` (same)             |
| Logging        | `go.uber.org/zap`   | `github.com/sirupsen/logrus`     |
| HTTP server    | `net/http`          | `github.com/cloudwego/hertz`     |
| SSE/streaming  | `gorilla/websocket` | `cloudwego/hertz pkg/protocol/sse` |
| Prometheus     | `prometheus/client_golang` | `prometheus/client_golang` (same) |

## Rationale

- Logrus provides structured JSON logging with a simple API, matching the
  project's observability requirements.
- Hertz is a high-performance HTTP framework that supports middleware,
  reverse proxies, and native SSE — eliminating the need for a separate
  WebSocket dependency.
- urfave/cli v2 is the de-facto Go CLI framework, providing flag parsing,
  help generation, and subcommand support.

## Consequences

All new code must use the mandated stack. Existing code was migrated in a
single compliance pass (2025-07). Violations are detected by CI linters.
