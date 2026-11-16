# ADR-053: Device flow, CI OIDC, and Cloud GraphQL

## Status
Accepted

## Context
`dagger login` uses RFC 8628, CI needs secretless authentication, and the
Dagger CLI/UI expect a Cloud-compatible GraphQL endpoint.

## Decision
- Implement `POST /oauth/device/code`, `POST /api/v1/auth/device/approve`, and
  `POST /oauth/token` (device_code + refresh_token grants). Codes are
  short-lived (10m) and leader-pinned.
- Implement `POST /v1/oidc?provider=github|gitlab`: verify the ID token against
  the configured issuer JWKS, match the repository claim to the project, and
  return an org-scoped token.
- Mount `POST /query` using `graph-gophers/graphql-go` executed directly from
  the Hertz request body (no `net/http` in `internal/handler/`).
- Guard `/query` and `/oauth/*` with go-secguard; webhook HMAC failures are
  never recorded as token failures.

## Consequences
The CLI can log in interactively and from CI without long-lived secrets, and
Cloud clients can use the standard GraphQL contract.

## Update — residual-risk remediation (PR #70 review)
- GraphQL introspection is disabled by default (`graphql.introspection_enabled:
  false`). Query depth, selection count, byte length, and parallelism are
  bounded by `graphql.max_depth` (15), `graphql.max_selections` (250),
  `graphql.max_query_bytes` (65536), and `graphql.max_parallelism` (10).
- `org.traces(first:, after:)` clamps `first` to `[1, MaxTraceLimit]` and
  implements cursor pagination (`pageInfo { hasNextPage endCursor }`).
- `Org.checks(first:)` exposes Cloud Checks for CLI/UI parity.
- `OrgService.EnsureToken` reuses a recoverable `dag_<org>_` token so OIDC
  exchanges and supervisor restarts never invalidate in-flight runs.
