package service

import (
	"context"
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
	// domain.ErrSessionRevoked when the credential is invalid/expired beyond
	// refresh (user must re-login) and domain.ErrForbidden when membership no
	// longer satisfies the allowlist.
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
	mappedGroups []string,
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
	if oauthGids, rerr := reconcileMemberships(ctx, groups, logger, u, mappedGroups); rerr != nil {
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
// memberships for u: add memberships for names that newly resolve, remove
// memberships for previously OAuth-managed names that no longer resolve.
// Admin-managed memberships (groups not in u.OAuthGroupIDs) are never touched.
// Returns the resulting supervisor group IDs.
func reconcileMemberships(
	ctx context.Context,
	groups domain.GroupRepository,
	logger *logrus.Logger,
	u *domain.User,
	mappedNames []string,
) ([]string, error) {
	wanted := make(map[string]bool, len(mappedNames))
	for _, name := range mappedNames {
		g, err := groups.GetByName(ctx, name)
		if err != nil {
			logger.WithError(err).WithField("group", name).Warn("oauth: group not found during revalidation, skipping")
			continue
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
