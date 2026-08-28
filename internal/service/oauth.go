package service

import (
	"context"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// OAuthProvider is the single active OAuth provider. Implementations:
// GitHubOAuthService (provider: github) and OIDCOAuthService (provider: oidc).
type OAuthProvider interface {
	LoginURL(state string) string
	Complete(ctx context.Context, code string) (access, refresh string, u *domain.User, err error)
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

// joinMappedGroups adds userID to each supervisor group named in mappedGroups
// that exists (never fatal, never auto-creates groups).
func joinMappedGroups(ctx context.Context, groups domain.GroupRepository, mappedGroups []string, userID string, logger *logrus.Logger) {
	for _, name := range mappedGroups {
		joinGroupByName(ctx, groups, name, userID, logger)
	}
}

// completeOAuthUser is the shared post-verification tail for both OAuth
// providers: ensure the local user exists, add mapped supervisor groups, fall
// back to the configured default_group (or "default") when the user still has zero
// memberships, and issue a JWT pair.
func completeOAuthUser(ctx context.Context, users *UserService, groups domain.GroupRepository, jwt *JWTService, logger *logrus.Logger, provider, oauthID, username, defaultGroup string, mappedGroups []string) (access, refresh string, u *domain.User, err error) {
	u, _, err = users.EnsureOAuthUser(ctx, provider, oauthID, username)
	if err != nil {
		return "", "", nil, err
	}
	joinMappedGroups(ctx, groups, mappedGroups, u.ID, logger)
	gids, _ := groups.GroupsForUser(ctx, u.ID)
	// If the user still has zero group memberships after mapping rules ran,
	// add them to the configured default_group (falling back to "default")
	// on every login, not just first creation. This ensures every OAuth user
	// can always provision engines.
	fallbackGroup := defaultGroup
	if fallbackGroup == "" {
		fallbackGroup = "default"
	}
	if len(gids) == 0 {
		joinGroupByName(ctx, groups, fallbackGroup, u.ID, logger)
		gids, _ = groups.GroupsForUser(ctx, u.ID)
	}
	access, refresh, err = jwt.IssuePair(u, groupIDs(gids))
	return access, refresh, u, err
}
