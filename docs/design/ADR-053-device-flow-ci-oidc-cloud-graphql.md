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
