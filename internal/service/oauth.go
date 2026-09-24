package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// OAuthProvider is the single active OAuth provider. Implementations:
// GitHubOAuthService (provider: github) and OIDCOAuthService (provider: oidc).
type OAuthProvider interface {
	LoginURL(state string) string
	Complete(ctx context.Context, code string) (access, refresh string, u *domain.User, err error)
	// Revalidate re-checks the user's current IdP group membership using the
	// stored credential and returns the current provider group names. Returns
	// errOAuthCredentialExpired (OIDC: the credential is unusable — expired,
	// rotated, or revoked are indistinguishable, so this is "cannot verify",
	// not proof of revocation) or domain.ErrSessionRevoked (GitHub: 401/404 on
	// its never-expiring access token = genuine revocation) when the credential
	// can no longer be used, and domain.ErrForbidden when membership no longer
	// satisfies the allowlist.
	Revalidate(ctx context.Context, u *domain.User) ([]string, error)
}

// orgsIntersect reports whether any element of allowed is present in have.
func orgsIntersect(allowed, have []string) bool {
	hset := make(map[string]struct{}, len(have))
	for _, h := range have {
		hset[h] = struct{}{}
	}
	for _, a := range allowed {
		if _, ok := hset[a]; ok {
			return true
		}
	}
	return false
}

// normalizeGroupList de-duplicates (preserving first occurrence, dropping
// empty strings) and sorts a group-name list for stable storage and display.
// Empty input yields nil so the omitempty JSON tag keeps the field absent.
func normalizeGroupList(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// applyOAuthAdminRole promotes/demotes u's role based on whether any RAW
// upstream group matches adminGroups (exact, case-sensitive via orgsIntersect).
//   - match + u is not admin      -> promote to RoleAdmin, OAuthAdmin=true.
//   - no match + OAuthAdmin       -> demote to RoleUser, OAuthAdmin=false.
//   - match + already admin        -> leave unchanged (do NOT take ownership of
//     a manually-promoted admin).
//   - no match + not OAuthAdmin    -> leave unchanged (manual admin or user).
//
// Returns true when the role changed (caller logs it).
func applyOAuthAdminRole(u *domain.User, adminGroups, upstreamGroups []string) bool {
	if len(adminGroups) == 0 {
		return false
	}
	match := orgsIntersect(adminGroups, upstreamGroups)
	if match && u.Role != domain.RoleAdmin {
		u.Role = domain.RoleAdmin
		u.OAuthAdmin = true
		return true
	}
	if !match && u.OAuthAdmin {
		u.Role = domain.RoleUser
		u.OAuthAdmin = false
		return true
	}
	return false
}

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
) (*domain.Group, error) {
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

// joinGroupByName best-effort adds userID to the named group. Missing groups
// and membership errors are logged (never fatal). It serves both the mapped
// (group_mappings) and default_group auto-join paths, so the log message is
// worded to cover both.
func joinGroupByName(ctx context.Context, groups domain.GroupRepository, name, userID string, logger *logrus.Logger) {
	g, err := groups.GetByName(ctx, name)
	if err != nil {
		logger.WithError(err).WithField("group", name).Warn("oauth: group not found, skipping")
		return
	}
	if err := addGroupMember(ctx, groups, g.ID, userID); err != nil {
		logger.WithError(err).WithField("group", name).Warn("oauth: group auto-join failed")
	}
}

// completeOAuthLogin is the shared post-verification tail for both OAuth
// providers: ensure the local user exists, clear any prior deactivation,
// reconcile OAuth-managed memberships, fall back to the configured default_group
// (or "default") when the user still has zero memberships, persist the stored
// credential, and issue a JWT pair.
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
) (access, refresh string, u *domain.User, err error) {
	u, _, err = users.EnsureOAuthUser(ctx, provider, oauthID, username)
	if err != nil {
		return "", "", nil, err
	}

	// A successful IdP login re-authorizes: clear any prior deactivation.
	if u.DeactivatedAt != nil {
		u.DeactivatedAt = nil
	}

	// Reconcile OAuth-managed memberships: add new, remove stale. Called even
	// when mappedGroups is empty so memberships that no longer map are removed.
	if oauthGids, rerr := reconcileMemberships(ctx, groups, logger, u, mappedGroups, mappedGroupMaxRunnerSessions); rerr != nil {
		logger.WithError(rerr).WithField("user_id", u.ID).Warn("oauth: membership reconciliation failed")
	} else {
		u.OAuthGroupIDs = oauthGids
	}

	memberGroups, _ := groups.GroupsForUser(ctx, u.ID)
	// If the user still has zero group memberships after mapping rules ran,
	// add them to the configured default_group (falling back to "default")
	// on every login, not just first creation. This ensures every OAuth user
	// can always provision engines.
	fallbackGroup := defaultGroup
	if fallbackGroup == "" {
		fallbackGroup = "default"
	}
	if len(memberGroups) == 0 {
		joinGroupByName(ctx, groups, fallbackGroup, u.ID, logger)
		memberGroups, _ = groups.GroupsForUser(ctx, u.ID)
	}

	// Persist upstream OAuth groups for display.
	u.OAuthGroups = normalizeGroupList(upstreamGroups)

	// Apply admin_groups role promotion/demotion.
	if roleChanged := applyOAuthAdminRole(u, adminGroups, upstreamGroups); roleChanged {
		logger.WithFields(logrus.Fields{
			"user_id":        u.ID,
			"oauth_provider": provider,
			"role":           u.Role,
			"oauth_admin":    u.OAuthAdmin,
		}).Info("oauth: admin role changed via admin_groups")
	}

	logger.WithFields(logrus.Fields{
		"user_id":         u.ID,
		"oauth_provider":  provider,
		"upstream_groups": upstreamGroups,
		"mapped_groups":   mappedGroups,
		"oauth_group_ids": u.OAuthGroupIDs,
	}).Info("oauth: group mapping applied")

	// Persist the encrypted credential.
	ct, err := encryptOAuthCredential(encKey, credential)
	if err != nil {
		return "", "", nil, fmt.Errorf("encrypt oauth credential: %w", err)
	}
	u.OAuthTokenCiphertext = ct

	if err := users.Update(ctx, u); err != nil {
		return "", "", nil, fmt.Errorf("persist oauth user: %w", err)
	}

	access, refresh, err = jwt.IssuePair(u, groupIDs(memberGroups))
	return access, refresh, u, err
}

// reconcileMemberships applies the desired OAuth-managed supervisor group
// memberships for u: add memberships for names that newly resolve (auto-creating
// the group when missing, see ensureGroupByName), remove memberships for
// previously OAuth-managed names that no longer resolve. Admin-managed
// memberships (groups not in u.OAuthGroupIDs) are never touched. Returns the
// resulting supervisor group IDs.
func reconcileMemberships(
	ctx context.Context,
	groups domain.GroupRepository,
	logger *logrus.Logger,
	u *domain.User,
	mappedNames []string,
	defaultMaxRunnerSessions int,
) ([]string, error) {
	wanted := make(map[string]bool, len(mappedNames))
	for _, name := range mappedNames {
		g, err := ensureGroupByName(ctx, groups, name, defaultMaxRunnerSessions, logger)
		if err != nil {
			logger.WithError(err).WithField("group", name).Warn("oauth: ensure mapped group failed, skipping")
			continue
		}
		if g == nil {
			continue // invalid name (already logged)
		}
		wanted[g.ID] = true
	}

	previous := make(map[string]bool, len(u.OAuthGroupIDs))
	for _, gid := range u.OAuthGroupIDs {
		previous[gid] = true
	}

	// adds
	for gid := range wanted {
		if !previous[gid] {
			if err := addGroupMember(ctx, groups, gid, u.ID); err != nil {
				return nil, fmt.Errorf("add oauth group %s: %w", gid, err)
			}
		}
	}
	// removes (only within the previously OAuth-managed set)
	for gid := range previous {
		if !wanted[gid] {
			if err := removeGroupMember(ctx, groups, gid, u.ID); err != nil {
				return nil, fmt.Errorf("remove oauth group %s: %w", gid, err)
			}
		}
	}

	out := make([]string, 0, len(wanted))
	for gid := range wanted {
		out = append(out, gid)
	}
	sort.Strings(out)
	u.OAuthGroupIDs = out
	return out, nil
}
