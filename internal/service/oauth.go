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

// joinDefaultGroup best-effort adds userID to the configured default group.
// Missing groups and membership errors are logged (never fatal) — the user is
// still logged in, just not auto-joined.
func joinDefaultGroup(ctx context.Context, groups domain.GroupRepository, defaultGroup, userID string, logger *logrus.Logger) {
	g, err := groups.GetByName(ctx, defaultGroup)
	if err != nil {
		logger.WithError(err).WithField("group", defaultGroup).Warn("oauth: default group not found")
		return
	}
	if err := addGroupMember(ctx, groups, g.ID, userID); err != nil {
		logger.WithError(err).WithField("group", defaultGroup).Warn("oauth: default group auto-join failed")
	}
}

// completeOAuthUser is the shared post-verification tail for both OAuth
// providers: ensure the local user exists, auto-join the default group on
// first login, and issue a JWT pair.
func completeOAuthUser(ctx context.Context, users *UserService, groups domain.GroupRepository, jwt *JWTService, logger *logrus.Logger, provider, oauthID, username, defaultGroup string) (access, refresh string, u *domain.User, err error) {
	u, created, err := users.EnsureOAuthUser(ctx, provider, oauthID, username)
	if err != nil {
		return "", "", nil, err
	}
	if created && defaultGroup != "" {
		joinDefaultGroup(ctx, groups, defaultGroup, u.ID, logger)
	}
	gids, _ := groups.GroupsForUser(ctx, u.ID)
	access, refresh, err = jwt.IssuePair(u, groupIDs(gids))
	return access, refresh, u, err
}
