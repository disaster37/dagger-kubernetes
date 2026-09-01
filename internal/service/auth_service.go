package service

import (
	"context"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// AuthService resolves bearer tokens to identities and handles login/refresh.
type AuthService struct {
	users       *UserService
	groups      domain.GroupRepository
	tokens      *TokenService
	jwt         *JWTService
	legacy      domain.TokenValidator // nil when no tokens_file
	logger      *logrus.Logger
	revalidator *OAuthRevalidator // nil = revalidation disabled
}

// NewAuthService returns an AuthService. legacy may be nil when no tokens_file
// is configured.
func NewAuthService(
	users *UserService,
	groups domain.GroupRepository,
	tokens *TokenService,
	jwtSvc *JWTService,
	legacy domain.TokenValidator,
	logger *logrus.Logger,
) *AuthService {
	return &AuthService{
		users:  users,
		groups: groups,
		tokens: tokens,
		jwt:    jwtSvc,
		legacy: legacy,
		logger: logger,
	}
}

// SetOAuthRevalidator wires the OAuth membership revalidator. nil (default)
// disables revalidation; only wired when OAuth is the configured provider.
func (a *AuthService) SetOAuthRevalidator(r *OAuthRevalidator) { a.revalidator = r }

// Resolve maps a bearer token to an Identity. Resolution order:
// 1. empty bearer -> ErrUnauthenticated
// 2. dct_ prefix -> API token
// 3. JWT access token
// 4. legacy flat-file fallback
// 5. ErrUnauthenticated
func (a *AuthService) Resolve(ctx context.Context, bearer string) (*domain.Identity, error) {
	if bearer == "" {
		return nil, domain.ErrUnauthenticated
	}

	if strings.HasPrefix(bearer, "dct_") {
		tok, err := a.tokens.Validate(ctx, bearer)
		if err != nil {
			a.logger.WithError(err).Debug("api token validate failed")
			return nil, domain.ErrUnauthenticated
		}
		return a.identityForUser(ctx, tok.UserID, domain.AuthAPIToken)
	}

	if claims, err := a.jwt.ParseAccess(bearer); err == nil {
		return a.identityForUser(ctx, claims.UserID, domain.AuthJWT)
	}

	if a.legacy != nil {
		if _, err := a.legacy.ValidateToken(bearer); err == nil {
			return &domain.Identity{
				UserID:   "legacy",
				Username: "legacy",
				Role:     domain.RoleAdmin,
				Method:   domain.AuthLegacyTok,
			}, nil
		}
	}

	a.logger.Debug("unauthenticated: no matching credential")
	return nil, domain.ErrUnauthenticated
}

// identityForUser loads a fresh user + group membership from the DB (claims
// can be stale) and builds the Identity. A missing user yields
// ErrUnauthenticated. For OAuth users, IdP revalidation is enforced.
func (a *AuthService) identityForUser(ctx context.Context, userID string, method domain.AuthMethod) (*domain.Identity, error) {
	u, err := a.users.Get(ctx, userID)
	if err != nil {
		a.logger.WithError(err).Debug("resolved user missing")
		return nil, domain.ErrUnauthenticated
	}
	gids, err := a.loadAuthorizedGroups(ctx, u)
	if err != nil {
		return nil, err
	}
	return &domain.Identity{
		UserID:   u.ID,
		Username: u.Username,
		Role:     u.Role,
		GroupIDs: gids,
		Method:   method,
	}, nil
}

// loadAuthorizedGroups returns the user's current effective supervisor group
// IDs, enforcing deactivation and (for OAuth users) IdP revalidation. A
// non-nil error means DENY (unauthenticated).
func (a *AuthService) loadAuthorizedGroups(ctx context.Context, u *domain.User) ([]string, error) {
	if u.Deactivated() {
		a.logger.WithField("user_id", u.ID).Debug("user deactivated by IdP revalidation")
		return nil, domain.ErrUnauthenticated
	}
	if u.OAuthProvider != "" && a.revalidator != nil {
		gids, err := a.revalidator.Check(ctx, u)
		if err != nil {
			a.logger.WithError(err).WithFields(logrus.Fields{
				"user_id": u.ID, "oauth_provider": u.OAuthProvider,
			}).Warn("oauth revalidation denied")
			return nil, domain.ErrUnauthenticated
		}
		return gids, nil
	}
	gids, _ := a.groups.GroupsForUser(ctx, u.ID)
	return groupIDs(gids), nil
}

// Login authenticates a user and issues a fresh JWT pair.
func (a *AuthService) Login(ctx context.Context, username, password string) (access, refresh string, u *domain.User, err error) {
	u, err = a.users.Authenticate(ctx, username, password)
	if err != nil {
		a.logger.WithField("username", username).Debug("login failed")
		return "", "", nil, err
	}
	gids, _ := a.groups.GroupsForUser(ctx, u.ID)
	access, refresh, err = a.jwt.IssuePair(u, groupIDs(gids))
	if err != nil {
		return "", "", nil, err
	}
	a.logger.WithField("username", username).Info("login succeeded")
	return access, refresh, u, nil
}

// Refresh validates a refresh token, reloads the user, enforces session max age
// (for OAuth users), revalidates IdP membership, and issues a new pair
// (rotation).
func (a *AuthService) Refresh(ctx context.Context, refreshToken string) (access, refresh string, err error) {
	claims, err := a.jwt.ParseRefresh(refreshToken)
	if err != nil {
		return "", "", domain.ErrUnauthenticated
	}
	u, err := a.users.Get(ctx, claims.UserID)
	if err != nil {
		return "", "", domain.ErrUnauthenticated
	}

	// Session max-age backstop for OAuth users.
	if a.revalidator != nil && u.OAuthProvider != "" {
		if maxAge := a.revalidator.SessionMaxAge(); maxAge > 0 {
			if age := time.Since(claims.IssuedAt.Time); age > maxAge {
				a.logger.WithFields(logrus.Fields{
					"user_id": u.ID, "age": age, "max_age": maxAge,
				}).Warn("oauth session exceeded max age; re-login required")
				return "", "", domain.ErrSessionRevoked
			}
		}
	}

	gids, err := a.loadAuthorizedGroups(ctx, u)
	if err != nil {
		return "", "", err
	}
	return a.jwt.IssuePair(u, gids)
}

func groupIDs(gs []*domain.Group) []string {
	if len(gs) == 0 {
		return nil
	}
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.ID)
	}
	return out
}
