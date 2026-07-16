# ADR-007: Outbound HTTP Clients

**Status:** Accepted · **Date:** 2025-07-15 · **Author:** compliance team

## Context

The telemetry package (`internal/telemetry/`) makes outbound HTTP calls
to Tempo (traces), Loki (logs), and VictoriaMetrics (metrics). These are
unidirectional queries (request→response) with no streaming or bidirectional
requirements.

## Decision

Keep the stdlib `net/http` for outbound client requests. Do **not** use
Hertz as an HTTP client.

Rationale:
- `net/http` is the standard Go HTTP client — well-tested, well-understood,
  and zero-overhead for simple REST queries.
- Hertz's HTTP client (`pkg/app/client`) is optimized for server-to-server
  communication in high-throughput microservice architectures, which adds
  complexity without benefit for our simple query patterns.
- There is no AGENTS.md rule requiring Hertz for outbound clients — the
  mandate applies to the HTTP **server** only.

Each telemetry client (`LogsClient`, `MetricsClient`, `SpanTreeReconstructor`)
constructs its own `http.Client` with a 30-second timeout. This allows
independent timeouts and transport configuration per backend.

## Consequences

- `net/http` remains in the dependency graph for telemetry clients.
- This is a documented exception to the "no net/http" rule, scoped to
  outbound client requests only.
- The inbound HTTP server and all its middleware use Hertz exclusively.
