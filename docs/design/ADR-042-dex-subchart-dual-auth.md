# ADR-042: Dex as an optional subchart + concurrent internal/OIDC auth for the OIDC test environment

- **Status:** accepted
- **Date:** 2026-09-24
- **Deciders:** dagger-kubernetes maintainers
- **Resolves:** issue #30 live-validation gap — the test cluster ran internal
  (admin/password) auth only, so the OIDC expiry path (`stateExpired`, grace,
  fail-closed, re-login recovery, LDAP group-removal revocation) could not be
  exercised end-to-end against a real IdP.

## Context

The #30 fix (issue #30, PR #36) reclassified unusable OIDC credentials as
"cannot verify" instead of "revoked", but its live scenario — a stateless Dex
restart invalidating refresh tokens, then cached-group serving within
`revalidate_grace` — needs a real OIDC IdP **on the test cluster**. Three
questions had to be settled first:

1. **Can the supervisor run internal auth and OIDC at the same time?**
   Yes — no supervisor code change is required:
   - `config/loader.go` `validateAuthConfig` fails only when **both** are
     disabled; `auth.internal.enabled: true` **and** `auth.oauth.enabled: true`
     together pass validation (no either/or gate).
   - `cmd/api/main.go` wires the bootstrap admin and the OAuth service +
     revalidator **independently** of `cfg.Auth.Internal.Enabled`;
     `authSvc.SetOAuthRevalidator(...)` runs whenever `auth.oauth.enabled`.
   - `internal/handler/auth_endpoints.go` `handleProviders` reports `internal`,
     `oauth_github`, `oauth_oidc` independently; `handleLogin` is gated on
     internal auth, `handleOAuthOIDCLogin`/`handleOAuthOIDCCallback` on the
     OIDC provider. Both login paths appear side by side in the UI.
   - `internal/service/auth_service.go` `loadAuthorizedGroups` revalidates at
     the IdP **only** for users with `u.OAuthProvider != ""`; internal users
     bypass revalidation entirely, so there is no shared state to corrupt.
2. **Which Dex connector provides group membership?** Dex's `local` /
   `passwordDB` connector has **no groups** (its `staticPasswords` entries carry
   only `email`/`hash`/`username`/`userID`). Of the self-contained connectors,
   only **LDAP** is listed as stable while providing `groups=yes` **and**
   `refresh tokens=yes` — so Dex is paired with an in-cluster **OpenLDAP**
   (Dex's own `examples/ldap` pattern), seeded with a test user `jane` and a
   group `devs`.
3. **Do the new dependencies fit the existing chart pattern?** The chart
   already fetches 6 public Helm charts over the network during CI
   (`helm dependency update`; archives gitignored, not vendored). Dex
   (`https://charts.dexidp.io`, chart `0.24.1`, appVersion `2.44.0`) and
   OpenLDAP are added as 2 more **condition-gated** dependencies
   (`dex.enabled`, `openldap.enabled`, both `false` by default), with a
   `helmTemplateMatrix` variant proving they render when enabled — no workflow
   change, no vendored `.tgz`.

## Decision

1. **Dex + OpenLDAP as optional subcharts.** `dex.enabled` / `openldap.enabled`
   (default `false`) gate the dependencies in `Chart.yaml`; the `dex:` /
   `openldap:` values sections ship the full test IdP: issuer
   `https://dex-test.home.webcenter.fr` behind the nginx ingress with a
   Let's Encrypt cert (so the supervisor's default TLS trust accepts it —
   `auth.oauth.ca_cert_path` is **not** needed), plaintext in-cluster LDAP for
   the connector (dev/test only), and a LDIF that seeds `jane` +
   `ou=users`/`ou=groups` + group `devs` via `customLdifFiles`.
   - The Dex LDAP group matcher must be `userAttr: DN` / `groupAttr: member`
     for `groupOfNames` entries: Dex filters `(member=<userAttr value>)`, and
     `userAttr: uid` would filter `(member=jane)`, which never matches
     DN-valued members (verified in dex v2.44.0 `connector/ldap/ldap.go`
     `getAttrs`/`queryGroups`).
   - The OpenLDAP chart is the community jp-gouin `openldap` chart
     (`https://jp-gouin.github.io/helm-openldap/`, pinned `2.0.4`): bitnami's
     `openldap` chart was **removed** from `https://charts.bitnami.com/bitnami`
     (absent from the index; `.tgz` fetches 403) and from `bitnami/charts`.
   - The same changeset adds the four existing Go revalidation keys
     (`revalidate_interval`, `revalidate_grace`, `revalidate_fail_open`,
     `session_max_age`) to `values.yaml` + `templates/configmap.yaml` — the
     chart previously could not set them, so the test release could not shorten
     `revalidate_grace` to make the #30 scenario run in minutes.
2. **Concurrent internal + OIDC auth on the test release** — pure values:
   `auth.internal.enabled` stays `true` (default) and `auth.oauth.enabled=true`
   with `provider: oidc`. Both login buttons are served; internal users never
   hit the IdP revalidator, OIDC users (`jane`) do.
3. **Short-expiry test tuning, deliberately different from production tuning.**
   The test release sets Dex `oauth2.refreshTokens.absoluteLifetime: 10m` /
   `validIfNotUsedFor: 5m` / `reuseInterval: 5m`, **`expiry.idTokens: 1m`** and
   the supervisor `revalidate_interval: 30s` / `revalidate_grace: 2m`, so the
   whole expiry → grace → fail-closed → re-login-recovery scenario runs in
   minutes, and a stateless Dex restart (memory storage — intentionally not
   persistent) is the #30 trigger. `expiry.idTokens` was added during live
   validation: Dex issues **JWT access tokens whose `groups` claim is a
   snapshot from issuance**, with a **24h default validity**
   (`server/server.go` `idTokensValidFor`) — the supervisor only refreshes a
   credential once the access token is (nearly) expired, so with the default a
   group removal in LDAP would surface at the next refresh **up to a day
   later**, not within `revalidate_interval`. At `1m` (oauth2's expiry delta is
   60s) every revalidation refreshes the credential, Dex's LDAP connector
   `Refresh` re-queries the directory, and group changes are detected within
   one `revalidate_interval`. `reuseInterval` was widened from the planned `1m`
   to `5m` during live validation: Dex rotates the refresh token on every
   grant, but only the Raft leader can persist a rotation (followers log
   `not the raft leader`); with `1m` the leader missed the reuse window by
   seconds under sparse traffic and the stored credential stranded until the
   next login. Production keeps the `docs/README.md` Dex recipe
   instead: persistent storage, no `absoluteLifetime`, generous
   `validIfNotUsedFor`, `reuseInterval > revalidate_interval`, plus a finite
   `session_max_age` backstop (and, if fast de-provisioning matters, a shorter
   `expiry.idTokens` than the 24h default).
4. **Client-secret sync is a hard invariant:** the same secret string must be
   set in `dex.config.staticClients[0].secret` and `auth.oauth.clientSecret`
   (rendered into the `<fullname>-oauth` Secret); a mismatch fails with
   `invalid_client` at the token endpoint.

## Consequences

- CI's `helm dependency update` now also reaches `charts.dexidp.io` and
  `jp-gouin.github.io` on every run (8 public charts total; DAGGER.md updated).
- The default render is unchanged: both subcharts stay out unless enabled
  (proved by the default template-matrix variant plus the new
  `dex.enabled=true`/`openldap.enabled=true` variant asserting
  `name: dagger-kubernetes-dex`).
- The test release gains a `dex` Deployment and an `openldap` StatefulSet;
  `https://dex-test.home.webcenter.fr` must resolve publicly (DNS-01 via
  cert-manager `letsencrypt-prod`, same as the existing hosts).
- `AGENTS.local.md` documents the machine-specific recipe (issuer host,
  client-secret sync warning, short grace/interval, the live validation
  scenario); the subchart is dev/test oriented — plaintext LDAP and example
  credentials are **not** for production.

### References

- ADR-017: Auth always enforced + multi-provider OAuth
- ADR-027: OAuth group-membership revalidation & token invalidation (issue #30)
- issue #30 / PR #36: OIDC refresh-token expiry wrongly deactivated users
- Dex docs: connectors table + LDAP connector (`dexidp.io/docs/connectors/ldap/`)
