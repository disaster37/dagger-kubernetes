# Default Group Auto-Assign — Implementation Plan

## Goal Summary

Ensure every user always belongs to at least one group with `AgentAvailable=true`, so engines can be provisioned. Bootstrap a hardcoded "default" system group at first startup (when no groups exist), and auto-assign users to it when they have zero group memberships after all other group-assignment logic has run.

## Design Decisions

| ID | Decision | Rationale |
|----|----------|-----------|
| **D1** | Group name hardcoded as `"default"` — no config keys | User directive. Simplest possible design. |
| **D2** | Bootstrap trigger: `groupsSvc.List()` returns empty (same pattern as `bootstrapAdmin` which uses `users.Count() == 0`) | Idempotent; only fires on very first boot; matches existing bootstrap convention. |
| **D3** | Bootstrap attributes: `AgentAvailable=true`, `MaxRunnerSessions=0` (unlimited) | Matches the "legacy" group pattern in `legacy_import.go`. Ensures engines can be provisioned. |
| **D4** | Migration sweep runs at bootstrap time: every existing user with zero memberships gets added to the default group | Handles upgrade path: users created before this feature existed. |
| **D5** | `UserService.Create` auto-assigns the new user to the "default" group (best-effort, non-fatal) | Internal users always land in a group immediately. |
| **D6** | `completeOAuthUser` adds user to "default" group when post-mapping memberships are empty — on **every** login, not just first creation | Replaces the old `defaultGroup` first-creation-only behavior. Covers users whose mapped groups were deleted or never matched. |
| **D7** | Bootstrap runs in `cmd/api/main.go` after `bootstrapAdmin`, after Raft init and leader wait | Follows existing bootstrap pattern. Writes go through Raft (leader-only). |
| **D8** | Leader-only write handling: `bootstrapDefaultGroup` relies on `groupsSvc.List()` returning non-empty on followers (replicated state). If a follower races ahead, `Create()` returns `ErrNotLeader` → startup fails (same pre-existing pattern as `bootstrapAdmin`) | StatefulSet sequential pod startup makes this race extremely unlikely. |
| **D9** | No changes to `domain.Config`, `config/loader.go`, Helm values, or configmap | Fully hardcoded feature needs no configuration. |

## Files to Modify

### 1. `cmd/api/main.go` — Bootstrap default group at startup

**Add new function `bootstrapDefaultGroup`** (modeled on `bootstrapAdmin` at line 628):

```go
// bootstrapDefaultGroup creates the "default" system group when no groups exist
// yet (first boot). Existing users with zero memberships are swept into it.
// Idempotent: when groups already exist this is a no-op.
func bootstrapDefaultGroup(ctx context.Context, groups *service.GroupService, users *service.UserService, groupRepo domain.GroupRepository, logger *logrus.Logger) error {
    existing, err := groups.List(ctx)
    if err != nil {
        return err
    }
    if len(existing) > 0 {
        return nil // idempotent: groups already exist
    }

    g, err := groups.Create(ctx, service.GroupInput{
        Name:              "default",
        AgentAvailable:    true,
        MaxRunnerSessions: 0,
    })
    if err != nil {
        return fmt.Errorf("create default group: %w", err)
    }
    logger.WithField("group_id", g.ID).Info("bootstrap default group created")

    // Sweep existing users with zero memberships into the default group.
    allUsers, err := users.List(ctx)
    if err != nil {
        return fmt.Errorf("list users for default group sweep: %w", err)
    }
    swept := 0
    for _, u := range allUsers {
        userGroups, err := groups.GroupsForUser(ctx, u.ID)
        if err != nil {
            logger.WithError(err).WithField("user_id", u.ID).Warn("default group sweep: check memberships failed")
            continue
        }
        if len(userGroups) > 0 {
            continue
        }
        if err := groups.EnsureMember(ctx, g.ID, u.ID); err != nil {
            logger.WithError(err).WithFields(logrus.Fields{
                "user_id":  u.ID,
                "username": u.Username,
            }).Warn("default group sweep: add member failed")
            continue
        }
        swept++
    }
    if swept > 0 {
        logger.WithField("swept", swept).Info("default group sweep: added existing users")
    }
    return nil
}
```

**Call it in `run()`** after `bootstrapAdmin` (after line 204):

```go
// Bootstrap default group (idempotent: only when groups table is empty).
if err := bootstrapDefaultGroup(ctx, groupsSvc, usersSvc, groupRepo, logger); err != nil {
    return fmt.Errorf("bootstrap default group: %w", err)
}
```

**Import additions**: `groupRepo` is already available (`domain.GroupRepository`). No new imports needed.

---

### 2. `internal/service/group_service.go` — Add `EnsureMember` public method

Add after `SetUserGroups` (after line 162):

```go
// EnsureMember adds userID to the group identified by groupID if they are not
// already a member. Idempotent — safe to call when the user is already present.
func (s *GroupService) EnsureMember(ctx context.Context, groupID, userID string) error {
    if _, err := s.groups.Get(ctx, groupID); err != nil {
        return fmt.Errorf("group %s: %w", groupID, err)
    }
    if _, err := s.users.Get(ctx, userID); err != nil {
        return fmt.Errorf("user %s: %w", userID, err)
    }
    members, err := s.groups.Members(ctx, groupID)
    if err != nil {
        return fmt.Errorf("list members of %s: %w", groupID, err)
    }
    for _, m := range members {
        if m.ID == userID {
            return nil // already a member
        }
    }
    return addGroupMember(ctx, s.groups, groupID, userID)
}
```

This method is needed by `bootstrapDefaultGroup` in `main` (can't call unexported `addGroupMember` from `package main`). It also serves as a clean public API for future use.

---

### 3. `internal/service/user_service.go` — Auto-assign on `Create`

In the `Create` method (after line 71, before the `return u, nil` at line 76), add:

```go
// Auto-assign to the default group (best-effort, never blocks user creation).
if g, err := s.groups.GetByName(ctx, "default"); err == nil {
    if err := addGroupMember(ctx, s.groups, g.ID, u.ID); err != nil {
        s.logger.WithError(err).WithFields(logrus.Fields{
            "user_id":  u.ID,
            "username": u.Username,
        }).Warn("auto-assign to default group failed")
    }
}
```

The `domain.GroupRepository` satisfies `membershipStore` (it has `Members` and `SetMembers`), so `addGroupMember` accepts it directly. No new dependencies.

---

### 4. `internal/service/oauth.go` — Change default group logic in `completeOAuthUser`

**Current code** (lines 65-76):
```go
func completeOAuthUser(ctx context.Context, users *UserService, groups domain.GroupRepository, jwt *JWTService, logger *logrus.Logger, provider, oauthID, username, defaultGroup string, mappedGroups []string) (access, refresh string, u *domain.User, err error) {
    u, created, err := users.EnsureOAuthUser(ctx, provider, oauthID, username)
    if err != nil {
        return "", "", nil, err
    }
    if created && defaultGroup != "" {
        joinDefaultGroup(ctx, groups, defaultGroup, u.ID, logger)
    }
    joinMappedGroups(ctx, groups, mappedGroups, u.ID, logger)
    gids, _ := groups.GroupsForUser(ctx, u.ID)
    access, refresh, err = jwt.IssuePair(u, groupIDs(gids))
    return access, refresh, u, err
}
```

**New code** — replace the `if created && defaultGroup != ""` block with post-mapping zero-membership check:

```go
func completeOAuthUser(ctx context.Context, users *UserService, groups domain.GroupRepository, jwt *JWTService, logger *logrus.Logger, provider, oauthID, username, defaultGroup string, mappedGroups []string) (access, refresh string, u *domain.User, err error) {
    u, _, err := users.EnsureOAuthUser(ctx, provider, oauthID, username)
    if err != nil {
        return "", "", nil, err
    }
    joinMappedGroups(ctx, groups, mappedGroups, u.ID, logger)
    gids, _ := groups.GroupsForUser(ctx, u.ID)
    // If the user still has zero group memberships after mapping rules ran,
    // add them to the hardcoded "default" group (on every login, not just first
    // creation). This ensures every OAuth user can always provision engines.
    if len(gids) == 0 {
        joinGroupByName(ctx, groups, "default", u.ID, logger)
        gids, _ = groups.GroupsForUser(ctx, u.ID)
    }
    access, refresh, err = jwt.IssuePair(u, groupIDs(gids))
    return access, refresh, u, err
}
```

**Key changes:**
- `created` variable removed (no longer used)
- `defaultGroup` parameter kept but unused (preserves function signature; `oauth_github.go` and `oauth_oidc.go` callers unchanged)
- Old `joinDefaultGroup` function becomes dead code — **remove it** to satisfy `golangci-lint unused` linter
- `joinGroupByName` is reused with hardcoded `"default"` name

**Remove dead code**: Delete `joinDefaultGroup` (lines 34-37 in `oauth.go`):
```go
// REMOVE:
func joinDefaultGroup(ctx context.Context, groups domain.GroupRepository, defaultGroup, userID string, logger *logrus.Logger) {
    joinGroupByName(ctx, groups, defaultGroup, userID, logger)
}
```

---

### 5. `internal/service/oauth_github.go` and `internal/service/oauth_oidc.go`

**No changes needed.** Both already pass `s.defaultGroup` (the `auth.oauth.default_group` config value) to `completeOAuthUser`. The parameter is now ignored inside `completeOAuthUser`, but the call sites remain unchanged. This preserves backward compatibility for existing configs.

---

### 6. Documentation updates

#### `docs/README.md`

Add a section under "Authentication" or "Groups" documenting the auto-created default group:

```markdown
### Default group

On first boot with an empty groups table, the supervisor auto-creates a system
group named `default` with `AgentAvailable=true` and unlimited runner sessions.
Every user that would otherwise have zero group memberships is automatically
added to this group:

- **Internal users** (created via the admin API): assigned on creation.
- **OAuth users**: assigned on every login if no group-mapping rules matched.
- **Existing users at upgrade time**: swept into the default group on first
  boot when the groups table was previously empty.

Admins can rename, reconfigure, or delete the `default` group through the
admin UI/API after bootstrap. If deleted, users with no other groups will not
be able to provision engines until manually assigned to a group.
```

---

## Test Plan

### `internal/service/group_service_test.go`

Add `TestGroupServiceEnsureMember`:
- Create a group and a user via the service helpers.
- Call `EnsureMember(ctx, groupID, userID)` — verify user appears in `Members()`.
- Call `EnsureMember` again with same args — verify idempotent (no duplicate, no error).
- Call `EnsureMember` with non-existent group ID — verify `domain.ErrNotFound`.
- Call `EnsureMember` with non-existent user ID — verify `domain.ErrNotFound`.

### `internal/service/user_service_test.go`

Add `TestUserServiceCreateAutoAssignDefault`:
- Create a "default" group via `GroupService.Create`.
- Create a user via `UserService.Create`.
- Verify the user is a member of the "default" group via `GroupService.Members`.
- Verify the user has exactly one membership.

Add `TestUserServiceCreateNoDefaultGroup`:
- Do NOT create a "default" group.
- Create a user via `UserService.Create`.
- Verify user creation succeeds (no error).
- Verify user has zero group memberships.

### `internal/service/oauth_test.go` (new or existing)

Add `TestCompleteOAuthUserFallbackToDefault`:
- Create a "default" group.
- Call `completeOAuthUser` with a user that has no mapped groups.
- Verify the user is a member of the "default" group.
- Verify JWT claims include the default group ID.

Add `TestCompleteOAuthUserWithMappedGroups`:
- Create a mapped group and a "default" group.
- Call `completeOAuthUser` with mapping rules that match.
- Verify user is a member of the mapped group.
- Verify user is NOT a member of the "default" group (mapping rules matched).

Add `TestCompleteOAuthUserNoDefaultGroupExists`:
- Do NOT create a "default" group.
- Call `completeOAuthUser` with a user that has no mapped groups.
- Verify user creation and login succeed (no error).
- Verify user has zero memberships.

### `config/loader_test.go`

No new tests needed (no config changes).

### Integration test

In `tests/integration/`, add a test that:
1. Starts the supervisor with an empty Raft store.
2. Verifies `GET /api/v1/groups` returns a group named "default" with `agent_available: true`.
3. Creates an internal user via `POST /api/v1/users`.
4. Verifies `GET /api/v1/groups/default/members` includes the new user.

---

## Migration / Upgrade Path

1. **Fresh installs**: The default group is created on first boot. All users (bootstrap admin, OAuth users, internal users) get auto-assigned.
2. **Upgrades from a version without this feature**: On first boot after upgrade, if the groups table is empty (no groups existed before), the default group is created and ALL existing users with zero memberships are swept into it. If groups already exist (admin created some), the bootstrap is skipped — the admin is responsible for group management.
3. **Existing `auth.oauth.default_group` config**: This field is now ignored. The hardcoded "default" group replaces it. Operators can remove the config key; leaving it is harmless.
4. **Backward compatibility**: OAuth services still pass `defaultGroup` to `completeOAuthUser`; the parameter is accepted but unused. No signature changes in the OAuth provider constructors.

---

## Verification Checklist

- [ ] `go build ./...` passes
- [ ] `go vet ./...` passes
- [ ] `go test -race -covermode=atomic ./...` passes with 100% coverage on changed packages
- [ ] `golangci-lint run` passes (no unused symbols)
- [ ] Full CI gate: `dagger call -m ./dagger --src . ci export --path out` passes
- [ ] Manual test on local cluster (`AGENTS.local.md`):
  - [ ] Fresh install → `GET /api/v1/groups` shows "default" group
  - [ ] Bootstrap admin is a member of "default" group
  - [ ] Create internal user via API → auto-assigned to "default"
  - [ ] OAuth login with no mapping rules → auto-assigned to "default"
  - [ ] OAuth login with mapping rules that match → assigned to mapped group(s), NOT "default"
  - [ ] Delete "default" group → restart supervisor → group is NOT re-created (groups table is not empty)
  - [ ] Engine provision succeeds for a user in "default" group

---

## Open Questions

None. All design decisions resolved.

| Decision | Resolution |
|----------|------------|
| Config location | Fully hardcoded — no config section |
| Group name | Always `"default"` |
| Group attributes | `AgentAvailable=true`, `MaxRunnerSessions=0` |
| Bootstrap trigger | Groups table empty (`List()` returns 0) |
| Migration sweep | Runs on first bootstrap when groups table was empty |
| OAuth fallback | Every login, not just first creation |
| Old `default_group` config | Ignored; parameter kept for backward compat |
| Leader-only safety | Same pre-existing pattern as `bootstrapAdmin` |
