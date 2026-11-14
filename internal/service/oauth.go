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

// resolveOAuthGroupIDs resolves the desired OAuth-managed supervisor group IDs
// from mapped upstream names, auto-creating valid missing groups (see
// ensureGroupByName). An invalid mapped name is logged and skipped; a store
// lookup/creation failure aborts preparation so the caller never commits a
// partial membership set. The result is de-duplicated and sorted.
func resolveOAuthGroupIDs(
	ctx context.Context,
	groups domain.GroupRepository,
	logger *logrus.Logger,
	mappedNames []string,
	defaultMaxRunnerSessions int,
) ([]string, error) {
	seen := make(map[string]struct{}, len(mappedNames))
	out := make([]string, 0, len(mappedNames))
	for _, name := range mappedNames {
		g, err := ensureGroupByName(ctx, groups, name, defaultMaxRunnerSessions, logger)
		if err != nil {
			return nil, err
		}
		if g == nil {
			continue // invalid name (already logged)
		}
		if _, dup := seen[g.ID]; dup {
			continue
		}
		seen[g.ID] = struct{}{}
		out = append(out, g.ID)
	}
	sort.Strings(out)
	return out, nil
}

// resolveFallbackGroupID resolves the configured default_group (falling back to
// "default") to a group ID without creating it. A missing group yields an empty
// ID (no fallback); an operational store error is surfaced.
func resolveFallbackGroupID(ctx context.Context, groups domain.GroupRepository, defaultGroup string) (string, error) {
	name := defaultGroup
	if name == "" {
		name = "default"
	}
	g, err := groups.GetByName(ctx, name)
	if err == nil {
		return g.ID, nil
	}
	if errors.Is(err, domain.ErrNotFound) {
		return "", nil
	}
	return "", fmt.Errorf("resolve default group %q: %w", name, err)
}

// completeOAuthLogin is the shared post-verification tail for both OAuth
// providers. It ensures the local user exists, prepares the desired OAuth-managed
// membership set (auto-creating mapped groups) and the default-group fallback,
// then applies the login as ONE revision-checked atomic commit. No membership
// write happens before the CAS, and no JWT is issued on a rejected commit.
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

	// Prepare the desired OAuth-managed memberships and the fallback BEFORE the
	// commit. Group creation is allowed here; membership writes are not.
	managedIDs, err := resolveOAuthGroupIDs(ctx, groups, logger, mappedGroups, mappedGroupMaxRunnerSessions)
	if err != nil {
		return "", "", nil, fmt.Errorf("prepare oauth groups: %w", err)
	}
	fallbackID, err := resolveFallbackGroupID(ctx, groups, defaultGroup)
	if err != nil {
		return "", "", nil, err
	}

	// A successful IdP login re-authorizes: it clears ONLY IdP deactivation.
	// applyOAuthAdminRole mutates u (a private copy returned by the repository).
	if roleChanged := applyOAuthAdminRole(u, adminGroups, upstreamGroups); roleChanged {
		logger.WithFields(logrus.Fields{
			"user_id":        u.ID,
			"oauth_provider": provider,
			"role":           u.Role,
			"oauth_admin":    u.OAuthAdmin,
		}).Info("oauth: admin role changed via admin_groups")
	}

	ct, err := encryptOAuthCredential(encKey, credential)
	if err != nil {
		return "", "", nil, fmt.Errorf("encrypt oauth credential: %w", err)
	}

	res, err := users.CommitOAuth(ctx, domain.OAuthUserMutation{
		Kind: domain.OAuthLoginCommit,
		User: &domain.User{
			ID:                   u.ID,
			OAuthProvider:        provider,
			OAuthID:              oauthID,
			Role:                 u.Role,
			OAuthAdmin:           u.OAuthAdmin,
			OAuthGroups:          normalizeGroupList(upstreamGroups),
			OAuthTokenCiphertext: ct,
		},
		ExpectedRevision: u.Revision,
		ManagedGroupIDs:  managedIDs,
		FallbackGroupID:  fallbackID,
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("persist oauth login: %w", err)
	}

	logger.WithFields(logrus.Fields{
		"user_id":         res.User.ID,
		"oauth_provider":  provider,
		"upstream_groups": upstreamGroups,
		"mapped_groups":   mappedGroups,
		"oauth_group_ids": res.User.OAuthGroupIDs,
	}).Info("oauth: group mapping applied")

	access, refresh, err = jwt.IssuePair(res.User, res.GroupIDs)
	if err != nil {
		return "", "", nil, fmt.Errorf("issue oauth session: %w", err)
	}
	return access, refresh, res.User, nil
}
