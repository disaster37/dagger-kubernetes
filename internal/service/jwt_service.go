package service

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// Claims is the JWT body issued by the supervisor.
type Claims struct {
	UserID   string   `json:"uid"`
	Username string   `json:"username"`
	Role     string   `json:"role"`
	GroupIDs []string `json:"groups"`
	Type     string   `json:"typ"` // "access" | "refresh" | "oauth_state"
	Nonce    string   `json:"nonce,omitempty"`
	jwt.RegisteredClaims
}

const (
	typAccess     = "access"
	typRefresh    = "refresh"
	typOAuthState = "oauth_state"
	oauthStateTTL = 10 * time.Minute
)

// JWTService issues and parses HS256 JWTs.
type JWTService struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	clock      func() time.Time
}

// NewJWTService returns a JWTService.
func NewJWTService(secret []byte, accessTTL, refreshTTL time.Duration) *JWTService {
	return &JWTService{
		secret:     secret,
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
		clock:      func() time.Time { return time.Now().UTC() },
	}
}

// IssuePair returns signed access + refresh tokens for the user.
func (s *JWTService) IssuePair(u *domain.User, groupIDs []string) (access, refresh string, err error) {
	access, err = s.issue(u, groupIDs, typAccess, s.accessTTL)
	if err != nil {
		return "", "", fmt.Errorf("issue access: %w", err)
	}
	refresh, err = s.issue(u, groupIDs, typRefresh, s.refreshTTL)
	if err != nil {
		return "", "", fmt.Errorf("issue refresh: %w", err)
	}
	return access, refresh, nil
}

// ParseAccess validates the signature, expiry, and typ=="access".
func (s *JWTService) ParseAccess(token string) (*Claims, error) {
	return s.parse(token, typAccess, "parse access token")
}

// ParseRefresh validates the signature, expiry, and typ=="refresh".
func (s *JWTService) ParseRefresh(token string) (*Claims, error) {
	return s.parse(token, typRefresh, "parse refresh token")
}

// ParseOAuthState validates an OAuth state token.
func (s *JWTService) ParseOAuthState(token string) (*Claims, error) {
	return s.parse(token, typOAuthState, "parse oauth state")
}

// IssueOAuthState issues a short-lived state token for the OAuth flow. The
// redirect path is carried in the Username claim and the login-CSRF nonce in
// the Nonce claim; the callback must present the matching nonce cookie.
func (s *JWTService) IssueOAuthState(redirectPath, nonce string) (string, error) {
	claims := s.claims(typOAuthState, oauthStateTTL)
	claims.Username = redirectPath
	claims.Nonce = nonce
	tok, err := s.sign(claims)
	if err != nil {
		return "", fmt.Errorf("sign oauth state: %w", err)
	}
	return tok, nil
}

func (s *JWTService) issue(u *domain.User, groupIDs []string, typ string, ttl time.Duration) (string, error) {
	claims := s.claims(typ, ttl)
	claims.UserID = u.ID
	claims.Username = u.Username
	claims.Role = string(u.Role)
	claims.GroupIDs = groupIDs
	return s.sign(claims)
}

// claims builds the base claims with iat/exp around the service clock.
func (s *JWTService) claims(typ string, ttl time.Duration) *Claims {
	now := s.clock()
	return &Claims{
		Type: typ,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
}

func (s *JWTService) sign(claims *Claims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

func (s *JWTService) parse(token, wantTyp, label string) (*Claims, error) {
	claims := &Claims{}
	if _, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (interface{}, error) {
		return s.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"})); err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if claims.Type != wantTyp {
		return nil, fmt.Errorf("%s: unexpected token type %q, want %q", label, claims.Type, wantTyp)
	}
	return claims, nil
}
