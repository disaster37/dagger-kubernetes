# ADR-059: GitHub CLI integration — OAuth credential linking, sealed tokens, and repository claims

- **Status:** Accepted
- **Date:** 2026-11-24
- **Supersedes:** the `githubOAuthURL` / `updateOrgRepoSettings` self-hosted
  stubs described in ADR-053 (when `forge.github.oauth` is configured)
- **Related:** ADR-015 (Raft FSM), ADR-016 (Raft transport), ADR-050 (shared
  go-auth/go-secguard), ADR-053 (device flow + Cloud GraphQL), ADR-056 (Cloud
  Checks), ADR-058 (org tenancy)

## Context

The Dagger CLI `v1.0.0-beta.28` ships `dagger cloud integration create github`,
`dagger cloud integration list`, and `dagger cloud checks on|off|status|list`.
Against `dagger-kubernetes` the UI/REST per-repo PAT integration worked, but the
native CLI path failed because:

1. `Query.githubOAuthURL` always returned a self-hosted error.
2. Repo-setting/claim mutations always returned a self-hosted error.
3. No persisted per-repo claim or trigger model existed; `org.repoSettings` and
   `user.repositories` synthesized constant values from `ForgeSource` rows, and
   `ChecksService.HandleWebhook` ignored per-repo trigger toggles.
4. Per-repo `ForgeSource`s cannot represent an owner before its first repo and
   duplicate owner rows in `Query.sources` (the CLI selects the first source
   whose `name` matches the repo owner as its `installationId`).
5. There was no user-scoped GitHub identity/token store, and deployments whose
   humans log in with internal credentials or OIDC (not GitHub) had no way to
   link a GitHub account.

## Decisions

### A. GitHub OAuth App primary, PAT fallback, GitHub App optional

- `forge.github.oauth` configures a GitHub **OAuth App** used for credential
  linking. It is combined with org account sources (one per
  `(orgID, baseURL, lower(owner))`) and per-repo claims.
- PAT paste is always available with zero OAuth config:
  `POST /api/v1/orgs/:org/github/accounts` (account-wide PAT) and the existing
  `POST /api/v1/orgs/:org/integrations` (per-repo PAT). Creating a GitHub
  `ForgeSource` with a token idempotently mirrors a parent `GitHubAccountSource`
  and `RepoSetting`, so CLI commands work no matter how the integration was
  created.
- `ForgeSource.AppID` / `InstallationID` / `PrivateKeyCiphertext` remain for a
  future GitHub App integration. `ChecksService.reportToForge` calls
  `CreateCheckRun` only when `AppID != ""`; OAuth App tokens (`gho_...`) and PATs
  use `SetCommitStatus` because GitHub rejects check-runs for them.

### B. GitHub RFC 8628 device flow coexistence

Outbound GitHub device flow is implemented server-side and exposed at
`POST /api/v1/github/device/code` + `POST /api/v1/github/device/poll`. It is the
UI fallback for port-forwarded / private-hostname deployments where only one
callback URL can be registered. Default scopes are `repo`, `read:org`, and
`admin:repo_hook`; OAuth App tokens do not expire by default and have no refresh
token. A revoked token is surfaced as `ErrGitHubTokenRevoked` and can be replaced
by re-running OAuth/device flow or pasting a new PAT.

### C. Three-tier token/claim ownership

1. **User scope (`UserGitHubConnection`)** — owned by one supervisor
   `User.ID`; AAD `github_user|<userID>`. Used for interactive owner/repo
   discovery and to provision org accounts. Never used at webhook/check runtime.
2. **Org account scope (`GitHubAccountSource`)** — owned by one Dagger org for
   one GitHub owner; AAD `github_org_account|<orgID>/<accountSourceID>`.
3. **Runtime repo scope (`ForgeSource`)** — materialized per claimed repo;
   AAD `org|<orgID>` (unchanged, read by Checks/Workflow/Forge services).

Disconnecting a user erases only the user ciphertext; org accounts and running
checks are unaffected. Disconnecting an org account (optionally cascading)
erases the org ciphertext, claims, materialized sources, and remote webhooks,
without touching the user connection.

Authorization is enforced after authentication: personal connection endpoints
require a real `UserID`; org reads require membership; all integration/claim
mutations require `org_admin` (or global `RoleAdmin`). Missing or inaccessible
orgs return a uniform `forbidden` (anti-enumeration).

### D. OAuth CSRF + PKCE and non-consuming user mismatch

`githubOAuthURL(redirectURI)` (and `POST /api/v1/orgs/:org/github/oauth/start`)
generates a 32-byte hex `state` plus an RFC 7636 PKCE verifier. Both are
persisted in Raft (`githubOAuthStates`, capped at 4096, 10-minute default TTL)
and consumed atomically in the FSM. `redirectURI` is strictly allowlisted to
`<public_url>/github/callback`, `<cors_allowed_origin>/github/callback`, or an
exact `forge.github.oauth.allowed_redirect_uris` entry.

The SPA posts `{code,state}` to `POST /api/v1/github/oauth/complete`.
`CompleteOAuth` compares `caller.UserID` with `state.UserID` using
`subtle.ConstantTimeCompare` **before** consuming the state or exchanging the
code. On mismatch it returns `403` (`*domain.OAuthStateUserMismatchError`
wrapping `domain.ErrForbidden`) with `code: state_user_mismatch`,
`expected_username`, `current_username`, and leaves the state intact. This keeps
the state redeemable by the legitimate owner and prevents a wrong-account
browser session or a third party from causing a trivial DoS. Only a matching
user consumes the state; replays then return `400`.

### E. Browser-to-CLI handback

The CLI runs no local callback listener. After completion the user re-runs
`dagger cloud integration list` / `checks on`. Completion auto-provisions the
personal `GitHubAccountSource` when the flow was bound to an org (and the caller
is its admin) or when the caller administers exactly one Dagger org. `/github/callback`
also offers GitHub organization owners from `GET /user/orgs` so additional
owners can be connected with one click.

### F. Guaranteed `CLOUD_CHECKS` / `CLOUD_MODULES`

`Org.Features` always emits the deduplicated union of `domain.DefaultOrgFeatures`
and the persisted feature list, all with status `ACTIVE`. `storeOrg` backfills
defaults for empty feature lists (including snapshot restore). Self-hosted
deployments do not gate features behind SaaS billing; `ChecksService` remains
the runtime dispatch gate.

### G. Credential linking is orthogonal to human login

`forge.github.oauth` is **not** a login provider. It never reads or depends on
`auth.oauth.*`; separate namespaces, callback paths
(`/github/callback` + `/api/v1/github/oauth/complete` vs.
`/api/v1/auth/oauth/github/*`), state stores (Raft vs. `oauth_state` cookie),
and token stores (`UserGitHubConnection` vs. `User.OAuthTokenCiphertext`).
Identity binding is always `domain.User.ID`: an internal user `admin` or `jane`
can link GitHub account `@octocat`. Synthetic org-scoped tokens
(`UserID == ""`) and legacy flat-file tokens are rejected on personal GitHub
endpoints. When the browser is unauthenticated, `/github/callback` bounces
through the normal login flow preserving `?code=&state=`; when it is
authenticated as another user the SPA renders a sign-out-and-continue card.

## Persistence and rollout

New Raft command kinds were appended **after** the existing maximum so numeric
IDs never shift: `47` upsert/`48` delete GitHub connection, `49`/`50`
account source, `51` batch upsert repo settings, `52` delete repo setting,
`53`/`54` create/consume OAuth state. Snapshot payload fields are additive with
`omitempty`; pre-upgrade snapshots restore into empty maps and backfill org
features. User deletion and org deletion/cascade erase the matching GitHub
state; deleting a `ForgeSource` also removes its `RepoSetting` and global repo
claim.

`forge.github.oauth.enabled` defaults to `false`, so existing deployments
upgrade with no config change and no Raft reset. Operators upgrade every pod
before enabling the OAuth app or invoking repo-claim mutations so no old
follower receives kinds `47..54`.

## Consequences

- The Dagger CLI GitHub integration and checks commands work end-to-end,
  including `checks off` (triggers become false while the claim and webhook stay
  in place) and `checks on` (triggers re-enable without duplicating webhooks).
- The existing REST/UI per-repo PAT flow and all legacy `ForgeSource` data keep
  working; existing sources appear in CLI listings and can be toggled without
  re-registering hooks.
- Operators must register one GitHub OAuth App callback URL
  (`https://<public-host>/github/callback`) and grant `repo`, `read:org`, and
  `admin:repo_hook`. Air-gapped / CLI-only environments can use the PAT paths.
- Classic OAuth Apps grant coarse `repo` scope. Operators requiring least
  privilege can paste a fine-grained PAT
  (`Contents: Read`, `Commit statuses: Write`, `Webhooks: Read & Write`) through
  the same account-source model.
- GitHub API access is hardened: HTTPS/TLS >= 1.2, cross-host redirect refusal,
  2 MiB response cap, bounded pagination (10 pages / 1000 repos), GET-only
  retries on 502/503/504 with jitter, rate-limit mapping (`429` with
  `Retry-After`), token-prefix scrubbing in error messages, and path-escaping of
  owner/repo/hook/sha segments.
