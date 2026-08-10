package service

import (
	"context"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// AuthServiceConfig configures the AuthService resolution behavior.
type AuthServiceConfig struct {
	Disabled bool // auth.internal.enabled == false
}

// AuthService resolves bearer tokens to identities and handles login/refresh.
type AuthService struct {
	cfg    AuthServiceConfig
	users  *UserService
	groups domain.GroupRepository
	tokens *TokenService
	jwt    *JWTService
	legacy domain.TokenValidator // nil when no tokens_file
	logger *logrus.Logger
}

// NewAuthService returns an AuthService. legacy may be nil when no tokens_file
// is configured.
func NewAuthService(
	cfg AuthServiceConfig,
	users *UserService,
	groups domain.GroupRepository,
	tokens *TokenService,
	jwtSvc *JWTService,
	legacy domain.TokenValidator,
	logger *logrus.Logger,
) *AuthService {
	return &AuthService{
		cfg:    cfg,
		users:  users,
		groups: groups,
		tokens: tokens,
		jwt:    jwtSvc,
		legacy: legacy,
		logger: logger,
	}
}

// Resolve maps a bearer token to an Identity. Resolution order:
// 1. auth disabled -> anonymous admin
// 2. empty bearer -> ErrUnauthenticated
// 3. dct_ prefix -> API token
// 4. JWT access token
// 5. legacy flat-file fallback
// 6. ErrUnauthenticated
func (a *AuthService) Resolve(ctx context.Context, bearer string) (*domain.Identity, error) {
	if a.cfg.Disabled {
		return &domain.Identity{
			UserID:   "anonymous",
			Username: "anonymous",
			Role:     domain.RoleAdmin,
			Method:   domain.AuthNone,
		}, nil
	}
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
// ErrUnauthenticated.
func (a *AuthService) identityForUser(ctx context.Context, userID string, method domain.AuthMethod) (*domain.Identity, error) {
	u, err := a.users.Get(ctx, userID)
	if err != nil {
		a.logger.WithError(err).Debug("resolved user missing")
		return nil, domain.ErrUnauthenticated
	}
	gids, _ := a.groups.GroupsForUser(ctx, u.ID)
	return &domain.Identity{
		UserID:   u.ID,
		Username: u.Username,
		Role:     u.Role,
		GroupIDs: groupIDs(gids),
		Method:   method,
	}, nil
}

// Login authenticates a user and issues a fresh JWT pair.
func (a *AuthService) Login(ctx context.Context, username, password string) (access, refresh string, u *domain.User, err error) {
	u, err = a.users.Authenticate(ctx, username, password)
	if err != nil {
		a.logger.WithField("username", username).Debug("login failed")
		return "", "", nil, err
	}
	access, refresh, err = a.issuePairForUser(ctx, u)
	if err != nil {
		return "", "", nil, err
	}
	a.logger.WithField("username", username).Info("login succeeded")
	return access, refresh, u, nil
}

// Refresh validates a refresh token, reloads the user, and issues a new pair
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
	return a.issuePairForUser(ctx, u)
}

// issuePairForUser issues a JWT pair with the user's current group membership.
func (a *AuthService) issuePairForUser(ctx context.Context, u *domain.User) (access, refresh string, err error) {
	gids, _ := a.groups.GroupsForUser(ctx, u.ID)
	return a.jwt.IssuePair(u, groupIDs(gids))
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
