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

## Follow-up (2026-11-18)
- **Quickstart-org mutation.** `Mutation.createQuickstartOrg(name: String!): Org!`
  lets a freshly authenticated user with no org memberships bootstrap their own
  org from `dagger cloud login` (creating it and granting `org_admin`, or
  idempotently returning an existing membership; server-side slug sanitization
  and collision suffixes apply). `User.orgs` and `canAccessOrg` now also include
  group-derived orgs.
- **`CLINotice` object.** `Org.cliNotice` is typed as
  `CLINotice { level: String!, title: String!, body: String! }` (null when
  none), so the per-command `GetOrgCLINotice` query (`cliNotice { level title
  body }`) validates and succeeds.
- **Host-based internal data host + SNI routing.** See
  [ADR-052](ADR-052-semver-prereleases-org-tenancy.md) — the internal data host
  is chosen from the request `Host`, and internal `.svc` SNIs receive the
  minting-CA leaf.

## Follow-up (2026-11-21)
- **RFC 8628 / RFC 6749 error compliance on `POST /oauth/token`.** The device
  token exchange previously mapped every failure to a non-standard body
  (`ErrUnauthenticated` → 400 with a raw message), so `golang.org/x/oauth2`
  `Config.DeviceAccessToken` (used by the Dagger CLI 1.0 device flow) aborted on
  the first poll instead of waiting for approval. The endpoint now returns the
  RFC 8628 §3.5 / RFC 6749 §5.2 error payload
  `{ "error": ..., "error_description": ... }` (with `Cache-Control: no-store`):
  - `authorization_pending` (400) while an unexpired, unconsumed device code has
    not been approved — the client keeps polling.
  - `expired_token` (400) once the device code's `ExpiresAt` has passed.
  - `invalid_grant` (400) for an unknown, malformed, or already-consumed device
    code, and for an invalid / expired / revoked `refresh_token`.
  - `unsupported_grant_type` (400) for any other `grant_type`.
  - `invalid_request` (400/413) for an unreadable or oversized body.
  These are backed by new domain sentinels `ErrAuthorizationPending`,
  `ErrExpiredDeviceToken`, and `ErrInvalidGrant` in
  `internal/domain/device_auth.go`; `DeviceAuthService.ExchangeDeviceCode`
  wraps them (cheapest-rejection ordering: unknown → consumed → expired →
  pending → issuer checks) and the handler maps them with `errors.Is`. The
  `POST /api/v1/auth/refresh` endpoint keeps its existing 401 mapping.
