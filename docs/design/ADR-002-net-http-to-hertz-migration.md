# ADR-002: net/http to Hertz Migration

**Status:** Accepted · **Date:** 2025-07-15 · **Author:** compliance team

## Context

The control plane HTTP server used `net/http` with `http.ServeMux` and
`httputil.ReverseProxy`. AGENTS.md mandates `github.com/cloudwego/hertz`
for all HTTP serving, including middleware, routing, and reverse proxying.

## Decision

Rewrite every `internal/api/` handler from `net/http` to Hertz:

- Use `server.Default(server.WithHostPorts(cfg.ControlAddr))` to create
  the server.
- Register routes via `h.GET/POST/Any(...)` instead of `mux.HandleFunc`.
- Mount Prometheus `/metrics` via `adaptor.HertzHandler(promhttp.Handler())`
  from Hertz's built-in `pkg/common/adaptor`.
- Use `github.com/hertz-contrib/reverseproxy` for OTel/VictoriaMetrics/
  cache reverse proxies.
- Handlers use `func(ctx context.Context, c *app.RequestContext)`.
- Middleware via `h.Use(...)`.
- Error responses via `c.JSON(status, ErrorResponse{Message: msg})`.

The TLS L4 data plane (`handleDataConn`) remains on `net`/`crypto/tls` —
it is a raw TCP proxy, not an HTTP endpoint, and cannot be migrated to Hertz.

## Rationale

Hertz provides first-class support for middleware chaining, request context,
and high-throughput connection handling. The `reverseproxy` contrib package
integrates with Hertz's request context, eliminating the need for the
stdlib `httputil` package.

## Consequences

- All HTTP handlers now follow the Hertz signature.
- Reverse proxies are constructed once at startup (not per-request).
- Request body reads are capped at 1 MB (DoS mitigation).
- Internal error strings are never leaked to clients.
