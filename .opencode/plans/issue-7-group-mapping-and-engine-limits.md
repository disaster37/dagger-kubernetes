# Plan: Auto-create mapped OAuth groups with a configurable default engine limit (issue #7)

## Goal

When an OAuth (GitHub / OIDC) group-mapping rule matches and produces a
supervisor group name, the supervisor must **auto-create that group if it does
not already exist** (idempotently), instead of the current behavior of logging
`oauth: group not found … skipping` and leaving the user unassigned. The
auto-created group must receive a **default engine limit**
(`max_runner_sessions`) that is settable via application config (viper, env
prefix `DAGGER_KUBERNETES_`) **and** exposed in the Helm chart.

This applies to both the login path and the IdP revalidation path, because both
share `reconcileMemberships`.

---

## 1. Current behavior (verified in code)

- `service.GroupMapper.Map` (`internal/service/group_mapper.go`) maps upstream
  provider groups → supervisor group names (first-match-wins, capture
  substitution, de-dupe). Empty list → identity; `mapIfActive` returns nil when
  no rules are configured.
- `service.completeOAuthLogin` (`internal/service/oauth.go`) is called by both
  `GitHubOAuthService.Complete` and `OIDCOAuthService.Complete`. It calls
  `reconcileMemberships(ctx, groups, logger, u, mappedGroups)`.
- `service.reconcileMemberships` (`oauth.go`) does `groups.GetByName(ctx, name)`
  and **skips** (Warn) any name that is not found. It never creates groups.
- `service.OAuthRevalidator.refresh` (`oauth_revalidator.go`) calls
  `reconcileMemberships` too, so revalidation shares the same skip behavior.
- `domain.Group.MaxRunnerSessions int` (`json:"max_runner_sessions"`) is the
  engine-session quota; `0` = unlimited (see `service.QuotaService`). Groups
  are created today only via `GroupService.Create` (admin API) and the
  `default` group bootstrap (`cmd/api/main.go` `bootstrapDefaultGroup`, which
  sets `AgentAvailable: true`, `MaxRunnerSessions: 0`).
- `repository.GroupRepo.Create` → `fsmState.upsertGroup(create=true)` returns
  `domain.ErrConflict` when a group with the same ID or the same
  **case-insensitive** name already exists (`repository/fsm.go`). Group-name
  uniqueness is case-insensitive at the storage layer; the name charset is
  enforced only by `service.ValidateGroupName`
  (`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`), NOT by the FSM.

---

## 2. Key decisions (made; implement these, do not re-open)

### D1 — Config key & default

- New config key (snake_case): **`auth.oauth.mapped_group_max_runner_sessions`**
- Type: `int`. Default: **`0`** (= unlimited), matching the `default` group and
  the UI's "max sessions (0=∞)" semantics.
- Env var: `DAGGER_KUBERNETES_AUTH_OAUTH_MAPPED_GROUP_MAX_RUNNER_SESSIONS`.
- Helm value (camelCase): **`auth.oauth.mappedGroupMaxRunnerSessions`**.
- Validation: reject `< 0` at config load. `0` is valid and means unlimited.

### D2 — "Default limit" semantics

The value is applied **only at creation time** of an auto-created group. It is
**never** applied to, and **never overwrites**, a group that already exists
(either admin-created or previously auto-created). Auto-created groups also get
`AgentAvailable: true` (hardcoded — otherwise the user could not provision
engines, defeating the feature), `AutoAssignPattern: ""`, `Description: ""`.

### D3 — Fail-open on group creation / membership sync

The `allowed_groups`/`allowed_orgs`/`allowed_teams` allowlist remains
**fail-closed** and is evaluated on raw upstream groups **before** mapping
(unchanged). Once the allowlist passes, group auto-creation and membership sync
remain **best-effort (fail-open)**: if `ensureGroupByName` cannot create a group
(invalid name, store error, `ErrNotLeader` on a follower), the group is skipped
with a Warn and the login/refresh still succeeds. Justification:
1. Authentication already succeeded (allowlist passed); denying the user here
   would be surprising.
2. Revalidation runs on **every** pod via `AuthService.Resolve`, but only the
   Raft leader can write — a follower's `Create`/`SetMembers` returns
   `ErrNotLeader`. Failing the request on `ErrNotLeader` would break login on
   all non-leader pods. Best-effort + Warn is the only safe choice and matches
   the existing handling of `reconcileMemberships` errors (already
   logged-as-Warn, non-fatal).

### D4 — Scope: mapped groups only

Only groups produced by `group_mappings` are auto-created. The `default_group`
fallback (`joinGroupByName`) keeps its current behavior (must pre-exist;
missing → Warn + skip). `default` is still bootstrapped at first boot. This
matches the issue ("…and map rules match").

### D5 — Race safety

`ensureGroupByName` is `GetByName` → (if `ErrNotFound`) `Create` → (if
`ErrConflict`) `GetByName`. On the Raft leader the FSM serializes creates, so a
simultaneous first login yields one `Create` winner and the loser re-fetches by
name. The re-fetched group's quota (set by the winner) is respected — never
overwritten.

### D6 — Group-name validation before create

`ensureGroupByName` runs `service.ValidateGroupName(name)` **before** creating.
Invalid names are skipped with a Warn (never stored). This is required because
the FSM does not validate names, and mapping replacements can expand to
arbitrary strings (spaces, >64 chars, empty after capture expansion, etc.).
Empty-string results are already dropped by `GroupMapper.Map`.

---

## 3. Data structures

### 3.1 `internal/domain/config.go` — `OAuthConfig`

Insert between `GroupMappings` and `DefaultGroup`:

```go
GroupMappings []GroupMappingRule `mapstructure:"group_mappings"` // provider group -> supervisor group regex mapping
// MappedGroupMaxRunnerSessions is the max_runner_sessions (concurrent engine
// sessions per group; 0 = unlimited) applied to supervisor groups that are
// AUTO-CREATED by group_mappings when the mapped target group does not exist.
// It is applied only at creation time — a pre-existing group's quota is never
// modified. Applied on both login and IdP revalidation.
MappedGroupMaxRunnerSessions int `mapstructure:"mapped_group_max_runner_sessions"`
DefaultGroup string `mapstructure:"default_group"` // auto-membership for new OAuth users; empty = none
```

No other domain type changes. `domain` remains stdlib-only (this is a plain int
field).

---

## 4. Function signatures (exact)

### 4.1 `internal/service/oauth.go` — NEW helper + 2 modified signatures

Add import `"errors"` to the import block (currently: `context`, `fmt`, `sort`,
`logrus`, `domain`).

```go
// ensureGroupByName resolves the supervisor group named name, creating it (with
// defaultMaxRunnerSessions and AgentAvailable=true) when it does not exist.
//
// Return contract (tri-state):
//   - (nil, nil): name is not a valid supervisor group name (logged + skip).
//   - (g, nil):   group resolved (pre-existing, or just created / re-fetched).
//   - (nil, err): store error; caller logs and skips the group (best-effort).
//
// Creation is idempotent and race-safe: on domain.ErrConflict the group is
// re-fetched by name (the concurrent winner's quota is respected). A
// pre-existing group's quota and agent_available are never modified.
func ensureGroupByName(
	ctx context.Context,
	groups domain.GroupRepository,
	name string,
	defaultMaxRunnerSessions int,
	logger *logrus.Logger,
) (*domain.Group, error)
```

Implementation outline (mechanical):

```go
func ensureGroupByName(ctx, groups, name, defaultMaxRunnerSessions, logger) (*domain.Group, error) {
	g, err := groups.GetByName(ctx, name)
	if err == nil {
		return g, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("get group %q: %w", name, err)
	}
	if !ValidateGroupName(name) {
		logger.WithField("group", name).Warn("oauth: mapped group name invalid, skipping (not auto-created)")
		return nil, nil
	}
	g = &domain.Group{
		ID:                newID(),
		Name:              name,
		MaxRunnerSessions: defaultMaxRunnerSessions,
		AgentAvailable:    true,
	}
	if err := groups.Create(ctx, g); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			// Concurrent first-login won the race: reuse the winner's group.
			return groups.GetByName(ctx, name)
		}
		return nil, fmt.Errorf("create group %q: %w", name, err)
	}
	logger.WithFields(logrus.Fields{
		"group":               name,
		"max_runner_sessions": defaultMaxRunnerSessions,
	}).Info("oauth: auto-created mapped group")
	return g, nil
}
```

Modified signature — `reconcileMemberships` (adds the default-limit int; the
"GetByName then skip" loop now calls `ensureGroupByName`):

```go
func reconcileMemberships(
	ctx context.Context,
	groups domain.GroupRepository,
	logger *logrus.Logger,
	u *domain.User,
	mappedNames []string,
	defaultMaxRunnerSessions int,
) ([]string, error)
```

Loop body changes from:

```go
g, err := groups.GetByName(ctx, name)
if err != nil {
	logger.WithError(err).WithField("group", name).Warn("oauth: group not found during revalidation, skipping")
	continue
}
wanted[g.ID] = true
```

to:

```go
g, err := ensureGroupByName(ctx, groups, name, defaultMaxRunnerSessions, logger)
if err != nil {
	logger.WithError(err).WithField("group", name).Warn("oauth: ensure mapped group failed, skipping")
	continue
}
if g == nil {
	continue // invalid name (already logged)
}
wanted[g.ID] = true
```

Modified signature — `completeOAuthLogin` (adds the int between `mappedGroups`
and `credential`):

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
	mappedGroupMaxRunnerSessions int,
	credential *oauthCredential,
) (access, refresh string, u *domain.User, err error)
```

Inside, the `reconcileMemberships(...)` call gains the extra argument:

```go
if oauthGids, rerr := reconcileMemberships(ctx, groups, logger, u, mappedGroups, mappedGroupMaxRunnerSessions); rerr != nil {
```

(`joinGroupByName` is unchanged.)

### 4.2 `internal/service/oauth_github.go` — `GitHubOAuthService`

Add field (in the struct, e.g. after `mapper`):

```go
mappedGroupMaxRunnerSessions int
```

Constructor `NewGitHubOAuthService` already receives `cfg *domain.OAuthConfig`;
add to the returned struct literal:

```go
mappedGroupMaxRunnerSessions: cfg.MappedGroupMaxRunnerSessions,
```

In `Complete`, the `completeOAuthLogin(...)` call gains
`s.mappedGroupMaxRunnerSessions` as the new argument (between `mappedGroups` and
`cred`).

### 4.3 `internal/service/oauth_oidc.go` — `OIDCOAuthService`

Same as 4.2: add `mappedGroupMaxRunnerSessions int` field, set from
`cfg.MappedGroupMaxRunnerSessions` in `NewOIDCOAuthService`, and pass
`s.mappedGroupMaxRunnerSessions` into `completeOAuthLogin`.

### 4.4 `internal/service/oauth_revalidator.go` — `OAuthRevalidator`

Add field to struct:

```go
mappedGroupMaxRunnerSessions int
```

Change constructor to add the parameter **after `adminGroups`**:

```go
func NewOAuthRevalidator(
	provider OAuthProvider,
	mapper *GroupMapper,
	adminGroups []string,
	mappedGroupMaxRunnerSessions int,
	users *UserService,
	groups domain.GroupRepository,
	tokens *TokenService,
	logger *logrus.Logger,
	cfg OAuthRevalidatorConfig,
) *OAuthRevalidator
```

Set the field in the returned struct. In `refresh()`, change:

```go
gids, err := reconcileMemberships(ctx, r.groups, r.logger, u, mapped)
```

to:

```go
gids, err := reconcileMemberships(ctx, r.groups, r.logger, u, mapped, r.mappedGroupMaxRunnerSessions)
```

### 4.5 `config/loader.go` — default + validator

Add default (in the `auth.oauth.*` block):

```go
v.SetDefault("auth.oauth.mapped_group_max_runner_sessions", 0)
```

Add validator and call it from `Load` immediately after `validateGroupMappings`:

```go
// validateOAuthGroupMappingDefaultLimit rejects a negative default engine
// limit for auto-created mapped groups. 0 (unlimited) is valid and is the
// default.
func validateOAuthGroupMappingDefaultLimit(cfg *domain.Config) error {
	if cfg.Auth.OAuth.MappedGroupMaxRunnerSessions < 0 {
		return fmt.Errorf("auth.oauth.mapped_group_max_runner_sessions must be >= 0 (0 = unlimited)")
	}
	return nil
}
```

In `Load`:

```go
if err := validateGroupMappings(&cfg); err != nil {
	return nil, fmt.Errorf("validate group mappings: %w", err)
}
if err := validateOAuthGroupMappingDefaultLimit(&cfg); err != nil {
	return nil, fmt.Errorf("validate group mapping default limit: %w", err)
}
```

### 4.6 `cmd/api/main.go` — revalidator wiring

The `NewOAuthRevalidator(...)` call gains
`cfg.Auth.OAuth.MappedGroupMaxRunnerSessions` after
`cfg.Auth.OAuth.AdminGroups`:

```go
revalidator := service.NewOAuthRevalidator(oauthSvc, mapper, cfg.Auth.OAuth.AdminGroups, cfg.Auth.OAuth.MappedGroupMaxRunnerSessions, usersSvc, groupRepo, tokensSvc, logger, service.OAuthRevalidatorConfig{ ... })
```

The GitHub/OIDC service constructors need **no** call-site change (they read the
new field from `&cfg.Auth.OAuth` internally).

---

## 5. Behavior spec (summary)

- **Rule matching** (unchanged): first-match-wins per provider group, Go regexp
  (RE2), case-sensitive by default (`(?i)` to opt out), no-match → dropped,
  empty capture expansion → dropped, de-dupe preserving first occurrence.
- **Auto-create**: for each mapped name, `ensureGroupByName` creates the group
  if absent with `MaxRunnerSessions = mapped_group_max_runner_sessions` and
  `AgentAvailable = true`; joins the user via the existing `addGroupMember`.
- **Existing group**: returned as-is; quota/`agent_available` untouched.
- **No rules configured** (`mapper.Active() == false`): `mapIfActive` returns
  nil → no creation, no membership sync (unchanged).
- **Duplicate rule targets**: already de-duped by `GroupMapper.Map`.
- **Idempotency/races**: `GetByName` → `Create` → on `ErrConflict` re-fetch
  (see D5).
- **Apply on both** login (`completeOAuthLogin`) and revalidation
  (`OAuthRevalidator.refresh`).

---

## 6. Edge cases

| Case | Behavior |
|---|---|
| `mapped_group_max_runner_sessions` negative | `config.Load` fails (`validateOAuthGroupMappingDefaultLimit`). |
| `mapped_group_max_runner_sessions` = 0 | Unlimited (valid default). |
| Mapped name already exists | Returned; quota NOT modified. |
| Mapped name invalid (`ValidateGroupName` false) | Warn + skip; not created, not joined; user may fall back to `default_group`. |
| Empty mapped name | Already dropped by `GroupMapper.Map`; never reaches `ensureGroupByName`. |
| Concurrent first login (same target) | One creates, the other gets `ErrConflict` and re-fetches by name (D5). |
| Store `Create` returns `ErrConflict` | Re-fetch by name. |
| Store `Create`/`GetByName` returns other error (incl. `ErrNotLeader`) | Warn + skip group; login/refresh succeeds (fail-open, D3). |
| No upstream groups / no allowlist match behavior | Unchanged (allowlist fail-closed first; empty groups → no mapping). |
| Mapping disabled (`group_mappings: []`) | No creation, no sync (unchanged). |
| Invalid regex in `group_mappings` | `config.Load`/`NewGroupMapper` already reject (unchanged). |
| `default_group` missing | Unchanged (skip; only `default` bootstrapped). |
| Case-differing existing group (`Engine` vs `engine`) | FSM `GetByName` is case-insensitive → found, not duplicated. |

---

## 7. Error handling & logging

- Wrap store errors with `%w` and `fmt.Errorf` (e.g. `"get group %q: %w"`,
  `"create group %q: %w"`). Use `errors.Is(err, domain.ErrNotFound)` /
  `domain.ErrConflict`.
- Logs (logrus): INFO `oauth: auto-created mapped group` (fields `group`,
  `max_runner_sessions`); WARN `oauth: mapped group name invalid, skipping (not
  auto-created)` (field `group`); WARN `oauth: ensure mapped group failed,
  skipping` (fields `error`, `group`). Keep the existing INFO
  `oauth: group mapping applied` summary.
- **HTTP**: no new status codes. The auth path never fails because of a group
  create (fail-open, D3). Existing `writeServiceError` mappings
  (`ErrConflict`→409 etc.) are untouched.

---

## 8. Files to create / modify

### Create
- `internal/service/oauth_test.go` — unit tests for `ensureGroupByName` +
  `reconcileMemberships` (see §10).
- `tests/integration/oauth_group_autocreate_test.go` — black-box end-to-end
  test (see §10).
- `docs/design/ADR-035-oauth-group-mapping-auto-create.md` — new ADR (see §11).

### Modify
- `internal/domain/config.go` — `MappedGroupMaxRunnerSessions` field (§3.1).
- `config/loader.go` — default + validator (§4.5).
- `config/config.app.yaml.sample` — add key + comment; update the
  `group_mappings` note (see §11).
- `config/config.app.yaml` — mirror the key + comment (dev default).
- `internal/service/oauth.go` — `ensureGroupByName`, modified
  `reconcileMemberships` + `completeOAuthLogin`, add `errors` import (§4.1).
- `internal/service/oauth_github.go` — field + constructor + call site (§4.2).
- `internal/service/oauth_oidc.go` — field + constructor + call site (§4.3).
- `internal/service/oauth_revalidator.go` — field + constructor param + call
  site (§4.4).
- `cmd/api/main.go` — `NewOAuthRevalidator` arg (§4.6).
- `deploy/helm/dagger-kubernetes/values.yaml` — value + `@param` (§9).
- `deploy/helm/dagger-kubernetes/templates/configmap.yaml` — render key (§9).
- `deploy/helm/dagger-kubernetes/README.md` — param table row (§9).
- `docs/README.md` — config table row + group-mapping section + troubleshooting
  + quota note (§11).
- `docs/design/index.md` — register ADR-035.
- Test files: `internal/service/oauth_github_test.go`,
  `internal/service/oauth_oidc_test.go`,
  `internal/service/oauth_revalidator_test.go`, `config/loader_test.go`
  (§10).

---

## 9. Helm chart changes

### 9.1 `deploy/helm/dagger-kubernetes/values.yaml`

In the `auth.oauth` section, after `defaultGroup: ""` and before the
`adminGroups` doc block, add:

```yaml
    ## @param auth.oauth.mappedGroupMaxRunnerSessions Default max_runner_sessions (concurrent engine sessions; 0 = unlimited) applied to supervisor groups auto-created by groupMappings. Applied only at creation time; never overwrites an existing group's quota.
    mappedGroupMaxRunnerSessions: 0
```

Also add a line to the GitHub example block (§ comment) and the Dex example
block (§ comment) demonstrating it, e.g.:

```yaml
#     mappedGroupMaxRunnerSessions: 4   # optional: cap auto-created mapped groups
```

### 9.2 `deploy/helm/dagger-kubernetes/templates/configmap.yaml`

In the `auth.oauth` block, after `default_group:` and before `admin_groups:`:

```yaml
        mapped_group_max_runner_sessions: {{ .Values.auth.oauth.mappedGroupMaxRunnerSessions }}
```

### 9.3 `deploy/helm/dagger-kubernetes/README.md`

Add a row to the auth parameters table (alphabetically/positionally after
`defaultGroup`, before `adminGroups`):

```markdown
| `auth.oauth.mappedGroupMaxRunnerSessions` | int | `0` | Default `max_runner_sessions` (0 = unlimited) for supervisor groups auto-created by `groupMappings`; applied only at creation time. |
```

Note: `scripts/update-helm-docs.sh` only rewrites version markers (it does NOT
generate the values table) — edit the README table row by hand.

---

## 10. Testing plan (stdlib `testing` only, table-driven, 100% coverage of touched packages)

### 10.1 `internal/service/oauth_test.go` (new)

- `TestEnsureGroupByName` — table-driven over the tri-state contract, using a
  real `repository.GroupRepo` via `newServiceDB(t)` for the success paths and a
  purpose-built stub for the error/conflict paths:
  - existing group returned unchanged (custom `MaxRunnerSessions` preserved);
  - missing group created with `MaxRunnerSessions == default` and
    `AgentAvailable == true`;
  - invalid name (`"has space"`, `""`, 65-char) → `(nil, nil)`;
  - `GetByName` returns a non-`ErrNotFound` error → `(nil, err)`;
  - `Create` returns `domain.ErrConflict` (via a `conflictOnCreateGroupRepo`
    test stub that, on first `Create`, pre-seeds the group with a different
    limit and returns `ErrConflict`) → re-fetch returns the winner's group
    (assert its limit is NOT overwritten by `default`).
- `TestReconcileMembershipsAutoCreates` — a mapped name that does not exist is
  auto-created and the user becomes a member; a pre-existing mapped group keeps
  its quota; an invalid mapped name is skipped (user not a member of it).

### 10.2 `internal/service/oauth_github_test.go` (modify)

- **Replace** `TestOAuthCompleteGroupMappingMissingGroup` (which currently
  asserts "missing mapped group should be skipped") with
  `TestOAuthCompleteGroupMappingAutoCreatesGroup`: same setup, but
  `cfg.MappedGroupMaxRunnerSessions = 3`; assert the user is a member of
  `does-not-exist` and that the auto-created group has
  `MaxRunnerSessions == 3` and `AgentAvailable == true`.
- **Extend** `TestOAuthCompleteGroupMapping` (pre-created groups): pre-create
  `acme-all` with `MaxRunnerSessions: 7`, set
  `cfg.MappedGroupMaxRunnerSessions = 3`, and assert `acme-all` still has
  `MaxRunnerSessions == 7` (existing quota never overwritten).
- Add `TestOAuthCompleteGroupMappingInvalidNameSkipped`: rule replacement
  produces an invalid name (`Replacement: "has space"`); assert login succeeds,
  no group with an invalid name exists, and the user falls back to
  `default_group` (or zero memberships if `default_group` empty).

### 10.3 `internal/service/oauth_oidc_test.go` (modify)

- Add `TestOIDCCompleteGroupMappingAutoCreatesGroup`: OIDC groups claim value
  mapped to a non-existent group; `cfg.MappedGroupMaxRunnerSessions = 2`;
  assert the group is auto-created with the limit and the user is a member.

### 10.4 `internal/service/oauth_revalidator_test.go` (modify)

- Add `TestOAuthRevalidatorRefreshAutoCreatesMappedGroup`: build a revalidator
  (with the new constructor arg) whose provider returns a mapped group name;
  run `refresh`; assert the group exists with the default limit and the user is
  a member.
- Update **all** existing `NewOAuthRevalidator(...)` call sites in this file to
  pass the new `mappedGroupMaxRunnerSessions int` argument (grep for
  `NewOAuthRevalidator(`).

### 10.5 `config/loader_test.go` (modify)

- Add `TestValidateOAuthGroupMappingDefaultLimit`: `0` ok, `3` ok, `-1` error
  (assert `auth.oauth.mapped_group_max_runner_sessions must be >= 0`).
- Extend an existing default-check test (or add one) asserting
  `cfg.Auth.OAuth.MappedGroupMaxRunnerSessions == 0` after a clean `Load`, and
  that `DAGGER_KUBERNETES_AUTH_OAUTH_MAPPED_GROUP_MAX_RUNNER_SESSIONS=5` is
  honored via `t.Setenv`.

### 10.6 `tests/integration/oauth_group_autocreate_test.go` (new)

Follow `tests/integration/oauth_oidc_test.go` + `net_helpers_test.go`
conventions exactly:
- `controlLn, dataLn := freeListener(t), freeListener(t)`; pass
  `ServerConfig.ControlListener/DataListener`; timed `srv.Shutdown` in
  `t.Cleanup`.
- Reuse `newOIDCIssuer(t, clientID, groups)` (add an exported/package helper or
  a local copy; it is package-private in `oauth_oidc_test.go` so reuse it
  directly — same `integration` package).
- Configure `oauthCfg.GroupMappings` with a rule mapping `devs` →
  `auto-created-dev` (target absent) and `MappedGroupMaxRunnerSessions: 4`.
- Drive the login → callback flow (as `TestOIDCLoginForbiddenFlow` does) and
  assert the callback redirects to the SPA (not `group_required`).
- Then assert via `groupRepo.GetByName(ctx, "auto-created-dev")` that the group
  now exists with `MaxRunnerSessions == 4`, and via
  `groupRepo.GroupsForUser(ctx, userID)` (resolve the user from the store by
  OAuth provider/sub, or via `userRepo.GetByOAuth`) that the user is a member.

---

## 11. Documentation updates

### 11.1 `config/config.app.yaml.sample`

- After `group_mappings`, add:

```yaml
    mapped_group_max_runner_sessions: 0   # default max_runner_sessions (concurrent engine sessions) for groups AUTO-CREATED by group_mappings; 0 = unlimited. Applied only at creation time; never overwrites an existing group's quota.
```

- Update the existing `group_mappings` comment block: change "Mapped target
  groups must already exist in the supervisor (only \"default\" is
  bootstrapped); missing groups are skipped with a Warn." to "Mapped target
  groups are AUTO-CREATED when missing, with
  `mapped_group_max_runner_sessions` and `agent_available=true`; a group that
  already exists is left unchanged."

### 11.2 `config/config.app.yaml`

Mirror the same new key + comment (dev default).

### 11.3 `docs/README.md`

- Config reference table: add row
  `| auth.oauth | mapped_group_max_runner_sessions | 0 | Default max_runner_sessions (0 = unlimited) for groups auto-created by group_mappings; applied only at creation time. |`
- "Both providers support regex group mapping" paragraph + "Important notes":
  replace "missing groups are skipped with a warning, never auto-created" and
  "Mapped target groups must already exist" with the auto-create semantics
  (created with the default limit + `agent_available=true`; existing groups
  untouched).
- "Troubleshooting group mapping" section: replace the "must already exist"
  bullet with an auto-create bullet; note the new INFO log
  `oauth: auto-created mapped group`.
- "Groups, projects, and quota" section: add a sentence that mapped groups
  auto-created by OAuth carry the configured default `max_runner_sessions`.

### 11.4 `docs/design/ADR-035-oauth-group-mapping-auto-create.md` (new)

Document: context (issue #7), the decision (auto-create mapped groups with a
configurable `mapped_group_max_runner_sessions` default + `agent_available=true`,
applied only at creation), the fail-open justification for membership sync
(D3), the race-safety design (D5), the name-validation requirement (D6), and the
config/Helm keys. Follow the existing ADR format (Status, Context, Decision,
Consequences, Related).

### 11.5 `docs/design/index.md`

Append row:

```markdown
| 035  | [OAuth group-mapping auto-creates missing groups with a default engine limit](ADR-035-oauth-group-mapping-auto-create.md) |
```

### 11.6 `DAGGER.md`

**Not required** — no change under `dagger/`, `.github/workflows/`, or CI
scripts.

---

## 12. Lint / CI compliance notes

- **No dead symbols** (`golangci-lint` `unused`): every new symbol has a call
  site — `ensureGroupByName` (called by `reconcileMemberships`),
  `MappedGroupMaxRunnerSessions` (read in both OAuth constructors + main.go),
  `validateOAuthGroupMappingDefaultLimit` (called in `Load`). Grep after
  refactor; delete anything orphaned.
- **Strings**: use `fmt.Sprintf`/`fmt.Errorf` — never `+` concatenation (the
  plan's examples already do).
- **Errors**: wrap with `%w`; use `errors.Is`.
- **Formatting**: `gofmt` + `goimports` with local prefix
  `github.com/disaster/dagger-kubernetes`.
- **Libraries**: only the mandated set (urfave/cli, viper, logrus, hertz; plus
  existing stdlib / `github.com/coreos/go-oidc` etc. that are already in use).
  No new third-party deps.
- **Dependency rule**: all new logic in `service` (business) and `config`
  (validation) + one int field in `domain`. `handler` and `repository` are
  unchanged. `domain` stays stdlib-only.

---

## 13. Verification

Minimum (no Docker daemon):

```bash
go build ./... && go vet ./... && go test ./...
dagger call -m ./dagger --src . lint
```

Full CI gate (Docker daemon available):

```bash
dagger call -m ./dagger --src . ci export --path out
```

Also:

```bash
helm lint deploy/helm/dagger-kubernetes
helm template dagger-kubernetes deploy/helm/dagger-kubernetes | grep -n mapped_group_max_runner_sessions
```

Integration tests run under `go test ./...` and start real Hertz servers via
`freeListener` (no hardcoded ports).

---

## 14. Git workflow

- Branch: **`feat/oauth-group-mapping-auto-create`** (created from `main`).
- Commit granularity: one logical change per commit (domain/config, service,
  wiring, helm, docs, tests) or a single well-scoped commit.
- PR title: **`fix(oauth): auto-create mapped groups with configurable default engine limit (#7)`**
- PR body outline:
  1. Problem: mapped OIDC/GitHub groups that don't exist in the supervisor are
     silently skipped, leaving users unassigned (issue #7).
  2. Solution: `group_mappings` targets are now auto-created (idempotent,
     race-safe) with `agent_available=true` and a configurable
     `auth.oauth.mapped_group_max_runner_sessions` default (0 = unlimited),
     exposed in the Helm chart. Applied on login and revalidation; existing
     groups are never modified.
  3. Fail-open rationale for membership sync (allowlist stays fail-closed).
  4. Validation performed (build/vet/test/lint/CI gate + helm lint/template).
  5. Docs + ADR-035 + chart updated.

---

## 15. Definition of done

- `go build ./... && go vet ./... && go test ./...` pass (race: `go test
  -race ./...` in CI gate).
- `dagger call -m ./dagger --src . lint` passes (no dead symbols).
- Full gate `dagger call -m ./dagger --src . ci export --path out` passes.
- `helm lint` + `helm template` render `mapped_group_max_runner_sessions`.
- A GitHub/OIDC login whose mapped target is absent auto-creates it with the
  configured limit and joins the user (unit + integration tests prove it).
- An existing mapped group's quota is never overwritten (tested).
- Redeploy + validate on the "home" cluster per `AGENTS.local.md` §4–§6.
