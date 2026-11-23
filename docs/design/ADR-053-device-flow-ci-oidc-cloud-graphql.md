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

## Follow-up (2026-11-21 — CLI v1.0.0-beta.28 Cloud GraphQL parity)
The Dagger CLI 1.0.0-beta.28 issues a wider set of Cloud GraphQL operations.
The self-hosted schema is extended to accept them; operations whose backing
service does not exist on a self-hosted deployment resolve to a deterministic
"unsupported" error **after** the uniform authorization check (auth first, so a
caller can never distinguish an inaccessible org from an unsupported feature —
CWE-639 / anti-enumeration). The schema is split across
`internal/repository/cloud_graphql.go` (schema, builders, helpers, and the
org/subscription/billing families), `cloud_graphql_traces.go` (the `Time`
scalar and traces), and `cloud_graphql_integrations.go` (checks, integrations,
and repos).

- **Org / subscription / features.** `Org.subscription` gains `trialStart`,
  `trialEnd`, and `subscriptionID` (all self-hosted: no trial, id
  `self-hosted`). `Org.features` is now the `Feature` object list
  (`name`, `status` = `ACTIVE`, null trial bounds) rather than a scalar list.
- **Billing.** `Mutation.createPortalSession` is rejected with
  `billing is not available on this self-hosted deployment`. `Query.billingUsage`
  returns a **zero-state** report for the requested `org`/`month` (validated as
  `YYYY-MM`, else `invalid month ...`): revision `0`, empty strings/null times,
  `warnings: []` (non-nil), and null `pricing`/`bill`. All nested
  pricing/bill/resource types are bound so the CLI query validates.
- **Traces.** A custom `Time` scalar marshals RFC3339Nano and unmarshals either
  an RFC3339 timestamp (`TraceFilter.since`/`until`) or a base64url offset
  cursor (`Org.traces(after:)`). `Org.traces` accepts the full `TraceFilter`
  input and a `TraceSort` enum (`START`/`DURATION`); `first` clamps to
  `[1, 500]`. `START` paginates with `pageInfo { hasNextPage endCursor }`
  (base64 offset); `DURATION` is single-page and rejects a cursor. `Trace` is
  enriched with `name`, `status { code message }` (null while running), a
  non-nil `endTime` for terminal traces (even at 0 ms), `local`, optional
  `sender`, and an optional `ci { isNativeCI provider repository }` block (null
  for local traces). `Query.trace` and `Org.lastUserTrace` enforce org access.
- **Checks.** `Org.checks`/`User.checks` return a `CheckCommitConnection` of
  `CheckCommit` nodes (commit refs as a `CheckCommitRef` union of tag/branch/PR;
  the SDK `Check` object; a synthesized internal `load` check when no items are
  discovered). `Org.moduleChecks(moduleRef, moduleVersion)` matches a canonical
  module ref + version. `Mutation.rerunChecks`/`rerunLoad` require the build-time
  `checks.enabled` rerunner (wired from `ChecksService.Rerun`); without it they
  return `check rerun is not available on this self-hosted deployment
  (checks.enabled is false)`. Check IDs are `<runID>/<checkName>`; cross-org IDs
  return `check not found`.
- **Integrations & repos.** `sources`, `sourceRepositories`,
  `githubConnection`, `gitlabOrganizationSources`/`gitlabSources`,
  `entireInstallations`, `getRepos`, `User.repositories`, and `Org.repoSettings`
  are backed by the forge-source and project repositories. Unsupported
  (GitHub OAuth URL, GitLab account linking, Entire, repo visibility/trigger
  mutations) return deterministic self-hosted errors.
- **Self-hosted error strings (verbatim).**
  - `billing is not available on this self-hosted deployment`
  - `GitHub OAuth app integration is not configured on this self-hosted
    deployment; configure forge integrations via /api/v1/orgs/<org>/integrations`
  - `personal GitLab account linking is not supported on this self-hosted
    deployment; configure GitLab integrations via /api/v1/orgs/<org>/integrations`
  - `Entire integration is not available on this self-hosted deployment`
  - `repository visibility settings are not supported on this self-hosted
    deployment; manage forge integrations via /api/v1/orgs/<org>/integrations`
  - `repository check triggers are managed via forge integrations on this
    self-hosted deployment (/api/v1/orgs/<org>/integrations)`
  - `check rerun is not available on this self-hosted deployment
    (checks.enabled is false)`

Shared helpers on the resolver root: `requireAuthenticated` (uniform
`unauthenticated`), `resolveAccessibleOrg` (ID then slug, uniform `forbidden`
for missing-or-inaccessible), `accessibleOrgs` (org-scoped token → its org;
global admin → all; else direct + group-derived memberships), and `scopedOrgs`
(scopes a resolver to its optional org argument when provided).

## Follow-up (2026-11-22 — forge-source semantics for `githubConnection`)

Live testing of the CLI v1.0.0-beta.28 against a self-hosted instance exposed
two integration semantics that diverge from the Dagger Cloud client
expectations; both are fixed here:

- **`Query.githubConnection` is keyed on forge sources, not the login method.**
  It now returns a `GitHubConnection` iff the caller can access at least one org
  with a GitHub forge source (`github` login = the owner segment of the source
  repo, `connectedAt` = the first such source's creation time, deterministic
  order: accessible orgs, then sources per org); otherwise `null`. The previous
  `OAuthProvider == "github"` check conflated a user's login provider with an
  org's integration.
- **`Query.sourceRepositories` scopes by its `org` argument** (resolved once via
  `resolveAccessibleOrg`), so a multi-org caller passing one org never sees
  another org's sources. `User.checks(first:)` caps **globally** across all
  accessible orgs (running total), not per org.

On self-hosted, GitHub/GitLab integrations are configured via the platform
(UI or `POST /api/v1/orgs/<org>/integrations`; the webhook receiver is
`POST /v1/webhooks/<provider>/<sourceID>`). `githubConnection` therefore
reflects **configured GitHub forge sources**, not a linked OAuth account.
The CLI's own GitHub App flow — `dagger cloud integration create github` and an
unmapped `dagger cloud checks on` — falls back client-side to the real Dagger
Cloud OAuth app (a hardcoded app URL) and is **not usable** on self-hosted
instances. Once a repo is mapped by a forge source, `dagger cloud checks
on`/`status`/`list` and `dagger cloud integration list` work. `dagger cloud
checks off` (`updateOrgRepoSettings`) returns a clear self-hosted error because
check triggers are managed by the forge source, not by per-repo settings.

## Follow-up (2026-11-23 — webhook secret handling)

Creating a forge integration without a webhook secret registered the remote
webhook with an empty secret; the forge then sent unsigned deliveries and
`verifyHMACSHA256` failed closed, so every delivery/ping returned 401. Fixed by
auto-generating a 32-byte hex secret when none is supplied, sealing that same
value, and passing it to `RegisterWebhook`. The previously dead
`forge.webhook_secret` config is now wired as the fallback HMAC secret for
sources stored without one (a non-empty per-source secret always wins; both
empty stays fail-closed).

Deleting a forge source now also best-effort removes its remote webhook:
`ForgeService.Delete` opens the stored token and calls the provider's
`DeleteWebhook` with an empty ID, which lists the repo's hooks and deletes every
one whose configured URL ends with `/v1/webhooks/<provider>/<sourceID>`
(suffix-only, since the public base URL is unknown to the provider). Creating a
source pre-cleans any stale hook with the same callback URL, so recreates do not
duplicate and pre-fix orphans are cleaned up. Cleanup requires the stored token
to still be valid; otherwise the hook must be deleted manually in the forge
settings.
