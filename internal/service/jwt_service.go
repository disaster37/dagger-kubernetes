package service

import (
	"fmt"
	"time"

	goauthdomain "github.com/disaster37/go-auth/domain"
	goauthjwt "github.com/disaster37/go-auth/jwt"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// Claims is the JWT body issued by the supervisor. It is the go-auth wire
// format (HS256, TokenVersion, optional AuthEpoch).
type Claims = goauthjwt.Claims

// JWTService issues and parses HS256 JWTs via go-auth. UseHKDF is false so the
// raw configured secret signs tokens, preserving active cluster sessions across
// the upgrade.
type JWTService struct {
	svc *goauthjwt.Service
}

// NewJWTService returns a JWTService. A secret shorter than the go-auth minimum
// is a startup invariant violation (validated by jwt.ValidateSecret in
// cmd/api); it panics rather than silently issuing weak tokens.
func NewJWTService(secret []byte, accessTTL, refreshTTL time.Duration) *JWTService {
	svc, err := goauthjwt.NewService(goauthjwt.Config{
		Secret:     secret,
		UseHKDF:    false,
		AccessTTL:  accessTTL,
		RefreshTTL: refreshTTL,
	})
	if err != nil {
		panic(fmt.Sprintf("new jwt service: %v", err))
	}
	return &JWTService{svc: svc}
}

// AccessTTL returns the configured access-token TTL.
func (s *JWTService) AccessTTL() time.Duration { return s.svc.AccessTTL() }

// RefreshTTL returns the configured refresh-token TTL.
func (s *JWTService) RefreshTTL() time.Duration { return s.svc.RefreshTTL() }

// IssuePair returns signed access + refresh tokens for the user.
func (s *JWTService) IssuePair(u *domain.User, groupIDs []string) (access, refresh string, err error) {
	return s.svc.IssuePair(toGoAuthUser(u), groupIDs, 0)
}

// ParseAccess validates the signature, expiry, and typ=="access".
func (s *JWTService) ParseAccess(token string) (*Claims, error) {
	return s.svc.ParseAccess(token)
}

// ParseRefresh validates the signature, expiry, and typ=="refresh".
func (s *JWTService) ParseRefresh(token string) (*Claims, error) {
	return s.svc.ParseRefresh(token)
}

// ParseOAuthState validates an OAuth state token.
func (s *JWTService) ParseOAuthState(token string) (*Claims, error) {
	return s.svc.ParseOAuthState(token)
}

// IssueOAuthState issues a short-lived state token for the OAuth flow. The
// redirect path is carried in the Username claim and the login-CSRF nonce in
// the Nonce claim; the callback must present the matching nonce cookie.
func (s *JWTService) IssueOAuthState(redirectPath, nonce string) (string, error) {
	return s.svc.IssueOAuthState(redirectPath, nonce, "", "")
}

// toGoAuthUser adapts the supervisor's domain.User to the go-auth user shape
// used for JWT issuance.
func toGoAuthUser(u *domain.User) *goauthdomain.User {
	return &goauthdomain.User{
		ID:           u.ID,
		Username:     u.Username,
		Role:         goauthdomain.Role(u.Role),
		TokenVersion: u.TokenVersion,
	}
}
