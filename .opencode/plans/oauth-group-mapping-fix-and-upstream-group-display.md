# Plan: Fix OAuth group mapping + persist & display upstream OAuth groups + `admin_groups` role promotion

## Goal

1. Fix why a Dex/OIDC user is not mapped into the intended supervisor `admin` group.
2. Persist the **raw upstream OAuth groups** per user (OIDC `groups` claim values;
   GitHub org names + `"org/team"` slugs) so the UI can display them.
3. Expose, per user, **current supervisor groups** (already returned) **and**
   **upstream OAuth groups** in the API and the admin Users page (both OIDC and
   GitHub).
4. Add an `auth.oauth.admin_groups` allowlist that promotes OAuth users to
   `RoleAdmin` automatically on login and revalidation (group mapping maps to
   *groups* only, never roles — this new allowlist is what makes Dex users in
   e.g. `HM_ADM_ETL_Outils` actually **admins**).

---

## 1. Root-cause analysis (fix part 1)

The user's pasted config is **not** the literal deployed file (it is malformed
YAML with unclosed quotes, and `issuerUrl`/`clientId` are camelCase while the
schema is snake_case; if it were literal, `config.Load` would fail and auth
would not succeed). Auth "succeeds", so the deployed file parses. The concrete
causes of "login works but user is not in the `admin` group" are:

1. **CamelCase vs snake_case keys (most likely primary cause).** The config keys
   are snake_case (`group_mappings`, `allowed_groups`, `issuer_url`,
   `username_claim`, `groups_claim`, `client_id`, `client_secret`). The snippet
   uses camelCase (`groupMappings`, `allowedGroups`, `issuerUrl`, `usernameClaim`,
   `groupsClaim`, `clientId`, `clientSecretRef`). `group_mappings` decoded from a
   `groupMappings` key is silently ignored → **no mapping runs at all**. This is
   a **config/docs** issue, but its silence is a diagnosability gap we fix with
   logging + docs.
2. **Mapped target groups must already exist in the supervisor.** Only the
   `default` group is bootstrapped. `reconcileMemberships`/`joinGroupByName`
   look up mapped names via `GetByName` and **skip missing groups with a Warn
   log, never auto-create** (`internal/service/oauth.go`). If the `admin` and
   `etlo` groups don't exist, the user is not added; they fall back to
   `default_group`. → **fix**: add an INFO diagnostic log showing upstream →
   mapped → joined/missing; document the requirement.
3. **`allowed_groups` runs on RAW upstream groups, BEFORE mapping** (documented
   in ADR-022). This is correct (fail-closed); keep it and document it. A user
   whose groups-claim values don't literally match `allowed_groups` gets
   `group_required` (not "success"), so this is not the user's blocker, but it
   must be documented to avoid confusion.
4. **Unanchored patterns.** `pattern: '^HM_ADM_ETL_Outils'` (no `$`) also matches
   `HM_ADM_ETL_Outils_jenkins` (prefix match). With first-match-wins, ordering
   decides the result. Harmless in this specific case (both map to `admin`) but
   fragile. → **docs/config** fix: recommend `^...$` anchors.
5. **Semantic: mapping maps to a *group*, not the admin *role*.** A group named
   `admin` is an ordinary group; the user's `Role` stays `user`. Group mapping
   can never grant admin *privileges*. This is resolved by the new
   `admin_groups` allowlist (§2), which is the mechanism the user actually wants.

**Code vs docs/config split for part 1:**
- **Docs/config (no code):** correct the YAML example (quotes + `$` anchors),
  use snake_case keys, document that mapped groups must pre-exist, document
  allowlist-before-mapping, document group-vs-role.
- **Code (diagnostics only, no mapping-semantics change):**
  - Add a summary INFO log at login/revalidation: `upstream_groups` →
    `mapped_groups` → resulting `oauth_group_ids`.
  - Add a startup WARN for mapping rules whose replacement is a literal (no `$`
    capture) that can never be a valid supervisor group name.

---

## 2. `auth.oauth.admin_groups` → `RoleAdmin` promotion (new)

### 2.1 Config

`internal/domain/config.go` — add to `OAuthConfig` (after `AllowedGroups`):

```go
// AdminGroups, when non-empty, promotes OAuth users to RoleAdmin when any of
// their RAW upstream provider groups (pre-mapping) exactly matches an entry.
// Case-sensitive. Empty = feature disabled (no OAuth user is auto-promoted).
AdminGroups []string `mapstructure:"admin_groups"`
```

`config/loader.go`:
- `v.SetDefault("auth.oauth.admin_groups", []string{})`
- New validator `validateOAuthAdminGroups(cfg *domain.Config) error`, called from
  `Load` right after `validateGroupMappings`. Rules:
  - every entry must be non-empty (reject `""` and whitespace-only, checked via
    `strings.TrimSpace`; do **not** mutate/trim the stored value — matching is
    exact);
  - reject duplicates (exact string equality, case-sensitive, matching the
    case-sensitive match semantics);
  - do **NOT** apply the supervisor group-name regex (entries are upstream IdP
    names, not supervisor group names).
  - Error text names the index, e.g. `auth.oauth.admin_groups[%d] must not be empty`
    and `auth.oauth.admin_groups[%d] %q duplicates an earlier entry`.

### 2.2 Role-assignment semantics (CRITICAL — decided)

Track OAuth-granted admin with a persisted boolean so revalidation can demote
OAuth-granted admins without clobbering manually-promoted admins.

`internal/domain/user.go` — add to `User` (after `OAuthGroups`):

```go
// OAuthAdmin records that the user's admin role was granted by the
// auth.oauth.admin_groups allowlist (not manual promotion). Revalidation uses
// it to demote OAuth-granted admins whose upstream groups no longer match,
// while never auto-demoting a manually-promoted admin (OAuthAdmin=false).
OAuthAdmin bool `json:"oauth_admin,omitempty"`
```

`internal/repository/fsm.go` — add `OAuthAdmin bool \`json:"oauth_admin,omitempty"\``
to `cmdUser`, and to `cmdUser.toDomain()` / `cmdUserFrom()`.

`internal/service/oauth.go` — add a shared pure helper (used by both login and
revalidation; lives beside `orgsIntersect`/`normalizeGroupList`):

```go
// applyOAuthAdminRole promotes/demotes u's role based on whether any RAW
// upstream group matches adminGroups (exact, case-sensitive via orgsIntersect).
//   - match + u is not admin      -> promote to RoleAdmin, OAuthAdmin=true.
//   - no match + OAuthAdmin       -> demote to RoleUser, OAuthAdmin=false.
//   - match + already admin        -> leave unchanged (do NOT take ownership of
//                                     a manually-promoted admin).
//   - no match + not OAuthAdmin    -> leave unchanged (manual admin or user).
// Returns true when the role changed (caller logs it).
func applyOAuthAdminRole(u *domain.User, adminGroups, upstreamGroups []string) bool
```

Consequences (documented, not hidden):
- A **manually-promoted admin** (`PUT /api/v1/users/:id` → `UpdateRole`) is never
  auto-demoted: on a non-matching login/revalidation `OAuthAdmin` stays false and
  `Role` is untouched.
- A **manually-promoted admin who also matches an admin group** is *not* taken
  over by OAuth (`match + already admin` keeps `OAuthAdmin=false`), so they
  remain safe from auto-demotion.
- An **OAuth-granted admin** removed from the admin group in the IdP is demoted to
  `RoleUser` on the next login or revalidation.
- To permanently remove an OAuth-granted admin, remove the user from the admin
  group in the IdP (or remove the group from `admin_groups`); an admin manually
  setting the role back to `user` via the API will be re-promoted on the next
  matching login (same "footgun" family already documented in ADR-022 for group
  membership). This is intentional and documented.

### 2.3 Where it is applied

`internal/service/oauth.go` — extend `completeOAuthLogin` signature with
`adminGroups []string` (before `upstreamGroups`), and inside (after setting
`u.OAuthGroups`, before `users.Update`) call
`applyOAuthAdminRole(u, adminGroups, upstreamGroups)` and log the change:

```go
func completeOAuthLogin(
    ctx context.Context,
    users *UserService,
    groups domain.GroupRepository,
    jwt *JWTService,
    logger *logrus.Logger,
    encKey []byte,
    provider, oauthID, username, defaultGroup string,
    adminGroups []string,
    upstreamGroups []string,
    mappedGroups []string,
    credential *oauthCredential,
) (access, refresh string, u *domain.User, err error)
```

`internal/service/oauth_oidc.go` + `internal/service/oauth_github.go`:
- Add `adminGroups []string` field to both service structs; set from
  `cfg.AdminGroups` in `NewOIDCOAuthService` / `NewGitHubOAuthService`.
- In `Complete()`, pass `s.adminGroups` into `completeOAuthLogin` (before the raw
  `groups`/`providerGroups`).
- The RAW groups are used (pre-mapping), consistent with `allowed_groups`
  (`orgsIntersect`). Match is exact and case-sensitive.

`internal/service/oauth_revalidator.go`:
- Add `adminGroups []string` field + constructor parameter to
  `NewOAuthRevalidator` (before `cfg`):
  `NewOAuthRevalidator(provider, mapper, adminGroups, users, groups, tokens, logger, cfg)`.
- In `refresh()` success path, after `reconcileMemberships` and before
  `r.users.Update(ctx, u)`, call `applyOAuthAdminRole(u, r.adminGroups, groups)`
  and log the change. Revoked / no-credential / unavailable paths leave `Role`
  and `OAuthAdmin` untouched.

`cmd/api/main.go`:
- Providers get `admin_groups` via `cfg.Auth.OAuth.AdminGroups` inside their
  constructors (no change needed at the call sites).
- `NewOAuthRevalidator(...)` call gains `cfg.Auth.OAuth.AdminGroups`.

**Same-login effect:** `EnsureOAuthUser` still creates the user as `RoleUser`;
`completeOAuthLogin` promotes them to `RoleAdmin` in the same login before
`users.Update`. `Identity` loads `Role` fresh from the users table at resolve time
(`auth_service.go` `identityForUser`), not from JWT claims, so the promotion is
effective immediately (subsequent `/me` and admin endpoints see `RoleAdmin`).

---

## 3. Persist upstream OAuth groups (part 2)

Add an informational field to `domain.User`, persisted through the existing Raft
command/snapshot path (no new store, no schema migration — the FSM is JSON-based).

### 3.1 `internal/domain/user.go`

Add to `User` (after `OAuthGroupIDs`):

```go
// OAuthGroups are the raw upstream provider group names captured at the last
// successful OAuth login or revalidation (OIDC `groups` claim values; GitHub
// org logins plus "org/team" team slugs). Informational only — surfaced in the
// UI. Never used for authorization or membership reconciliation (that uses
// OAuthGroupIDs).
OAuthGroups []string `json:"oauth_groups,omitempty"`
```

(`domain` stays stdlib-only. `OAuthAdmin` from §2.2 is added right after.)

### 3.2 `internal/repository/fsm.go`

- Add to `cmdUser` (between `OAuthGroupIDs` and `DeactivatedAt`):
  ```go
  OAuthGroups []string `json:"oauth_groups,omitempty"`
  OAuthAdmin  bool     `json:"oauth_admin,omitempty"`
  ```
- `cmdUser.toDomain()`: add `OAuthGroups: c.OAuthGroups,` and `OAuthAdmin: c.OAuthAdmin,`
- `cmdUserFrom(u *domain.User)`: add `OAuthGroups: u.OAuthGroups,` and `OAuthAdmin: u.OAuthAdmin,`

No `commandKind` change; no `fsm_snapshot.go` change (snapshot serializes
`cmdUser`). JSON `omitempty` keeps forward/backward compatibility (older nodes
ignore the unknown keys; newer nodes see nil/zero for pre-upgrade users).

### 3.3 `internal/service/oauth.go`

- New helper (dedupe + sort for deterministic storage/display):
  ```go
  // normalizeGroupList de-duplicates (preserving first occurrence, dropping
  // empty strings) and sorts a group-name list for stable storage and display.
  // Empty input yields nil so the omitempty JSON tag keeps the field absent.
  func normalizeGroupList(names []string) []string
  ```
  (`sort` is already imported in this file.)
- In `completeOAuthLogin`, after the deactivation-clear block (before
  `users.Update`): `u.OAuthGroups = normalizeGroupList(upstreamGroups)`.
- Add a summary diagnostic log after reconciliation (and before `users.Update`):
  ```go
  logger.WithFields(logrus.Fields{
      "user_id":         u.ID,
      "oauth_provider":  provider,
      "upstream_groups": upstreamGroups,
      "mapped_groups":   mappedGroups,
      "oauth_group_ids": u.OAuthGroupIDs,
  }).Info("oauth: group mapping applied")
  ```
  (The existing per-group `Warn("oauth: group not found, skipping")` /
  `"... during revalidation, skipping"` remains the "missing group" signal.)

### 3.4 `internal/service/oauth_oidc.go`

In `Complete()`, the raw groups are `groups` (from `resolveGroups`). Change the
`completeOAuthLogin(...)` call to pass `s.adminGroups` then `groups` then
`mappedGroups`:
```go
access, refresh, u, err = completeOAuthLogin(ctx, s.users, s.groups, s.jwt,
    s.logger, s.encKey, "oidc", sub, username, s.defaultGroup,
    s.adminGroups, groups, mappedGroups, cred)
```

### 3.5 `internal/service/oauth_github.go`

In `Complete()`, the raw groups are `providerGroups` (orgs + teams). Pass them:
```go
access, refresh, u, err = completeOAuthLogin(ctx, s.users, s.groups, s.jwt,
    s.logger, s.encKey, "github", strconv.Itoa(ghUser.ID), ghUser.Login,
    s.defaultGroup, s.adminGroups, providerGroups, mappedGroups, cred)
```

### 3.6 `internal/service/oauth_revalidator.go`

In `refresh()` success path, after `mapped := r.mapper.mapIfActive(groups)` and
`reconcileMemberships(...)`, and before `r.users.Update(ctx, u)`:
```go
u.OAuthGroups = normalizeGroupList(groups)
applyOAuthAdminRole(u, r.adminGroups, groups) // from §2.3
```
(The `errOAuthNoCredential` / revoked / unavailable paths leave `u.OAuthGroups`
and `u.OAuthAdmin` unchanged.)

---

## 4. Expose upstream + current groups (part 3)

### 4.1 `internal/handler/users.go`

- Add to `userRow`:
  ```go
  OAuthGroups []string `json:"oauth_groups,omitempty"`
  ```
- In `toUserRow`, set `OAuthGroups: u.OAuthGroups,`.

(`OAuthAdmin` is internal bookkeeping and is intentionally **not** exposed.)

### 4.2 `internal/handler/auth_endpoints.go`

- Add to `authMeResponse`:
  ```go
  OAuthGroups []string `json:"oauth_groups,omitempty"`
  ```
- In `toAuthMeResponse`, set `OAuthGroups: u.OAuthGroups,`.
- `syntheticUserResponse()` (legacy flat-file identity) leaves it zero/omitted.

### 4.3 UI — `ui/src/api/types.ts`

- Add `oauth_groups?: string[]` to both `UserRow` and `AuthUser`.

### 4.4 UI — `ui/src/views/admin/Users.vue`

- Add a table column **"OAuth groups"** (upstream) next to the existing
  **"Groups"** (current) column. Render `u.oauth_groups` as muted badges:
  ```html
  <th>OAuth groups</th>
  ...
  <td>
    <span v-for="og in u.oauth_groups" :key="og" class="badge badge-muted" style="margin-right: 4px;">{{ og }}</span>
    <span v-if="!u.oauth_groups?.length">—</span>
  </td>
  ```
  Add a small `.badge-muted` style (e.g. `background:#161b22; color:#8b949e;`)
  to visually distinguish upstream from current groups.

### 4.5 UI — `ui/src/views/Settings.vue` (current-user view)

- Next to the existing groups list (line ~18), render `auth.user?.oauth_groups`
  as a muted "OAuth groups (upstream)" list, or `—` when empty. Optional but
  recommended for completeness.

No router or `client.ts` changes required (`listUsers`/`fetchMe` already return
the whole row shape).

---

## 5. Validation / diagnostics additions

### 5.1 `internal/service/group_mapper.go` (or `group_service.go`)

Export a thin wrapper over the existing `groupNameRe` so `main` can warn without
duplicating the regex:

```go
// ValidateGroupName reports whether name satisfies the supervisor group-name
// rule (^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$).
func ValidateGroupName(name string) bool { return groupNameRe.MatchString(name) }
```

(`groupNameRe` already exists in `internal/service/group_service.go`; the wrapper
lives beside the mapper or in `group_service.go`. It is exercised by tests so it
is not a dead symbol.)

### 5.2 `cmd/api/main.go`

After `mapper, err = service.NewGroupMapper(cfg.Auth.OAuth.GroupMappings)` succeeds,
add a startup warning loop:

```go
for i, rule := range cfg.Auth.OAuth.GroupMappings {
    if !strings.Contains(rule.Replacement, "$") && !service.ValidateGroupName(rule.Replacement) {
        logger.WithFields(logrus.Fields{
            "rule_index":  i,
            "replacement": rule.Replacement,
        }).Warn("oauth: group_mappings replacement is not a valid supervisor group name and has no capture reference; it can never match an existing group")
    }
}
```

(`strings` and `logrus` are already imported in `cmd/api/main.go`.)

### 5.3 `config/loader.go` — `validateOAuthAdminGroups` (§2.1)

Also add `v.SetDefault("auth.oauth.admin_groups", []string{})`.

---

## 6. Edge cases & error handling

**Group mapping / display:**
- **Nil/empty upstream groups** → `normalizeGroupList` returns nil → field
  omitted from JSON; UI renders `—`.
- **Groups with empty strings** → dropped by `normalizeGroupList`.
- **Pre-upgrade users** (existing FSM rows without `oauth_groups`/`oauth_admin`)
  → field nil/zero; populated on next login/revalidation; no migration needed.
- **Mixed-version cluster** → additive `omitempty` fields are JSON-compatible both
  directions.
- **Missing mapped group** → unchanged: Warn + skip (never fatal, never
  auto-created); the new summary INFO log makes this visible.
- **allowlist** → still evaluated on RAW upstream groups before mapping
  (unchanged, documented). Non-matching → `domain.ErrForbidden` →
  `/auth/login?error=group_required`.
- **Legacy flat-file identity** → `syntheticUserResponse()` has no
  `oauth_groups`; omitted.
- **Revalidation unavailable/revoked** → `u.OAuthGroups` left as last-known
  (stale display is acceptable; it is informational only, never used for authz).

**`admin_groups` role assignment:**
- **Empty `admin_groups`** → `orgsIntersect(empty, …)` = false → no promotion,
  feature disabled.
- **Empty/absent groups claim** → `upstreamGroups` nil → non-match → no promotion.
- **Non-matching user** (never OAuth admin) → `Role` unchanged (`RoleUser` on new
  users; a manually-promoted admin keeps `RoleAdmin`).
- **Privilege escalation impossible**: match requires a literal upstream group
  name the IdP controls (exact, case-sensitive), matched pre-mapping; a mapped
  group name can never satisfy `admin_groups`.
- **Group removal** → `OAuthAdmin`-tracked admins are demoted on next login and
  revalidation; manually-promoted admins are never auto-demoted.
- **Revoked/expired credential** → revalidation returns an error before
  `applyOAuthAdminRole`, so `Role`/`OAuthAdmin` stay unchanged (and the user may
  be deactivated by the existing revoke path).
- **Manual promotion + matching group** → not taken over (`OAuthAdmin` stays
  false), so it survives group removal.
- **Manual API demote of an OAuth-granted admin** → re-promoted on the next
  matching login (documented; remove from the IdP group to make it stick).

---

## 7. Test plan (stdlib `testing`, table-driven, 100% coverage target)

- **`internal/service/group_mapper_test.go`** — add `TestValidateGroupName`:
  table of valid/invalid names (`admin`, `etlo`, `acme-eng`, `9bad`, `UPPER ok`,
  `with space`, `""`, 64-char boundary).
- **`internal/service/oauth.go` (new `oauth_test.go` or reuse)**:
  - `TestNormalizeGroupList`: nil→nil, empty→nil, dedupe, sort, drop empties.
  - `TestApplyOAuthAdminRole`: table covering every branch — promote non-admin on
    match; demote `OAuthAdmin` on non-match; preserve manual admin on non-match;
    leave `OAuthAdmin=false` when already admin + match; no-change when empty
    `adminGroups`; no-change when empty `upstreamGroups`; case-sensitivity.
- **`internal/service/oauth_oidc_test.go`** — extend existing `Complete` tests:
  - `u.OAuthGroups == normalizeGroupList(groupsClaimValues)` after success (nil
    when claim absent);
  - promotion to `RoleAdmin` (+`OAuthAdmin=true`) when a group is in
    `admin_groups`; stays `RoleUser` on non-match; manually-promoted admin stays
    `RoleAdmin` on non-match; OAuth-admin demoted on non-match.
- **`internal/service/oauth_github_test.go`** — same for GitHub: `u.OAuthGroups`
  equals sorted dedupe of orgs + `"org/team"` teams; `admin_groups` promotion
  using org/team raw groups.
- **`internal/service/oauth_revalidator_test.go`** — `refresh()` promotes on
  match; demotes `OAuthAdmin` on group removal; preserves manual admin
  (`OAuthAdmin=false`) on non-match; revoked/no-credential paths leave
  `Role`/`OAuthAdmin` unchanged; `u.OAuthGroups` refreshed on success.
- **`internal/repository/fsm_test.go`** — `TestUserOAuthFieldsRoundTrip`:
  `cmdUserFrom(u)` → `toDomain()` preserves `OAuthGroups` and `OAuthAdmin`; and
  an upsert→readUserByID→snapshot/restore round-trip preserves both fields.
- **`internal/handler/users_test.go`** — assert `userRow.OAuthGroups` populated;
  role still correct (and `OAuthAdmin` is not exposed).
- **`internal/handler/auth_endpoints_test.go`** — assert `/api/v1/auth/me` (and
  login) include `oauth_groups`.
- **`config/loader_test.go`** — add `TestValidateOAuthAdminGroups`: empty (ok),
  valid entries (ok), empty-string entry (err), whitespace-only entry (err),
  duplicate entry (err); assert `admin_groups` default is non-nil empty. Also
  keep existing `validateGroupMappings` tests.
- **UI** — no unit test framework for the SPA; rely on `vue-tsc` type-check (part
  of CI) for the new optional field. Manual verification per AGENTS.local.md §5.2.

The service test stubs in `internal/service/stubs_test.go` need **no change**
(the in-memory `stubUserRepo` stores `*domain.User` by reference; new fields flow
through).

---

## 8. Docs to update (per AGENTS.md)

- **`config/config.app.yaml.sample`** — replace the commented `group_mappings`
  example with a fully-correct one (proper single quotes, `^...$` anchors), add
  inline comments: (a) `allowed_groups` is checked against **raw upstream**
  groups before mapping; (b) mapped target groups must already exist (only
  `default` is auto-created); (c) mapping assigns **group membership**, not the
  admin **role** — use `admin_groups` for role promotion. Add:
  ```yaml
  admin_groups: []                 # upstream group names that grant RoleAdmin on login/revalidation (exact, case-sensitive); empty = disabled.
  ```
- **`config/config.app.yaml`** — mirror the same corrected `group_mappings`
  comment/example and add `admin_groups: []` (it is the dev default).
- **Helm chart**:
  - `deploy/helm/dagger-kubernetes/values.yaml`: add `## @param auth.oauth.adminGroups [array] ...`
    doc comment, `adminGroups: []` + an example comment, and add `adminGroups` to
    the GitHub/OIDC example blocks.
  - `deploy/helm/dagger-kubernetes/templates/configmap.yaml`: render
    `admin_groups:` under `oauth:` (mirroring `allowed_groups`):
    ```yaml
            admin_groups:
    {{ toYaml .Values.auth.oauth.adminGroups | indent 10 }}
    ```
  - `deploy/helm/dagger-kubernetes/README.md`: regenerate via
    `scripts/update-helm-docs.sh` (adds the `auth.oauth.adminGroups` row).
- **`docs/README.md`**:
  - Add `admin_groups` to the OAuth config reference table (near the
    `allowed_groups`/`group_mappings` rows) and a new **"Automatic admin role via
    admin_groups"** subsection under Authentication (semantics, demotion/tracking
    trade-off, security rationale).
  - Fix the OIDC Dex example (`allowed_groups` + anchored `group_mappings` +
    `admin_groups`) and the GitHub example (anchored patterns).
  - Add a **"Troubleshooting group mapping"** subsection covering: snake_case
    keys; allowlist-before-mapping; mapped groups must pre-exist; group-vs-role
    (now solvable with `admin_groups`); the new `oauth: group mapping applied`
    INFO log; and the new `oauth_groups` field on the Users page + `/me`.
  - Note the new API field `oauth_groups` (array of upstream group names) on
    `/api/v1/users` and `/api/v1/auth/me`.
- **`docs/design/`**:
  - `ADR-030-upstream-oauth-group-display-and-diagnostics.md` (new): the
    `User.OAuthGroups` display field + diagnostic logs.
  - `ADR-031-oauth-admin-groups-role-promotion.md` (new): `admin_groups` →
    `RoleAdmin`, the `OAuthAdmin` tracking field, demotion semantics, and the
    security rationale (match is literal IdP-controlled upstream group name,
    pre-mapping; manual admins never auto-demoted).
  - Register both in `docs/design/index.md` (append rows `030`, `031`).
- **`DAGGER.md`** — **not** required (no change under `dagger/`, `.github/`, or
  CI scripts).

---

## 9. Ordered implementation steps

1. `internal/domain/config.go`: add `AdminGroups []string` (`admin_groups`).
2. `internal/domain/user.go`: add `OAuthGroups []string` (`oauth_groups`) and
   `OAuthAdmin bool` (`oauth_admin`).
3. `internal/repository/fsm.go`: add both fields to `cmdUser`, `toDomain()`,
   `cmdUserFrom()`.
4. `internal/service/oauth.go`: add `normalizeGroupList` and
   `applyOAuthAdminRole`; extend `completeOAuthLogin` with `adminGroups` +
   `upstreamGroups`; set `u.OAuthGroups`, call `applyOAuthAdminRole`; add summary
   INFO log.
5. `internal/service/oauth_oidc.go` + `internal/service/oauth_github.go`: add
   `adminGroups` field + constructor wiring; pass `s.adminGroups` and raw
   upstream groups into `completeOAuthLogin`.
6. `internal/service/oauth_revalidator.go`: add `adminGroups` field/constructor
   param; refresh `u.OAuthGroups` and apply `applyOAuthAdminRole` on success.
7. `internal/service/group_mapper.go` (or `group_service.go`): add
   `ValidateGroupName`.
8. `cmd/api/main.go`: add startup replacement-validity WARN loop; pass
   `cfg.Auth.OAuth.AdminGroups` to `NewOAuthRevalidator`.
9. `config/loader.go`: add `admin_groups` default + `validateOAuthAdminGroups`.
10. `internal/handler/users.go` + `internal/handler/auth_endpoints.go`: add
    `OAuthGroups` to `userRow` / `authMeResponse` and populate.
11. UI: `ui/src/api/types.ts` (`oauth_groups?`), `ui/src/views/admin/Users.vue`
    (new column + badge style), `ui/src/views/Settings.vue` (upstream groups).
12. Helm: `values.yaml` (`adminGroups` + `@param`), `configmap.yaml`
    (`admin_groups`), regenerate `README.md` via `scripts/update-helm-docs.sh`.
13. Tests: add/extend the files in §7.
14. Docs: §8 files (config samples, README, ADR-030 + ADR-031 + index).
15. Run formatting/lint/tests; then the CI gate (below).

---

## 10. Definition of done

- `go build ./... && go vet ./... && go test ./...` pass (race: `go test -race`).
- `dagger call -m ./dagger --src . lint` passes (no dead symbols — verify
  `ValidateGroupName`, `normalizeGroupList`, `applyOAuthAdminRole`,
  `validateOAuthAdminGroups`, and the new fields/params all have call sites).
- Full CI gate: `dagger call -m ./dagger --src . ci export --path out`
  (golangci-lint incl. `unused`, `go vet`, `go test -race`, UI build incl.
  `vue-tsc`, binary builds, Dockerfile smoke, Helm lint/template).
- Helm lint/template passes with the new `admin_groups` configmap rendering.
- UI type-check passes with the new optional field.
- Group-mapping diagnostic log appears on login; upstream groups render on the
  admin Users page and `/me`.
- An OAuth user in an `admin_groups` entry is `RoleAdmin` on first login and
  after revalidation; removal from the group demotes them; manual admins are
  unaffected.
- Redeploy + validate on the "home" cluster per AGENTS.local.md §4–§6 (build →
  push → capture values → helm upgrade → rollout restart → §5.1 checks →
  §5.2 human UI confirmation).
